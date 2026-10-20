// Package drive implements the Google Drive provider.
//
// This file covers the parts that must exist before any Drive call can succeed:
// the OAuth token exchange, the narrow-scope consent flow, and the connection
// registry that persists an encrypted refresh token per tenant. The upload and
// download verbs land in the next milestone; the availability check is here
// because routing depends on it.
package drive

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/shovan04/storvia/internal/apperr"
	"github.com/shovan04/storvia/internal/persistence"
	"github.com/shovan04/storvia/internal/storage/model"
	"github.com/shovan04/storvia/internal/storage/policy"
)

// The single scope Storvia requests.
//
// Broadening this to the full drive scope would let the app read and modify
// unrelated user files, and would pull the project into Google's restricted-scope
// verification and security-assessment path. It is the one scope the MVP asks for.
const ScopeDriveFile = "https://www.googleapis.com/auth/drive.file"

// ScopeDriveAppData is the app-data scope.
//
// It lets Storvia store files in Drive's appDataFolder, which is visible only to
// Storvia and not to the user in Drive web or mobile. Files stored there do not
// appear in the user's My Drive, shared drives, or any folder listing.
const ScopeDriveAppData = "https://www.googleapis.com/auth/drive.appdata"

// Google OAuth and Drive endpoints. They are constants rather than configuration
// because a client must not be able to redirect an authorization code to an
// operator-chosen host.
//
// Google splits its OAuth web endpoints across two hosts, and the split is not
// obvious: the authorization endpoint is NOT under oauth2.googleapis.com.
//
//	oauth2.googleapis.com/token          - exchanging the code for tokens
//	oauth2.googleapis.com/revoke         - revoking a token
//	accounts.google.com/o/oauth2/v2/auth - sending the user to consent
//
// Deriving all three from one base looks tidier and is wrong: it yields
// oauth2.googleapis.com/auth, which is not a route. Google answers it with a 404,
// so the browser lands on an error page and the whole thing looks like a
// configuration problem rather than a wrong constant. The two hosts are kept
// separate here so the next reader does not "tidy" them back together.
const (
	oauthAPIBase   = "https://oauth2.googleapis.com"
	googleAccounts = "https://accounts.google.com"

	tokenEndpoint  = oauthAPIBase + "/token"
	revokeEndpoint = oauthAPIBase + "/revoke"
	authorizeURL   = googleAccounts + "/o/oauth2/v2/auth"

	driveFilesURL  = "https://www.googleapis.com/drive/v3/files"
	driveUploadURL = "https://www.googleapis.com/upload/drive/v3/files"
	// driveAboutURL reports the authenticated user. It is its own constant because
	// it is NOT under /files — appending "about" to driveFilesURL would produce
	// /drive/v3/files/about, which is not a route.
	driveAboutURL = "https://www.googleapis.com/drive/v3/about"
)

// Credentials are the Google OAuth client credentials, resolved from the
// environment at startup.
type Credentials struct {
	ClientID     string
	ClientSecret string
	RedirectURI  string
}

// ErrNotConfigured reports that Google credentials are absent.
//
// It is deliberately distinct from an unavailable Drive: a deployment without
// credentials must not be treated as an outage, because that would route every
// new upload into the fallback bucket on the first request.
var ErrNotConfigured = errors.New("drive: google oauth credentials are not configured")

// Config configures the Drive provider.
type Config struct {
	Credentials Credentials
	// Scopes is validated to contain only ScopeDriveFile and ScopeDriveAppData
	// at config load time.
	Scopes []string
	// RootFolderName is the visible folder Storvia manages in My Drive.
	// It is unused when StoreInAppDataFolder is true.
	RootFolderName string
	// StoreInAppDataFolder stores files in Drive's appDataFolder instead of a
	// visible folder in My Drive. Files there are invisible to the user in Drive
	// web and mobile, but Storvia can still read and delete them.
	StoreInAppDataFolder bool
	// PreflightTimeout bounds one read-only availability call.
	PreflightTimeout time.Duration
	// RefreshSkew is how long before expiry a token is proactively refreshed.
	RefreshSkew time.Duration
	// HTTPClient is shared so connections are reused across calls.
	HTTPClient *http.Client
}

// Client is the Drive provider.
type Client struct {
	cfg Config
	log *slog.Logger
	// configured records whether Google credentials were present at
	// construction. It is a field rather than a recomputation so the status
	// endpoint can report a missing deployment configuration without duplicating
	// the credential check.
	configured bool
	// faults gates deliberate failure injection for testing. See faults.go.
	faults FaultInjection

	// tokens caches short-lived access tokens per connection. It is an
	// interface, so a multi-instance deployment can put the shared cache behind
	// it instead of this process-local map.
	tokens TokenCache

	// mu serializes refresh per connection.
	//
	// Google rotates refresh tokens rarely but a concurrent refresh storm can
	// still produce invalid_grant, which would look to the user like a revoked
	// grant. One refresh at a time per connection is the cheap fix.
	mu sync.Mutex

	// states holds pending OAuth state values with their browser binding.
	states *StateStore

	// conns resolves a tenant's Drive connection.
	conns ConnectionResolver
}

// TokenCache stores access tokens keyed by connection ID.
type TokenCache interface {
	// Get returns a cached access token and its expiry.
	Get(ctx context.Context, connectionID string) (token string, expiry time.Time, ok bool)
	// Set stores an access token with a TTL. The TTL must be shorter than the
	// token's real lifetime so a revoked grant takes effect promptly.
	Set(ctx context.Context, connectionID, token string, ttl time.Duration) error
	// Delete removes a cached token, which is what a disconnect or an
	// invalid_grant does.
	Delete(ctx context.Context, connectionID string) error
}

// ConnectionResolver looks up a tenant's Drive connection and its state.
//
// It is an interface so the provider does not depend on a repository directly,
// and so a deployment can back it with any store.
type ConnectionResolver interface {
	// Connection returns the connection for a scope.
	Connection(ctx context.Context, scope persistence.Scope) (Connection, error)
	// SaveConnection records a new or refreshed connection.
	SaveConnection(ctx context.Context, connection Connection) error
	// MarkStatus changes a connection's status.
	MarkStatus(ctx context.Context, connectionID, status string) error
	// ConnectionByID reads a connection when the tenant is already known, which is
	// the shape background work has.
	ConnectionByID(ctx context.Context, connectionID string) (Connection, error)
	// SetChangesCursor advances how far the change log has been processed.
	SetChangesCursor(ctx context.Context, connectionID, cursor string) error
	// AccountProfile reports the connected account's display details.
	//
	// It is separate from Connection because Connection decrypts a refresh token, and
	// rendering a name has no reason to hold a usable credential.
	AccountProfile(ctx context.Context, scope persistence.Scope) (persistence.ConnectionProfile, error)
	// SaveAccountProfile persists the display columns only, never a credential.
	SaveAccountProfile(ctx context.Context, scope persistence.Scope, profile persistence.ConnectionProfile) error
}

// Connection is the stored Drive connection for a tenant.
type Connection struct {
	ID           string
	TenantID     string
	UserID       string
	Subject      string
	RefreshToken string
	RootFolderID string
	Status       string
	// ChangesCursor is how far the change log has been processed. It is empty until
	// the first poll succeeds, which is what makes the first poll start from the
	// newest change rather than from the beginning of time.
	ChangesCursor *string
	// Email, DisplayName and PictureURL describe the connected Google account so the
	// console can show who is signed in.
	//
	// None of them is identity. Subject is; these can be absent and can change, and
	// nothing may key or authorise on them.
	Email       string
	DisplayName string
	PictureURL  string
}

// Connection status values.
const (
	StatusConnected      = "connected"
	StatusReauthRequired = "reauth_required"
	StatusDisconnected   = "disconnected"
	StatusBlocked        = "blocked"
)

// New builds a Drive client.
//
// A missing credential set is not an error: it returns a client whose
// availability check reports a configuration failure. Failing construction
// instead would prevent the API from starting and serving health checks, and a
// hard start failure is a worse operator experience than an honest not-ready.
func New(ctx context.Context, cfg Config, deps ClientDeps) (*Client, error) {
	if cfg.Scopes == nil {
		cfg.Scopes = []string{ScopeDriveFile}
	}
	for _, scope := range cfg.Scopes {
		if scope != ScopeDriveFile && scope != ScopeDriveAppData {
			return nil, fmt.Errorf("drive: scope %q is not permitted in this release", scope)
		}
	}
	if cfg.RootFolderName == "" {
		cfg.RootFolderName = "Storvia"
	}
	if cfg.PreflightTimeout <= 0 {
		cfg.PreflightTimeout = 3 * time.Second
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 60 * time.Second
	}
	if cfg.HTTPClient == nil {
		// One shared client with sane connection limits. Constructing a client per
		// request would defeat connection reuse and leak idle connections.
		cfg.HTTPClient = &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
			},
		}
	}
	if deps.Tokens == nil {
		return nil, errors.New("drive: a token cache is required")
	}
	if deps.Resolver == nil {
		return nil, errors.New("drive: a connection resolver is required")
	}

	return &Client{
		cfg: cfg,
		// configured is fixed at construction so the status endpoint can report a
		// missing deployment configuration without duplicating the check.
		configured: cfg.Credentials.ClientID != "" && cfg.Credentials.ClientSecret != "",
		log:        deps.Logger,
		tokens:     deps.Tokens,
		states:     deps.States,
		conns:      deps.Resolver,
		faults:     deps.Faults,
	}, nil
}

// ClientDeps are the collaborators a Drive client needs.
type ClientDeps struct {
	Tokens   TokenCache
	Resolver ConnectionResolver
	States   *StateStore
	Logger   *slog.Logger
	// Faults gates deliberate failure injection. It is only ever set by
	// development wiring.
	Faults FaultInjection
}

// Configured reports whether Google credentials are present.
func (c *Client) Configured() bool { return c.configured }

// RedirectURI returns the configured normal API callback. It is exposed for
// the connection service's default flow; callers cannot replace it through a
// request parameter.
func (c *Client) RedirectURI() string { return c.cfg.Credentials.RedirectURI }

// IsConfigured exposes the credential check to callers outside this package, so
// the service layer can report a deployment gap without inspecting credentials.
func (c *Client) IsConfigured() bool { return c.configured }

// UsesAppDataFolder reports whether files are stored in Drive's hidden appDataFolder
// rather than a visible folder in My Drive.
func (c *Client) UsesAppDataFolder() bool { return c.cfg.StoreInAppDataFolder }

// Name implements provider.Storage.
func (c *Client) Name() string { return string(model.ProviderGoogleDrive) }

// Availability performs the read-only check the router depends on.
//
// It reads only the app-managed folder with a minimal field selection. It never
// lists unrelated user files, because the narrow drive.file scope means it could
// not see them anyway and a broader query would be a scope violation in spirit
// even when the token allows it.
func (c *Client) Availability(ctx context.Context, scope model.AvailabilityScope) error {
	return c.faults.simulateUnavailable(ctx, scope, c.availability)
}

// availability is the real read-only check. Availability wraps it so fault
// injection can short-circuit it without duplicating the logic.
func (c *Client) availability(ctx context.Context, scope model.AvailabilityScope) error {
	if !c.Configured() {
		return &policy.ProviderError{
			Provider:  c.Name(),
			Operation: "Availability",
			Class:     policy.ClassConfiguration,
			// Configuration is not an outage. Marking it unavailable would push
			// every upload into the fallback bucket on the first request.
			SafeDetail: "google oauth credentials are not configured",
		}
	}

	connection, err := c.conns.Connection(ctx, persistence.Scope{TenantID: scope.TenantID, UserID: scope.UserID})
	if err != nil || connection.Status != StatusConnected {
		return &policy.ProviderError{
			Provider:   c.Name(),
			Operation:  "Availability",
			Class:      policy.ClassConsentRequired,
			SafeDetail: "the drive connection is not usable",
		}
	}

	token, err := c.accessToken(ctx, connection)
	if err != nil {
		return err
	}

	// Reading the folder metadata is read-only, so no mutation can have started
	// and a failure here is genuine availability information.
	callCtx, cancel := context.WithTimeout(ctx, c.cfg.PreflightTimeout)
	defer cancel()

	url := fmt.Sprintf("%s/%s?fields=%s&supportsAllDrives=true",
		driveFilesURL, connection.RootFolderID, "id,name,mimeType,trashed")
	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, url, nil)
	if err != nil {
		return &policy.ProviderError{Provider: c.Name(), Operation: "Availability", Class: policy.ClassValidation, Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return c.normalizeTransportError("Availability", err)
	}
	defer drainAndClose(resp)

	if resp.StatusCode >= 400 {
		return c.normalizeStatus("Availability", resp)
	}
	return nil
}

// ConnectionID returns the Drive connection for a tenant, or "" when none exists.
//
// Having no Drive connection is an ordinary state for a tenant that has not connected
// one yet, not a failure. Returning the repository's "no connection" error instead of an
// empty ID made the coordinator's `connectionID == ""` branch unreachable, so a tenant
// with no Drive grant got an internal error out of CreateUpload instead of a routing
// decision.
func (c *Client) ConnectionID(ctx context.Context, scope persistence.Scope) (string, error) {
	connection, err := c.conns.Connection(ctx, scope)
	if errors.Is(err, persistence.ErrNoConnection) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if connection.Status == StatusDisconnected {
		return "", nil
	}
	return connection.ID, nil
}

// ConnectionUsable reports whether the stored connection can be used now.
//
// A tenant with no connection is not usable, which is a routing input rather than an
// error, for the same reason as ConnectionID.
func (c *Client) ConnectionUsable(ctx context.Context, scope persistence.Scope) (bool, error) {
	connection, err := c.conns.Connection(ctx, scope)
	if errors.Is(err, persistence.ErrNoConnection) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return connection.Status == StatusConnected, nil
}

// ConnectionStatusInfo is the connection state reported to a client.
type ConnectionStatusInfo struct {
	ID            string
	Subject       string
	Status        string
	RootFolderID  string
	LastSuccessAt *time.Time
}

// ConnectionStatus returns the stored connection without exposing its token.
//
// It reports ErrNoConnection rather than a zero value when the tenant has never
// connected, so "not connected" is distinguishable from "connected but broken".
func (c *Client) ConnectionStatus(ctx context.Context, scope persistence.Scope) (ConnectionStatusInfo, error) {
	connection, err := c.conns.Connection(ctx, scope)
	if err != nil {
		return ConnectionStatusInfo{}, err
	}
	return ConnectionStatusInfo{
		ID:           connection.ID,
		Subject:      connection.Subject,
		Status:       connection.Status,
		RootFolderID: connection.RootFolderID,
	}, nil
}

// NewState mints a pending authorization state value.
func (c *Client) NewState() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("drive: generate state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// RememberState records a pending authorization for a browser binding.
func (c *Client) RememberState(state, browserBinding string) {
	if c.states == nil {
		return
	}
	c.states.Put(state, browserBinding)
}

// Disconnect marks a connection disconnected and drops its cached token.
//
// It deliberately leaves the user's Drive files alone. Deleting a user's data
// because they disconnected an app is not a defensible default, and the no-copies
// rule means Storvia has nothing to clean up on its side beyond the reference.
func (c *Client) Disconnect(ctx context.Context, scope persistence.Scope) error {
	connection, err := c.conns.Connection(ctx, scope)
	if err != nil {
		if errors.Is(err, persistence.ErrNoConnection) {
			return apperr.NotFound("no google drive connection exists for this account")
		}
		return apperr.Wrap(err, apperr.KindInternal, "Internal error", "the drive connection could not be read")
	}
	if err := c.conns.MarkStatus(ctx, connection.ID, StatusDisconnected); err != nil {
		return apperr.Wrap(err, apperr.KindInternal, "Internal error", "could not update the drive connection")
	}
	if err := c.tokens.Delete(ctx, connection.ID); err != nil {
		c.log.WarnContext(ctx, "could not drop cached access token", "error", err.Error())
	}
	// Revoking at Google is best effort: local state is already disconnected, and
	// a failure here must not block the user from disconnecting.
	if connection.RefreshToken != "" && c.Configured() {
		if err := c.revoke(ctx, connection.RefreshToken); err != nil {
			c.log.WarnContext(ctx, "token revocation at google failed; connection is still disconnected locally",
				"error", err.Error())
		}
	}
	return nil
}

// drainAndClose releases a response body without logging its contents.
//
// A provider error body can contain file names or identifiers, so it is read into
// a bounded buffer for classification and never written anywhere.
func drainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	// Bound the drain so a large or hostile error body cannot be used to exhaust
	// memory on the error path.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

// normalizeStatus maps a Drive error response onto a policy class.
//
// The provider's JSON error body is parsed for its reason code, which is the only
// reliable way to tell storageQuotaExceeded from a rate limit from a revoked grant.
func (c *Client) normalizeStatus(operation string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	reason := extractReason(body)
	class := policy.ClassifyGoogleError(resp.StatusCode, reason)

	// When the structured reason is empty, the raw body is the only thing that says
	// what went wrong. This runs unattended, so an error nobody can act on is worse
	// than a slightly longer message. The body is provider text, capped, and never
	// contains a credential.
	detail := fmt.Sprintf("google returned %d during %s", resp.StatusCode, operation)
	if reason == "" {
		if raw := strings.TrimSpace(string(body)); raw != "" {
			if len(raw) > 240 {
				raw = raw[:240] + "..."
			}
			detail += ": " + raw
		}
	} else {
		detail += ": " + reason
	}

	return &policy.ProviderError{
		Provider:   c.Name(),
		Operation:  operation,
		Class:      class,
		StatusCode: resp.StatusCode,
		Reason:     reason,
		Retryable:  class == policy.ClassUnavailable || class == policy.ClassRateLimited,
		SafeDetail: detail,
		Err:        fmt.Errorf("google drive: %s: status %d", operation, resp.StatusCode),
	}
}

// extractReason pulls the reason code from a Google API error body.
func extractReason(body []byte) string {
	var payload struct {
		Error struct {
			Errors []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	if len(payload.Error.Errors) > 0 && payload.Error.Errors[0].Reason != "" {
		return payload.Error.Errors[0].Reason
	}
	// The message is preferred over the status because it names the offending field.
	// "google returned 400" is not actionable; "Invalid value at 'changes'
	// (type.googleapis.com/...)" is, and this runs unattended.
	if payload.Error.Message != "" {
		return payload.Error.Message
	}
	return payload.Error.Status
}

// normalizeTransportError maps a network failure onto a policy class.
func (c *Client) normalizeTransportError(operation string, err error) error {
	class := policy.ClassUnavailable
	safeDetail := "the request to google drive failed before a response arrived"

	if errors.Is(err, context.DeadlineExceeded) {
		class = policy.ClassUnavailable
		safeDetail = "the request to google drive exceeded its deadline"
	}
	return &policy.ProviderError{
		Provider:   string(model.ProviderGoogleDrive),
		Operation:  operation,
		Class:      class,
		SafeDetail: safeDetail,
		Retryable:  true,
		Err:        err,
	}
}

// base64URL encodes a value the way OAuth expects it.
func base64URL(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

// constantTimeEqual compares two secrets without leaking their contents by timing.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// hashSecret returns a stable digest used to bind an OAuth state value to a
// browser session without storing the raw value.
func hashSecret(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}

// scopesParam renders the scope list for an authorize URL.
func scopesParam(scopes []string) string { return strings.Join(scopes, " ") }

// parseTokenResponse decodes a token endpoint response body.
//
// Google answers the token endpoint with `application/json`, not the
// form-encoding RFC 6749 permits. Parsing it as a query string yields an empty value
// set, so every exchange failed with "the token response contained no access token"
// even though the token was in the body in plain sight.
//
// JSON is tried first because that is what Google actually sends. The form fallback is
// kept because the spec allows it and a future provider might use it; it costs three
// lines and removes a class of surprise.
func parseTokenResponse(body []byte) (url.Values, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err == nil {
		values := url.Values{}
		for key, value := range payload {
			switch typed := value.(type) {
			case string:
				values.Set(key, typed)
			case float64, bool:
				values.Set(key, fmt.Sprintf("%v", typed))
			default:
				// Anything structured is not a scalar a token response carries.
				continue
			}
		}
		return values, nil
	}

	values, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, fmt.Errorf("drive: the token response was neither json nor form encoded")
	}
	return values, nil
}

// AccountProfile reports who the connected Google account is, for display.
//
// A missing profile is not an error: it means the connection predates the profile
// columns, and the console falls back to the configured id rather than refusing to sign
// in over a cosmetic field.
func (c *Client) AccountProfile(ctx context.Context, scope persistence.Scope) (AccountProfile, error) {
	stored, err := c.conns.AccountProfile(ctx, scope)
	if err != nil {
		return AccountProfile{}, err
	}

	profile := AccountProfile{
		Email:       stored.Email,
		DisplayName: stored.DisplayName,
		PictureURL:  stored.PictureURL,
	}
	if profile.DisplayName != "" || profile.Email != "" {
		return profile, nil
	}

	// Nothing stored. That means this connection was made before the profile columns
	// existed, and the fix is one live call rather than making the operator reconnect
	// to fix a cosmetic field.
	//
	// It happens once: the next read finds the columns populated and returns. A failure
	// is not reported, because an account with no name is still a perfectly usable
	// connection.
	connection, err := c.usableConnection(ctx, scope)
	if err != nil {
		return profile, nil
	}
	accessToken, err := c.accessToken(ctx, connection)
	if err != nil {
		return profile, nil
	}
	fetched, err := c.fetchAccountProfile(ctx, accessToken)
	if err != nil {
		return profile, nil
	}
	_ = c.conns.SaveAccountProfile(ctx, scope, persistence.ConnectionProfile{
		Email:       fetched.Email,
		DisplayName: fetched.DisplayName,
		PictureURL:  fetched.PictureURL,
	})
	return AccountProfile{
		Email:       fetched.Email,
		DisplayName: fetched.DisplayName,
		PictureURL:  fetched.PictureURL,
	}, nil
}

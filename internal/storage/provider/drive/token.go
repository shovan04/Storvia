package drive

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/shovan04/storvia-project/internal/persistence"
	"github.com/shovan04/storvia-project/internal/storage/policy"
)

// accessToken returns a usable access token for a connection, refreshing if needed.
//
// The refresh is serialized per client. Two concurrent refreshes can race such
// that one invalidates the other's token, which surfaces to the user as a sudden
// invalid_grant and looks like a revoked grant. A mutex per connection is far
// cheaper than the support burden of that confusion.
//
// The cache is checked first and every error is reported as a policy.ProviderError
// so the router can classify it. invalid_grant in particular must never look like
// an outage: it is a consent problem and belongs on the reconnect path.
func (c *Client) accessToken(ctx context.Context, connection Connection) (string, error) {
	if !c.Configured() {
		return "", &policy.ProviderError{
			Provider: c.Name(), Operation: "Token", Class: policy.ClassConfiguration,
			SafeDetail: "google oauth credentials are not configured",
		}
	}

	if token, _, ok := c.tokens.Get(ctx, connection.ID); ok && token != "" {
		return token, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Another goroutine may have refreshed while this one waited for the lock.
	if token, _, ok := c.tokens.Get(ctx, connection.ID); ok && token != "" {
		return token, nil
	}

	form := url.Values{}
	form.Set("client_id", c.cfg.Credentials.ClientID)
	form.Set("client_secret", c.cfg.Credentials.ClientSecret)
	form.Set("refresh_token", connection.RefreshToken)
	form.Set("grant_type", "refresh_token")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", &policy.ProviderError{
			Provider: c.Name(), Operation: "TokenRefresh", Class: policy.ClassConfiguration, Err: err,
			SafeDetail: "the token refresh request could not be built",
		}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", c.normalizeTransportError("TokenRefresh", err)
	}
	defer drainAndClose(resp)

	body, _ := readBounded(resp.Body)
	if resp.StatusCode >= 400 {
		return "", c.classifyRefreshFailure(connection, resp.StatusCode, body)
	}

	values, err := parseTokenResponse(body)
	if err != nil {
		return "", &policy.ProviderError{
			Provider: c.Name(), Operation: "TokenRefresh", Class: policy.ClassUnknown, Err: err,
			SafeDetail: "the token response could not be parsed",
		}
	}

	accessToken := values.Get("access_token")
	if accessToken == "" {
		return "", &policy.ProviderError{
			Provider: c.Name(), Operation: "TokenRefresh", Class: policy.ClassUnknown,
			SafeDetail: "the token response contained no access token",
		}
	}

	// Google sometimes rotates the refresh token. When it does, the old one is
	// invalidated, so the new value must be persisted or the next refresh fails.
	if rotated := values.Get("refresh_token"); rotated != "" && rotated != connection.RefreshToken {
		if err := c.persistRotatedToken(ctx, connection.ID, rotated); err != nil {
			// The rotated token is now the only valid one. Losing it means the
			// connection is broken until the user reconnects, so this is worth
			// surfacing loudly rather than logging and continuing.
			return "", &policy.ProviderError{
				Provider: c.Name(), Operation: "TokenRefresh", Class: policy.ClassConfiguration, Err: err,
				SafeDetail: "google rotated the refresh token and it could not be stored; reconnect required",
			}
		}
	}

	ttl := time.Hour
	if seconds, err := time.ParseDuration(values.Get("expires_in") + "s"); err == nil && seconds > 0 {
		ttl = seconds
	}
	if cacheErr := c.tokens.Set(ctx, connection.ID, accessToken, ttl-c.cfg.RefreshSkew); cacheErr != nil {
		// A cache failure is not fatal: the token is still valid in hand.
		c.log.WarnContext(ctx, "could not cache the refreshed access token", "error", cacheErr.Error())
	}
	return accessToken, nil
}

// classifyRefreshFailure maps a refresh rejection onto a policy class.
//
// invalid_grant is the important case. It means the grant is gone, so the
// connection is marked for reconnection and the class is consent_required. If it
// were classified as unavailable, the router could route a new upload to the S3
// fallback for a user who has simply revoked access, which is exactly the kind of
// policy violation this project is built to avoid.
func (c *Client) classifyRefreshFailure(connection Connection, status int, body []byte) error {
	err := classifyTokenError(status, body)

	class := policy.ClassAuthentication
	safeDetail := "the stored google authorization could not be refreshed"

	if strings.Contains(err.Error(), "reconnect") || strings.Contains(err.Error(), "expired or was revoked") {
		class = policy.ClassConsentRequired
		safeDetail = "the google grant is no longer valid; the connection needs to be re-established"
		if markErr := c.conns.MarkStatus(context.Background(), connection.ID, StatusReauthRequired); markErr != nil {
			c.log.ErrorContext(context.Background(), "could not mark the connection as needing reauthorization",
				"error", markErr.Error())
		}
		// Drop the cached token so a stale value is never handed out again.
		if delErr := c.tokens.Delete(context.Background(), connection.ID); delErr != nil {
			c.log.WarnContext(context.Background(), "could not drop the cached token after a failed refresh",
				"error", delErr.Error())
		}
	} else if status >= 500 {
		class = policy.ClassUnavailable
		safeDetail = "the google token service is unavailable"
	}

	return &policy.ProviderError{
		Provider:   c.Name(),
		Operation:  "TokenRefresh",
		Class:      class,
		StatusCode: status,
		SafeDetail: safeDetail,
		Err:        err,
	}
}

// persistRotatedToken stores a refresh token that Google replaced.
//
// It goes through the store's dedicated update path rather than a full
// connection save: a save would also rewrite status and folder fields, and a
// partially-applied save could leave a working connection looking disconnected.
func (c *Client) persistRotatedToken(ctx context.Context, connectionID, refreshToken string) error {
	type rotator interface {
		SetRefreshToken(ctx context.Context, connectionID, refreshToken string) error
	}
	if r, ok := c.conns.(rotator); ok {
		return r.SetRefreshToken(ctx, connectionID, refreshToken)
	}
	// Fall back to a full save when the store does not expose the narrow path.
	connection, err := c.conns.Connection(ctx, persistence.Scope{})
	if err == nil && connection.ID == connectionID {
		connection.RefreshToken = refreshToken
		return c.conns.SaveConnection(ctx, connection)
	}
	return errors.New("drive: the connection store cannot persist a rotated refresh token")
}

// MemoryTokens is the process-local token cache.
//
// It is the development default. A multi-instance deployment must use a shared
// cache, because an access token cached on one instance and revoked on another
// would keep working until its TTL expired.
type MemoryTokens struct {
	mu      sync.RWMutex
	entries map[string]tokenEntry
	now     func() time.Time
}

type tokenEntry struct {
	token   string
	expires time.Time
}

// NewMemoryTokens builds an in-memory token cache.
func NewMemoryTokens() *MemoryTokens {
	return &MemoryTokens{entries: make(map[string]tokenEntry), now: time.Now}
}

// Get returns a cached token.
func (m *MemoryTokens) Get(_ context.Context, connectionID string) (string, time.Time, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	entry, ok := m.entries[connectionID]
	if !ok || m.now().After(entry.expires) {
		return "", time.Time{}, false
	}
	return entry.token, entry.expires, true
}

// Set stores a token for ttl.
func (m *MemoryTokens) Set(_ context.Context, connectionID, token string, ttl time.Duration) error {
	if ttl <= 0 {
		return errors.New("drive: token cache ttl must be positive")
	}
	m.mu.Lock()
	m.entries[connectionID] = tokenEntry{token: token, expires: m.now().Add(ttl)}
	m.mu.Unlock()
	return nil
}

// Delete removes a cached token.
func (m *MemoryTokens) Delete(_ context.Context, connectionID string) error {
	m.mu.Lock()
	delete(m.entries, connectionID)
	m.mu.Unlock()
	return nil
}

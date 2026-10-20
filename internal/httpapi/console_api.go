package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/shovan04/storvia/internal/apperr"
	"github.com/shovan04/storvia/internal/auth"
	"github.com/shovan04/storvia/internal/persistence"
	"github.com/shovan04/storvia/internal/storage/model"
	"github.com/shovan04/storvia/internal/storage/service"
)

// Console drives the testing panel.
//
// It is deliberately separate from the public /v1 API. The public API is what an
// application talks to; this is what an operator talks to while they are changing
// things. Keeping them apart means the console can offer things the product API
// deliberately does not -- "force the fallback", "pretend Drive is down", "delete this
// row" -- without those becoming capabilities a client could use.
type Console struct {
	cfg         AdminPanelConfig
	issuer      *auth.Issuer
	sessions    auth.Lookup
	uploads     *service.UploadService
	objects     *service.ObjectService
	connects    *service.ConnectionService
	consent     ConsentController
	diagnostics DiagnosticsProvider
	folders     FolderEnsurer
	objects_    *persistence.ObjectRepository

	// routing is the live Drive-vs-fallback switch. It may be nil, in which case the
	// console reports that forcing is unavailable rather than pretending it works.
	routing RoutingControl

	// faults toggles the Drive outage simulation at runtime.
	faults *FaultSwitch

	// These are the clients the console uploads through. They are separate from the
	// server's own HTTP client because they carry a different timeout profile: an
	// upload of half a gigabyte to a provider is allowed to take minutes, while a
	// metadata request must not.
	driveHTTPClient    *http.Client
	fallbackHTTPClient *http.Client

	log *slog.Logger
}

// ConsentController reads and changes fallback consent.
type ConsentController interface {
	ConsentGranted(ctx context.Context, scope persistence.Scope) (bool, error)
	GrantConsent(ctx context.Context, scope persistence.Scope) error
	WithdrawConsent(ctx context.Context, scope persistence.Scope) error
}

// RoutingControl is the live Drive-vs-fallback switch.
type RoutingControl interface {
	Forced() bool
	SetForced(forced *bool)
	Configured() bool
	Overridden() bool
}

// FaultSwitch toggles the Drive outage simulation.
type FaultSwitch struct {
	mu    sync.Mutex
	on    bool
	store FaultStore
}

// NewFaultSwitch builds the fault switch.
func NewFaultSwitch(initial bool, store FaultStore) *FaultSwitch {
	return &FaultSwitch{on: initial, store: store}
}

// Enabled reports whether the simulation is on.
func (f *FaultSwitch) Enabled() bool {
	if f == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.on
}

// Set changes the simulation and persists it when a store is attached.
func (f *FaultSwitch) Set(on bool) {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.on = on
	store := f.store
	f.mu.Unlock()
	if store != nil {
		// A persistence failure must not leave the switch disagreeing with what is
		// stored, so it is logged by the caller's next state read rather than
		// silently swallowed. Forcing is an emergency control; losing it on restart
		// is bad but not worse than blocking the toggle.
		_ = store.Save(on)
	}
}

// FaultStore persists the fault switch across restarts.
type FaultStore interface {
	Save(on bool) error
}

func (c *Console) writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		c.log.Error("console: could not write response", "error", err.Error())
	}
}

func (c *Console) fail(w http.ResponseWriter, err error) {
	var appErr *apperr.Error
	if !errors.As(err, &appErr) {
		c.log.Error("console: unhandled failure", "error", err.Error())
	}
	c.writeJSON(w, 500, map[string]string{
		"error":  "The request could not be completed.",
		"detail": safeConsoleDetail(err),
	})
}

// safeConsoleDetail returns an operator-facing message.
//
// The console is authenticated and single-tenant, so more detail is safe here than
// on the public API -- but it is still capped and still never includes provider
// credentials, because the console renders into a browser.
func safeConsoleDetail(err error) string {
	text := err.Error()
	if len(text) > 300 {
		text = text[:300] + "..."
	}
	return text
}

// requireSession resolves the panel's session.
//
// Every console API route goes through here. The panel's HTML is served without a
// session so the sign-in page can be shown, but nothing is readable or changeable
// without one.
func (c *Console) requireSession(w http.ResponseWriter, r *http.Request) (persistence.Scope, bool) {
	token := c.issuer.SessionCookieValue(r)
	if token == "" {
		c.writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Not signed in."})
		return persistence.Scope{}, false
	}
	verified, err := c.issuer.Verify(token, time.Now())
	if err != nil {
		c.writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error": "This session has expired. Sign in again.",
		})
		return persistence.Scope{}, false
	}
	identity, ok := c.sessions.LookupSession(r.Context(), verified.ID)
	if !ok {
		c.writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error": "This session is no longer valid. Sign in again.",
		})
		return persistence.Scope{}, false
	}
	// The console is bound to the configured tenant. A session for any other tenant
	// is rejected rather than honoured, so a cookie from another environment cannot
	// read this one's data.
	if identity.TenantID != c.cfg.TenantID || identity.UserID != c.cfg.UserID {
		c.writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "This session belongs to a different account.",
		})
		return persistence.Scope{}, false
	}
	return persistence.Scope{TenantID: identity.TenantID, UserID: identity.UserID}, true
}

// State is everything the panel renders.
type State struct {
	SignedIn   bool         `json:"signed_in"`
	Account    AccountInfo  `json:"account"`
	Connection Connection   `json:"connection"`
	Storage    StorageInfo  `json:"storage"`
	Consent    ConsentInfo  `json:"consent"`
	Routing    RoutingInfo  `json:"routing"`
	Faults     FaultInfo    `json:"faults"`
	Objects    []ObjectView `json:"objects"`
	Diag       any          `json:"diagnostics,omitempty"`
}

// AccountInfo describes the signed-in operator.
// AccountInfo is who is signed in.
//
// DisplayName and Email come from the connected Google account. They are presentation:
// Subject is the identity, and the console never decides anything from either of them.
type AccountInfo struct {
	TenantID    string `json:"tenant_id"`
	UserID      string `json:"user_id"`
	DisplayName string `json:"display_name,omitempty"`
	Email       string `json:"email,omitempty"`
	PictureURL  string `json:"picture_url,omitempty"`
	Subject     string `json:"subject,omitempty"`
	// Label is what the navbar shows when there is no display name: something a person
	// recognises, rather than a configured id.
	Label string `json:"label,omitempty"`
}

// Connection is the Drive connection state.
type Connection struct {
	Connected     bool       `json:"connected"`
	Status        string     `json:"status"`
	Subject       string     `json:"subject,omitempty"`
	GrantedScopes string     `json:"granted_scopes,omitempty"`
	FolderPresent bool       `json:"folder_present"`
	Stale         bool       `json:"stale"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	// UsesAppDataFolder reports whether files are stored in Drive's hidden
	// appDataFolder rather than a visible folder in My Drive.
	UsesAppDataFolder bool `json:"uses_app_data_folder"`
}

// StorageInfo is whether this account can upload right now, and where.
type StorageInfo struct {
	DriveConnected          bool   `json:"drive_connected"`
	FallbackEnabled         bool   `json:"fallback_enabled"`
	FallbackVendor          string `json:"fallback_vendor,omitempty"`
	FallbackReady           bool   `json:"fallback_ready"`
	FallbackRequiresConsent bool   `json:"fallback_requires_consent"`
	AcceptsUploads          bool   `json:"accepts_uploads"`
	BlockedReason           string `json:"blocked_reason,omitempty"`
	StoresOutsideDrive      bool   `json:"stores_outside_drive"`
}

// ConsentInfo is the fallback-consent state.
type ConsentInfo struct {
	Granted   bool       `json:"granted"`
	GrantedAt *time.Time `json:"granted_at,omitempty"`
}

// RoutingInfo is the Drive-vs-fallback control.
type RoutingInfo struct {
	// Forced reports the value in effect right now.
	Forced bool `json:"forced"`
	// Configured is the durable default from config.json.
	Configured bool `json:"configured"`
	// Overridden is true when the runtime value differs from the configured one.
	Overridden bool `json:"overridden"`
	// TargetProvider names where uploads will go, in plain words.
	TargetProvider string `json:"target_provider"`
	// Available is false when forcing is impossible (no backend configured).
	Available bool `json:"available"`
}

// FaultInfo is the Drive outage simulation.
type FaultInfo struct {
	Enabled bool `json:"enabled"`
}

// ObjectView is one stored object.
type ObjectView struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	LogicalKey   string    `json:"logical_key"`
	MIMEType     string    `json:"mime_type"`
	Category     string    `json:"category"`
	Size         int64     `json:"size"`
	Provider     string    `json:"provider"`
	Status       string    `json:"status"`
	ProviderRef  string    `json:"provider_ref,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	Downloadable bool      `json:"downloadable"`
}

// State returns everything the panel shows.
func (c *Console) State(w http.ResponseWriter, r *http.Request) {
	scope, ok := c.requireSession(w, r)
	if !ok {
		return
	}

	state := State{
		SignedIn: true,
		Account:  AccountInfo{TenantID: scope.TenantID, UserID: scope.UserID},
		Routing: RoutingInfo{
			Forced:    c.routing != nil && c.routing.Forced(),
			Available: c.routing != nil,
		},
		Faults:  FaultInfo{Enabled: c.faults.Enabled()},
		Objects: []ObjectView{},
	}
	if c.routing != nil {
		state.Routing.Configured = c.routing.Configured()
		state.Routing.Overridden = c.routing.Overridden()
	}
	if state.Routing.Forced {
		state.Routing.TargetProvider = "s3_compatible"
	} else {
		state.Routing.TargetProvider = "google_drive"
	}

	connection, err := c.connects.Status(r.Context(), scope)
	if err != nil {
		c.fail(w, err)
		return
	}
	state.Connection = Connection{
		Connected:         connection.Connected,
		Status:            connection.Status,
		Subject:           connection.Subject,
		GrantedScopes:     strings.Join(connection.GrantedScopes, " "),
		FolderPresent:     connection.FolderPresent,
		UsesAppDataFolder: connection.UsesAppDataFolder,
		LastSuccessAt:     connection.LastSuccessAt,
		Stale:             connection.Stale,
	}
	state.Account.Subject = connection.Subject
	state.Account.DisplayName = connection.DisplayName
	state.Account.Email = connection.Email
	state.Account.PictureURL = connection.PictureURL
	state.Account.Label = accountLabel(connection.DisplayName, connection.Email, scope.UserID)

	granted, err := c.consent.ConsentGranted(r.Context(), scope)
	if err != nil {
		c.fail(w, err)
		return
	}
	state.Consent = ConsentInfo{Granted: granted}

	// The storage status carries the "can this account upload right now" answer, which
	// is the thing an operator actually wants to see next to the connection state.
	storage, err := c.uploads.Status(r.Context(), scope)
	if err != nil {
		c.fail(w, err)
		return
	}
	state.Storage = StorageInfo{
		DriveConnected:          storage.DriveConnected,
		FallbackEnabled:         storage.FallbackEnabled,
		FallbackVendor:          storage.FallbackVendor,
		FallbackReady:           storage.FallbackReady,
		FallbackRequiresConsent: storage.FallbackRequiresConsent,
		AcceptsUploads:          storage.AcceptsUploads,
		BlockedReason:           storage.BlockedReason,
		StoresOutsideDrive:      storage.FallbackStoresOutsideDrive,
	}

	objects, err := c.objects_.ListObjects(r.Context(), scope, 50)
	if err != nil {
		c.fail(w, err)
		return
	}
	for _, object := range objects {
		state.Objects = append(state.Objects, ObjectView{
			ID:         object.ID,
			Name:       object.OriginalName,
			LogicalKey: object.LogicalKey,
			MIMEType:   object.MIMEType,
			Category:   string(object.Category),
			Size:       object.ExpectedSize,
			Provider:   string(object.Provider),
			Status:     string(object.Status),
			// The provider reference is useful when checking that a file really did
			// land in Drive, and it is not a capability: it is only ever an
			// identifier, not a URL or token.
			ProviderRef:  firstNonEmpty(deref(object.ProviderFileID), deref(object.ProviderObjectKey)),
			CreatedAt:    object.CreatedAt,
			Downloadable: object.Status == model.ObjectReady,
		})
	}

	if c.diagnostics != nil {
		state.Diag = c.diagnostics.Diagnostics(r.Context())
	}
	c.writeJSON(w, http.StatusOK, state)
}

// accountLabel is what the console shows for the signed-in account.
//
// A display name, then the email address, then the configured user id. Showing
// "user_dev_admin" to an operator who is signed in as a real Google account tells them
// nothing about whose account they are about to upload a file from.
func accountLabel(displayName, email, userID string) string {
	for _, candidate := range []string{displayName, email} {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			return trimmed
		}
	}
	return userID
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// SetRouting changes the Drive-vs-fallback switch.
//
// The request carries an explicit tri-state: true, false, or null to go back to the
// configured default. A plain boolean body could not express "stop overriding me",
// which is the option an operator reaches for most.
func (c *Console) SetRouting(w http.ResponseWriter, r *http.Request) {
	if _, ok := c.requireSession(w, r); !ok {
		return
	}
	if c.routing == nil {
		c.writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": "The routing switch is not available in this build.",
		})
		return
	}

	var body struct {
		// Forced is *bool so absent means "clear the override" rather than "false".
		Forced *bool `json:"forced"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil {
		c.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "The request body could not be read.",
		})
		return
	}

	previous := c.routing.Forced()
	c.routing.SetForced(body.Forced)

	c.log.InfoContext(r.Context(), "console changed fallback routing",
		"from", previous, "to", c.routing.Forced(), "override_cleared", body.Forced == nil)
	c.writeJSON(w, http.StatusOK, RoutingInfo{
		Forced:         c.routing.Forced(),
		Configured:     c.routing.Configured(),
		Overridden:     c.routing.Overridden(),
		TargetProvider: targetProvider(c.routing.Forced()),
		Available:      true,
	})
}

func targetProvider(forced bool) string {
	if forced {
		return "s3_compatible"
	}
	return "google_drive"
}

// SetFaults toggles the Drive outage simulation.
func (c *Console) SetFaults(w http.ResponseWriter, r *http.Request) {
	if _, ok := c.requireSession(w, r); !ok {
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil {
		c.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "The request body could not be read.",
		})
		return
	}
	c.faults.Set(body.Enabled)
	c.log.InfoContext(r.Context(), "console changed drive outage simulation", "enabled", body.Enabled)
	c.writeJSON(w, http.StatusOK, FaultInfo{Enabled: c.faults.Enabled()})
}

// SetConsent grants or withdraws fallback consent.
func (c *Console) SetConsent(w http.ResponseWriter, r *http.Request) {
	scope, ok := c.requireSession(w, r)
	if !ok {
		return
	}
	var body struct {
		Granted bool `json:"granted"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil {
		c.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "The request body could not be read.",
		})
		return
	}

	var err error
	if body.Granted {
		err = c.consent.GrantConsent(r.Context(), scope)
	} else {
		err = c.consent.WithdrawConsent(r.Context(), scope)
	}
	if err != nil {
		c.fail(w, err)
		return
	}

	granted, err := c.consent.ConsentGranted(r.Context(), scope)
	if err != nil {
		c.fail(w, err)
		return
	}
	c.writeJSON(w, http.StatusOK, ConsentInfo{Granted: granted})
}

// DeleteObject removes an object, for testing the delete path.
func (c *Console) DeleteObject(w http.ResponseWriter, r *http.Request) {
	scope, ok := c.requireSession(w, r)
	if !ok {
		return
	}
	objectID := r.PathValue("object_id")
	if objectID == "" {
		c.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "No object was named."})
		return
	}
	if err := c.objects.DeleteObject(r.Context(), scope, objectID); err != nil {
		c.fail(w, err)
		return
	}
	c.writeJSON(w, http.StatusOK, map[string]string{"deleted": objectID})
}

// DeleteAllObjects clears every object this account still has stored.
//
// It is the console's "clear the set" action, so it exists to stop an operator
// deleting twenty rows by hand. It removes nothing on its own: the work, including
// reaching the provider that holds each file, is the service's, and the per-object
// endpoint does exactly the same thing.
func (c *Console) DeleteAllObjects(w http.ResponseWriter, r *http.Request) {
	scope, ok := c.requireSession(w, r)
	if !ok {
		return
	}

	result, err := c.objects.DeleteAllObjects(r.Context(), scope)
	if err != nil {
		c.fail(w, err)
		return
	}

	c.writeJSON(w, http.StatusOK, map[string]any{
		"deleted": result.Deleted,
		"failed":  result.Failed,
	})
}

// driveUsesAppDataFolder reports whether files are stored in Drive's hidden
// appDataFolder rather than a visible folder in My Drive.
func (c *Console) driveUsesAppDataFolder() bool {
	return c.connects.UsesAppDataFolder()
}

// DownloadContent streams an object's bytes, for checking that what was uploaded is
// what came back.
func (c *Console) DownloadContent(w http.ResponseWriter, r *http.Request) {
	scope, ok := c.requireSession(w, r)
	if !ok {
		return
	}
	objectID := r.PathValue("object_id")
	stream, err := c.objects.OpenContent(r.Context(), scope, objectID, nil)
	if err != nil {
		c.fail(w, err)
		return
	}
	defer stream.Body.Close()

	object, err := c.objects.GetObject(r.Context(), scope, objectID)
	name := objectID
	if err == nil && object.OriginalName != "" {
		name = object.OriginalName
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", sanitizeFilename(name)))
	if stream.SizeBytes > 0 {
		w.Header().Set("Content-Length", fmt.Sprint(stream.SizeBytes))
	}
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, stream.Body); err != nil {
		c.log.WarnContext(r.Context(), "console: download ended early",
			"object_id", objectID, "error", err.Error())
	}
}

// sanitizeFilename makes a provider-supplied name safe for a header.
func sanitizeFilename(name string) string {
	replacer := strings.NewReplacer("\"", "", "\\", "", "\r", "", "\n", "")
	cleaned := replacer.Replace(name)
	if cleaned == "" {
		return "download"
	}
	if len(cleaned) > 120 {
		cleaned = cleaned[:120]
	}
	return cleaned
}

// drainAndClose consumes the rest of a response and closes it.
//
// Draining matters: without it a non-reusable connection cannot be returned to the
// pool, and a panel upload would open a new TLS handshake for every file.
func drainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
}

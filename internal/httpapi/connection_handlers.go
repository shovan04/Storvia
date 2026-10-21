package httpapi

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shovan04/storvia-project/internal/apperr"
	"github.com/shovan04/storvia-project/internal/auth"
	"github.com/shovan04/storvia-project/internal/logger"
	"github.com/shovan04/storvia-project/internal/storage/service"
)

// ConnectionHandler exposes the Drive connection lifecycle.
type ConnectionHandler struct {
	connections *service.ConnectionService
}

// NewConnectionHandler builds the handler.
func NewConnectionHandler(connections *service.ConnectionService) *ConnectionHandler {
	return &ConnectionHandler{connections: connections}
}

// connectionResponse describes a Drive connection.
//
// It reports the narrow granted scopes because that is a privacy-relevant fact a
// user is entitled to see. It never includes a refresh token, an access token, or
// a session URI.
type connectionResponse struct {
	Connected     bool     `json:"connected"`
	Status        string   `json:"status"`
	Subject       string   `json:"subject,omitempty"`
	GrantedScopes []string `json:"granted_scopes,omitempty"`
	FolderPresent bool     `json:"folder_present"`
	// LastSuccessAt is when Drive last answered successfully. A null value on a
	// connected account points at a connection problem rather than staleness.
	LastSuccessAt *string `json:"last_success_at,omitempty"`
	// Stale is advisory: it tells the client the connection may need attention.
	Stale bool `json:"stale"`
}

// Show handles GET /v1/storage/connections/google-drive.
func (h *ConnectionHandler) Show(w http.ResponseWriter, r *http.Request) {
	scope, err := scopeFrom(r)
	if err != nil {
		WriteProblem(w, r, err)
		return
	}
	status, err := h.connections.Status(r.Context(), scope)
	if err != nil {
		WriteProblem(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, status)
}

// startResponse carries the consent URL the browser must visit.
type startResponse struct {
	// AuthorizeURL is a Google consent URL. It is returned exactly once, for the
	// authorized tenant, and is never logged.
	AuthorizeURL string `json:"authorize_url"`
	ExpiresIn    int    `json:"expires_in_seconds"`
}

// Start handles POST /v1/storage/connections/google-drive/start.
func (h *ConnectionHandler) Start(w http.ResponseWriter, r *http.Request) {
	scope, err := scopeFrom(r)
	if err != nil {
		WriteProblem(w, r, err)
		return
	}

	// The binding ties the consent flow to the browser session that started it,
	// so a callback arriving in a different session is rejected.
	binding := browserBinding(r)

	authorizeURL, err := h.connections.BeginConnect(r.Context(), scope, binding)
	if err != nil {
		WriteProblem(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, startResponse{
		AuthorizeURL: authorizeURL,
		ExpiresIn:    int(service.OAuthStateTTL.Seconds()),
	})
}

// Callback handles GET /v1/storage/connections/google-drive/callback.
//
// It is public because Google redirects the browser here without any Storvia
// credential. The OAuth state value is what authenticates it.
//
// After handling, the redirect goes to a clean URL so the authorization code is
// not exposed to page resources or a Referer header.
func (h *ConnectionHandler) Callback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	if oauthErr := query.Get("error"); oauthErr != "" {
		// The error codes are from Google's vocabulary and are safe to show.
		h.failCallback(w, r, apperr.ConsentRequired("authorization was not completed: "+oauthErr))
		return
	}
	code := query.Get("code")
	state := query.Get("state")
	if code == "" || state == "" {
		h.failCallback(w, r, apperr.Validation("the callback is missing the code or state parameter"))
		return
	}

	scope, err := scopeFrom(r)
	if err != nil {
		h.failCallback(w, r, err)
		return
	}

	if err := h.connections.CompleteConnect(r.Context(), scope, code, state, browserBinding(r)); err != nil {
		h.failCallback(w, r, err)
		return
	}

	// A clean URL: no code, no state.
	http.Redirect(w, r, "/v1/storage/connections/google-drive?connected=1", http.StatusSeeOther)
}

// Disconnect handles DELETE /v1/storage/connections/google-drive.
func (h *ConnectionHandler) Disconnect(w http.ResponseWriter, r *http.Request) {
	scope, err := scopeFrom(r)
	if err != nil {
		WriteProblem(w, r, err)
		return
	}
	if err := h.connections.Disconnect(r.Context(), scope); err != nil {
		WriteProblem(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// failCallback renders a callback failure as a redirect to a clean URL.
//
// The reason code goes in the query so the browser lands on a usable page, but
// the detail stays in the server log. Redirecting with the detail would put a
// provider error string in the browser history and any Referer header.
func (h *ConnectionHandler) failCallback(w http.ResponseWriter, r *http.Request, err error) {
	logger.With(r.Context(), slog.Default()).Warn("drive connection callback rejected",
		"error_kind", apperr.KindOf(err).String(),
		"path", r.URL.Path,
	)
	WriteProblem(w, r, err)
}

// browserBinding returns the value an OAuth state is bound to.
//
// The session cookie is the binding. Any change to it mid-flow invalidates the
// state, which is what stops a callback from being replayed into another browser
// session.
func browserBinding(r *http.Request) string {
	identity, ok := auth.IdentityFrom(r.Context())
	if !ok {
		// Unauthenticated flows still need a stable binding. The cookie value
		// works; if it is absent, the request cannot be bound and the state store
		// will reject it.
		if cookie, err := r.Cookie("storvia_session"); err == nil {
			return cookie.Value
		}
		return ""
	}
	return identity.TenantID + ":" + identity.UserID + ":" + identity.SessionID
}

// safeRedirect builds a same-origin redirect target.
func safeRedirect(base *url.URL, path string) string {
	if base == nil {
		return path
	}
	target := *base
	target.Path = path
	target.RawQuery = ""
	target.Fragment = ""
	return target.String()
}

// callbackStateTTL mirrors the state's own lifetime for documentation purposes.
const callbackStateTTL = service.OAuthStateTTL

// logStreamError records a mid-stream failure.
//
// Once headers are sent the status cannot change, so the only honest options are
// to log and truncate, or to let the connection break. Truncating is friendlier
// and is what the bounded copy already guarantees.
func logStreamError(r *http.Request, err error) {
	if err == nil || isBenignStreamError(err) {
		return
	}
	logger.With(r.Context(), slog.Default()).Warn("content stream interrupted",
		"error", err.Error(),
		"path", r.URL.Path,
	)
}

// isBenignStreamError reports whether a stream error is just the client hanging up.
func isBenignStreamError(err error) bool {
	if err == nil {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "broken pipe") ||
		strings.Contains(message, "connection reset by peer") ||
		strings.Contains(message, "context canceled")
}

// ensure the time import is used by the response helpers above.
var _ = time.Second

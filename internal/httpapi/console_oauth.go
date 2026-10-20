package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shovan04/storvia/internal/persistence"
)

var (
	errMissingFlowCookie = errors.New("console: the sign-in flow cookie is missing")
	errMissingCode       = errors.New("console: google returned no authorization code")
)

// consoleRedirect is where the browser lands after a console OAuth round trip, when the
// console is served by this binary.
//
// When the console runs as a separate frontend on another port, that is not this path:
// the browser has to go back to wherever it started. ConsoleURL carries that, and the
// browser is handed to it once the session exists.
var consoleRedirect = "/console/"

// SignIn starts the Google OAuth flow for the console.
//
// The browser binding is stored in a cookie so the callback can prove the flow that
// started in this browser is the one returning. Without it, a second person could
// complete someone else's consent by feeding a state value into the callback.
func (c *Console) SignIn(w http.ResponseWriter, r *http.Request) {
	if !c.enabled(w) {
		return
	}

	authorizeURL, err := c.beginConnect(w, r)
	if err != nil {
		c.log.WarnContext(r.Context(), "console: could not start sign-in", "error", err.Error())
		http.Redirect(w, r, c.consoleURL()+"?error="+url.QueryEscape(
			"Could not start the Google sign-in."), http.StatusFound)
		return
	}
	http.Redirect(w, r, authorizeURL, http.StatusFound)
}

// Callback completes the Google OAuth flow and issues the console session.
func (c *Console) Callback(w http.ResponseWriter, r *http.Request) {
	if !c.enabled(w) {
		return
	}

	if oauthErr := r.URL.Query().Get("error"); oauthErr != "" {
		// A declined consent screen is not a failure of this service, and it is
		// reported as a normal outcome rather than an error page.
		http.Redirect(w, r, c.consoleURL()+"?error="+url.QueryEscape(
			"Google did not grant access: "+oauthErr), http.StatusFound)
		return
	}

	if err := c.completeConnect(w, r); err != nil {
		c.log.WarnContext(r.Context(), "console: sign-in callback failed", "error", err.Error())
		// The operator is told what actually went wrong.
		//
		// A generic "could not be completed" is useless when the only thing left to do
		// is fix it: the same message covers a replayed state, a code Google will not
		// exchange, and a redirect URI that is not registered. classifySignInError maps
		// the cause to something a person can act on, and the full detail stays in the
		// log.
		http.Redirect(w, r, c.consoleURL()+"?error="+url.QueryEscape(
			classifySignInError(err)), http.StatusFound)
		return
	}
	// The session exists and the cookie is set. Now hand the browser to the console.
	//
	// This is the whole point of doing OAuth in the backend: the frontend never receives
	// an authorization code and never verifies anything. It is told "you are signed in"
	// and nothing more, and it reads the session from the API it already talks to.
	http.Redirect(w, r, c.consoleURL(), http.StatusFound)
}

// classifySignInError turns a sign-in failure into something a person can act on.
//
// The full cause stays in the log. What reaches the browser is deliberately specific but
// bounded: enough to distinguish "reconnect", "register the redirect URI", and "something
// is wrong here", and never anything containing a code, a token, or a client secret.
func classifySignInError(err error) string {
	switch {
	case errors.Is(err, errMissingFlowCookie):
		return "The sign-in could not be verified: the browser that started it is not the " +
			"one that returned. Try signing in again from the same browser."
	case errors.Is(err, errMissingCode):
		return "Google returned no authorization code. Sign in again."
	case errors.Is(err, persistence.ErrNoConnection):
		return "No account is connected, so the sign-in could not be recorded. Sign in again."
	}

	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "redirect_uri") || strings.Contains(message, "unauthorized client"):
		return "Google rejected the redirect URI. It must be registered, character for " +
			"character, in the OAuth client."
	case strings.Contains(message, "invalid_grant") || strings.Contains(message, "expired"):
		return "Google's authorization code was refused, usually because it had already " +
			"been used or had expired. Sign in again."
	case strings.Contains(message, "access_denied"):
		return "Google did not grant access."
	}
	return "The Google sign-in could not be completed. Check the API log for the reason."
}

// consoleURL is where the browser goes after a completed sign-in.
func (c *Console) consoleURL() string {
	if c.cfg.ConsoleURL != "" {
		return c.cfg.ConsoleURL
	}
	return consoleRedirect
}

// SignOut ends the console session.
//
// It clears the cookie and revokes the server-side session. Revoking matters: a
// cleared cookie alone leaves a still-valid token in the store, which is harmless
// until the cookie is somehow recovered.
func (c *Console) SignOut(w http.ResponseWriter, r *http.Request) {
	token := c.issuer.SessionCookieValue(r)
	if token != "" {
		if verified, err := c.issuer.Verify(token, time.Now()); err == nil {
			if revoker, ok := c.sessions.(interface {
				RevokeSession(ctx context.Context, sessionID string) error
			}); ok {
				if err := revoker.RevokeSession(r.Context(), verified.ID); err != nil {
					c.log.WarnContext(r.Context(), "console: could not revoke the session",
						"error", err.Error())
				}
			}
		}
	}
	// Clearing a cookie only works on the origin that set it, so sign-out is a navigation
	// to Go rather than a fetch. That is also why the console renders it as a plain form
	// post: a fetch from another origin cannot clear it either.
	c.issuer.ClearSessionCookie(w)
	http.Redirect(w, r, c.consoleURL(), http.StatusFound)
}

// EnsureFolder creates the app-managed Storvia/ folder if it is absent.
//
// In app-data mode there is no visible folder to create: appDataFolder is implicit.
func (c *Console) EnsureFolder(w http.ResponseWriter, r *http.Request) {
	scope, ok := c.requireSession(w, r)
	if !ok {
		return
	}
	if c.driveUsesAppDataFolder() {
		c.writeJSON(w, http.StatusOK, map[string]bool{"created": false, "app_data_folder": true})
		return
	}
	if c.folders == nil {
		c.writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": "This deployment cannot create the Drive folder.",
		})
		return
	}
	if err := c.folders.EnsureRootFolder(r.Context(), scope); err != nil {
		c.fail(w, err)
		return
	}
	c.writeJSON(w, http.StatusOK, map[string]bool{"created": true})
}

// Disconnect ends the Drive connection without ending the console session.
//
// Signing out of Google and disconnecting Drive are different things: an operator may
// want to test a fresh connection while staying signed in to the console.
func (c *Console) Disconnect(w http.ResponseWriter, r *http.Request) {
	scope, ok := c.requireSession(w, r)
	if !ok {
		return
	}
	if err := c.connects.Disconnect(r.Context(), scope); err != nil {
		c.fail(w, err)
		return
	}
	c.writeJSON(w, http.StatusOK, map[string]bool{"disconnected": true})
}

// scopeFor is the tenant the console acts on.
func (c *Console) scopeFor() persistence.Scope {
	return persistence.Scope{TenantID: c.cfg.TenantID, UserID: c.cfg.UserID}
}

// enabled reports whether the console is available in this deployment.
//
// It answers 404 rather than a problem document: the console is a development surface,
// and a production deployment should simply not have one.
func (c *Console) enabled(w http.ResponseWriter) bool {
	if c.cfg.Enabled {
		return true
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte("The console is not enabled in this deployment.\n"))
	return false
}

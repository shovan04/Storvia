package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shovan04/storvia-project/internal/auth"
	"github.com/shovan04/storvia-project/internal/storage/service"
)

// TestOAuthIsEntirelyBackendLogic pins the division of responsibility.
//
// The console is a client. It never receives an authorization code, never verifies
// anything, and has no route that could take that job on:
//
//	:8080/console/sign-in         Go starts the consent flow
//	Google -> :8080/…/callback    Go verifies the state and exchanges the code
//	Go -> :3001/                  Go issues the session and hands the browser back
//
// A frontend proxy inverts this: the code would pass through the console, and the
// console would need its own copy of the verification. That was the wrong design once
// already, and it is easy to reintroduce by accident.
func TestOAuthIsEntirelyBackendLogic(t *testing.T) {
	console := newTestConsole(t, auth.NewMemorySessions(10), &stubRouting{})
	console.cfg.ConsoleURL = "http://127.0.0.1:3001/"
	console.connects = service.NewConnectionService(&stubConnector{})

	// Sign-in is a navigation to Go, which then 302s to Google.
	rec := httptest.NewRecorder()
	console.SignIn(rec, httptest.NewRequest(http.MethodGet, "/console/sign-in", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("sign-in = %d, want 302", rec.Code)
	}

	// The callback verifies, and on failure hands the browser back to the console --
	// not to some intermediate page of its own.
	rec = httptest.NewRecorder()
	console.Callback(rec, httptest.NewRequest(http.MethodGet,
		"/console/oauth/callback?code=x&state=y", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("callback = %d, want 302", rec.Code)
	}
	if got := rec.Header().Get("Location"); !strings.HasPrefix(got, "http://127.0.0.1:3001/") {
		t.Errorf("callback redirected to %q, want the console origin", got)
	}
}

// TestConsoleHandbackFallsBackToTheLocalConsole covers the embedded case: with no
// ConsoleURL configured, this binary serves the console itself and the redirect must
// stay local rather than point at a URL that does not exist.
func TestConsoleHandbackFallsBackToTheLocalConsole(t *testing.T) {
	console := newTestConsole(t, auth.NewMemorySessions(10), &stubRouting{})
	console.connects = service.NewConnectionService(&stubConnector{})
	if got := console.consoleURL(); !strings.HasPrefix(got, "/") {
		t.Errorf("consoleURL() = %q, want a local path when no console url is configured", got)
	}
}

// TestSignOutRedirectsToTheConsole checks the other handback: ending a session has to
// leave the browser somewhere it can see that it ended.
func TestSignOutRedirectsToTheConsole(t *testing.T) {
	console := newTestConsole(t, auth.NewMemorySessions(10), &stubRouting{})
	console.cfg.ConsoleURL = "http://127.0.0.1:3001/"
	console.connects = service.NewConnectionService(&stubConnector{})

	rec := httptest.NewRecorder()
	console.SignOut(rec, httptest.NewRequest(http.MethodPost, "/console/sign-out", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("sign-out = %d, want 302", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "http://127.0.0.1:3001/" {
		t.Errorf("sign-out redirected to %q, want the console origin", got)
	}
}

// TestSignOutClearsTheCookieOnTheBackend. The session lives in Go, so only Go can end
// it -- a cookie set on another origin cannot be cleared from here, which is why the
// console renders sign-out as a navigation rather than a fetch.
func TestSignOutClearsTheCookieOnTheBackend(t *testing.T) {
	console := newTestConsole(t, auth.NewMemorySessions(10), &stubRouting{})
	console.connects = service.NewConnectionService(&stubConnector{})

	identity := auth.Identity{TenantID: console.cfg.TenantID, UserID: console.cfg.UserID}
	token, session, err := console.issuerOrTest().Issue(identity, "s1", time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := console.sessions.StoreSession(t.Context(), session); err != nil {
		t.Fatalf("StoreSession: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/console/sign-out", nil)
	req.AddCookie(&http.Cookie{Name: "storvia_session", Value: token})

	rec := httptest.NewRecorder()
	console.SignOut(rec, req)

	var cleared bool
	for _, cookie := range rec.Result().Cookies() {
		if cookie.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("sign-out did not expire the session cookie")
	}
}

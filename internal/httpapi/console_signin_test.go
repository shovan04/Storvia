package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/shovan04/storvia-project/internal/auth"
	"github.com/shovan04/storvia-project/internal/persistence"
	"github.com/shovan04/storvia-project/internal/storage/service"
)

// stubConnector stands in for Google.
//
// It is the seam the real flow is tested at. Everything around it — the state, the
// browser binding, the cookie, the redirect, the error classification — is the real
// implementation. The only thing stubbed is Google's answer, because producing a genuine
// authorization code requires a human to click through a consent screen.
//
// That boundary is the honest limit of automation here, and it is why these tests exist:
// they cover everything on this side of the one step that cannot be automated.
type stubConnector struct {
	// completeErr is returned by CompleteConnectAt when set.
	completeErr error
	// wantCode is the code the flow must present.
	wantCode string
	// wantState is the state the flow must present.
	wantState string
	// wantBinding is the browser binding recorded at the start of the flow.
	wantBinding string

	completedWithCode    string
	completedWithState   string
	completedWithBinding string
	disconnected         bool
}

func (s *stubConnector) BeginConnect(context.Context, persistence.Scope, string) (string, error) {
	return "https://accounts.google.com/o/oauth2/v2/auth?stub=1", nil
}

func (s *stubConnector) BeginConnectAt(_ context.Context, _ persistence.Scope,
	browserBinding, _ string) (string, error) {
	s.wantBinding = browserBinding
	return "https://accounts.google.com/o/oauth2/v2/auth?stub=1", nil
}

func (s *stubConnector) CompleteConnect(context.Context, persistence.Scope, string, string, string) error {
	return nil
}

func (s *stubConnector) CompleteConnectAt(_ context.Context, _ persistence.Scope,
	code, state, browserBinding, _ string) error {
	s.completedWithCode = code
	s.completedWithState = state
	s.completedWithBinding = browserBinding
	if s.completeErr != nil {
		return s.completeErr
	}
	if s.wantCode != "" && code != s.wantCode {
		return errors.New("invalid_grant: the authorization code is invalid or expired")
	}
	return nil
}

func (s *stubConnector) Status(context.Context, persistence.Scope) (service.ConnectionStatus, error) {
	return service.ConnectionStatus{Connected: true, Status: "connected"}, nil
}

func (s *stubConnector) Disconnect(context.Context, persistence.Scope) error {
	s.disconnected = true
	return nil
}

func consoleWithConnector(t *testing.T, stub *stubConnector, sessions auth.Lookup) (*Console, http.Handler) {
	t.Helper()
	console := newTestConsole(t, sessions, &stubRouting{})
	console.connects = service.NewConnectionService(stub)
	console.cfg.ConsoleURL = "http://127.0.0.1:3001/"
	return console, consoleRouter(console)
}

// beginFlow drives the sign-in start and returns the flow cookie the browser would keep.
func beginFlow(t *testing.T, handler http.Handler) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/console/sign-in", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("sign-in = %d, want a redirect to Google", rec.Code)
	}
	location := rec.Header().Get("Location")
	if !strings.Contains(location, "accounts.google.com") {
		t.Fatalf("sign-in redirected to %q, want Google", location)
	}
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == "storvia_admin_flow" {
			return cookie
		}
	}
	t.Fatal("sign-in set no flow cookie, so the callback could not be bound to a browser")
	return nil
}

// TestCallbackIssuesASessionAndHandsBackToTheConsole is the whole chain, with only
// Google's answer stubbed.
//
// A console is only usable if this works, and it is the one step in the sign-in flow that
// no Go test could previously reach: the callback handler, the flow cookie binding, the
// session cookie, and the handback.
func TestCallbackIssuesASessionAndHandsBackToTheConsole(t *testing.T) {
	sessions := auth.NewMemorySessions(10)
	stub := &stubConnector{wantCode: "the-code"}
	console, handler := consoleWithConnector(t, stub, sessions)

	flow := beginFlow(t, handler)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/console/oauth/callback?code=the-code&state=the-state", nil)
	req.AddCookie(flow)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("callback = %d, want 302", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "http://127.0.0.1:3001/" {
		t.Fatalf("handed back to %q, want the console origin", got)
	}

	// The browser binding the flow started with is what the callback must present. A
	// callback from a different browser must not satisfy it.
	if stub.completedWithBinding != flow.Value {
		t.Errorf("the callback presented binding %q, but the flow started with %q",
			stub.completedWithBinding, flow.Value)
	}
	if stub.completedWithState != "the-state" {
		t.Errorf("state = %q, want the-state", stub.completedWithState)
	}

	// And a session cookie must have been issued, with a lifetime long enough to survive
	// closing the browser.
	var session *http.Cookie
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == "storvia_session" {
			session = cookie
		}
	}
	if session == nil {
		t.Fatal("no session cookie was set, so the console would ask for sign-in again")
	}
	if session.MaxAge <= 0 {
		t.Errorf("session cookie MaxAge = %d, want positive: a cookie with no MaxAge "+
			"disappears when the browser closes, which is not what an operator expects",
			session.MaxAge)
	}

	// Finally, the issued cookie must actually resolve a session.
	identity, ok := sessions.LookupSession(context.Background(),
		sessionIDFromToken(t, console, session.Value))
	if !ok {
		t.Fatal("the issued cookie does not resolve to a session")
	}
	if identity.TenantID != "tenant-dev" {
		t.Errorf("session tenant = %q, want tenant-dev", identity.TenantID)
	}
}

// sessionIDFromToken pulls the session id out of the token the issuer produced.
func sessionIDFromToken(t *testing.T, console *Console, token string) string {
	t.Helper()
	verified, err := console.issuerOrTest().Verify(token, nowForTest())
	if err != nil {
		t.Fatalf("the issued token does not verify: %v", err)
	}
	return verified.ID
}

// TestCallbackWithoutTheFlowCookieIssuesNothing. The binding is what proves the browser
// returning is the browser that started, so a callback with no cookie must get nowhere.
func TestCallbackWithoutTheFlowCookieIssuesNothing(t *testing.T) {
	sessions := auth.NewMemorySessions(10)
	_, handler := consoleWithConnector(t, &stubConnector{}, sessions)
	_ = beginFlow(t, handler)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/console/oauth/callback?code=c&state=s", nil))

	if got := rec.Header().Get("Location"); !strings.Contains(got, "error=") {
		t.Errorf("redirected to %q, want an error back to the console", got)
	}
	// The message must be the specific one, not the catch-all.
	decoded, err := url.QueryUnescape(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("the error did not survive URL encoding: %v", err)
	}
	if !strings.Contains(decoded, "could not be verified") {
		t.Errorf("Location = %q, want a message naming the browser-binding failure", decoded)
	}
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == "storvia_session" {
			t.Error("a session was issued for a callback with no flow cookie")
		}
	}
}

// TestCallbackFailureIsExplainedToTheOperator.
//
// A generic "could not be completed" is useless when the only remaining action is to fix
// something, because the same message covers a replayed state, an unregistered redirect
// URI, and a code Google will not exchange.
func TestCallbackFailureIsExplainedToTheOperator(t *testing.T) {
	tests := []struct {
		name     string
		cause    error
		contains string
	}{
		{
			name:     "no flow cookie",
			cause:    errMissingFlowCookie,
			contains: "browser",
		},
		{
			name:     "no code from google",
			cause:    errMissingCode,
			contains: "no authorization code",
		},
		{
			name:     "redirect uri not registered",
			cause:    errors.New("google: Error 400: redirect_uri_mismatch"),
			contains: "registered",
		},
		{
			name:     "code already used",
			cause:    errors.New("google: Error 400: invalid_grant"),
			contains: "refused",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifySignInError(tc.cause)
			if !strings.Contains(strings.ToLower(got), strings.ToLower(tc.contains)) {
				t.Errorf("message %q does not mention %q", got, tc.contains)
			}
			// Whatever the cause, nothing secret may reach the browser.
			for _, forbidden := range []string{"secret", "client_secret", "Bearer", "code="} {
				if strings.Contains(got, forbidden) {
					t.Errorf("message %q leaks %q", got, forbidden)
				}
			}
		})
	}
}

// TestCallbackDeclinedIsNotAnError. Pressing Back on Google's screen is a normal outcome
// and should say so, rather than reporting a failure.
func TestCallbackDeclinedIsNotAnError(t *testing.T) {
	_, handler := consoleWithConnector(t, &stubConnector{}, auth.NewMemorySessions(10))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/console/oauth/callback?error=access_denied", nil))

	location := rec.Header().Get("Location")
	decoded, err := url.QueryUnescape(location)
	if err != nil {
		t.Fatalf("the error did not survive URL encoding: %v", err)
	}
	if !strings.Contains(decoded, "did not grant access") {
		t.Errorf("Location = %q, want the decline reported plainly", decoded)
	}
}

func nowForTest() time.Time { return time.Now() }

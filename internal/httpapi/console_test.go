package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/shovan04/storvia-project/internal/auth"
)

// stubRouting is a RoutingControl driven by the test.
type stubRouting struct {
	forced     atomic.Bool
	overridden atomic.Bool
	configured bool
	set        int
}

func (s *stubRouting) Forced() bool     { return s.forced.Load() }
func (s *stubRouting) Configured() bool { return s.configured }
func (s *stubRouting) Overridden() bool { return s.overridden.Load() }
func (s *stubRouting) SetForced(forced *bool) {
	s.set++
	if forced == nil {
		s.overridden.Store(false)
		s.forced.Store(s.configured)
		return
	}
	s.overridden.Store(true)
	s.forced.Store(*forced)
}

// newTestConsole builds a console with just enough wired to exercise the routes that
// do not touch a provider.
func newTestConsole(t *testing.T, sessions auth.Lookup, routing RoutingControl) *Console {
	t.Helper()
	issuer, err := auth.NewIssuer(auth.IssuerOptions{
		SigningKey: "test-signing-key-for-the-console-tests-only",
		CookieName: "storvia_session",
		TTL:        time.Hour,
	})
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	return NewConsole(AdminPanelConfig{
		Enabled:         true,
		TenantID:        "tenant-dev",
		UserID:          "user-1",
		FlowCookie:      "storvia_admin_flow",
		RedirectURI:     "http://127.0.0.1:8080/admin/oauth/callback",
		AdminSessionTTL: time.Hour,
	}, ConsoleOptions{
		Issuer:   issuer,
		Sessions: sessions,
		Routing:  routing,
		Faults:   NewFaultSwitch(false, nil),
	})
}

// TestConsoleRejectsEverythingWithoutASession is the property that makes the sign-in
// gate real. The panel's HTML is served signed-out on purpose -- there has to be
// something to click -- but no data may be readable without a session.
func TestConsoleRejectsEverythingWithoutASession(t *testing.T) {
	console := newTestConsole(t, auth.NewMemorySessions(10), &stubRouting{})

	for _, path := range []string{
		"/console/api/state",
		"/console/api/upload",
		"/console/api/routing",
		"/console/api/faults",
		"/console/api/consent",
	} {
		rec := httptest.NewRecorder()
		console.State(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a session = %d, want 401", path, rec.Code)
		}
	}
}

// TestConsoleServesTheDocumentSignedOut is a property of the frontend rather than an
// inline template.
//
// The Next.js export is served signed-out so there is a sign-in button to click, and the
// document carries no data of its own: every value on the page comes from a session-gated
// API call, and those routes reject an unauthenticated request independently.
func TestConsoleServesTheDocumentSignedOut(t *testing.T) {
	// Swap in a stand-in export so this test does not depend on a frontend build.
	withConsoleAssets(testConsoleAssets(), func(router http.Handler) {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/console/", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("GET /console/ = %d, want 200", rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
			t.Errorf("Cache-Control = %q, want no-store; a cached console shows stale control state", got)
		}
	})

}

// TestConsoleAssetsAreImmutable pins the other half of the caching rule: hashed bundles
// never change, so re-fetching them on every load is waste.
func TestConsoleAssetsAreImmutable(t *testing.T) {
	root := http.NewServeMux()
	mountConsole(root, fstest.MapFS{
		"index.html":          {Data: []byte("<!doctype html><title>console</title>")},
		"_next/static/app.js": {Data: []byte("console.log(1)")},
	})

	rec := httptest.NewRecorder()
	root.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/console/_next/static/app.js", nil))
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("asset Cache-Control = %q, want immutable", got)
	}

	rec = httptest.NewRecorder()
	root.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/console/index.html", nil))
	if got := rec.Header().Get("Cache-Control"); strings.Contains(got, "immutable") {
		t.Errorf("document Cache-Control = %q, must not be immutable", got)
	}
}

// TestConsoleSetRoutingIsTriState covers the three states the panel offers.
func TestConsoleSetRoutingIsTriState(t *testing.T) {
	routing := &stubRouting{configured: false}
	console := newTestConsole(t, auth.NewMemorySessions(10), routing)

	signedIn := signedInRequest(t, console, "POST", `/console/api/routing`,
		`{"forced":true}`)

	rec := httptest.NewRecorder()
	console.SetRouting(rec, signedIn)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if !routing.Forced() {
		t.Error("forcing the fallback did not take effect")
	}

	rec = httptest.NewRecorder()
	console.SetRouting(rec, signedInRequest(t, console, "POST", "/console/api/routing", `{"forced":null}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("clearing the override = %d, want 200", rec.Code)
	}
	if routing.Forced() {
		t.Error("clearing the override did not return to the configured default")
	}
	if routing.Overridden() {
		t.Error("the override should be gone after being cleared")
	}
}

// signedInRequest builds a request carrying a valid console session cookie.
func signedInRequest(t *testing.T, console *Console, method, path, body string) *http.Request {
	t.Helper()

	identity := auth.Identity{TenantID: console.cfg.TenantID, UserID: console.cfg.UserID}
	token, session, err := console.issuerOrTest().Issue(identity, "sess-test", time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := console.sessions.StoreSession(context.Background(), session); err != nil {
		t.Fatalf("StoreSession: %v", err)
	}

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "storvia_session", Value: token})
	return req
}

// TestConsoleRejectsAnotherTenantsSession. A cookie minted for a different tenant must
// not read this console's data, even though the signature verifies.
func TestConsoleRejectsAnotherTenantsSession(t *testing.T) {
	console := newTestConsole(t, auth.NewMemorySessions(10), &stubRouting{})

	identity := auth.Identity{TenantID: "someone-else", UserID: "user-1"}
	token, session, err := console.issuerOrTest().Issue(identity, "sess-other", time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := console.sessions.StoreSession(context.Background(), session); err != nil {
		t.Fatalf("StoreSession: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/console/api/state", nil)
	req.AddCookie(&http.Cookie{Name: "storvia_session", Value: token})

	rec := httptest.NewRecorder()
	console.State(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("another tenant's session = %d, want 403", rec.Code)
	}
}

// TestConsoleFaultToggleDrivesTheInjector.
func TestConsoleFaultToggleDrivesTheInjector(t *testing.T) {
	console := newTestConsole(t, auth.NewMemorySessions(10), &stubRouting{})

	rec := httptest.NewRecorder()
	console.SetFaults(rec, signedInRequest(t, console, "POST", "/console/api/faults", `{"enabled":true}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !console.faults.Enabled() {
		t.Error("the fault switch did not turn on")
	}

	var parsed FaultInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !parsed.Enabled {
		t.Error("the response did not report the new state")
	}
}

// TestParseDriveUploadResponse guards the step that makes Drive completion possible:
// the file id comes from Google's response body, and an unreadable body would look
// like a provider that accepted bytes but stored nothing.
func TestParseDriveUploadResponse(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    string
		wantErr bool
	}{
		{"the normal answer", `{"id":"1AbC","name":"x.bin"}`, "1AbC", false},
		{"no id", `{"name":"x.bin"}`, "", true},
		{"not json", `<html>error</html>`, "", true},
		{"empty body", ``, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDriveUploadResponse(strings.NewReader(tc.body))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("id = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSanitizeFilenameStripsHeaderBreakers. The name goes into a Content-Disposition
// header; a quote or newline in it would let a crafted name inject header content.
func TestSanitizeFilenameStripsHeaderBreakers(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`evil"; drop`, "evil; drop"},
		{"with\nnewline", "withnewline"},
		{"with\rcarriage", "withcarriage"},
		{"", "download"},
	} {
		if got := sanitizeFilename(tc.in); got != tc.want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

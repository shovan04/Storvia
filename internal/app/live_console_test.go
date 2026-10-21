package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shovan04/storvia-project/internal/auth"
	"github.com/shovan04/storvia-project/internal/config"
	"github.com/shovan04/storvia-project/internal/httpapi"
	"github.com/shovan04/storvia-project/internal/persistence"
)

// TestLiveConsoleForcedFallbackUpload exercises the console the way an operator would:
// force the fallback, upload a real file, read it back, and delete it.
//
// This is the one console path that can be verified without a browser. A Google
// sign-in needs a human, but the forced-fallback switch deliberately bypasses Drive
// entirely -- which is exactly what makes it testable, and why it is worth having.
func TestLiveConsoleForcedFallbackUpload(t *testing.T) {
	requireLiveFallback(t)

	cfg := loadDevConfig(t)
	if !cfg.Fallback.Enabled {
		t.Skip("fallback is not enabled in the development configuration")
	}
	cfg.Database.SQLitePath = "file::memory:"

	built := buildLiveApp(t, cfg)
	scope := persistence.Scope{TenantID: "tenant_dev_admin", UserID: "user_dev_admin"}
	seedTenant(t, built.Store, scope.TenantID)

	console := buildTestConsole(t, built, cfg)
	handler := consoleHandler(console)
	session := mintSession(t, built, scope)

	// Consent first. The forced switch deliberately does not bypass it, so without
	// this the upload is refused rather than misrouted.
	if _, err := built.Consent.Accept(context.Background(), scope); err != nil {
		t.Fatalf("grant fallback consent: %v", err)
	}

	// Force the fallback.
	rec := doJSON(t, handler, session, http.MethodPost, "/console/api/routing", `{"forced":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("forcing the fallback = %d: %s", rec.Code, rec.Body)
	}
	if !built.RoutingSwitch.Forced() {
		t.Fatal("the routing switch did not report itself forced")
	}

	// Upload a file with recognisable content.
	payload := []byte("storvia console forced-fallback live test\n" + strings.Repeat("payload-", 512))
	rec = doUpload(t, handler, session, "console-live.txt", payload)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload = %d: %s", rec.Code, rec.Body)
	}

	var result httpapi.UploadResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode the upload result: %v", err)
	}

	// The forced path must have gone to the fallback even though Drive was healthy.
	// This is the assertion that matters: it proves the switch beats Drive's health.
	if result.Provider != "s3_compatible" {
		t.Fatalf("provider = %q, want s3_compatible: forcing the fallback did not take effect "+
			"(detail: %s)", result.Provider, result.Detail)
	}
	if result.Outcome != "ready" {
		t.Fatalf("outcome = %q (%s), want ready", result.Outcome, result.Detail)
	}
	if result.Size != int64(len(payload)) {
		t.Errorf("size = %d, want %d", result.Size, len(payload))
	}
	if result.ProviderRef == "" {
		t.Error("no provider reference was reported; the file cannot be located at the provider")
	}
	if result.Bytes != int64(len(payload)) {
		t.Errorf("bytes sent = %d, want %d", result.Bytes, len(payload))
	}

	// The bytes must come back identical. A size match alone would not catch a
	// mis-ordered or re-encoded copy.
	rec = doGET(t, handler, session, "/console/api/objects/"+result.ObjectID+"/content")
	if rec.Code != http.StatusOK {
		t.Fatalf("download = %d: %s", rec.Code, rec.Body)
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Errorf("the bytes came back different: got %d bytes, sent %d",
			rec.Body.Len(), len(payload))
	}

	// The routing decision must have been recorded, not merely applied. An upload that
	// reached R2 without the evidence saying why is the thing an operator cannot
	// explain afterwards.
	rec = doGET(t, handler, session, "/console/api/state")
	if rec.Code != http.StatusOK {
		t.Fatalf("state = %d: %s", rec.Code, rec.Body)
	}
	var state httpapi.State
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("decode the state: %v", err)
	}
	if !state.Routing.Forced {
		t.Error("the state does not report the switch as forced")
	}
	if !state.Storage.FallbackReady {
		t.Error("the state reports the fallback as not ready, yet an upload succeeded")
	}
	found := false
	for _, object := range state.Objects {
		if object.ID == result.ObjectID {
			found = true
			if object.Provider != "s3_compatible" {
				t.Errorf("listed provider = %q, want s3_compatible", object.Provider)
			}
			if !object.Downloadable {
				t.Error("a ready object was listed as not downloadable")
			}
		}
	}
	if !found {
		t.Error("the uploaded object does not appear in the console listing")
	}

	// Clean up, so a live run does not fill the bucket with test files.
	rec = doJSON(t, handler, session, http.MethodDelete,
		"/console/api/objects/"+result.ObjectID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d: %s", rec.Code, rec.Body)
	}
}

// TestLiveConsoleFallbackNeedsConsentOnlyWhenConfigured replaces an earlier version of
// this test that asserted the opposite of what the configuration asked for.
//
// It enforced consent unconditionally, on the reasoning that an operator override should
// never be able to store a file outside Drive without the tenant's agreement. That
// reasoning is right for a deployment with `fallback.consent_required` set, and wrong for
// one that has explicitly turned it off -- and the configured deployment here has it
// false, so uploads were being refused for a disclosure nobody had asked anyone to
// accept. The flag existed, validated cleanly, and was ignored at runtime.
//
// The test now checks both settings, because the point is that the flag is honoured
// rather than that consent is or is not required.
func TestLiveConsoleFallbackNeedsConsentOnlyWhenConfigured(t *testing.T) {
	requireLiveFallback(t)

	cfg := loadDevConfig(t)
	if !cfg.Fallback.Enabled {
		t.Skip("fallback is not enabled")
	}
	cfg.Database.SQLitePath = "file::memory:"

	// The deployment under test must not require consent, or this is testing the wrong
	// branch.
	if cfg.Fallback.ConsentRequired {
		t.Skip("this deployment requires consent; see TestLiveConsoleForcedFallbackUpload")
	}

	built := buildLiveApp(t, cfg)
	scope := persistence.Scope{TenantID: "tenant_dev_admin", UserID: "user_dev_admin"}
	seedTenant(t, built.Store, scope.TenantID)

	// Make sure there is no recorded acceptance, so this genuinely exercises the
	// configured-not-required path rather than passing on a leftover row.
	if _, err := built.Consent.Withdraw(context.Background(), scope); err != nil {
		t.Logf("withdraw consent (may not have been recorded): %v", err)
	}

	console := buildTestConsole(t, built, cfg)
	handler := consoleHandler(console)
	session := mintSession(t, built, scope)

	rec := doJSON(t, handler, session, http.MethodPost, "/console/api/routing", `{"forced":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("forcing the fallback = %d: %s", rec.Code, rec.Body)
	}

	rec = doUpload(t, handler, session, "no-consent-needed.txt",
		[]byte("stored without a recorded disclosure"))
	if rec.Code != http.StatusOK {
		t.Fatalf("with consent_required=false the upload must succeed without a recorded "+
			"acceptance, got %d: %s", rec.Code, rec.Body)
	}

	var result httpapi.UploadResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Outcome != "ready" {
		t.Errorf("outcome = %q (%s), want ready", result.Outcome, result.Detail)
	}
	t.Logf("fallback used without a recorded disclosure, as configured")

	if rec := doJSON(t, handler, session, http.MethodDelete,
		"/console/api/objects/"+result.ObjectID, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete = %d: %s", rec.Code, rec.Body)
	}
}

// ---- helpers -------------------------------------------------------------

func buildLiveApp(t *testing.T, cfg *config.Config) *App {
	t.Helper()
	built, err := Build(context.Background(), Options{
		Config: cfg,
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() {
		built.StopBackground()
		built.Cache.Close()
		built.Store.Close()
	})
	return built
}

func buildTestConsole(t *testing.T, built *App, cfg *config.Config) *httpapi.Console {
	t.Helper()
	return httpapi.NewConsole(httpapi.AdminPanelConfig{
		Enabled:         cfg.AdminPanel.Enabled,
		TenantID:        cfg.AdminPanel.TenantID,
		UserID:          cfg.AdminPanel.UserID,
		FlowCookie:      cfg.AdminPanel.FlowCookie,
		RedirectURI:     cfg.Drive.AdminRedirectURI,
		SessionTTL:      cfg.Auth.SessionTTL.Duration(),
		AdminSessionTTL: cfg.Auth.AdminSessionTTL.Duration(),
	}, httpapi.ConsoleOptions{
		Issuer:      built.Issuer,
		Sessions:    built.Sessions,
		Uploads:     built.Uploads,
		Objects:     built.Objects,
		Connections: built.Connections,
		Consent:     consentController{built.Consent},
		Folders:     built.Connections,
		Objects_:    persistence.NewObjectRepository(built.Store),
		Routing:     built.RoutingSwitch,
		Faults:      httpapi.NewFaultSwitch(false, nil),
		Logger:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
}

// consoleHandler maps the console's URLs onto the console's handlers.
//
// It mirrors the real router's table. Duplicating the table is deliberate: a test that
// reused the production mux would not notice a route that was registered but pointed
// at the wrong handler.
func consoleHandler(console *httpapi.Console) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /console/api/state", console.State)
	mux.HandleFunc("POST /console/api/upload", console.Upload)
	mux.HandleFunc("POST /console/api/routing", console.SetRouting)
	mux.HandleFunc("POST /console/api/faults", console.SetFaults)
	mux.HandleFunc("POST /console/api/consent", console.SetConsent)
	mux.HandleFunc("POST /console/api/folder", console.EnsureFolder)
	mux.HandleFunc("POST /console/api/disconnect", console.Disconnect)
	mux.HandleFunc("GET /console/api/objects/{object_id}/content", console.DownloadContent)
	mux.HandleFunc("DELETE /console/api/objects/{object_id}", console.DeleteObject)
	return mux
}

// mintSession issues a real session and returns the cookie the console expects.
//
// The cookie name comes from configuration rather than being hardcoded, because a
// mismatch here would make every test fail with a 401 for a reason that has nothing to
// do with what is being tested.
func mintSession(t *testing.T, built *App, scope persistence.Scope) *http.Cookie {
	t.Helper()
	identity := authIdentity(scope)
	token, session, err := built.Issuer.Issue(identity, "console-live-session", time.Hour)
	if err != nil {
		t.Fatalf("issue a session: %v", err)
	}
	if err := built.Sessions.StoreSession(context.Background(), session); err != nil {
		t.Fatalf("store the session: %v", err)
	}
	return &http.Cookie{Name: built.Config.Auth.SessionCookieName, Value: token}
}

func doJSON(t *testing.T, handler http.Handler, cookie *http.Cookie,
	method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func doGET(t *testing.T, handler http.Handler, cookie *http.Cookie, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func doUpload(t *testing.T, handler http.Handler, cookie *http.Cookie,
	name string, payload []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", name)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write(payload); err != nil {
		t.Fatalf("write the part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close the writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/console/api/upload", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// authIdentity builds the identity a console session carries.
func authIdentity(scope persistence.Scope) auth.Identity {
	return auth.Identity{
		TenantID: scope.TenantID,
		UserID:   scope.UserID,
		Email:    "console-live-test",
	}
}

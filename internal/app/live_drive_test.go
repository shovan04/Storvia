package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shovan04/storvia-project/internal/httpapi"
	"github.com/shovan04/storvia-project/internal/persistence"
)

// liveDriveEnvOptIn gates the tests that write to a real Google Drive account.
//
// It is separate from the fallback opt-in on purpose. These tests create, read, and
// delete files in someone's actual Drive, which is a different kind of risk from
// writing to a test bucket, and it should not be something `make test-live` turns on
// by surprise.
const liveDriveEnvOptIn = "STORVIA_LIVE_DRIVE_TEST"

func requireLiveDrive(t *testing.T) {
	t.Helper()
	if os.Getenv(liveDriveEnvOptIn) != "1" {
		t.Skipf("set %s=1 to run tests that write to a real google drive account", liveDriveEnvOptIn)
	}
}

// TestLiveDefaultUploadGoesToDrive is the test that answers the question this whole
// design turns on: with Drive connected and healthy, does an upload land in Drive?
//
// Everything else in the repository is mocked, so nothing else can answer it. The
// routing policy has a unit test for "preflight succeeded -> use Drive", but a unit
// test of a table is not evidence that a real preflight succeeds against real Google
// with real credentials.
//
// It uses the development database rather than an in-memory one, because the point is
// to use the real encrypted refresh token from the real OAuth round trip.
func TestLiveDefaultUploadGoesToDrive(t *testing.T) {
	requireLiveDrive(t)
	requireLiveFallback(t)

	cfg := loadDevConfig(t)
	if cfg.Fallback.Force {
		t.Fatal("fallback.force is on, so this test would prove nothing about the default")
	}
	if cfg.AdminPanel.SimulateDriveOutage {
		t.Fatal("fault injection is armed, so Drive is being lied about")
	}
	// The real database, not :memory:. The real connection is the subject.
	//
	// The configured path is relative to the *process* working directory, and `go test`
	// runs from the package directory. Left alone this test would silently open a
	// different, empty database, find no connection, and skip -- a green run that
	// proves nothing. So the path is resolved against the repository root.
	if repoRoot := findRepoRoot(); repoRoot != "" {
		if configured := strings.TrimPrefix(cfg.Database.SQLitePath, "file:"); configured != "" &&
			!strings.Contains(configured, ":memory:") && !filepath.IsAbs(configured) {
			absolute := filepath.Join(repoRoot, configured)
			if _, err := os.Stat(absolute); err == nil {
				cfg.Database.SQLitePath = "file:" + absolute
			} else {
				t.Skipf("the development database is not at %s", absolute)
			}
		}
	}
	if strings.Contains(cfg.Database.SQLitePath, ":memory:") {
		t.Skip("this test needs the real development database to reach the real drive connection")
	}

	built := buildLiveApp(t, cfg)
	scope := persistence.Scope{TenantID: "tenant_dev_admin", UserID: "user_dev_admin"}
	seedTenant(t, built.Store, scope.TenantID)

	// Refuse rather than quietly testing something else: an unconnected account would
	// route to the fallback and produce a green run that proves nothing.
	status, err := built.Connections.Status(context.Background(), scope)
	if err != nil {
		t.Fatalf("read the drive connection: %v", err)
	}
	if !status.Connected {
		t.Fatalf("no google drive connection in %s (status %q); sign in through the "+
			"console first -- this test cannot be satisfied by a fallback upload",
			cfg.Database.SQLitePath, status.Status)
	}
	if !status.FolderPresent {
		t.Fatalf("the drive connection has no Storvia/ folder; create it through the console")
	}

	console := buildTestConsole(t, built, cfg)
	handler := consoleHandler(console)
	session := mintSession(t, built, scope)

	// Nothing is forced and no fault is injected. This is the plain default path.
	if built.RoutingSwitch.Forced() {
		t.Fatal("the routing switch is forced; this test must exercise the default")
	}

	payload := []byte("storvia default-routing live test\n" + strings.Repeat("drive-", 512))
	rec := doUpload(t, handler, session, "default-routing.txt", payload)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload = %d: %s", rec.Code, rec.Body)
	}

	var result httpapi.UploadResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if result.Provider != "google_drive" {
		t.Fatalf("provider = %q, want google_drive.\n"+
			"A connected, healthy Drive must be the default. Fallback is for when Drive "+
			"cannot be used, not a peer preference.\nDetail: %s", result.Provider, result.Detail)
	}
	if result.Outcome != "ready" {
		t.Fatalf("outcome = %q (%s), want ready", result.Outcome, result.Detail)
	}
	if result.ProviderRef == "" {
		t.Error("no drive file id was reported, so the file cannot be located")
	}
	if result.Size != int64(len(payload)) || result.Bytes != int64(len(payload)) {
		t.Errorf("size = %d, bytes = %d, want %d", result.Size, result.Bytes, len(payload))
	}

	t.Logf("stored in Drive as file %s", result.ProviderRef)

	// Read it back. A size match alone would not catch a mis-ordered or re-encoded copy,
	// and this is the first time the Drive download path has ever run.
	rec = doGET(t, handler, session, "/console/api/objects/"+result.ObjectID+"/content")
	if rec.Code != http.StatusOK {
		t.Fatalf("download = %d: %s", rec.Code, rec.Body)
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Errorf("the bytes came back different: got %d bytes, sent %d",
			rec.Body.Len(), len(payload))
	}

	// And delete it, so a live run does not litter the account.
	rec = doJSON(t, handler, session, http.MethodDelete,
		"/console/api/objects/"+result.ObjectID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d: %s", rec.Code, rec.Body)
	}
	t.Logf("deleted from Drive and marked deleted locally")
}

// findRepoRoot walks up from the working directory to the module root.
//
// The configured database path is relative, and `go test` runs from the package
// directory. Resolving against the module root is what makes a live test read the same
// database the running server does, instead of an empty one it just created.
func findRepoRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// TestLiveUnavailableDriveFallsBackToTheProvider is the other half of the default.
//
// The whole design rests on a single asymmetry: Drive is where files go, and the
// fallback is only for when Drive cannot be used. This test makes Drive genuinely
// unusable -- the read-only preflight is forced to fail the way a transport failure
// would -- with a real connected account in place, and asserts the upload lands
// elsewhere instead.
//
// It uses the real connection deliberately. With no connection at all, routing would
// refuse the upload rather than fall back, which is a different and much easier thing
// to get right.
func TestLiveUnavailableDriveFallsBackToTheProvider(t *testing.T) {
	requireLiveDrive(t)
	requireLiveFallback(t)

	cfg := loadDevConfig(t)
	if !cfg.Fallback.Enabled {
		t.Skip("fallback is not enabled in the development configuration")
	}
	if repoRoot := findRepoRoot(); repoRoot != "" {
		cfg.Database.SQLitePath = "file:" + repoRoot + "/storvia.dev.db"
	}

	built := buildLiveApp(t, cfg)
	scope := persistence.Scope{TenantID: "tenant_dev_admin", UserID: "user_dev_admin"}
	seedTenant(t, built.Store, scope.TenantID)

	status, err := built.Connections.Status(context.Background(), scope)
	if err != nil {
		t.Fatalf("read the drive connection: %v", err)
	}
	if !status.Connected {
		t.Skip("no google drive connection in the development database; sign in first")
	}

	// No consent step. The configured deployment sets fallback.consent_required=false,
	// so a Drive outage must route to the fallback on its own. Granting consent here
	// would hide the very bug that was reported: the flag existed, validated, and was
	// ignored at runtime, so uploads were refused for a disclosure nobody asked for.
	if _, err := built.Consent.Show(context.Background(), scope); err == nil {
		t.Log("a consent record exists; the fallback still does not depend on it here")
	}

	// Make Drive unavailable the way an outage would, not by switching routing: this
	// is the path that runs when nobody has touched a switch.
	if !built.Drive.SetFaultsEnabled(true) {
		t.Skip("fault injection is not available in this environment")
	}
	t.Cleanup(func() { built.Drive.SetFaultsEnabled(false) })

	console := buildTestConsole(t, built, cfg)
	handler := consoleHandler(console)
	session := mintSession(t, built, scope)

	payload := []byte("storvia drive-unavailable live test\n" + strings.Repeat("fallback-", 256))
	rec := doUpload(t, handler, session, "drive-down.txt", payload)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload = %d: %s", rec.Code, rec.Body)
	}

	var result httpapi.UploadResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if result.Provider == "google_drive" {
		t.Fatal("the upload went to Drive while Drive was forced unavailable; the " +
			"preflight did not move the routing decision")
	}
	if result.Outcome != "ready" {
		t.Fatalf("outcome = %q (%s), want ready", result.Outcome, result.Detail)
	}
	t.Logf("drive unavailable -> routed to %s at %s", result.Provider, result.ProviderRef)

	rec = doJSON(t, handler, session, http.MethodDelete,
		"/console/api/objects/"+result.ObjectID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d: %s", rec.Code, rec.Body)
	}
}

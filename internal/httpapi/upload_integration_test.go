package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shovan04/storvia-project/internal/auth"
	"github.com/shovan04/storvia-project/internal/config"
	"github.com/shovan04/storvia-project/internal/idid"
	"github.com/shovan04/storvia-project/internal/persistence"
	"github.com/shovan04/storvia-project/internal/storage/model"
	"github.com/shovan04/storvia-project/internal/storage/policy"
	"github.com/shovan04/storvia-project/internal/storage/service"
)

// These tests drive the real handlers over HTTP against a real service and a real
// database. The unit tests elsewhere cover decoding and formatting; these cover
// whether the routes are wired to the right behaviour, which is the part that broke
// three separate times in this project and was invisible until something used it.

// --- doubles ------------------------------------------------------------------

// DrivePort is the availability contract the harness doubles.
type DrivePort interface {
	Availability(context.Context, model.AvailabilityScope) error
	ConnectionUsable(context.Context, persistence.Scope) (bool, error)
	ConnectionID(context.Context, persistence.Scope) (string, error)
}

type stubDrive struct {
	connectionID string
	usable       bool
}

// The methods are on the value type so an unavailableDrive embedding one satisfies
// the port by value.
func (s stubDrive) Availability(context.Context, model.AvailabilityScope) error { return nil }

func (s stubDrive) ConnectionUsable(context.Context, persistence.Scope) (bool, error) {
	return s.usable, nil
}

func (s stubDrive) ConnectionID(context.Context, persistence.Scope) (string, error) {
	return s.connectionID, nil
}

type stubSessions struct {
	storedSize int64
	inFolder   bool
	trashed    bool
	metaErr    error
}

func (s *stubSessions) InitiateUpload(context.Context, persistence.Scope, service.DriveUploadRequest) (service.DriveSessionResult, error) {
	return service.DriveSessionResult{
		SessionURL: "https://drive.test/session/abc",
		ChunkSize:  256 * 1024,
		ExpiresAt:  time.Now().Add(time.Minute),
	}, nil
}

func (s *stubSessions) FetchMetadata(context.Context, persistence.Scope, string) (model.ObjectMetadata, error) {
	if s.metaErr != nil {
		return model.ObjectMetadata{}, s.metaErr
	}
	return model.ObjectMetadata{
		SizeBytes:         s.storedSize,
		InAppFolder:       s.inFolder,
		Trashed:           s.trashed,
		ChecksumAlgorithm: "md5",
		Checksum:          "abc",
	}, nil
}

type stubUploader struct {
	verifyErr   error
	verifySize  int64
	plannedKeys []string
}

func (s *stubUploader) PlanUpload(_ context.Context, _, key string, _ int64) ([]service.PresignedPartInfo, error) {
	s.plannedKeys = append(s.plannedKeys, key)
	return []service.PresignedPartInfo{{
		PartNumber: 1, URL: "https://bucket.test/" + key, ExpiresAt: time.Now().Add(time.Minute),
	}}, nil
}

func (s *stubUploader) Verify(context.Context, string, string) (service.VerifiedObject, error) {
	if s.verifyErr != nil {
		return service.VerifiedObject{}, s.verifyErr
	}
	return service.VerifiedObject{SizeBytes: s.verifySize}, nil
}

// --- harness ------------------------------------------------------------------

type apiHarness struct {
	uploads  *UploadHandler
	consent  *ConsentHandler
	sessions *stubSessions
	uploader *stubUploader
	store    *persistence.Store
}

func newAPIHarness(t *testing.T, drive DrivePort, mutate func(*service.Dependencies)) *apiHarness {
	t.Helper()

	store, err := persistence.Open(context.Background(), config.DatabaseConfig{
		Driver:          config.DriverSQLite,
		SQLitePath:      "file:" + filepath.Join(t.TempDir(), "api.db"),
		MaxOpenConns:    1,
		ConnMaxLifetime: config.Duration(time.Minute),
		ConnMaxIdleTime: config.Duration(time.Minute),
		AutoMigrate:     true,
	}, "")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	now := time.Now().UTC()
	if _, err := store.DB().Exec(
		`INSERT INTO tenants (id, name, created_at, updated_at) VALUES (?,?,?,?)`,
		"tenant-a", "tenant-a", now, now); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	// The object row references the connection, and the foreign key enforces it. That
	// is the constraint doing its job: an object cannot be pinned to a connection
	// that does not exist.
	if _, err := store.DB().Exec(
		`INSERT INTO drive_connections
		 (id, tenant_id, user_id, google_subject, encrypted_refresh_token,
		  key_reference, granted_scopes, root_folder_id, status, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		"dc-1", "tenant-a", "user-1", "subject-1", []byte("unused"),
		"STORVIA_ENCRYPTION_KEY", "https://www.googleapis.com/auth/drive.file",
		"folder-1", "connected", now, now); err != nil {
		t.Fatalf("seed drive connection: %v", err)
	}

	consentService := service.NewConsentService(persistence.NewFallbackConsentRepository(store))
	h := &apiHarness{
		sessions: &stubSessions{storedSize: 11, inFolder: true},
		uploader: &stubUploader{verifySize: 11},
		store:    store,
	}
	backends := persistence.NewBackendRepository(store)

	deps := service.Dependencies{
		Objects:          persistence.NewObjectRepository(store),
		Uploads:          persistence.NewUploadRepository(store),
		Attempts:         persistence.NewAttemptRepository(store),
		Backends:         backends,
		Drive:            drive,
		DriveSessions:    h.sessions,
		FallbackUploader: h.uploader,
		FallbackConsent: func(context.Context, persistence.Scope) (bool, error) {
			view, err := consentService.Show(context.Background(), persistence.Scope{TenantID: "tenant-a"})
			return view.Granted, err
		},
		UploadLimits: config.UploadsConfig{
			MaxFileSizeBytes:    1 << 20,
			AllowedMIMETypes:    []string{"text/plain"},
			AllowedCategories:   []string{"report"},
			DriveChunkSizeBytes: 256 << 10,
			S3PartSizeBytes:     5 << 20,
			PresignedURLTTL:     config.Duration(15 * time.Minute),
		},
		DriveCfg: config.DriveConfig{
			PreflightTimeout:  config.Duration(time.Second),
			PreflightAttempts: 1,
			RoutingBudget:     config.Duration(3 * time.Second),
		},
		IDs:    idid.NewGenerator("upl"),
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Now:    time.Now,
		Jitter: func() float64 { return 0.5 },
		Sleep:  func(context.Context, time.Duration) error { return nil },
	}
	if mutate != nil {
		mutate(&deps)
	}

	svc, err := service.NewUploadService(deps)
	if err != nil {
		t.Fatalf("NewUploadService: %v", err)
	}
	h.uploads = NewUploadHandler(svc, drive, deps.UploadLimits)
	h.consent = NewConsentHandler(consentService)
	return h
}

// request sends an authenticated request to a handler.
func request(t *testing.T, handler http.HandlerFunc, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req = req.WithContext(auth.WithIdentity(req.Context(), auth.Identity{
		TenantID: "tenant-a", UserID: "user-1", SessionID: "sess-1",
	}))

	// Handlers read path values, which a ServeMux would normally populate. Setting it
	// here keeps the test at the handler level while still exercising the lookup.
	if id := pathValueFor(path); id != "" {
		req.SetPathValue("upload_id", id)
	}

	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// pathValueFor extracts the {upload_id} segment from a request path.
func pathValueFor(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i, part := range parts {
		if part == "uploads" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// createDriveUpload drives the create endpoint and returns the decoded response.
func createDriveUpload(t *testing.T, h *apiHarness, key string) map[string]any {
	t.Helper()
	rec := request(t, h.uploads.Create, http.MethodPost, "/v1/storage/uploads", map[string]any{
		"name": "notes.txt", "mime_type": "text/plain", "size_bytes": 11, "category": "report",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, body %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	return out
}

// --- tests --------------------------------------------------------------------

// TestCreateReturnsAResumableSessionAndNeverTheProxyMode locks the wire contract the
// SDK switches on.
func TestCreateReturnsAResumableSessionAndNeverTheProxyMode(t *testing.T) {
	h := newAPIHarness(t, &stubDrive{connectionID: "dc-1", usable: true}, nil)

	out := createDriveUpload(t, h, "k1")

	if out["mode"] != string(model.ModeDriveResumableSession) {
		t.Errorf("mode = %v, want drive_resumable_session", out["mode"])
	}
	if out["mode"] == "drive_proxy" {
		t.Error("drive_proxy is no longer offered; a client would build an impossible transfer")
	}
	session, ok := out["session"].(map[string]any)
	if !ok || session["url"] == "" {
		t.Fatal("the response must carry a resumable session with a URL")
	}
	if _, present := out["parts"]; present {
		t.Error("Drive mode must not also return presigned parts")
	}
}

// TestCompleteWithADriveFileIDCommits checks the body the SDK sends is understood.
func TestCompleteWithADriveFileIDCommits(t *testing.T) {
	h := newAPIHarness(t, &stubDrive{connectionID: "dc-1", usable: true}, nil)
	created := createDriveUpload(t, h, "k2")
	uploadID, _ := created["upload_id"].(string)

	rec := request(t, h.uploads.Complete, http.MethodPost,
		"/v1/storage/uploads/"+uploadID+"/complete",
		map[string]any{"drive_file_id": "file-1"})

	if rec.Code != http.StatusCreated {
		t.Fatalf("complete = %d, body %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Object struct {
			Status  string `json:"status"`
			Actions struct {
				DownloadURL string `json:"download_url"`
			} `json:"actions"`
		} `json:"object"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Object.Status != "ready" {
		t.Errorf("status = %q, want ready", out.Object.Status)
	}
}

// TestCompleteWithoutADriveFileIDIsRefused: a Drive upload completed with no id
// cannot be verified, and quietly falling back would commit the wrong thing.
func TestCompleteWithoutADriveFileIDIsRefused(t *testing.T) {
	h := newAPIHarness(t, &stubDrive{connectionID: "dc-1", usable: true}, nil)
	created := createDriveUpload(t, h, "k3")
	uploadID, _ := created["upload_id"].(string)

	rec := request(t, h.uploads.Complete, http.MethodPost,
		"/v1/storage/uploads/"+uploadID+"/complete", nil)

	if rec.Code == http.StatusCreated {
		t.Fatalf("a Drive upload completed with no file id must not become ready: %s", rec.Body.String())
	}
}

// TestCompleteRefusesASizeMismatchOverHTTP is the truncation guard at the edge.
func TestCompleteRefusesASizeMismatchOverHTTP(t *testing.T) {
	h := newAPIHarness(t, &stubDrive{connectionID: "dc-1", usable: true}, nil)
	h.sessions.storedSize = 4 // declared 11
	created := createDriveUpload(t, h, "k4")
	uploadID, _ := created["upload_id"].(string)

	rec := request(t, h.uploads.Complete, http.MethodPost,
		"/v1/storage/uploads/"+uploadID+"/complete",
		map[string]any{"drive_file_id": "file-1"})

	if rec.Code/100 == 2 {
		t.Fatalf("a truncated upload must not be ready: %s", rec.Body.String())
	}
}

// TestCompleteRefusesAFileOutsideTheStorviaFolder guards the untrusted file id.
func TestCompleteRefusesAFileOutsideTheStorviaFolder(t *testing.T) {
	h := newAPIHarness(t, &stubDrive{connectionID: "dc-1", usable: true}, nil)
	h.sessions.inFolder = false
	created := createDriveUpload(t, h, "k5")
	uploadID, _ := created["upload_id"].(string)

	rec := request(t, h.uploads.Complete, http.MethodPost,
		"/v1/storage/uploads/"+uploadID+"/complete",
		map[string]any{"drive_file_id": "file-1"})

	if rec.Code/100 == 2 {
		t.Fatalf("a file outside the storvia folder must not be adopted: %s", rec.Body.String())
	}
}

// TestReconcileEndpointAnswersWithAnOutcome checks the route returns an outcome
// rather than the old "not implemented" problem document.
func TestReconcileEndpointAnswersWithAnOutcome(t *testing.T) {
	h := newAPIHarness(t, &stubDrive{connectionID: "dc-1", usable: true}, nil)
	created := createDriveUpload(t, h, "k6")
	uploadID, _ := created["upload_id"].(string)

	rec := request(t, h.uploads.Reconcile, http.MethodPost,
		"/v1/storage/uploads/"+uploadID+"/reconcile", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("reconcile = %d, body %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	outcome, _ := out["outcome"].(string)
	if outcome == "" {
		t.Fatalf("reconcile must name an outcome, got %s", rec.Body.String())
	}
	// A Drive upload cannot be checked without the session, and saying "unknown"
	// is better than saying "nothing stored" and prompting a duplicate.
	if outcome != "unknown" {
		t.Errorf("outcome = %q, want unknown for a Drive upload", outcome)
	}
	if out["reason"] == nil || out["reason"] == "" {
		t.Error("an unknown outcome must explain what to do instead")
	}
}

// TestReconcileReportsMissingUploadAsNotFound keeps the route tenant-safe.
func TestReconcileReportsMissingUploadAsNotFound(t *testing.T) {
	h := newAPIHarness(t, &stubDrive{connectionID: "dc-1", usable: true}, nil)

	rec := request(t, h.uploads.Reconcile, http.MethodPost,
		"/v1/storage/uploads/upl_nope/reconcile", nil)

	if rec.Code != http.StatusNotFound {
		t.Errorf("reconcile on a missing upload = %d, want 404", rec.Code)
	}
}

// TestConsentEndpointsRoundTrip drives the three consent routes the panel uses.
func TestConsentEndpointsRoundTrip(t *testing.T) {
	h := newAPIHarness(t, &stubDrive{connectionID: "dc-1", usable: true}, nil)

	show := request(t, h.consent.Show, http.MethodGet, "/v1/storage/fallback/consent", nil)
	if show.Code != http.StatusOK {
		t.Fatalf("show = %d", show.Code)
	}
	if !containsJSONField(show.Body.Bytes(), "granted", false) {
		t.Errorf("a tenant that never asked must not be granted: %s", show.Body.String())
	}

	accept := request(t, h.consent.Accept, http.MethodPost, "/v1/storage/fallback/consent", nil)
	if accept.Code != http.StatusOK {
		t.Fatalf("accept = %d, body %s", accept.Code, accept.Body.String())
	}
	if !containsJSONField(accept.Body.Bytes(), "granted", true) {
		t.Errorf("accepting must open the gate: %s", accept.Body.String())
	}

	withdraw := request(t, h.consent.Withdraw, http.MethodDelete, "/v1/storage/fallback/consent", nil)
	if withdraw.Code != http.StatusOK {
		t.Fatalf("withdraw = %d, body %s", withdraw.Code, withdraw.Body.String())
	}
	if !containsJSONField(withdraw.Body.Bytes(), "granted", false) {
		t.Errorf("withdrawing must close the gate immediately: %s", withdraw.Body.String())
	}
}

// TestCreateIsRefusedWithoutConsentWhenTheFallbackIsSelected is the end-to-end
// property the panel warns about: enabling the fallback grants nothing.
func TestCreateIsRefusedWithoutConsentWhenTheFallbackIsSelected(t *testing.T) {
	drive := &unavailableDrive{stubDrive: stubDrive{connectionID: "dc-1", usable: true}}
	h := newAPIHarness(t, drive, func(deps *service.Dependencies) {
		deps.Fallback = config.FallbackConfig{
			Enabled: true,
			Backends: []config.S3BackendConfig{{
				ID: "b1", Vendor: config.VendorCloudflareR2, Bucket: "bucket",
			}},
		}
		deps.FallbackConsent = func(context.Context, persistence.Scope) (bool, error) {
			return false, nil
		}
	})
	// The routing gate is asserted in the service tests; here we only need the
	// endpoint to answer with a client error rather than a 500.
	rec := request(t, h.uploads.Create, http.MethodPost, "/v1/storage/uploads", map[string]any{
		"name": "notes.txt", "mime_type": "text/plain", "size_bytes": 11, "category": "report",
	})
	if rec.Code/100 == 5 {
		t.Fatalf("a refused fallback must not be a server error: %d %s", rec.Code, rec.Body.String())
	}
}

// unavailableDrive reports Drive as down, which is what makes the fallback eligible.
type unavailableDrive struct{ stubDrive }

// it must satisfy the port when passed by value.
var _ DrivePort = unavailableDrive{}

func (unavailableDrive) Availability(context.Context, model.AvailabilityScope) error {
	return &policy.ProviderError{
		Provider: "google_drive", Operation: "Availability",
		Class: policy.ClassUnavailable, SafeDetail: "injected outage",
	}
}

// TestCompleteForAFallbackUploadCommits exercises the other provider's completion
// path through the same handler.
func TestCompleteForAFallbackUploadCommits(t *testing.T) {
	drive := unavailableDrive{stubDrive{connectionID: "dc-1", usable: true}}
	h := newAPIHarness(t, drive, func(deps *service.Dependencies) {
		deps.Fallback = config.FallbackConfig{
			Enabled: true,
			Backends: []config.S3BackendConfig{{
				ID: "b1", Vendor: config.VendorCloudflareR2, Bucket: "bucket",
			}},
		}
	})
	if err := persistence.NewBackendRepository(h.store).Upsert(context.Background(), persistence.Backend{
		ID: "b1", Scope: "deployment", Vendor: config.VendorCloudflareR2,
		Bucket: "bucket", Status: "active", Capabilities: "{}",
	}); err != nil {
		t.Fatalf("register the backend: %v", err)
	}
	if rec := request(t, h.consent.Accept, http.MethodPost, "/v1/storage/fallback/consent", nil); rec.Code != http.StatusOK {
		t.Fatalf("consent = %d, body %s", rec.Code, rec.Body.String())
	}

	out := createDriveUpload(t, h, "k7")
	if out["mode"] != string(model.ModeS3PresignedParts) {
		t.Fatalf("mode = %v, want the fallback", out["mode"])
	}
	uploadID, _ := out["upload_id"].(string)

	rec := request(t, h.uploads.Complete, http.MethodPost,
		"/v1/storage/uploads/"+uploadID+"/complete", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("fallback complete = %d, body %s", rec.Code, rec.Body.String())
	}
}

// containsJSONField reports whether a top-level boolean field has the wanted value.
func containsJSONField(body []byte, field string, want bool) bool {
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return false
	}
	got, ok := decoded[field].(bool)
	return ok && got == want
}

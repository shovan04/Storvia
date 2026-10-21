package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/shovan04/storvia-project/internal/apperr"
	"github.com/shovan04/storvia-project/internal/config"
	"github.com/shovan04/storvia-project/internal/idid"
	"github.com/shovan04/storvia-project/internal/persistence"
	"github.com/shovan04/storvia-project/internal/storage/model"
	"github.com/shovan04/storvia-project/internal/storage/policy"
)

// fakeDrive is a DriveAvailability stub.
//
// Every field is controllable so a test can express one condition from the policy
// table without a network, a Google account, or a real outage.
type fakeDrive struct {
	// errors is consumed one entry per Availability call; the last entry repeats.
	errors []error
	calls  int

	connectionID  string
	usable        bool
	configured    bool
	resolveErr    error
	usableErr     error
	connectionErr error
}

func (f *fakeDrive) Availability(_ context.Context, _ model.AvailabilityScope) error {
	index := f.calls
	f.calls++
	if index >= len(f.errors) {
		if len(f.errors) == 0 {
			return nil
		}
		index = len(f.errors) - 1
	}
	return f.errors[index]
}

func (f *fakeDrive) ConnectionID(_ context.Context, _ persistence.Scope) (string, error) {
	if f.connectionErr != nil {
		return "", f.connectionErr
	}
	return f.connectionID, nil
}

func (f *fakeDrive) ConnectionUsable(_ context.Context, _ persistence.Scope) (bool, error) {
	if f.usableErr != nil {
		return false, f.usableErr
	}
	return f.usable, nil
}

func (f *fakeDrive) IsConfigured() bool { return f.configured }

// testUploadLimits returns a permissive limits block.
func testUploadLimits() config.UploadsConfig {
	return config.UploadsConfig{
		MaxFileSizeBytes:    1024 * 1024,
		AllowedMIMETypes:    []string{"application/pdf", "text/plain"},
		AllowedCategories:   []string{"report"},
		DriveChunkSizeBytes: 256 << 10,
		S3PartSizeBytes:     5 << 20,
		PresignedURLTTL:     config.Duration(15 * time.Minute),
	}
}

// testDriveConfig returns the documented routing budget values.
func testDriveConfig() config.DriveConfig {
	return config.DriveConfig{
		PreflightTimeout:  config.Duration(3 * time.Second),
		PreflightAttempts: 2,
		RoutingBudget:     config.Duration(8 * time.Second),
		CircuitFailures:   2,
		CircuitCooldown:   config.Duration(30 * time.Second),
	}
}

// newTestService assembles a service over an isolated SQLite database.
func newTestService(t *testing.T, drive DriveAvailability, mutate func(*Dependencies)) *UploadService {
	t.Helper()

	store, err := persistence.Open(context.Background(), config.DatabaseConfig{
		Driver:          config.DriverSQLite,
		SQLitePath:      "file:" + filepath.Join(t.TempDir(), "svc.db"),
		MaxOpenConns:    1,
		ConnMaxLifetime: config.Duration(time.Minute),
		ConnMaxIdleTime: config.Duration(time.Minute),
	}, "")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if _, err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	insertTestDriveConnection(t, store)

	deps := Dependencies{
		Objects:       persistence.NewObjectRepository(store),
		Uploads:       persistence.NewUploadRepository(store),
		Attempts:      persistence.NewAttemptRepository(store),
		Backends:      persistence.NewBackendRepository(store),
		Drive:         drive,
		DriveSessions: &fakeDriveSessions{},
		UploadLimits:  testUploadLimits(),
		DriveCfg:      testDriveConfig(),
		IDs:           idid.NewGenerator("upl"),
		Logger:        slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Now:           time.Now,
		Jitter:        func() float64 { return 0.5 },
		// Never actually sleep in a test; the budget logic is asserted separately.
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	if mutate != nil {
		mutate(&deps)
	}

	svc, err := NewUploadService(deps)
	if err != nil {
		t.Fatalf("NewUploadService: %v", err)
	}
	return svc
}

// insertTestDriveConnection seeds the connection the stub Drive adapter reports.
//
// Objects carry a foreign key to drive_connections, so a location can never point
// at a connection that does not exist. The service tests therefore have to seed
// one, which is itself a demonstration that the constraint is live.
func insertTestDriveConnection(t *testing.T, store *persistence.Store) {
	t.Helper()
	now := time.Now().UTC()
	_, err := store.DB().ExecContext(context.Background(), `
		INSERT INTO drive_connections (
			id, tenant_id, user_id, google_subject, encrypted_refresh_token,
			key_reference, granted_scopes, root_folder_id, status, created_at, updated_at
		) VALUES ('dc-1', ?, ?, 'subject-1', ?, 'test-key',
			'https://www.googleapis.com/auth/drive.file', 'folder-1', 'connected', ?, ?)`,
		testScope().TenantID, testScope().UserID, []byte("ciphertext"), now, now)
	if err != nil {
		t.Fatalf("insert test drive connection: %v", err)
	}
}

// testScope is the authenticated tenant/user pair.
func testScope() persistence.Scope {
	return persistence.Scope{TenantID: "tenant-a", UserID: "user-1"}
}

// validRequest is an upload request that passes validation.
func validRequest() CreateUploadRequest {
	return CreateUploadRequest{
		Name:      "report.pdf",
		MIMEType:  "application/pdf",
		SizeBytes: 2048,
		Category:  "report",
	}
}

// unavailable builds an eligible preflight availability failure.
func unavailable() *policy.ProviderError {
	return &policy.ProviderError{
		Provider:  "google_drive",
		Operation: "Availability",
		Class:     policy.ClassUnavailable,
	}
}

func TestValidationRejectsBadRequests(t *testing.T) {
	svc := newTestService(t, &fakeDrive{connectionID: "dc-1", usable: true, configured: true}, nil)

	tests := []struct {
		name    string
		mutate  func(*CreateUploadRequest)
		wantErr bool
	}{
		{"valid", func(r *CreateUploadRequest) {}, false},
		{"empty name", func(r *CreateUploadRequest) { r.Name = "" }, true},
		{"blank name", func(r *CreateUploadRequest) { r.Name = "   " }, true},
		{"long name", func(r *CreateUploadRequest) { r.Name = string(make([]byte, 513)) }, true},
		{"zero size", func(r *CreateUploadRequest) { r.SizeBytes = 0 }, true},
		{"negative size", func(r *CreateUploadRequest) { r.SizeBytes = -1 }, true},
		{"oversize", func(r *CreateUploadRequest) { r.SizeBytes = 2 * 1024 * 1024 }, true},
		{"unknown category", func(r *CreateUploadRequest) { r.Category = "video" }, true},
		{"empty category", func(r *CreateUploadRequest) { r.Category = "" }, true},
		{"unlisted mime", func(r *CreateUploadRequest) { r.MIMEType = "application/x-msdownload" }, true},
		{"empty mime", func(r *CreateUploadRequest) { r.MIMEType = "" }, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			request := validRequest()
			tc.mutate(&request)
			err := svc.Validate(request)
			if tc.wantErr && err == nil {
				t.Error("want a validation error")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if tc.wantErr && err != nil && !apperr.Is(err, apperr.KindValidation) {
				t.Errorf("error kind = %q, want validation", apperr.KindOf(err))
			}
		})
	}
}

// TestCreateUploadSelectsDriveWhenAvailable is the primary happy path.
func TestCreateUploadSelectsDriveWhenAvailable(t *testing.T) {
	drive := &fakeDrive{connectionID: "dc-1", usable: true, configured: true}
	svc := newTestService(t, drive, nil)

	result, err := svc.CreateUpload(context.Background(), testScope(), "idem-1", validRequest())
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}

	if result.Provider != model.ProviderGoogleDrive {
		t.Errorf("provider = %q, want google_drive", result.Provider)
	}
	if result.Mode != model.ModeDriveResumableSession {
		t.Errorf("mode = %q, want drive_resumable_session", result.Mode)
	}
	if result.Session == nil || result.Session.URL == "" {
		t.Error("a Drive upload must hand the client a resumable session")
	}
	if result.FallbackReason != model.FallbackNone {
		t.Errorf("fallback reason = %q, want none", result.FallbackReason)
	}
	if result.Replayed {
		t.Error("a first request is not a replay")
	}
	if result.ChunkSize != testUploadLimits().DriveChunkSizeBytes {
		t.Errorf("chunk size = %d, want %d", result.ChunkSize, testUploadLimits().DriveChunkSizeBytes)
	}
	if drive.calls == 0 {
		t.Error("the Drive preflight must run before a provider is selected")
	}
}

// TestCreateUploadSelectsFallbackOnlyOnAnEligibleOutage is the routing contract.
func TestCreateUploadSelectsFallbackOnlyOnAnEligibleOutage(t *testing.T) {
	tests := []struct {
		name        string
		preflight   error
		consent     bool
		backend     bool
		wantProv    model.Provider
		wantErrKind apperr.Kind
	}{
		{
			name:      "eligible outage with consent and a backend selects S3",
			preflight: unavailable(),
			consent:   true, backend: true,
			wantProv: model.ProviderS3Compatible,
		},
		{
			name:      "quota never selects S3",
			preflight: &policy.ProviderError{Class: policy.ClassQuota, Reason: "storageQuotaExceeded"},
			consent:   true, backend: true,
			// A quota error is not an outage, so the upload fails with a quota
			// state rather than being written anywhere.
			wantErrKind: apperr.KindQuota,
		},
		{
			name:      "rate limiting never selects S3",
			preflight: &policy.ProviderError{Class: policy.ClassRateLimited, StatusCode: 429},
			consent:   true, backend: true,
			wantProv: model.ProviderGoogleDrive,
		},
		{
			name:      "a revoked grant never selects S3",
			preflight: &policy.ProviderError{Class: policy.ClassConsentRequired, Reason: "invalid_grant"},
			consent:   true, backend: true,
			// A revoked grant must be surfaced, not silently written elsewhere.
			wantErrKind: apperr.KindConsentRequired,
		},
		{
			name:      "missing consent never selects S3",
			preflight: unavailable(),
			consent:   false, backend: true,
			wantProv: model.ProviderGoogleDrive,
		},
		{
			name:      "no configured backend never selects S3",
			preflight: unavailable(),
			consent:   true, backend: false,
			wantProv: model.ProviderGoogleDrive,
		},
		{
			name:      "an unclassified failure never selects S3",
			preflight: &policy.ProviderError{Class: policy.ClassUnknown},
			consent:   true, backend: true,
			// An unclassified failure is undecidable, so the upload must not be
			// written anywhere until the condition clears.
			wantErrKind: apperr.KindUnavailable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			drive := &fakeDrive{
				connectionID: "dc-1",
				usable:       true,
				configured:   true,
				errors:       []error{tc.preflight, tc.preflight},
			}

			svc := newTestService(t, drive, func(deps *Dependencies) {
				deps.FallbackUploader = newStubFallback()
				deps.Fallback = config.FallbackConfig{
					Enabled: tc.backend,
					Backends: []config.S3BackendConfig{{
						ID: "b1", Vendor: config.VendorCloudflareR2, Bucket: "bucket",
					}},
				}
				deps.FallbackConsent = func(context.Context, persistence.Scope) (bool, error) {
					return tc.consent, nil
				}
			})

			// Record the backend so the routing gate can see a real destination.
			if tc.backend {
				if err := svc.deps.Backends.Upsert(context.Background(), persistence.Backend{
					ID: "b1", Scope: "deployment", Vendor: "cloudflare_r2",
					Bucket: "bucket", Status: "active", Capabilities: "{}",
				}); err != nil {
					t.Fatal(err)
				}
			}

			result, err := svc.CreateUpload(context.Background(), testScope(), "idem-1", validRequest())
			if tc.wantErrKind != "" {
				if err == nil {
					t.Fatalf("want a %s error", tc.wantErrKind)
				}
				return
			}
			if err != nil {
				t.Fatalf("CreateUpload: %v", err)
			}
			if result.Provider != tc.wantProv {
				t.Errorf("provider = %q, want %q (reason %q)",
					result.Provider, tc.wantProv, result.FallbackReason)
			}
		})
	}
}

func TestCreateUploadRequiresConsentBeforeAnyProviderWrite(t *testing.T) {
	// With Drive reachable, an upload must go to Drive and never consult the
	// fallback, which is what consent gates.
	drive := &fakeDrive{connectionID: "dc-1", usable: true, configured: true}
	svc := newTestService(t, drive, nil)

	_, err := svc.CreateUpload(context.Background(), testScope(), "idem-1", validRequest())
	if err != nil {
		t.Fatalf("CreateUpload with Drive available: %v", err)
	}
	if drive.calls != 1 {
		t.Errorf("preflight ran %d times, want 1", drive.calls)
	}
}

func TestCreateUploadRequiresADriveConnection(t *testing.T) {
	drive := &fakeDrive{connectionID: "", usable: false, configured: true}
	svc := newTestService(t, drive, nil)

	_, err := svc.CreateUpload(context.Background(), testScope(), "idem-1", validRequest())
	if err == nil {
		t.Fatal("an upload without a Drive connection must be refused")
	}
	if !apperr.Is(err, apperr.KindConsentRequired) {
		t.Errorf("error kind = %q, want consent_required", apperr.KindOf(err))
	}
}

// TestIdempotentReplayReturnsTheSameUpload is the duplicate-upload guard.
func TestIdempotentReplayReturnsTheSameUpload(t *testing.T) {
	drive := &fakeDrive{connectionID: "dc-1", usable: true, configured: true}
	svc := newTestService(t, drive, nil)
	ctx := context.Background()

	first, err := svc.CreateUpload(ctx, testScope(), "idem-1", validRequest())
	if err != nil {
		t.Fatal(err)
	}

	callsAfterFirst := drive.calls

	second, err := svc.CreateUpload(ctx, testScope(), "idem-1", validRequest())
	if err != nil {
		t.Fatalf("replay: %v", err)
	}

	if second.UploadID != first.UploadID {
		t.Errorf("replay upload id = %q, want the original %q", second.UploadID, first.UploadID)
	}
	if second.ObjectID != first.ObjectID {
		t.Errorf("replay object id = %q, want the original %q", second.ObjectID, first.ObjectID)
	}
	if !second.Replayed {
		t.Error("the second response must be marked as a replay")
	}
	// A replay must not run a second preflight, because that would be a second
	// chance to route to a different provider for the same logical upload.
	if drive.calls != callsAfterFirst {
		t.Errorf("preflight ran %d times after a replay, want %d", drive.calls, callsAfterFirst)
	}
}

// TestReusingAKeyWithDifferentMetadataConflicts prevents a client bug from
// silently retargeting an in-flight upload.
func TestReusingAKeyWithDifferentMetadataConflicts(t *testing.T) {
	drive := &fakeDrive{connectionID: "dc-1", usable: true, configured: true}
	svc := newTestService(t, drive, nil)
	ctx := context.Background()

	if _, err := svc.CreateUpload(ctx, testScope(), "idem-1", validRequest()); err != nil {
		t.Fatal(err)
	}

	changed := validRequest()
	changed.SizeBytes = 4096
	changed.MIMEType = "text/plain"

	_, err := svc.CreateUpload(ctx, testScope(), "idem-1", changed)
	if err == nil {
		t.Fatal("reusing a key with different metadata must conflict")
	}
	if !apperr.Is(err, apperr.KindConflict) {
		t.Errorf("error kind = %q, want conflict", apperr.KindOf(err))
	}
}

// TestIdempotencyIsTenantScoped keeps one tenant's key from colliding with
// another's.
func TestIdempotencyIsTenantScoped(t *testing.T) {
	drive := &fakeDrive{connectionID: "dc-1", usable: true, configured: true}
	svc := newTestService(t, drive, nil)
	ctx := context.Background()

	first, err := svc.CreateUpload(ctx, testScope(), "shared-key", validRequest())
	if err != nil {
		t.Fatal(err)
	}

	otherScope := persistence.Scope{TenantID: "tenant-b", UserID: "user-2"}
	second, err := svc.CreateUpload(ctx, otherScope, "shared-key", validRequest())
	if err != nil {
		t.Fatalf("a different tenant must be able to use the same key: %v", err)
	}
	if second.ObjectID == first.ObjectID {
		t.Error("two tenants produced the same object id")
	}
}

func TestGetUploadIsTenantScoped(t *testing.T) {
	drive := &fakeDrive{connectionID: "dc-1", usable: true, configured: true}
	svc := newTestService(t, drive, nil)
	ctx := context.Background()

	created, err := svc.CreateUpload(ctx, testScope(), "idem-1", validRequest())
	if err != nil {
		t.Fatal(err)
	}

	intruder := persistence.Scope{TenantID: "tenant-b", UserID: "user-2"}
	if _, err := svc.GetUpload(ctx, intruder, created.UploadID); !apperr.Is(err, apperr.KindNotFound) {
		t.Errorf("cross-tenant GetUpload = %v, want not_found", err)
	}
}

func TestGetUploadReportsThePinnedProvider(t *testing.T) {
	drive := &fakeDrive{connectionID: "dc-1", usable: true, configured: true}
	svc := newTestService(t, drive, nil)
	ctx := context.Background()

	created, err := svc.CreateUpload(ctx, testScope(), "idem-1", validRequest())
	if err != nil {
		t.Fatal(err)
	}

	view, err := svc.GetUpload(ctx, testScope(), created.UploadID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Provider != string(model.ProviderGoogleDrive) {
		t.Errorf("provider = %q, want google_drive", view.Provider)
	}
	if view.MutationMayHaveStarted {
		t.Error("a freshly routed upload has not started writing bytes yet")
	}
	if view.ExpectedBytes != 2048 {
		t.Errorf("expected bytes = %d, want 2048", view.ExpectedBytes)
	}
}

// TestCancelRefusedOnceAMutationMayHaveStarted is the ambiguity guard: cancelling
// an upload that might have written data would strand a file at the provider.
func TestCancelRefusedOnceAMutationMayHaveStarted(t *testing.T) {
	drive := &fakeDrive{connectionID: "dc-1", usable: true, configured: true}
	svc := newTestService(t, drive, nil)
	ctx := context.Background()

	created, err := svc.CreateUpload(ctx, testScope(), "idem-1", validRequest())
	if err != nil {
		t.Fatal(err)
	}

	attempt, err := svc.deps.Attempts.GetActiveForUpload(ctx, created.UploadID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.deps.Attempts.MarkMutationStarted(ctx, attempt.ID, 2048); err != nil {
		t.Fatal(err)
	}

	err = svc.CancelUpload(ctx, testScope(), created.UploadID)
	if err == nil {
		t.Fatal("cancelling an upload that may have written data must be refused")
	}
	if !apperr.Is(err, apperr.KindUncertain) {
		t.Errorf("error kind = %q, want uncertain", apperr.KindOf(err))
	}
}

func TestCancelSucceedsBeforeAnyWrite(t *testing.T) {
	drive := &fakeDrive{connectionID: "dc-1", usable: true, configured: true}
	svc := newTestService(t, drive, nil)
	ctx := context.Background()

	created, err := svc.CreateUpload(ctx, testScope(), "idem-1", validRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.CancelUpload(ctx, testScope(), created.UploadID); err != nil {
		t.Errorf("CancelUpload before any write: %v", err)
	}
}

// TestPreflightRetryStopsOnANonAvailabilityClass checks the bounded retry loop
// does not burn its budget on an error that will not change.
func TestPreflightRetryStopsOnANonAvailabilityClass(t *testing.T) {
	drive := &fakeDrive{
		connectionID: "dc-1",
		usable:       true,
		configured:   true,
		errors: []error{
			&policy.ProviderError{Class: policy.ClassQuota, Reason: "storageQuotaExceeded"},
		},
	}
	svc := newTestService(t, drive, nil)

	// A quota error surfaces as a quota state to the user.
	if _, err := svc.CreateUpload(context.Background(), testScope(), "idem-1", validRequest()); !apperr.Is(err, apperr.KindQuota) {
		t.Fatalf("error = %v, want a quota error", err)
	}
	if drive.calls != 1 {
		t.Errorf("preflight ran %d times for a quota error, want 1; a quota error will not change on retry", drive.calls)
	}
}

// TestPreflightRetriesAnAvailabilityFailure confirms the bounded retry loop does
// retry a genuine outage.
func TestPreflightRetriesAnAvailabilityFailure(t *testing.T) {
	drive := &fakeDrive{
		connectionID: "dc-1",
		usable:       true,
		configured:   true,
		errors:       []error{unavailable(), unavailable()},
	}
	svc := newTestService(t, drive, nil)

	_, err := svc.CreateUpload(context.Background(), testScope(), "idem-1", validRequest())
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	if drive.calls < 2 {
		t.Errorf("preflight ran %d times, want at least 2 for a retryable outage", drive.calls)
	}
}

// TestPreflightRetriesAreBounded checks the documented attempt cap.
func TestPreflightRetriesAreBounded(t *testing.T) {
	drive := &fakeDrive{
		connectionID: "dc-1",
		usable:       true,
		configured:   true,
		errors:       []error{unavailable()},
	}
	svc := newTestService(t, drive, nil)

	_, _ = svc.CreateUpload(context.Background(), testScope(), "idem-1", validRequest())

	if drive.calls > testDriveConfig().PreflightAttempts {
		t.Errorf("preflight ran %d times, want at most %d",
			drive.calls, testDriveConfig().PreflightAttempts)
	}
}

// TestUnnormalizedDriveErrorIsTreatedAsUnknown checks that a buggy adapter cannot
// accidentally route uploads to the fallback bucket.
func TestUnnormalizedDriveErrorIsTreatedAsUnknown(t *testing.T) {
	drive := &fakeDrive{
		connectionID: "dc-1",
		usable:       true,
		configured:   true,
		// A bare error with no policy class: the service must not treat it as an
		// outage.
		errors: []error{errors.New("boom")},
	}
	svc := newTestService(t, drive, nil)

	_, err := svc.CreateUpload(context.Background(), testScope(), "idem-1", validRequest())
	if err == nil {
		t.Fatal("an unnormalized error must not be routed as an outage")
	}
}

func TestStatusReportsTheStoragePosture(t *testing.T) {
	drive := &fakeDrive{connectionID: "dc-1", usable: true, configured: true}
	svc := newTestService(t, drive, nil)

	status, err := svc.Status(context.Background(), testScope())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.DriveConnected {
		t.Error("DriveConnected should be true")
	}
	if !status.AcceptsUploads {
		t.Errorf("AcceptsUploads = false with a working Drive connection: %q", status.BlockedReason)
	}
	if status.PrimaryProvider != "google_drive" {
		t.Errorf("primary provider = %q, want google_drive", status.PrimaryProvider)
	}
}

func TestStatusExplainsAMissingDriveConnection(t *testing.T) {
	drive := &fakeDrive{connectionID: "", usable: false, configured: true}
	svc := newTestService(t, drive, nil)

	status, err := svc.Status(context.Background(), testScope())
	if err != nil {
		t.Fatal(err)
	}
	if status.AcceptsUploads {
		t.Error("AcceptsUploads must be false without a connection")
	}
	if status.BlockedReason == "" {
		t.Error("a blocked status must explain why")
	}
}

func TestStatusExplainsAMissingDeploymentConfiguration(t *testing.T) {
	drive := &fakeDrive{connectionID: "dc-1", usable: true, configured: false}
	svc := newTestService(t, drive, nil)

	status, err := svc.Status(context.Background(), testScope())
	if err != nil {
		t.Fatal(err)
	}
	if status.AcceptsUploads {
		t.Error("AcceptsUploads must be false when Google credentials are absent")
	}
	if status.BlockedReason == "" {
		t.Error("the reason must name the deployment configuration gap")
	}
}

func TestNewUploadServiceRequiresItsCollaborators(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Dependencies)
	}{
		{"no objects repository", func(d *Dependencies) { d.Objects = nil }},
		{"no uploads repository", func(d *Dependencies) { d.Uploads = nil }},
		{"no attempts repository", func(d *Dependencies) { d.Attempts = nil }},
		{"no drive", func(d *Dependencies) { d.Drive = nil }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newServiceTestStore(t)
			deps := baseDeps(store)
			tc.mutate(&deps)
			if _, err := NewUploadService(deps); err == nil {
				t.Error("want an error")
			}
		})
	}
}

// TestFallbackConsentLookupFailureIsTreatedAsNoConsent is a fail-closed check.
func TestFallbackConsentLookupFailureIsTreatedAsNoConsent(t *testing.T) {
	drive := &fakeDrive{
		connectionID: "dc-1",
		usable:       true,
		configured:   true,
		errors:       []error{unavailable()},
	}
	svc := newTestService(t, drive, func(deps *Dependencies) {
		deps.Fallback = config.FallbackConfig{
			Enabled: true,
			Backends: []config.S3BackendConfig{{
				ID: "b1", Vendor: config.VendorCloudflareR2, Bucket: "bucket",
			}},
		}
		deps.FallbackConsent = func(context.Context, persistence.Scope) (bool, error) {
			return true, errors.New("consent store unreachable")
		}
	})
	if err := svc.deps.Backends.Upsert(context.Background(), persistence.Backend{
		ID: "b1", Scope: "deployment", Vendor: "cloudflare_r2", Bucket: "bucket",
		Status: "active", Capabilities: "{}",
	}); err != nil {
		t.Fatal(err)
	}

	result, err := svc.CreateUpload(context.Background(), testScope(), "idem-1", validRequest())
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	// An error while reading consent must never be treated as consent.
	if result.Provider != model.ProviderGoogleDrive {
		t.Errorf("provider = %q, want google_drive; a consent lookup failure must fail closed", result.Provider)
	}
}

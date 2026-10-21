package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shovan04/storvia-project/internal/apperr"
	"github.com/shovan04/storvia-project/internal/config"
	"github.com/shovan04/storvia-project/internal/idid"
	"github.com/shovan04/storvia-project/internal/persistence"
	"github.com/shovan04/storvia-project/internal/storage/model"
	"github.com/shovan04/storvia-project/internal/storage/policy"
)

// newServiceTestStore opens an isolated SQLite database for a wiring test.
func newServiceTestStore(t *testing.T) *persistence.Store {
	t.Helper()
	store, err := persistence.Open(context.Background(), config.DatabaseConfig{
		Driver:          config.DriverSQLite,
		SQLitePath:      "file:" + filepath.Join(t.TempDir(), "wiring.db"),
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
	return store
}

// baseDeps returns a minimal valid dependency set for wiring tests.
func baseDeps(store *persistence.Store) Dependencies {
	return Dependencies{
		Objects:      persistence.NewObjectRepository(store),
		Uploads:      persistence.NewUploadRepository(store),
		Attempts:     persistence.NewAttemptRepository(store),
		Backends:     persistence.NewBackendRepository(store),
		Drive:        &fakeDrive{connectionID: "dc-1", usable: true, configured: true},
		UploadLimits: testUploadLimits(),
		DriveCfg:     testDriveConfig(),
		IDs:          idid.NewGenerator("upl"),
		Logger:       slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Now:          time.Now,
	}
}

// fakeSource is a ContentSource stub for the object service tests.
type fakeSource struct {
	openErr    error
	deleteErr  error
	opened     int
	deleted    int
	lastObject model.LogicalObject
	body       string
}

func (f *fakeSource) Open(_ context.Context, object model.LogicalObject, _ *model.ByteRange) (io.ReadCloser, error) {
	f.opened++
	f.lastObject = object
	if f.openErr != nil {
		return nil, f.openErr
	}
	return io.NopCloser(readerFromString(f.body)), nil
}

func (f *fakeSource) Delete(_ context.Context, object model.LogicalObject) error {
	f.deleted++
	f.lastObject = object
	return f.deleteErr
}

// newObjectService assembles an object service over a ready test object.
func newObjectService(t *testing.T, source ContentSource) (*ObjectService, *UploadService, persistence.Scope, string) {
	t.Helper()

	drive := &fakeDrive{connectionID: "dc-1", usable: true, configured: true}
	svc := newTestService(t, drive, nil)
	ctx := context.Background()

	created, err := svc.CreateUpload(ctx, testScope(), "idem-1", validRequest())
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}

	objectService := NewObjectService(svc.deps.Objects, svc.deps.Uploads, source)
	return objectService, svc, testScope(), created.ObjectID
}

// newS3ObjectService assembles an object service whose object was routed to the
// S3 fallback, which is how the stay-in-place rule gets exercised.
//
// Routing happens at upload creation, so the Drive stub must report an eligible
// outage; a provider cannot be switched after the fact, which is the point.
func newS3ObjectService(t *testing.T, source ContentSource) (*ObjectService, *UploadService, persistence.Scope, string) {
	t.Helper()

	outage := unavailable()
	drive := &fakeDrive{
		connectionID: "dc-1",
		usable:       true,
		configured:   true,
		errors:       []error{outage},
	}
	svc := newTestService(t, drive, func(deps *Dependencies) {
		deps.FallbackUploader = newStubFallback()
		deps.Fallback = config.FallbackConfig{
			Enabled: true,
			Backends: []config.S3BackendConfig{{
				ID: "b1", Vendor: config.VendorCloudflareR2, Bucket: "bucket",
			}},
		}
		deps.FallbackConsent = func(context.Context, persistence.Scope) (bool, error) {
			return true, nil
		}
	})

	if err := svc.deps.Backends.Upsert(context.Background(), persistence.Backend{
		ID: "b1", Scope: "deployment", Vendor: "cloudflare_r2", Bucket: "bucket",
		Status: "active", Capabilities: "{}",
	}); err != nil {
		t.Fatal(err)
	}

	created, err := svc.CreateUpload(context.Background(), testScope(), "idem-s3", validRequest())
	if err != nil {
		t.Fatalf("CreateUpload routed to the fallback: %v", err)
	}
	if created.Provider != model.ProviderS3Compatible {
		t.Fatalf("provider = %q, want s3_compatible", created.Provider)
	}

	objectService := NewObjectService(svc.deps.Objects, svc.deps.Uploads, source)
	return objectService, svc, testScope(), created.ObjectID
}

// makeReady promotes an object to ready on its recorded provider.
func makeReady(t *testing.T, svc *UploadService, scope persistence.Scope, objectID string, provider model.Provider) model.LogicalObject {
	t.Helper()
	ctx := context.Background()

	objects := svc.deps.Objects
	current, err := objects.Get(ctx, scope, objectID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	connectionID := "dc-1"
	var backendID *string
	fileID, objectKey := "file-1", ""

	if provider == model.ProviderS3Compatible {
		backend := "b1"
		backendID = &backend
		connectionID = ""
		fileID = ""
		objectKey = "storvia/tenants/" + scope.TenantID + "/objects/" + objectID
	}
	var connection *string
	if connectionID != "" {
		connection = &connectionID
	}

	if err := objects.CommitReady(ctx, scope, objectID, current.RowVersion,
		provider, connection, backendID, fileID, objectKey,
		current.ExpectedSize, "", ""); err != nil {
		t.Fatalf("CommitReady: %v", err)
	}

	ready, err := objects.Get(ctx, scope, objectID)
	if err != nil {
		t.Fatalf("Get after ready: %v", err)
	}
	return ready
}

func TestOpenContentStreamsFromTheRecordedProvider(t *testing.T) {
	source := &fakeSource{body: "file bytes"}
	objectService, svc, scope, objectID := newObjectService(t, source)
	makeReady(t, svc, scope, objectID, model.ProviderGoogleDrive)

	stream, err := objectService.OpenContent(context.Background(), scope, objectID, nil)
	if err != nil {
		t.Fatalf("OpenContent: %v", err)
	}
	defer stream.Body.Close()

	got, _ := io.ReadAll(stream.Body)
	if string(got) != "file bytes" {
		t.Errorf("body = %q, want the content", got)
	}
	if stream.MIMEType != "application/pdf" {
		t.Errorf("mime type = %q, want application/pdf", stream.MIMEType)
	}
}

// TestOpenContentIsTenantScoped proves the IDOR guard at the service boundary.
func TestOpenContentIsTenantScoped(t *testing.T) {
	source := &fakeSource{body: "secret"}
	objectService, svc, scope, objectID := newObjectService(t, source)
	makeReady(t, svc, scope, objectID, model.ProviderGoogleDrive)

	intruder := persistence.Scope{TenantID: "tenant-b", UserID: "user-2"}
	_, err := objectService.OpenContent(context.Background(), intruder, objectID, nil)
	if err == nil {
		t.Fatal("a cross-tenant read must be refused")
	}
	if source.opened != 0 {
		t.Error("a cross-tenant read must not reach the provider")
	}
}

func TestOpenContentRefusesANotReadyObject(t *testing.T) {
	source := &fakeSource{body: "x"}
	objectService, _, scope, objectID := newObjectService(t, source)

	_, err := objectService.OpenContent(context.Background(), scope, objectID, nil)
	if err == nil {
		t.Fatal("an object that is not ready must not be downloadable")
	}
	if source.opened != 0 {
		t.Error("a not-ready object must not reach the provider")
	}
}

func TestOpenContentRejectsAnOutOfBoundsRange(t *testing.T) {
	source := &fakeSource{body: "0123456789"}
	objectService, svc, scope, objectID := newObjectService(t, source)
	ready := makeReady(t, svc, scope, objectID, model.ProviderGoogleDrive)

	// A start offset beyond the verified size must be refused before any
	// provider call.
	outOfRange := &model.ByteRange{Start: ready.VerifiedSize + 1, End: ready.VerifiedSize + 100, HasRange: true}
	_, err := objectService.OpenContent(context.Background(), scope, objectID, outOfRange)
	if err == nil {
		t.Fatalf("a range beyond the object size (%d) must be refused", ready.VerifiedSize)
	}
	if source.opened != 0 {
		t.Error("an invalid range must not reach the provider")
	}
}

func TestDeleteRemovesFromTheRecordedProvider(t *testing.T) {
	source := &fakeSource{}
	objectService, svc, scope, objectID := newObjectService(t, source)
	makeReady(t, svc, scope, objectID, model.ProviderGoogleDrive)

	if err := objectService.DeleteObject(context.Background(), scope, objectID); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if source.deleted != 1 {
		t.Errorf("the provider was called %d times, want 1", source.deleted)
	}
	if source.lastObject.Provider != model.ProviderGoogleDrive {
		t.Errorf("deleted from %q, want the recorded provider google_drive", source.lastObject.Provider)
	}

	deleted, err := svc.deps.Objects.Get(context.Background(), scope, objectID)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.Status != model.ObjectDeleted {
		t.Errorf("status = %q, want deleted", deleted.Status)
	}
}

// TestDeleteIsPinnedToAnS3LocationAfterDriveRecovers is the stay-in-place rule:
// a file written to the fallback is deleted from the fallback, never from Drive.
func TestDeleteIsPinnedToAnS3Location(t *testing.T) {
	source := &fakeSource{}
	objectService, svc, scope, objectID := newS3ObjectService(t, source)
	makeReady(t, svc, scope, objectID, model.ProviderS3Compatible)

	if err := objectService.DeleteObject(context.Background(), scope, objectID); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if source.lastObject.Provider != model.ProviderS3Compatible {
		t.Errorf("deleted from %q, want the recorded provider s3_compatible", source.lastObject.Provider)
	}
	if source.lastObject.ProviderObjectKey == nil || *source.lastObject.ProviderObjectKey == "" {
		t.Error("the recorded object key must be used, never a freshly derived one")
	}
}

// TestDeleteTreatsProviderMissingAsAlreadyAbsent documents the deliberate choice
// not to delete anything elsewhere.
func TestDeleteTreatsProviderMissingAsAlreadyAbsent(t *testing.T) {
	source := &fakeSource{deleteErr: &policy.ProviderError{Class: policy.ClassMissing}}
	objectService, svc, scope, objectID := newObjectService(t, source)
	makeReady(t, svc, scope, objectID, model.ProviderGoogleDrive)

	err := objectService.DeleteObject(context.Background(), scope, objectID)
	if err != nil {
		t.Fatalf("an already-absent object is a successful deletion, got %v", err)
	}
	if source.deleted != 1 {
		t.Error("only the recorded provider may be called")
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	source := &fakeSource{}
	objectService, svc, scope, objectID := newObjectService(t, source)
	makeReady(t, svc, scope, objectID, model.ProviderGoogleDrive)

	if err := objectService.DeleteObject(context.Background(), scope, objectID); err != nil {
		t.Fatal(err)
	}
	callsAfterFirst := source.deleted

	if err := objectService.DeleteObject(context.Background(), scope, objectID); err != nil {
		t.Errorf("a repeated delete must be a no-op, got %v", err)
	}
	if source.deleted != callsAfterFirst {
		t.Errorf("the provider was called again after a completed deletion")
	}
}

// TestDeleteOfAnObjectThatNeverReachedAProviderIsLocal.
//
// An object can be created and then abandoned -- an upload that was never finished, or
// one whose first attempt failed before anything was written. It has no provider
// reference at all.
//
// Deleting it used to call the provider anyway, which returned a location error because
// there was nothing to point at. The delete then failed and scheduled a retry that could
// never succeed, so the object was stuck in `deleting` forever and the console's delete
// button did nothing. There is no provider copy to remove, so the delete is a local
// state transition.
func TestDeleteOfAnObjectThatNeverReachedAProviderIsLocal(t *testing.T) {
	source := &fakeSource{}
	objectService, svc, scope, objectID := newAbandonedObject(t, source)

	if err := objectService.DeleteObject(context.Background(), scope, objectID); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if source.deleted != 0 {
		t.Errorf("the provider was called %d times, want 0: there is nothing there to delete", source.deleted)
	}

	deleted, err := svc.deps.Objects.Get(context.Background(), scope, objectID)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.Status != model.ObjectDeleted {
		t.Errorf("status = %q, want deleted", deleted.Status)
	}
	if deleted.DeletedAt == nil {
		t.Error("deleted_at was not stamped; the row would still look live")
	}
}

// newAbandonedObject builds the object a failed upload leaves behind.
//
// CreateUpload is not used: it records a planned provider identity before the mutation
// so a retry knows what it was aiming at. The abandoned row is what exists when routing
// never got that far -- no provider, no connection, no file id, no object key. That is
// the shape nine objects in the development database actually had.
func newAbandonedObject(t *testing.T, source ContentSource) (*ObjectService, *UploadService, persistence.Scope, string) {
	t.Helper()

	drive := &fakeDrive{connectionID: "dc-1", usable: true, configured: true}
	svc := newTestService(t, drive, nil)
	scope := testScope()
	const objectID = "obj_abandoned"

	if err := svc.deps.Objects.Create(context.Background(), scope, model.LogicalObject{
		ID:           objectID,
		TenantID:     scope.TenantID,
		UserID:       scope.UserID,
		LogicalKey:   objectID,
		OriginalName: "abandoned.txt",
		MIMEType:     "text/plain",
		Category:     "document",
		ExpectedSize: 12,
		Status:       model.ObjectPending,
	}); err != nil {
		t.Fatalf("Create abandoned object: %v", err)
	}

	return NewObjectService(svc.deps.Objects, svc.deps.Uploads, source), svc, scope, objectID
}

func TestDeleteIsTenantScoped(t *testing.T) {
	source := &fakeSource{}
	objectService, svc, scope, objectID := newObjectService(t, source)
	makeReady(t, svc, scope, objectID, model.ProviderGoogleDrive)

	intruder := persistence.Scope{TenantID: "tenant-b", UserID: "user-2"}
	if err := objectService.DeleteObject(context.Background(), intruder, objectID); err == nil {
		t.Fatal("a cross-tenant delete must be refused")
	}
	if source.deleted != 0 {
		t.Error("a cross-tenant delete must not reach the provider")
	}
}

func TestProviderErrorsMapToStableAppErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"quota", &policy.ProviderError{Class: policy.ClassQuota}, "quota"},
		{"rate limited", &policy.ProviderError{Class: policy.ClassRateLimited}, "rate_limited"},
		{"missing", &policy.ProviderError{Class: policy.ClassMissing}, "not_found"},
		{"unavailable", &policy.ProviderError{Class: policy.ClassUnavailable}, "unavailable"},
		{"uncertain", &policy.ProviderError{Class: policy.ClassUncertain}, "uncertain"},
		{"consent", &policy.ProviderError{Class: policy.ClassConsentRequired}, "consent_required"},
		{"authorization", &policy.ProviderError{Class: policy.ClassAuthorization}, "authorization"},
		{"configuration", &policy.ProviderError{Class: policy.ClassConfiguration}, "provider_not_configured"},
		{"validation", &policy.ProviderError{Class: policy.ClassValidation}, "validation"},
		{"conflict", &policy.ProviderError{Class: policy.ClassConflict}, "conflict"},
		{"unknown becomes internal", &policy.ProviderError{Class: policy.ClassUnknown}, "internal"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := providerErrorToAppError(model.ProviderGoogleDrive, "read", tc.err)
			if string(kindOfErr(got)) != tc.want {
				t.Errorf("kind = %q, want %q", kindOfErr(got), tc.want)
			}
		})
	}
}

// TestUnavailableDownloadMentionsThatNoCopyExists checks the client-facing wording
// for the no-copies trade-off.
func TestUnavailableDownloadMentionsThatNoCopyExists(t *testing.T) {
	source := &fakeSource{openErr: &policy.ProviderError{Class: policy.ClassUnavailable}}
	objectService, svc, scope, objectID := newObjectService(t, source)
	makeReady(t, svc, scope, objectID, model.ProviderGoogleDrive)

	_, err := objectService.OpenContent(context.Background(), scope, objectID, nil)
	if err == nil {
		t.Fatal("want an error")
	}
	if !containsAll(err.Error(), "no second copy") {
		t.Errorf("error = %q, want it to explain that no copy exists to serve from", err.Error())
	}
}

// TestNonNormalizedProviderErrorDoesNotLeak checks the fallback path in the
// mapper.
//
// The assertion is on the client-facing Detail, not on Error(): the wrapped cause
// is deliberately retained for the log, and the global error handler is what
// guarantees it is never serialized.
func TestNonNormalizedProviderErrorDoesNotLeak(t *testing.T) {
	secret := errorf("connection to s3://bucket failed: AKIAIOSFODNN7EXAMPLE")
	got := providerErrorToAppError(model.ProviderS3Compatible, "read", secret)

	var appErr *apperr.Error
	if !errors.As(got, &appErr) {
		t.Fatalf("got %T, want an *apperr.Error", got)
	}
	if strings.Contains(appErr.Detail, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("the client-facing detail leaked a credential: %q", appErr.Detail)
	}
	if appErr.Cause == nil {
		t.Error("the cause must be retained for the log")
	}
}

// TestDeleteAllObjectsClearsEveryStoredObject.
//
// This is the console's clear-the-set action. It has to remove the objects that did
// reach a provider and the ones that never did, and it has to leave the deleted rows
// out of the listing afterwards -- otherwise the operator presses the button and the
// table does not change.
func TestDeleteAllObjectsClearsEveryStoredObject(t *testing.T) {
	source := &fakeSource{}
	objectService, svc, scope, readyID := newObjectService(t, source)
	makeReady(t, svc, scope, readyID, model.ProviderGoogleDrive)

	ctx := context.Background()
	for _, id := range []string{"obj_abandoned_1", "obj_abandoned_2"} {
		if err := svc.deps.Objects.Create(ctx, scope, model.LogicalObject{
			ID:           id,
			TenantID:     scope.TenantID,
			UserID:       scope.UserID,
			LogicalKey:   id,
			OriginalName: id + ".txt",
			MIMEType:     "text/plain",
			Category:     "document",
			ExpectedSize: 12,
			Status:       model.ObjectPending,
		}); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}

	result, err := objectService.DeleteAllObjects(ctx, scope)
	if err != nil {
		t.Fatalf("DeleteAllObjects: %v", err)
	}
	if result.Deleted != 3 {
		t.Errorf("deleted %d objects, want 3", result.Deleted)
	}
	if len(result.Failed) != 0 {
		t.Errorf("failures = %+v, want none", result.Failed)
	}

	// The listing is what the console renders, so it must now be empty.
	remaining, err := svc.deps.Objects.ListObjects(ctx, scope, 50)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("%d objects still listed after a clear, want 0: %+v", len(remaining), remaining)
	}
}

// TestDeleteAllObjectsReportsFailuresWithoutStopping.
//
// A provider can refuse one object while the rest delete cleanly. The operator needs to
// know both facts: what was removed and what was not, or they will assume the table is
// clear when a file is still sitting in Drive.
func TestDeleteAllObjectsReportsFailuresWithoutStopping(t *testing.T) {
	source := &fakeSource{deleteErr: &policy.ProviderError{
		Provider: string(model.ProviderGoogleDrive), Operation: "delete", Class: policy.ClassUnavailable}}
	objectService, svc, scope, readyID := newObjectService(t, source)
	makeReady(t, svc, scope, readyID, model.ProviderGoogleDrive)

	ctx := context.Background()
	// The abandoned object needs no provider call, so it deletes even while Drive is
	// refusing the ready one.
	if err := svc.deps.Objects.Create(ctx, scope, model.LogicalObject{
		ID:           "obj_abandoned",
		TenantID:     scope.TenantID,
		UserID:       scope.UserID,
		LogicalKey:   "obj_abandoned",
		OriginalName: "abandoned.txt",
		MIMEType:     "text/plain",
		Category:     "document",
		ExpectedSize: 12,
		Status:       model.ObjectPending,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	result, err := objectService.DeleteAllObjects(ctx, scope)
	if err != nil {
		t.Fatalf("DeleteAllObjects: %v", err)
	}
	if result.Deleted != 1 {
		t.Errorf("deleted %d objects, want 1 (the abandoned one)", result.Deleted)
	}
	if len(result.Failed) != 1 {
		t.Fatalf("failures = %+v, want exactly the one Drive refused", result.Failed)
	}
	if result.Failed[0].ObjectID != readyID {
		t.Errorf("failure names %q, want %q", result.Failed[0].ObjectID, readyID)
	}
	if result.Failed[0].Reason == "" {
		t.Error("the failure has no reason; an operator cannot act on a blank one")
	}
}

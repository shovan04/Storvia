package service

import (
	"context"
	"errors"
	"testing"

	"github.com/shovan04/storvia-project/internal/apperr"
	"github.com/shovan04/storvia-project/internal/persistence"
	"github.com/shovan04/storvia-project/internal/storage/model"
	"github.com/shovan04/storvia-project/internal/storage/policy"
)

// fallbackWithNilUploader plans against the fallback, then removes the capability so
// reconciliation is forced to report what it can rather than assume success.
func TestReconcileRefusesWithoutAFallbackProvider(t *testing.T) {
	drive := &fakeDrive{
		connectionID: "dc-1", usable: true, configured: true,
		errors: []error{unavailable(), unavailable()},
	}
	svc := newFallbackService(t, drive, newStubFallback())

	created, err := svc.CreateUpload(context.Background(), testScope(), "idem-rec-1", validRequest())
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	svc.deps.FallbackUploader = nil

	if _, err := svc.Reconcile(context.Background(), testScope(), created.UploadID); !apperr.Is(err, apperr.KindProviderNotConfigured) {
		t.Errorf("error = %v, want provider_not_configured", err)
	}
}

// TestReconcileReportsNothingStored is the case where the provider is certain the
// object is absent. That is the only answer that tells a client it may safely start
// over, so it must be distinguishable from "nobody knows".
func TestReconcileReportsNothingStored(t *testing.T) {
	drive := &fakeDrive{
		connectionID: "dc-1", usable: true, configured: true,
		errors: []error{unavailable(), unavailable()},
	}
	fallback := newStubFallback()
	fallback.verifyErr = &policy.ProviderError{
		Provider: "s3_compatible", Operation: "Head",
		Class: policy.ClassMissing, SafeDetail: "no such key",
	}
	svc := newFallbackService(t, drive, fallback)

	created, err := svc.CreateUpload(context.Background(), testScope(), "idem-rec-2", validRequest())
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}

	result, err := svc.Reconcile(context.Background(), testScope(), created.UploadID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.Outcome != "nothing_stored" {
		t.Errorf("outcome = %q, want nothing_stored", result.Outcome)
	}
}

// TestReconcileSaysUnknownWhenTheProviderCannotBeAsked. Guessing either way is the
// failure this endpoint exists to avoid.
func TestReconcileSaysUnknownWhenTheProviderCannotBeAsked(t *testing.T) {
	drive := &fakeDrive{
		connectionID: "dc-1", usable: true, configured: true,
		errors: []error{unavailable(), unavailable()},
	}
	fallback := newStubFallback()
	fallback.verifyErr = &policy.ProviderError{
		Provider: "s3_compatible", Operation: "Head",
		Class: policy.ClassUnavailable, SafeDetail: "provider unreachable",
	}
	svc := newFallbackService(t, drive, fallback)

	created, err := svc.CreateUpload(context.Background(), testScope(), "idem-rec-3", validRequest())
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}

	result, err := svc.Reconcile(context.Background(), testScope(), created.UploadID)
	if err != nil {
		t.Fatalf("Reconcile must report an outcome rather than an error: %v", err)
	}
	if result.Outcome != "unknown" {
		t.Errorf("outcome = %q, want unknown", result.Outcome)
	}
}

// TestReconcileRefusesToCommitASizeMismatch: bytes present but the wrong size is
// worse than nothing, and must never be adopted.
func TestReconcileRefusesToCommitASizeMismatch(t *testing.T) {
	drive := &fakeDrive{
		connectionID: "dc-1", usable: true, configured: true,
		errors: []error{unavailable(), unavailable()},
	}
	fallback := newStubFallback()
	fallback.storedSize = 3 // validRequest declares more than 3 bytes
	svc := newFallbackService(t, drive, fallback)

	created, err := svc.CreateUpload(context.Background(), testScope(), "idem-rec-4", validRequest())
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}

	result, err := svc.Reconcile(context.Background(), testScope(), created.UploadID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.Outcome != "size_mismatch" {
		t.Errorf("outcome = %q, want size_mismatch", result.Outcome)
	}
	if result.Reason == "" {
		t.Error("a mismatch must explain what did not match")
	}
}

// TestReconcileCommitsAnUploadThatDidInFactFinish: the provider is asked, and the
// answer is recorded. That is not a second write, because nothing is re-uploaded.
func TestReconcileCommitsAnUploadThatDidInFactFinish(t *testing.T) {
	drive := &fakeDrive{
		connectionID: "dc-1", usable: true, configured: true,
		errors: []error{unavailable(), unavailable()},
	}
	fallback := newStubFallback()
	fallback.storedSize = validRequest().SizeBytes
	svc := newFallbackService(t, drive, fallback)

	created, err := svc.CreateUpload(context.Background(), testScope(), "idem-rec-5", validRequest())
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}

	result, err := svc.Reconcile(context.Background(), testScope(), created.UploadID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.Outcome != "completed" {
		t.Fatalf("outcome = %q, want completed", result.Outcome)
	}

	// It must now be a ready object, and re-reconciling must say so rather than
	// doing the work twice.
	view, err := svc.GetUpload(context.Background(), testScope(), created.UploadID)
	if err != nil {
		t.Fatalf("GetUpload: %v", err)
	}
	if view.State != string(model.ObjectReady) {
		t.Errorf("state = %q, want ready", view.State)
	}

	again, err := svc.Reconcile(context.Background(), testScope(), created.UploadID)
	if err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if again.Outcome != "already_ready" {
		t.Errorf("second outcome = %q, want already_ready", again.Outcome)
	}
}

// TestReconcileIsTenantScoped: an upload id belonging to another tenant reads as
// missing, never as another tenant's outcome.
func TestReconcileIsTenantScoped(t *testing.T) {
	drive := &fakeDrive{
		connectionID: "dc-1", usable: true, configured: true,
		errors: []error{unavailable(), unavailable()},
	}
	svc := newFallbackService(t, drive, newStubFallback())

	created, err := svc.CreateUpload(context.Background(), testScope(), "idem-rec-6", validRequest())
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}

	other := persistence.Scope{TenantID: "tenant-b", UserID: "user-1"}
	if _, err := svc.Reconcile(context.Background(), other, created.UploadID); !apperr.Is(err, apperr.KindNotFound) {
		t.Errorf("error = %v, want not_found for another tenant's upload", err)
	}
}

// TestReconcileDriveSaysUnknownRatherThanNothingStored. Storvia deliberately does not
// hold the resumable session, so it cannot ask Google what landed.
//
// Reporting "nothing stored" here would be actively harmful: a client told its file
// was never written would start over and create a duplicate. Unknown is the truthful
// answer and the reason tells the client what to do instead.
func TestReconcileDriveSaysUnknownRatherThanNothingStored(t *testing.T) {
	sessions := &fakeDriveSessions{storedSize: 11, inAppFolder: true}
	svc := newTestService(t, &fakeDrive{connectionID: "dc-1", usable: true, configured: true},
		func(deps *Dependencies) { deps.DriveSessions = sessions })

	created, err := svc.CreateUpload(context.Background(), testScope(), "idem-rec-7", CreateUploadRequest{
		Name: "notes.txt", MIMEType: "text/plain", SizeBytes: 11, Category: "report",
	})
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}

	result, err := svc.Reconcile(context.Background(), testScope(), created.UploadID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.Outcome != "unknown" {
		t.Errorf("outcome = %q, want unknown; reporting nothing_stored would push the "+
			"client into creating a duplicate", result.Outcome)
	}
	if result.Reason == "" {
		t.Error("an unknown outcome must explain what the client should do instead")
	}
}

// TestReconcileNeverRetries guards the property that matters most: reconcile reports,
// it never writes again.
func TestReconcileNeverRetries(t *testing.T) {
	drive := &fakeDrive{
		connectionID: "dc-1", usable: true, configured: true,
		errors: []error{unavailable(), unavailable()},
	}
	fallback := newStubFallback()
	fallback.verifyErr = errors.New("provider exploded")
	svc := newFallbackService(t, drive, fallback)

	created, err := svc.CreateUpload(context.Background(), testScope(), "idem-rec-8", validRequest())
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}

	before := len(fallback.planned)
	for i := 0; i < 3; i++ {
		if _, err := svc.Reconcile(context.Background(), testScope(), created.UploadID); err != nil {
			t.Fatalf("Reconcile %d: %v", i, err)
		}
	}
	if len(fallback.planned) != before {
		t.Errorf("reconcile planned %d new transfers; it must never plan one",
			len(fallback.planned)-before)
	}
}

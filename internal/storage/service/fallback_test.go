package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/shovan04/storvia-project/internal/apperr"
	"github.com/shovan04/storvia-project/internal/config"
	"github.com/shovan04/storvia-project/internal/persistence"
	"github.com/shovan04/storvia-project/internal/storage/model"
	"github.com/shovan04/storvia-project/internal/storage/policy"
)

// stubFallback is a FallbackUploader that records what it was asked to do.
//
// It never touches the network, so the routing and completion logic is tested
// without a real bucket. The real S3 client is exercised separately against R2.
type stubFallback struct {
	planned    []string
	verifyErr  error
	storedSize int64
	stored     map[string]bool
	planErr    error
}

func newStubFallback() *stubFallback {
	return &stubFallback{stored: make(map[string]bool)}
}

func (s *stubFallback) PlanUpload(_ context.Context, backendID, objectKey string, _ int64) ([]PresignedPartInfo, error) {
	if s.planErr != nil {
		return nil, s.planErr
	}
	s.planned = append(s.planned, objectKey)
	s.stored[objectKey] = true
	// The URL contains the key so the service's own defence-in-depth check passes,
	// exactly as a real presigned URL does.
	return []PresignedPartInfo{{
		PartNumber: 1,
		URL:        "https://bucket.example.com/" + objectKey + "?signature=stub",
		ExpiresAt:  time.Now().Add(15 * time.Minute),
	}}, nil
}

func (s *stubFallback) Verify(_ context.Context, _, objectKey string) (VerifiedObject, error) {
	if s.verifyErr != nil {
		return VerifiedObject{}, s.verifyErr
	}
	if !s.stored[objectKey] {
		return VerifiedObject{}, &policy.ProviderError{Class: policy.ClassMissing}
	}
	size := s.storedSize
	if size == 0 {
		size = 2048
	}
	return VerifiedObject{SizeBytes: size, MIMEType: "application/pdf"}, nil
}

// newFallbackService builds a service whose routing can be driven directly: a
// Drive stub that reports whatever the test wants, and a recording fallback.
func newFallbackService(t *testing.T, drive DriveAvailability, fallback FallbackUploader) *UploadService {
	t.Helper()

	svc := newTestService(t, drive, func(deps *Dependencies) {
		deps.Fallback = config.FallbackConfig{
			Enabled: fallback != nil,
			Backends: []config.S3BackendConfig{{
				ID: "b1", Vendor: config.VendorCloudflareR2, Bucket: "bucket",
			}},
		}
		deps.FallbackUploader = fallback
		deps.FallbackConsent = func(context.Context, persistence.Scope) (bool, error) {
			return true, nil
		}
	})
	if fallback != nil {
		if err := svc.deps.Backends.Upsert(context.Background(), persistence.Backend{
			ID: "b1", Scope: "deployment", Vendor: "cloudflare_r2",
			Bucket: "bucket", Status: "active", Capabilities: "{}",
		}); err != nil {
			t.Fatal(err)
		}
	}
	return svc
}

// TestDriveOutageSelectsTheFallbackAndReturnsCapabilities is the end-to-end
// routing test the project cares most about.
func TestDriveOutageSelectsTheFallbackAndReturnsCapabilities(t *testing.T) {
	drive := &fakeDrive{
		connectionID: "dc-1",
		usable:       true,
		configured:   true,
		errors:       []error{unavailable(), unavailable()},
	}
	fallback := newStubFallback()
	svc := newFallbackService(t, drive, fallback)
	ctx := context.Background()

	result, err := svc.CreateUpload(ctx, testScope(), "idem-outage", validRequest())
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}

	if result.Provider != model.ProviderS3Compatible {
		t.Fatalf("provider = %q, want s3_compatible", result.Provider)
	}
	if result.Mode != model.ModeS3PresignedParts {
		t.Errorf("mode = %q, want s3_presigned_parts", result.Mode)
	}
	if result.FallbackReason == model.FallbackNone {
		t.Error("an outage routing decision must record a reason")
	}
	if len(result.Parts) == 0 {
		t.Fatal("no upload capabilities were returned; a fallback upload could not proceed")
	}
	// The capability must be scoped to the object this attempt planned, never to a
	// key the client chose.
	if !strings.Contains(result.Parts[0].URL, result.ObjectID) {
		t.Errorf("capability URL %q does not reference the object id %q", result.Parts[0].URL, result.ObjectID)
	}
	if !strings.Contains(result.Parts[0].URL, testScope().TenantID) {
		t.Errorf("capability URL %q is not scoped to the tenant", result.Parts[0].URL)
	}
}

// TestDriveAvailableSelectsDriveEvenWithAFallbackConfigured is the other half: the
// fallback must not be used when Drive answers.
func TestDriveAvailableSelectsDriveEvenWithAFallbackConfigured(t *testing.T) {
	drive := &fakeDrive{connectionID: "dc-1", usable: true, configured: true}
	fallback := newStubFallback()
	svc := newFallbackService(t, drive, fallback)

	result, err := svc.CreateUpload(context.Background(), testScope(), "idem-ok", validRequest())
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	if result.Provider != model.ProviderGoogleDrive {
		t.Errorf("provider = %q, want google_drive", result.Provider)
	}
	if len(fallback.planned) != 0 {
		t.Errorf("the fallback was planned %d times while Drive was healthy", len(fallback.planned))
	}
}

// TestCompleteCommitsExactlyOneLocation walks the fallback upload to ready.
func TestCompleteCommitsExactlyOneLocation(t *testing.T) {
	drive := &fakeDrive{
		connectionID: "dc-1", usable: true, configured: true,
		errors: []error{unavailable(), unavailable()},
	}
	fallback := newStubFallback()
	fallback.storedSize = 2048
	svc := newFallbackService(t, drive, fallback)
	ctx := context.Background()

	created, err := svc.CreateUpload(ctx, testScope(), "idem-c", validRequest())
	if err != nil {
		t.Fatal(err)
	}

	result, err := svc.CompleteUpload(ctx, testScope(), created.UploadID)
	if err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}
	if result.Object.Status != model.ObjectReady {
		t.Errorf("status = %q, want ready", result.Object.Status)
	}
	if result.Object.Provider != model.ProviderS3Compatible {
		t.Errorf("provider = %q, want s3_compatible", result.Object.Provider)
	}

	provider, err := result.Object.Location()
	if err != nil {
		t.Fatalf("the ready object must have exactly one location: %v", err)
	}
	if provider != model.ProviderS3Compatible {
		t.Errorf("location = %q, want s3_compatible", provider)
	}
	// The Drive pointer must be absent: this is the no-copies invariant.
	if result.Object.DriveConnectionID != nil {
		t.Error("the object carries a Drive pointer even though it was stored on the fallback")
	}
	if result.Object.VerifiedSize != result.Object.ExpectedSize {
		t.Errorf("verified %d != expected %d", result.Object.VerifiedSize, result.Object.ExpectedSize)
	}
}

// TestCompleteIsIdempotent is the duplicate-write guard.
func TestCompleteIsIdempotent(t *testing.T) {
	drive := &fakeDrive{
		connectionID: "dc-1", usable: true, configured: true,
		errors: []error{unavailable(), unavailable()},
	}
	fallback := newStubFallback()
	svc := newFallbackService(t, drive, fallback)
	ctx := context.Background()

	created, err := svc.CreateUpload(ctx, testScope(), "idem-i", validRequest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CompleteUpload(ctx, testScope(), created.UploadID); err != nil {
		t.Fatal(err)
	}

	verifyCallsAfterFirst := len(fallback.planned)
	second, err := svc.CompleteUpload(ctx, testScope(), created.UploadID)
	if err != nil {
		t.Fatalf("the second completion must succeed: %v", err)
	}
	if !second.AlreadyComplete {
		t.Error("the second completion must report that the object was already complete")
	}
	if second.Object.ID != created.ObjectID {
		t.Errorf("object id changed: %q vs %q", second.Object.ID, created.ObjectID)
	}
	if len(fallback.planned) != verifyCallsAfterFirst {
		t.Error("a repeated completion planned another upload, which is a second write")
	}
}

// TestCompleteRefusesASizeMismatch keeps a truncated upload from becoming ready.
func TestCompleteRefusesASizeMismatch(t *testing.T) {
	drive := &fakeDrive{
		connectionID: "dc-1", usable: true, configured: true,
		errors: []error{unavailable(), unavailable()},
	}
	fallback := newStubFallback()
	fallback.storedSize = 17 // the request declared 2048
	svc := newFallbackService(t, drive, fallback)
	ctx := context.Background()

	created, err := svc.CreateUpload(ctx, testScope(), "idem-m", validRequest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CompleteUpload(ctx, testScope(), created.UploadID); err == nil {
		t.Fatal("a provider size that disagrees with the declaration must not become a ready location")
	}

	object, getErr := svc.deps.Objects.Get(ctx, testScope(), created.ObjectID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if object.Status == model.ObjectReady {
		t.Error("the object was marked ready despite the size mismatch")
	}
}

// TestCompleteSurfacesAMissingObjectAsMissingNotAsARecreate checks that a provider
// 404 does not trigger a second write anywhere.
func TestCompleteSurfacesAMissingObjectAsMissingNotAsARecreate(t *testing.T) {
	drive := &fakeDrive{
		connectionID: "dc-1", usable: true, configured: true,
		errors: []error{unavailable(), unavailable()},
	}
	fallback := newStubFallback()
	svc := newFallbackService(t, drive, fallback)
	ctx := context.Background()

	created, err := svc.CreateUpload(ctx, testScope(), "idem-x", validRequest())
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the client never actually uploaded.
	fallback.stored = map[string]bool{}

	_, err = svc.CompleteUpload(ctx, testScope(), created.UploadID)
	if err == nil {
		t.Fatal("want an error when the object is not at the provider")
	}
	if !apperr.Is(err, apperr.KindNotFound) {
		t.Errorf("error kind = %q, want not_found", apperr.KindOf(err))
	}
	if len(fallback.planned) != 1 {
		t.Error("a missing object must not trigger a re-plan or a second write")
	}
}

// TestCompleteRequiresAFallbackUploader keeps the honest failure when the
// fallback is switched off.
func TestCompleteRequiresAFallbackUploader(t *testing.T) {
	// Drive is forced unavailable so the upload is planned against the fallback, and
	// then the fallback uploader is removed to prove completion refuses rather than
	// committing something it cannot verify.
	drive := &fakeDrive{
		connectionID: "dc-1", usable: true, configured: true,
		errors: []error{unavailable(), unavailable()},
	}
	// Build with a working fallback so the upload is planned against it, then remove
	// the uploader to represent the deployment losing that capability mid-upload.
	svc := newFallbackService(t, drive, newStubFallback())

	created, err := svc.CreateUpload(context.Background(), testScope(), "idem-need-uploader", validRequest())
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	if created.Provider != model.ProviderS3Compatible {
		t.Fatalf("provider = %v, want the fallback", created.Provider)
	}

	// The deployment loses the capability between planning and completion.
	svc.deps.FallbackUploader = nil

	_, err = svc.CompleteUpload(context.Background(), testScope(), created.UploadID)
	if err == nil {
		t.Fatal("completion must fail when no fallback uploader is configured")
	}
	if !apperr.Is(err, apperr.KindProviderNotConfigured) {
		t.Errorf("error kind = %q, want provider_not_configured", apperr.KindOf(err))
	}
}

// TestCompleteRefusesAnUploadThatDoesNotBelongToTheTenant is a consequence of moving
// the provider check after the upload lookup: an upload id belonging to another tenant
// must read as missing, not as a configuration problem.
func TestCompleteRefusesAnUploadThatDoesNotBelongToTheTenant(t *testing.T) {
	drive := &fakeDrive{
		connectionID: "dc-1", usable: true, configured: true,
		errors: []error{unavailable(), unavailable()},
	}
	svc := newFallbackService(t, drive, nil)

	created, err := svc.CreateUpload(context.Background(), testScope(), "idem-tenant", validRequest())
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}

	other := persistence.Scope{TenantID: "tenant-b", UserID: "user-1"}
	if _, err := svc.CompleteUpload(context.Background(), other, created.UploadID); !apperr.Is(err, apperr.KindNotFound) {
		t.Errorf("error = %v, want not_found for another tenant's upload", err)
	}
}

// TestCreateUploadFailsWhenTheFallbackCannotPlan stops an upload that could never
// be completed, rather than returning an unusable upload ID.
func TestCreateUploadFailsWhenTheFallbackCannotPlan(t *testing.T) {
	drive := &fakeDrive{
		connectionID: "dc-1", usable: true, configured: true,
		errors: []error{unavailable(), unavailable()},
	}
	fallback := newStubFallback()
	fallback.planErr = &policy.ProviderError{Class: policy.ClassConfiguration}
	svc := newFallbackService(t, drive, fallback)

	_, err := svc.CreateUpload(context.Background(), testScope(), "idem-p", validRequest())
	if err == nil {
		t.Fatal("an upload that cannot be planned must not be reported as created")
	}
	if !apperr.Is(err, apperr.KindProviderNotConfigured) {
		t.Errorf("error kind = %q, want provider_not_configured", apperr.KindOf(err))
	}
}

// TestPresignedCapabilityMustMatchTheObjectKey is the defence-in-depth check: an
// adapter that returned a capability for the wrong key would write the wrong bytes,
// so it is rejected even though the key is server-derived.
func TestPresignedCapabilityMustMatchTheObjectKey(t *testing.T) {
	misbehaving := &stubFallback{stored: map[string]bool{}}
	// Override planning to return a URL for a different key.
	bad := &mismatchingFallback{}
	_, err := presignedPartsFor(bad, "b1", "storvia/tenants/t/objects/obj_1", 2048, time.Minute)
	if err == nil {
		t.Fatal("a capability that does not reference the object key must be rejected")
	}
	if !strings.Contains(err.Error(), "object key") {
		t.Errorf("error = %v, want it to explain the mismatch", err)
	}
	_ = misbehaving
}

// mismatchingFallback returns a capability pointing at the wrong key.
type mismatchingFallback struct{ stubFallback }

func (m *mismatchingFallback) PlanUpload(_ context.Context, _, objectKey string, _ int64) ([]PresignedPartInfo, error) {
	return []PresignedPartInfo{{
		PartNumber: 1,
		URL:        "https://bucket.example.com/storvia/tenants/OTHER/objects/someone-else",
		ExpiresAt:  time.Now().Add(time.Minute),
	}}, nil
}

var _ = fmt.Sprintf
var _ = errors.New

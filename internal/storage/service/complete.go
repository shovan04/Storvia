package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/shovan04/storvia-project/internal/apperr"
	"github.com/shovan04/storvia-project/internal/persistence"
	"github.com/shovan04/storvia-project/internal/storage/model"
)

// FallbackUploader is the part of the S3-compatible adapter the upload service
// needs.
//
// It is declared here rather than importing the adapter, so the coordinator stays
// testable with a stub and a change to the S3 client does not ripple into routing.
type FallbackUploader interface {
	// PlanUpload returns short-lived, operation-scoped upload capabilities for a
	// generated key. The key is derived server-side; a client can never supply it.
	PlanUpload(ctx context.Context, backendID, objectKey string, sizeBytes int64) ([]PresignedPartInfo, error)
	// Verify confirms that the object exists at the key and reports its size.
	Verify(ctx context.Context, backendID, objectKey string) (VerifiedObject, error)
}

// VerifiedObject is what the provider reports about a stored object.
type VerifiedObject struct {
	SizeBytes int64
	MIMEType  string
	ETag      string
	Checksum  string
}

// CompleteUploadResult reports a completed upload.
type CompleteUploadResult struct {
	Object model.LogicalObject
	// AlreadyComplete is true when a repeated completion returned the existing
	// verified object rather than doing new work.
	AlreadyComplete bool
}

// CompleteUpload finalizes an upload and commits its single provider location.
//
// Completion is idempotent. A repeated call returns the object that is already
// ready rather than writing again, because a second write is precisely what the
// no-copies rule forbids.
func (s *UploadService) CompleteUpload(ctx context.Context, scope persistence.Scope, uploadID string) (CompleteUploadResult, error) {
	upload, err := s.deps.Uploads.Get(ctx, scope, uploadID)
	if err != nil {
		return CompleteUploadResult{}, apperr.NotFound("no such upload for this account")
	}

	object, err := s.deps.Objects.Get(ctx, scope, upload.ObjectID)
	if err != nil {
		return CompleteUploadResult{}, apperr.NotFound("no such object for this account")
	}

	// The already-complete check comes before the attempt lookup. Once an upload
	// finishes, its attempt is no longer "active", so checking the attempt first
	// would make a repeated completion fail instead of being idempotent.
	if object.Status == model.ObjectReady {
		return CompleteUploadResult{Object: object, AlreadyComplete: true}, nil
	}

	attempt, err := s.deps.Attempts.GetActiveForUpload(ctx, uploadID)
	if err != nil {
		return CompleteUploadResult{}, apperr.Uncertain("this upload has no active attempt to complete")
	}

	if object.Provider == model.ProviderGoogleDrive {
		// Reachable only if a caller uses the generic entry point for a Drive
		// upload. The HTTP handler routes Drive correctly; pointing here is more
		// useful than a generic "not implemented".
		return CompleteUploadResult{}, apperr.New(apperr.KindProviderNotConfigured,
			"Drive completion needs the file id",
			"complete a drive upload with CompleteDriveUpload, passing the file id google returned")
	}
	if s.deps.FallbackUploader == nil {
		return CompleteUploadResult{}, apperr.New(apperr.KindProviderNotConfigured,
			"Provider transfer not implemented",
			"upload completion is not available for the selected provider")
	}
	if object.BackendID == nil || object.ProviderObjectKey == nil {
		return CompleteUploadResult{}, apperr.Internal("the object's storage location is inconsistent")
	}

	verified, err := s.deps.FallbackUploader.Verify(ctx, *object.BackendID, *object.ProviderObjectKey)
	if err != nil {
		return CompleteUploadResult{}, providerErrorToAppError(model.ProviderS3Compatible, "verify", err)
	}

	// The provider's confirmed size must match what was declared. A mismatch means
	// a truncated or substituted upload, and it must not become a ready location.
	if verified.SizeBytes != object.ExpectedSize {
		return CompleteUploadResult{}, apperr.New(apperr.KindUncertain, "Uploaded size does not match",
			fmt.Sprintf("the provider stored %d bytes but %d were declared", verified.SizeBytes, object.ExpectedSize))
	}

	if err := s.deps.Objects.CommitReady(ctx, scope, object.ID, object.RowVersion,
		model.ProviderS3Compatible, nil, object.BackendID, "", *object.ProviderObjectKey,
		verified.SizeBytes, "", verified.Checksum); err != nil {
		return CompleteUploadResult{}, apperr.Wrap(err, apperr.KindInternal,
			"Internal error", "could not commit the verified location")
	}

	if err := s.deps.Attempts.SetState(ctx, attempt.ID, model.AttemptCompleted, ""); err != nil {
		return CompleteUploadResult{}, apperr.Wrap(err, apperr.KindInternal,
			"Internal error", "the upload was stored but its attempt state could not be recorded")
	}
	if err := s.deps.Uploads.SetActiveAttempt(ctx, scope, upload.ID, attempt.ID, model.ObjectReady); err != nil {
		return CompleteUploadResult{}, apperr.Wrap(err, apperr.KindInternal,
			"Internal error", "the upload was stored but its state could not be recorded")
	}

	s.deps.Metrics.RecordUploadFinished(string(model.ProviderS3Compatible), "ready")
	s.deps.Metrics.RecordBytesStored(string(model.ProviderS3Compatible), verified.SizeBytes)

	ready, err := s.deps.Objects.Get(ctx, scope, object.ID)
	if err != nil {
		return CompleteUploadResult{}, apperr.NotFound("no such object for this account")
	}
	return CompleteUploadResult{Object: ready}, nil
}

// presignedPartsFor plans the fallback upload capabilities for a new attempt.
//
// It is called only after the policy has already selected the fallback and the
// attempt row has been written, so a client can never ask for S3 directly.
func presignedPartsFor(uploader FallbackUploader, backendID, objectKey string, size int64, ttl time.Duration) ([]PresignedPartInfo, error) {
	if uploader == nil {
		return nil, apperr.ProviderNotConfigured("the fallback backend is not configured")
	}
	if backendID == "" || objectKey == "" {
		return nil, apperr.Internal("the fallback upload has no backend or object key")
	}

	parts, err := uploader.PlanUpload(context.Background(), backendID, objectKey, size)
	if err != nil {
		return nil, providerErrorToAppError(model.ProviderS3Compatible, "plan upload", err)
	}
	if len(parts) == 0 {
		return nil, apperr.ProviderNotConfigured("the fallback backend returned no upload capabilities")
	}

	// Defence in depth: the keys must belong to the object this attempt planned.
	// A client cannot influence them, but an adapter bug could, and a capability
	// for the wrong key would write the wrong bytes.
	for _, part := range parts {
		if !strings.Contains(part.URL, objectKey) {
			return nil, apperr.Internal("a planned upload capability did not match the object key")
		}
	}
	return parts, nil
}

package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/shovan04/storvia-project/internal/apperr"
	"github.com/shovan04/storvia-project/internal/persistence"
	"github.com/shovan04/storvia-project/internal/storage/model"
	"github.com/shovan04/storvia-project/internal/storage/policy"
)

// ReconcileResult reports what reconciliation found.
type ReconcileResult struct {
	UploadID string `json:"upload_id"`
	ObjectID string `json:"object_id"`
	Provider string `json:"provider"`
	State    string `json:"state"`
	Outcome  string `json:"outcome"`
	Reason   string `json:"reason,omitempty"`
	Location string `json:"location,omitempty"`
}

// Reconcile decides what really happened to an upload that did not finish.
//
// This is the question a client cannot answer for itself: it said it uploaded, then
// the connection dropped. Storvia holds the attempt, so it is the only party that can
// ask the provider what actually landed. The three honest answers are "it is there",
// "nothing landed", and "nobody can tell" — and the third is the one that matters,
// because guessing either way risks a second write to a file Storvia believes it does
// not have.
//
// It never retries and never re-uploads. Deciding to write again is not something this
// endpoint may do on its own.
func (s *UploadService) Reconcile(ctx context.Context, scope persistence.Scope, uploadID string) (ReconcileResult, error) {
	upload, err := s.deps.Uploads.Get(ctx, scope, uploadID)
	if err != nil {
		return ReconcileResult{}, apperr.NotFound("no such upload for this account")
	}
	object, err := s.deps.Objects.Get(ctx, scope, upload.ObjectID)
	if err != nil {
		return ReconcileResult{}, apperr.NotFound("no such object for this account")
	}

	result := ReconcileResult{
		UploadID: upload.ID,
		ObjectID: object.ID,
		Provider: string(object.Provider),
		State:    string(object.Status),
	}

	// A finished upload has nothing to reconcile. Saying so is more useful than
	// reporting an outcome for an object that was committed long ago.
	if object.Status == model.ObjectReady {
		result.Outcome = "already_ready"
		return result, nil
	}

	switch object.Provider {
	case model.ProviderS3Compatible:
		return s.reconcileS3(ctx, scope, upload.ID, object, result)
	case model.ProviderGoogleDrive:
		return s.reconcileDrive(ctx, scope, object, result)
	default:
		return ReconcileResult{}, apperr.Internal("the object's provider is not recorded")
	}
}

// reconcileS3 asks the fallback provider what it actually holds.
//
// The key was generated server-side and is already recorded, so this is a lookup
// rather than a guess: the answer is a fact about one specific key.
func (s *UploadService) reconcileS3(ctx context.Context, scope persistence.Scope, uploadID string, object model.LogicalObject, result ReconcileResult) (ReconcileResult, error) {
	if s.deps.FallbackUploader == nil {
		return ReconcileResult{}, apperr.ProviderNotConfigured(
			"the fallback provider is not available in this deployment")
	}
	if object.BackendID == nil || object.ProviderObjectKey == nil {
		return ReconcileResult{}, apperr.Internal("the object's storage location is inconsistent")
	}

	verified, err := s.deps.FallbackUploader.Verify(ctx, *object.BackendID, *object.ProviderObjectKey)
	if err != nil {
		var providerErr *policy.ProviderError
		if errors.As(err, &providerErr) && providerErr.Class == policy.ClassMissing {
			// The provider is certain nothing landed, which is the one case where a
			// client can safely start over.
			result.Outcome = "nothing_stored"
			return result, nil
		}
		result.Outcome = "unknown"
		result.Reason = "the provider could not be asked"
		return result, nil
	}

	result.Location = *object.ProviderObjectKey
	if verified.SizeBytes != object.ExpectedSize {
		// Something is there but it is not what was declared. Committing it would be
		// worse than reporting it, so it is surfaced rather than adopted.
		result.Outcome = "size_mismatch"
		result.Reason = fmt.Sprintf("the provider holds %d bytes but %d were declared",
			verified.SizeBytes, object.ExpectedSize)
		return result, nil
	}

	// The bytes are present and the right size, so the upload did in fact finish.
	// Committing it here is not a second write: the provider is asked what it has,
	// and Storvia records that fact.
	if err := s.deps.Objects.CommitReady(ctx, scope, object.ID, object.RowVersion,
		model.ProviderS3Compatible, nil, object.BackendID, "", *object.ProviderObjectKey,
		verified.SizeBytes, "", verified.Checksum); err != nil {
		return ReconcileResult{}, apperr.Wrap(err, apperr.KindInternal,
			"Internal error", "could not record the reconciled location")
	}
	if err := s.markUploadReconciled(ctx, scope, uploadID); err != nil {
		return ReconcileResult{}, err
	}
	result.Outcome = "completed"
	result.State = string(model.ObjectReady)
	return result, nil
}

// reconcileDrive reports honestly that a Drive upload cannot be reconciled.
//
// It could be, if Storvia kept the resumable session. It does not, deliberately: the
// session URI is a write capability for the file, and persisting it to survive a
// restart would mean storing that capability. The trade is deliberate and the cost is
// stated rather than hidden: a Drive upload interrupted mid-flight cannot be resumed
// and must be started again.
//
// The client does keep the session URL, so in principle it could ask Drive directly.
// Storvia choosing not to hold it is the point.
func (s *UploadService) reconcileDrive(_ context.Context, _ persistence.Scope, object model.LogicalObject, result ReconcileResult) (ReconcileResult, error) {
	// An object that is not ready has no real Drive file id recorded. What is in
	// provider_file_id is the placeholder written when the upload was planned, and
	// the only real id — the one Google returned to the browser — arrived at
	// completion and was committed then.
	//
	// So Storvia genuinely cannot answer this question. It would need the resumable
	// session to ask Drive how many bytes landed, and it deliberately does not hold
	// that session because the session URL is a write capability for the file.
	//
	// The answer is therefore "unknown", not "nothing stored". That distinction is
	// the whole point of the endpoint: a client told nothing was written may start
	// over and create a second file, which is exactly the duplicate this service
	// exists to prevent.
	result.Outcome = "unknown"
	result.Reason = "storvia does not hold the drive upload session, so it cannot ask " +
		"google what landed. If your client finished the transfer, retry the complete " +
		"call with the file id google returned."
	return result, nil
}

// markUploadReconciled closes the attempt that reconciliation superseded.
func (s *UploadService) markUploadReconciled(ctx context.Context, scope persistence.Scope, uploadID string) error {
	attempt, err := s.deps.Attempts.GetActiveForUpload(ctx, uploadID)
	if err != nil {
		// No active attempt is not a failure: an upload whose attempt already ended
		// still reconciles cleanly.
		return nil
	}
	if err := s.deps.Attempts.SetState(ctx, attempt.ID, model.AttemptCompleted, "reconciled"); err != nil {
		return apperr.Wrap(err, apperr.KindInternal,
			"Internal error", "the location was recorded but the attempt state was not")
	}
	if err := s.deps.Uploads.SetActiveAttempt(ctx, scope, uploadID, attempt.ID, model.ObjectReady); err != nil {
		return apperr.Wrap(err, apperr.KindInternal,
			"Internal error", "the location was recorded but the upload state was not")
	}
	return nil
}

package service

import (
	"context"
	"io"
	"log/slog"

	"github.com/shovan04/storvia-project/internal/apperr"
	"github.com/shovan04/storvia-project/internal/persistence"
	"github.com/shovan04/storvia-project/internal/storage/model"
	"github.com/shovan04/storvia-project/internal/storage/policy"
)

// ContentSource resolves a recorded provider location to a readable stream.
type ContentSource interface {
	// Open streams a recorded object from its own provider. It never falls back
	// to a second provider: Storvia holds no second copy to fall back to.
	Open(ctx context.Context, object model.LogicalObject, byteRange *model.ByteRange) (io.ReadCloser, error)
	// Delete removes an object from its recorded provider only.
	Delete(ctx context.Context, object model.LogicalObject) error
}

// ObjectService implements the object read and delete use cases.
type ObjectService struct {
	objects *persistence.ObjectRepository
	uploads *persistence.UploadRepository
	source  ContentSource
	// jobs schedules deferred work for an operation that did not complete. It is nil
	// when the deployment runs no worker, and then a failed delete stays failed
	// rather than being retried.
	jobs   *persistence.JobRepository
	logger *slog.Logger
}

// NewObjectService builds the service.
func NewObjectService(objects *persistence.ObjectRepository, uploads *persistence.UploadRepository, source ContentSource) *ObjectService {
	return &ObjectService{
		objects: objects,
		uploads: uploads,
		source:  source,
		logger:  slog.Default(),
	}
}

// WithJobs attaches the retry queue.
func (s *ObjectService) WithJobs(jobs *persistence.JobRepository, logger *slog.Logger) *ObjectService {
	s.jobs = jobs
	if logger != nil {
		s.logger = logger
	}
	return s
}

// GetObject returns one object, scoped to the tenant.
func (s *ObjectService) GetObject(ctx context.Context, scope persistence.Scope, objectID string) (model.LogicalObject, error) {
	object, err := s.objects.Get(ctx, scope, objectID)
	if err != nil {
		// A cross-tenant read is reported as not found rather than forbidden. A
		// forbidden response would confirm the ID exists, turning the endpoint
		// into an existence oracle.
		return model.LogicalObject{}, apperr.NotFound("no such object for this account")
	}
	return object, nil
}

// ContentStream is a resolved download.
type ContentStream struct {
	Body      io.ReadCloser
	Name      string
	MIMEType  string
	SizeBytes int64
	ETag      string
}

// OpenContent streams an object from its recorded location.
func (s *ObjectService) OpenContent(ctx context.Context, scope persistence.Scope, objectID string, byteRange *model.ByteRange) (ContentStream, error) {
	object, err := s.GetObject(ctx, scope, objectID)
	if err != nil {
		return ContentStream{}, err
	}

	provider, err := object.Location()
	if err != nil {
		// A ready row with no location, or with two, is a data integrity fault.
		// It is reported as an internal error rather than being papered over.
		return ContentStream{}, apperr.Wrap(err, apperr.KindInternal, "Internal error",
			"the object's storage location is inconsistent")
	}

	if object.Status == model.ObjectMissing {
		return ContentStream{}, apperr.NotFound("the stored file is no longer present at its provider")
	}
	if object.Status != model.ObjectReady {
		return ContentStream{}, apperr.New(apperr.KindUncertain, "File is not ready",
			"the upload has not finished; retry once it reaches the ready state")
	}
	if byteRange != nil && byteRange.HasRange && !byteRange.Valid(object.VerifiedSize) {
		return ContentStream{}, apperr.Validation("the requested range is outside the file")
	}

	body, sourceErr := s.source.Open(ctx, object, byteRange)
	if sourceErr != nil {
		return ContentStream{}, providerErrorToAppError(provider, "read", sourceErr)
	}

	return ContentStream{
		Body:      body,
		Name:      object.OriginalName,
		MIMEType:  object.MIMEType,
		SizeBytes: object.VerifiedSize,
		ETag:      object.VerifiedChecksum,
	}, nil
}

// DeleteObject removes an object from its recorded provider location.
//
// The deletion is pinned to the location the object was written to. If Drive has
// recovered since an S3 fallback upload, the file still goes to S3: there is no
// copy on Drive to delete, and deleting a same-named object on another provider
// would destroy unrelated user data.
func (s *ObjectService) DeleteObject(ctx context.Context, scope persistence.Scope, objectID string) error {
	object, err := s.GetObject(ctx, scope, objectID)
	if err != nil {
		return err
	}
	if object.Status == model.ObjectDeleted {
		// Already gone: deletion is idempotent, and a second call is not an error.
		return nil
	}
	if object.Status == model.ObjectDeleting {
		return apperr.Conflict("a deletion for this object is already in progress")
	}

	provider, err := object.Location()
	if err != nil && object.Status == model.ObjectReady {
		return apperr.Wrap(err, apperr.KindInternal, "Internal error", "the object's storage location is inconsistent")
	}

	if err := s.objects.UpdateStatus(ctx, scope, object.ID, object.RowVersion, model.ObjectDeleting); err != nil {
		if persistence.IsConflict(err) {
			return apperr.Conflict("the object changed while the deletion was being prepared; retry")
		}
		return apperr.Wrap(err, apperr.KindInternal, "Internal error", "the deletion could not be started")
	}

	// An object that never reached a provider has nothing to remove from one. It was
	// created and then abandoned -- an upload that was never finished, or one whose
	// first attempt failed before anything was written. Asking a provider to delete
	// what it never received returns a location error, which would schedule a retry
	// that can never succeed, leaving the object pinned in `deleting` forever.
	//
	// This is why deleting these from the console failed rather than working.
	if !object.ReachedProvider() {
		return s.finishDelete(ctx, scope, object, model.ObjectDeleted)
	}

	if err := s.source.Delete(ctx, object); err != nil {
		var providerErr *policy.ProviderError
		if errorsAs(err, &providerErr) && providerErr.Class == policy.ClassMissing {
			// The provider says the object is already absent. That is a documented
			// already-deleted outcome, not a failure, and it is not grounds to
			// delete anything elsewhere.
			return s.finishDelete(ctx, scope, object, model.ObjectMissing)
		}
		// The delete did not happen. Schedule it rather than leaving the object
		// stuck in `deleting`, which is what a caller retrying would then see as a
		// permanent conflict.
		//
		// The object stays in `deleting` on purpose: the job is pinned to this
		// object and this provider, so retrying cannot move the deletion somewhere
		// else.
		s.scheduleRetry(scope, object, persistence.JobDelete)
		return providerErrorToAppError(provider, "delete", err)
	}
	return s.finishDelete(ctx, scope, object, model.ObjectDeleted)
}

// scheduleRetry queues deferred work for an operation that did not complete.
//
// A failure here is logged rather than returned: the caller already has an error, and
// replacing it with "could not schedule" would hide the real reason the delete failed.
func (s *ObjectService) scheduleRetry(scope persistence.Scope, object model.LogicalObject, kind string) {
	if s.jobs == nil {
		return
	}
	objectID := object.ID
	if _, err := s.jobs.Enqueue(context.Background(), persistence.Job{
		ObjectID: &objectID,
		TenantID: scope.TenantID,
		Kind:     kind,
	}); err != nil {
		s.logger.Error("could not schedule a retry job",
			"object_id", object.ID, "kind", kind, "error", err.Error())
	}
}

// finishDelete records the terminal deletion state.
func (s *ObjectService) finishDelete(ctx context.Context, scope persistence.Scope, object model.LogicalObject, status model.ObjectStatus) error {
	// Re-read to get the row version the deleting update produced, so the
	// terminal update is not fighting its own optimistic lock.
	current, err := s.objects.Get(ctx, scope, object.ID)
	if err != nil {
		return apperr.NotFound("no such object for this account")
	}
	if err := s.objects.UpdateStatus(ctx, scope, current.ID, current.RowVersion, status); err != nil {
		if persistence.IsConflict(err) {
			return apperr.Conflict("the object changed while the deletion was being recorded; retry")
		}
		return apperr.Wrap(err, apperr.KindInternal, "Internal error", "the deletion could not be recorded")
	}
	return nil
}

// BulkDeleteResult summarises a clear-the-set request.
//
// It reports what happened one object at a time rather than a single boolean, because
// a bulk delete can partly succeed: one provider can be unreachable while the rest
// answer. Collapsing that to "failed" would hide the objects that were removed, and
// collapsing it to "ok" would claim a file was deleted that still exists.
type BulkDeleteResult struct {
	Deleted int
	Failed  []BulkDeleteFailure
}

// BulkDeleteFailure is one object that could not be deleted and why.
type BulkDeleteFailure struct {
	ObjectID string
	Name     string
	Reason   string
}

// DeleteAllObjects removes every object this account still has stored.
//
// It goes through DeleteObject one object at a time rather than issuing a bulk status
// update. A delete has to reach the provider that actually holds the bytes; flipping
// every row to `deleted` in one statement would leave the files sitting in Drive and
// the fallback bucket with nothing in Storvia that knows about them, which is the
// exact failure the single-object path exists to prevent.
//
// One failure does not stop the rest. An operator clearing a set wants as much removed
// as the providers will allow, and nine successes with one failure reported is more
// useful than stopping at the first hiccup.
//
// Each object is attempted once. A failure usually leaves the row in `deleting`, which
// is still returned by the list, so without that the loop would retry the same failure
// forever.
func (s *ObjectService) DeleteAllObjects(ctx context.Context, scope persistence.Scope) (BulkDeleteResult, error) {
	const batchSize = 200
	// A bound rather than a promise. It exists so a bug cannot turn this into an
	// unbounded loop; 25 batches of 200 is far more than a tenant should ever hold.
	const maxBatches = 25

	result := BulkDeleteResult{}
	attempted := make(map[string]bool)

	for batch := 0; batch < maxBatches; batch++ {
		objects, err := s.objects.ListObjects(ctx, scope, batchSize)
		if err != nil {
			return result, apperr.Wrap(err, apperr.KindInternal, "Internal error",
				"the stored objects could not be listed")
		}
		if len(objects) == 0 {
			break
		}

		fresh := 0
		for _, object := range objects {
			if attempted[object.ID] {
				continue
			}
			attempted[object.ID] = true
			fresh++

			if err := s.DeleteObject(ctx, scope, object.ID); err != nil {
				result.Failed = append(result.Failed, BulkDeleteFailure{
					ObjectID: object.ID,
					Name:     object.OriginalName,
					Reason:   deleteFailureReason(err),
				})
				continue
			}
			result.Deleted++
		}

		// Every listed object has already been attempted, so another pass would
		// only re-list the failures.
		if fresh == 0 {
			break
		}
	}

	return result, nil
}

// deleteFailureReason turns a service error into one line fit to show an operator.
//
// The Detail is used rather than the full Error() because the latter appends the
// wrapped cause, which can carry provider internals. When there is no app error to
// read a Detail from, the plain message is the best available and is still better
// than reporting nothing.
func deleteFailureReason(err error) string {
	var appErr *apperr.Error
	if errorsAs(err, &appErr) {
		if appErr.Detail != "" {
			return appErr.Detail
		}
		if appErr.Title != "" {
			return appErr.Title
		}
	}
	return err.Error()
}

// providerErrorToAppError maps a normalized provider error onto the shared error
// vocabulary, so the client sees a stable problem type regardless of provider.
func providerErrorToAppError(provider model.Provider, operation string, err error) error {
	var providerErr *policy.ProviderError
	if !errorsAs(err, &providerErr) {
		return apperr.Wrap(err, apperr.KindInternal, "Internal error", "the storage provider could not complete the request")
	}

	detail := operation + " failed: " + providerErr.SafeDetail
	if detail == operation+" failed: " {
		detail = operation + " failed"
	}

	switch providerErr.Class {
	case policy.ClassQuota:
		return apperr.Quota(detail)
	case policy.ClassRateLimited:
		return apperr.RateLimited(detail)
	case policy.ClassMissing:
		return apperr.NotFound("the stored file is no longer present at its provider")
	case policy.ClassUnavailable:
		return apperr.Unavailable("the storage provider is currently unavailable; no second copy exists to serve from")
	case policy.ClassUncertain:
		return apperr.Uncertain("the provider result is not yet known; retry rather than re-uploading elsewhere")
	case policy.ClassConsentRequired, policy.ClassAuthentication:
		return apperr.ConsentRequired("the google drive connection needs to be re-established")
	case policy.ClassAuthorization:
		return apperr.Authorization("the connected account may not perform this operation")
	case policy.ClassConfiguration:
		return apperr.ProviderNotConfigured(detail)
	case policy.ClassValidation:
		return apperr.Validation(detail)
	case policy.ClassConflict:
		return apperr.Conflict(detail)
	default:
		return apperr.Wrap(err, apperr.KindInternal, "Internal error",
			"the storage provider returned an unclassified failure")
	}
}

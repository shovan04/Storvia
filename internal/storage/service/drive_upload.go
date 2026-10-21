package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/shovan04/storvia-project/internal/apperr"
	"github.com/shovan04/storvia-project/internal/persistence"
	"github.com/shovan04/storvia-project/internal/storage/model"
)

// planDriveUpload mints a Drive resumable session and returns it to the client.
//
// This runs only after the policy has selected Drive and the attempt row is durable,
// so a client cannot use it to reach Drive for an upload Storvia did not plan.
//
// The session is created here rather than at completion. Drive mints the capability
// up front, and handing it over at completion would mean Storvia had to buffer the
// whole file, which is exactly the thing this service exists to avoid.
func (s *UploadService) planDriveUpload(
	ctx context.Context,
	scope persistence.Scope,
	object model.LogicalObject,
	attempt model.UploadAttempt,
	result CreateUploadResult,
) (CreateUploadResult, error) {
	if s.deps.DriveSessions == nil {
		return CreateUploadResult{}, apperr.ProviderNotConfigured(
			"Drive uploads are not available in this deployment")
	}

	connectionID := ""
	if attempt.DriveConnectionID != nil {
		connectionID = *attempt.DriveConnectionID
	}

	session, err := s.deps.DriveSessions.InitiateUpload(ctx, scope, DriveUploadRequest{
		ConnectionID: connectionID,
		FileName:     object.OriginalName,
		MimeType:     object.MIMEType,
		SizeBytes:    object.ExpectedSize,
	})
	if err != nil {
		// The attempt exists but has no session. Recording that keeps the row honest:
		// an attempt that never started must not look like one that is in flight.
		s.markAttemptFailed(ctx, attempt.ID, "drive did not issue an upload session")
		return CreateUploadResult{}, providerErrorToAppError(model.ProviderGoogleDrive, "initiate upload", err)
	}

	if strings.TrimSpace(session.SessionURL) == "" {
		s.markAttemptFailed(ctx, attempt.ID, "drive returned an empty session URL")
		return CreateUploadResult{}, apperr.Internal("the drive upload session could not be created")
	}

	result.Mode = model.ModeDriveResumableSession
	result.ChunkSize = session.ChunkSize
	result.ExpiresAt = session.ExpiresAt
	result.Session = &DriveSession{
		// The URL is handed over exactly once. It is never written to a log, an
		// attempt row, or any other durable store.
		URL:       session.SessionURL,
		ChunkSize: session.ChunkSize,
		ExpiresAt: session.ExpiresAt,
	}
	return result, nil
}

// markAttemptFailed records why an attempt will never complete.
//
// Without this an attempt stays `selected` forever, which is indistinguishable from an
// upload that is genuinely still running and would be reconciled as though it were.
func (s *UploadService) markAttemptFailed(ctx context.Context, attemptID, reason string) {
	if attemptID == "" {
		return
	}
	if err := s.deps.Attempts.SetState(ctx, attemptID, model.AttemptFailed, reason); err != nil {
		s.deps.Logger.WarnContext(ctx, "could not record a failed upload attempt",
			"attempt_id", attemptID, "error", err.Error())
	}
}

// CompleteDriveRequest is what the client reports when a resumable upload finishes.
//
// DriveFileID is untrusted input. The browser learns the real id from Drive's final
// chunk response and sends it back, because Storvia cannot learn it without persisting
// the session capability. It is therefore treated as a lookup key and every property
// that matters is re-read from Drive and checked, rather than being believed.
type CompleteDriveRequest struct {
	DriveFileID string
}

// CompleteDriveUpload verifies a Drive upload against Google and commits it.
//
// The client saying "I finished" is not evidence that the bytes arrived, and the file
// id it sends back is not evidence that the file is the one Storvia planned. Drive is
// asked what it actually stored, and the file is adopted only if it lives in the
// app-managed folder, is not trashed, and has the declared size.
func (s *UploadService) CompleteDriveUpload(
	ctx context.Context,
	scope persistence.Scope,
	uploadID string,
	req CompleteDriveRequest,
) (CompleteUploadResult, error) {
	if s.deps.DriveSessions == nil {
		return CompleteUploadResult{}, apperr.ProviderNotConfigured(
			"Drive uploads are not available in this deployment")
	}
	if strings.TrimSpace(req.DriveFileID) == "" {
		return CompleteUploadResult{}, apperr.Validation("drive_file_id is required to complete a Drive upload")
	}

	upload, err := s.deps.Uploads.Get(ctx, scope, uploadID)
	if err != nil {
		return CompleteUploadResult{}, apperr.NotFound("no such upload for this account")
	}
	object, err := s.deps.Objects.Get(ctx, scope, upload.ObjectID)
	if err != nil {
		return CompleteUploadResult{}, apperr.NotFound("no such object for this account")
	}
	if object.Status == model.ObjectReady {
		return CompleteUploadResult{Object: object, AlreadyComplete: true}, nil
	}
	if object.Provider != model.ProviderGoogleDrive {
		return CompleteUploadResult{}, apperr.Validation("this upload is not a Drive upload")
	}
	attempt, err := s.deps.Attempts.GetActiveForUpload(ctx, uploadID)
	if err != nil {
		return CompleteUploadResult{}, apperr.Uncertain("this upload has no active attempt to complete")
	}

	verified, err := s.deps.DriveSessions.FetchMetadata(ctx, scope, req.DriveFileID)
	if err != nil {
		return CompleteUploadResult{}, providerErrorToAppError(model.ProviderGoogleDrive, "verify", err)
	}
	if verified.SizeBytes != object.ExpectedSize {
		return CompleteUploadResult{}, apperr.New(apperr.KindUncertain, "Uploaded size does not match",
			fmt.Sprintf("google drive stored %d bytes but %d were declared", verified.SizeBytes, object.ExpectedSize))
	}
	// The file must be the one Storvia created, still present, and inside the folder
	// Storvia manages. Without this a client could point the upload at any other file
	// in the account that happens to be the right size and Storvia would adopt it.
	if verified.Trashed {
		return CompleteUploadResult{}, apperr.New(apperr.KindUncertain, "Stored file is not available",
			"google drive reports the file as deleted")
	}
	if !verified.InAppFolder {
		return CompleteUploadResult{}, apperr.New(apperr.KindUncertain, "Stored file is outside the Storvia folder",
			"the file reported by google drive is not inside the folder Storvia manages")
	}

	connectionID := ""
	if object.DriveConnectionID != nil {
		connectionID = *object.DriveConnectionID
	}
	if err := s.deps.Objects.CommitReady(ctx, scope, object.ID, object.RowVersion,
		// Argument order matters and the table enforces it: a Drive object carries a
		// file ID and no object key, the mirror image of the fallback path.
		model.ProviderGoogleDrive, &connectionID, nil, req.DriveFileID, "",
		verified.SizeBytes, verified.ChecksumAlgorithm, verified.Checksum); err != nil {
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

	s.deps.Metrics.RecordUploadFinished(string(model.ProviderGoogleDrive), "ready")
	s.deps.Metrics.RecordBytesStored(string(model.ProviderGoogleDrive), verified.SizeBytes)

	ready, err := s.deps.Objects.Get(ctx, scope, object.ID)
	if err != nil {
		return CompleteUploadResult{}, apperr.NotFound("no such object for this account")
	}
	return CompleteUploadResult{Object: ready}, nil
}

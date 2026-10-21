package service

import (
	"context"
	"time"

	"github.com/shovan04/storvia-project/internal/apperr"
	"github.com/shovan04/storvia-project/internal/persistence"
	"github.com/shovan04/storvia-project/internal/storage/model"
)

// UploadView is the client-facing state of an upload.
type UploadView struct {
	UploadID       string `json:"upload_id"`
	ObjectID       string `json:"object_id"`
	State          string `json:"state"`
	Provider       string `json:"provider"`
	FallbackReason string `json:"fallback_reason,omitempty"`
	UploadedBytes  int64  `json:"uploaded_bytes"`
	ExpectedBytes  int64  `json:"expected_bytes"`
	AttemptState   string `json:"attempt_state"`
	// MutationMayHaveStarted tells a client whether resending is safe. When it is
	// true the upload is pinned to its provider and must be reconciled, never
	// restarted elsewhere.
	MutationMayHaveStarted bool `json:"mutation_may_have_started"`
	// CreatedAt and UpdatedAt are RFC 3339 timestamps.
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// GetUpload returns an upload's state, scoped to the tenant.
func (s *UploadService) GetUpload(ctx context.Context, scope persistence.Scope, uploadID string) (UploadView, error) {
	upload, err := s.deps.Uploads.Get(ctx, scope, uploadID)
	if err != nil {
		// A cross-tenant read is a 404, not a 403, so the endpoint cannot be used
		// to discover which upload IDs exist.
		return UploadView{}, apperr.NotFound("no such upload for this account")
	}
	return s.view(ctx, scope, upload), nil
}

// view assembles the client view, including the active attempt's progress.
func (s *UploadService) view(ctx context.Context, scope persistence.Scope, upload model.Upload) UploadView {
	view := UploadView{
		UploadID:  upload.ID,
		ObjectID:  upload.ObjectID,
		State:     string(upload.State),
		CreatedAt: upload.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: upload.UpdatedAt.UTC().Format(time.RFC3339),
	}

	attempt, err := s.deps.Attempts.GetActiveForUpload(ctx, upload.ID)
	if err != nil {
		// No active attempt means the upload is settled, or was never routed.
		return view
	}
	view.Provider = string(attempt.Provider)
	view.AttemptState = string(attempt.State)
	view.UploadedBytes = attempt.ConfirmedOffset
	view.ExpectedBytes = attempt.ExpectedSize
	view.MutationMayHaveStarted = attempt.MutationMayHaveStarted

	if object, err := s.deps.Objects.Get(ctx, scope, upload.ObjectID); err == nil {
		view.FallbackReason = string(object.FallbackReason)
	}
	return view
}

// CancelUpload abandons an upload that has not begun writing bytes.
//
// Once a mutation may have started, cancellation is refused: the attempt has to be
// reconciled or cleaned up, because assuming it wrote nothing is exactly the
// assumption that produces a duplicate file.
func (s *UploadService) CancelUpload(ctx context.Context, scope persistence.Scope, uploadID string) error {
	upload, err := s.deps.Uploads.Get(ctx, scope, uploadID)
	if err != nil {
		return apperr.NotFound("no such upload for this account")
	}

	attempt, err := s.deps.Attempts.GetActiveForUpload(ctx, upload.ID)
	if err == nil && attempt.MutationMayHaveStarted {
		return apperr.New(apperr.KindUncertain, "Upload already started",
			"this upload may have written data to its provider and must be reconciled before it can be cancelled")
	}

	if upload.State == model.ObjectReady {
		return apperr.Conflict("a completed upload cannot be cancelled; delete the object instead")
	}

	if err := s.deps.Uploads.SetActiveAttempt(ctx, scope, upload.ID, "", model.ObjectFailed); err != nil {
		if persistence.IsConflict(err) {
			return apperr.Conflict("the upload changed while the cancellation was being applied; retry")
		}
		return apperr.Wrap(err, apperr.KindInternal, "Internal error", "the upload could not be cancelled")
	}
	return nil
}

// StorageStatus is the client-facing view of Storvia's storage posture.
//
// It exists so a client can tell the difference between "Drive is down, your
// upload went to the fallback", "Drive is down and the fallback is not available",
// and "your Drive connection needs attention". Those need different user-facing
// messages, and a generic error does not convey any of them.
type StorageStatus struct {
	// PrimaryProvider is always google_drive in this release.
	PrimaryProvider string `json:"primary_provider"`
	// DriveConnected reports whether new uploads can use Drive.
	DriveConnected bool `json:"drive_connected"`
	// FallbackEnabled reports whether outage fallback is configured at all.
	FallbackEnabled bool `json:"fallback_enabled"`
	// FallbackVendor names the configured fallback when there is one.
	FallbackVendor string `json:"fallback_vendor,omitempty"`
	// FallbackReady reports whether an enabled backend is currently usable.
	FallbackReady bool `json:"fallback_ready"`
	// FallbackRequiresConsent reports that outage uploads stay disabled until the
	// tenant accepts the disclosure.
	FallbackRequiresConsent bool `json:"fallback_requires_consent"`
	// AcceptsUploads is the single answer a client needs before offering upload.
	AcceptsUploads bool `json:"accepts_uploads"`
	// BlockedReason explains a false AcceptsUploads in operator-safe terms.
	BlockedReason string `json:"blocked_reason,omitempty"`
	// Uploads may be stored outside the user's Drive during an outage. This flag
	// lets a client repeat the disclosure rather than implying Drive-only storage.
	FallbackStoresOutsideDrive bool `json:"fallback_stores_outside_drive"`
}

// Status reports the storage posture for a tenant.
func (s *UploadService) Status(ctx context.Context, scope persistence.Scope) (StorageStatus, error) {
	status := StorageStatus{
		PrimaryProvider:            string(model.ProviderGoogleDrive),
		FallbackEnabled:            s.deps.Fallback.Enabled,
		FallbackStoresOutsideDrive: s.deps.Fallback.Enabled,
	}

	if !s.driveConfigured() {
		status.BlockedReason = "google drive integration is not configured on this deployment"
		return status, nil
	}

	connectionID, connUsable, err := s.driveConnection(ctx, scope)
	if err != nil {
		return StorageStatus{}, err
	}
	status.DriveConnected = connectionID != "" && connUsable

	if len(s.deps.Fallback.ActiveBackends()) > 0 {
		status.FallbackVendor = s.deps.Fallback.ActiveBackends()[0].Vendor
		status.FallbackReady = s.fallbackConfigured(ctx, scope)
	}

	consent, err := s.deps.FallbackConsent(ctx, scope)
	if err != nil || !consent {
		status.FallbackRequiresConsent = s.deps.Fallback.Enabled
	}

	// Uploads are accepted whenever Drive is usable, regardless of the fallback:
	// the fallback only matters when Drive is down.
	status.AcceptsUploads = status.DriveConnected
	if !status.AcceptsUploads {
		switch {
		case !s.driveConfigured():
			// Already set.
		case connectionID == "":
			status.BlockedReason = "connect a google drive account to start uploading"
		case !connUsable:
			status.BlockedReason = "the google drive connection needs to be re-authorized"
		}
	}
	return status, nil
}

// driveConfigured reports whether Google credentials are present on this
// deployment. It is a field on the service so the status endpoint can tell a
// missing deployment configuration apart from a user's disconnected account,
// which are very different problems for the person looking at the screen.
func (s *UploadService) driveConfigured() bool {
	if probe, ok := s.deps.Drive.(interface{ IsConfigured() bool }); ok {
		return probe.IsConfigured()
	}
	// An implementation that does not expose the probe is assumed configured: it
	// is a test double or a custom adapter that knows better.
	return true
}

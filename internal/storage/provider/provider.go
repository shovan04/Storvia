// Package provider defines the boundary between the Storvia service and a
// storage backend.
//
// A provider adapter owns HTTP details, credential use, signing, status parsing,
// and normalization of its errors into policy.FailureClass. It must never write
// an application row: it returns values and lets the service decide what to
// persist.
package provider

import (
	"context"
	"io"

	"github.com/shovan04/storvia-project/internal/storage/model"
)

// Availability reports whether a provider is reachable for a given scope.
//
// It is a hint, never a guarantee: a successful check does not reserve capacity,
// and a failed one does not prove the next call would fail.
type Availability interface {
	// Availability performs a read-only reachability check scoped to one
	// connection. It must not mutate anything.
	Availability(ctx context.Context, scope model.AvailabilityScope) error
}

// Storage is the full provider surface Storvia needs.
//
// The methods are intentionally not symmetric. Drive's resumable session and S3
// multipart upload are different protocols, and collapsing them into one fake
// common protocol is what makes implementations leak incorrect assumptions.
type Storage interface {
	Availability

	// Name returns the provider identifier, for example google_drive.
	Name() string

	// Initiate begins an upload and returns the session handle. A timeout from
	// this call must be reported with MutationMayHaveStarted set, because Drive
	// may already hold a resumable session.
	Initiate(ctx context.Context, meta model.ObjectMetadata) (model.Session, error)

	// UploadChunk sends one chunk and returns the provider-confirmed offset.
	UploadChunk(ctx context.Context, session model.Session, chunk model.Chunk) (model.Progress, error)

	// Complete finalizes the upload and returns the provider object.
	Complete(ctx context.Context, session model.Session) (model.ProviderObject, error)

	// InspectUpload reports the provider's view of an in-flight upload, which is
	// how Storvia resolves an unknown outcome without opening a second location.
	InspectUpload(ctx context.Context, session model.Session) (model.UploadState, error)

	// Head returns metadata for an existing object, used for verification.
	Head(ctx context.Context, object model.ProviderObject) (model.ObjectMetadata, error)

	// Open streams the object, optionally a single byte range.
	Open(ctx context.Context, object model.ProviderObject, byteRange *model.ByteRange) (io.ReadCloser, error)

	// Delete removes the object. The caller must have pinned the deletion to this
	// provider's recorded location.
	Delete(ctx context.Context, object model.ProviderObject) error

	// Abort discards an in-flight session. It is called only during cleanup,
	// after the prior attempt is known to be terminal.
	Abort(ctx context.Context, session model.Session) error
}

// ModeSelector is implemented by providers that advertise a client transfer mode.
type ModeSelector interface {
	UploadMode() model.UploadMode
}

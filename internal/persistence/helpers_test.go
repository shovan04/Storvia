package persistence

import (
	"github.com/shovan04/storvia-project/internal/storage/model"
)

// LogicalObjectForTest lets the persistence tests build object fixtures without
// repeating every field. It is test-only plumbing, kept out of the production
// API surface.
type LogicalObjectForTest struct {
	ID           string
	LogicalKey   string
	OriginalName string
	MIMEType     string
	Category     string
	ExpectedSize int64
	Status       string
}

// model converts the fixture into a domain object.
func (f LogicalObjectForTest) model() model.LogicalObject {
	return model.LogicalObject{
		ID:           f.ID,
		LogicalKey:   f.LogicalKey,
		OriginalName: f.OriginalName,
		MIMEType:     f.MIMEType,
		Category:     f.Category,
		ExpectedSize: f.ExpectedSize,
		Status:       model.ObjectStatus(f.Status),
	}
}

// uploadFor builds an upload fixture.
func uploadFor(id, objectID, idempotencyKey string) model.Upload {
	return model.Upload{
		ID:                 id,
		ObjectID:           objectID,
		IdempotencyKey:     idempotencyKey,
		RequestFingerprint: "fingerprint-" + idempotencyKey,
		State:              model.ObjectPending,
	}
}

// attemptFor builds an attempt fixture.
func attemptFor(id, uploadID string) model.UploadAttempt {
	provider := model.ProviderGoogleDrive
	return model.UploadAttempt{
		ID:                  id,
		UploadID:            uploadID,
		Provider:            provider,
		State:               model.AttemptSelected,
		OperationGeneration: 1,
	}
}

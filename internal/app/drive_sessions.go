package app

import (
	"context"

	"github.com/shovan04/storvia-project/internal/persistence"
	"github.com/shovan04/storvia-project/internal/storage/model"
	"github.com/shovan04/storvia-project/internal/storage/provider/drive"
	"github.com/shovan04/storvia-project/internal/storage/service"
)

// driveSessions adapts the Drive provider to the coordinator's upload port.
//
// It exists so the coordinator depends on a narrow interface rather than the whole
// Drive client. That matters for more than tidiness: it keeps the write capability
// explicit, and it means a test can supply a fake without standing up an HTTP server.
type driveSessions struct {
	client *drive.Client
}

// newDriveSessions returns nil when there is no Drive client, so the coordinator can
// report "Drive uploads are not available in this deployment" instead of a nil panic.
func newDriveSessions(client *drive.Client) service.DriveSessionPort {
	if client == nil {
		return nil
	}
	return driveSessions{client: client}
}

func (d driveSessions) InitiateUpload(
	ctx context.Context, scope persistence.Scope, req service.DriveUploadRequest,
) (service.DriveSessionResult, error) {
	result, err := d.client.InitiateUpload(ctx, scope, drive.InitiateUploadRequest{
		ConnectionID: req.ConnectionID,
		FileName:     req.FileName,
		MimeType:     req.MimeType,
		SizeBytes:    req.SizeBytes,
	})
	if err != nil {
		return service.DriveSessionResult{}, err
	}
	return service.DriveSessionResult{
		SessionURL: result.SessionURL,
		ChunkSize:  result.ChunkSize,
		ExpiresAt:  result.ExpiresAt,
	}, nil
}

func (d driveSessions) FetchMetadata(
	ctx context.Context, scope persistence.Scope, fileID string,
) (model.ObjectMetadata, error) {
	meta, err := d.client.FetchFile(ctx, scope, fileID)
	if err != nil {
		return model.ObjectMetadata{}, err
	}
	// FetchFile already refused to run without a usable connection, so the folder is
	// known here; ask for it rather than threading it through the provider-neutral
	// result.
	appFolder := d.client.AppFolderID(ctx, scope)
	return drive.FileMetadataToModel(meta, appFolder), nil
}

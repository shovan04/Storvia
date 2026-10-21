// Package storage provides the Storvia storage domain: object metadata,
// downloads, and the storage status endpoint.
//
// It is a modular sub-package of storvia: it imports the root storvia package
// for the core client and error types, and adds domain-specific types and
// functions. The root storvia package does not import this package, so there
// are no import cycles.
//
// Use this package when you want storage functionality without pulling in the
// full storvia client API:
//
//	client, _ := storvia.New(storvia.Config{BaseURL: "..."})
//	object, err := storage.GetObject(client, ctx, "obj_1")
//	status, err := storage.Status(client, ctx)
package storage

import (
	"context"

	storvia "github.com/shovan04/storvia-sdk"
)

// StorviaObject describes a stored file.
type StorviaObject = storvia.Object

// StorageStatus reports whether Storvia can accept an upload and why.
type StorageStatus = storvia.StorageStatus

// DriveConnection describes a Drive connection without any credential.
type DriveConnection = storvia.DriveConnection

// Status reports the storage posture.
func Status(client *storvia.Client, ctx context.Context) (*StorageStatus, error) {
	return client.Status(ctx)
}

// GetObject reads an object's metadata.
func GetObject(client *storvia.Client, ctx context.Context, objectID string) (*StorviaObject, error) {
	return client.GetObject(ctx, objectID)
}

// DeleteObject removes an object from its recorded location.
func DeleteObject(client *storvia.Client, ctx context.Context, objectID string) error {
	return client.DeleteObject(ctx, objectID)
}

// Download streams an object's content.
func Download(client *storvia.Client, ctx context.Context, objectID string) (*storvia.Download, error) {
	return client.Download(ctx, objectID)
}

// DownloadRange streams a byte range of an object.
func DownloadRange(client *storvia.Client, ctx context.Context, objectID string, start, end int64) (*storvia.Download, error) {
	return client.DownloadRange(ctx, objectID, start, end)
}

// GetDriveConnection reads the Drive connection state.
func GetDriveConnection(client *storvia.Client, ctx context.Context) (*DriveConnection, error) {
	return client.GetDriveConnection(ctx)
}

// StartDriveConnection begins the Drive consent flow.
func StartDriveConnection(client *storvia.Client, ctx context.Context) (string, error) {
	return client.StartDriveConnection(ctx)
}

// DisconnectDrive removes the Drive connection.
func DisconnectDrive(client *storvia.Client, ctx context.Context) error {
	return client.DisconnectDrive(ctx)
}

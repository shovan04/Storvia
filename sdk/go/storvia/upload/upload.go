// Package upload provides the Storvia upload domain: upload creation, Drive
// resumable sessions, presigned parts, and the chunked upload helper.
//
// It is a modular sub-package of storvia: it imports the root storvia package
// for the core client and error types, and adds domain-specific types and
// functions. The root storvia package does not import this package, so there
// are no import cycles.
//
// Use this package when you want upload functionality without pulling in the
// full storvia client API:
//
//	client, _ := storvia.New(storvia.Config{BaseURL: "..."})
//	result, err := upload.CreateUpload(client, ctx, "stable-key", storvia.CreateUploadRequest{...})
//	u := upload.New(client, req, result)
//	err = u.UploadAll(ctx, fileReader)
package upload

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	storvia "github.com/shovan04/storvia/sdk/go"
)

// UploadMode is the transfer mechanism the server selected. The client never
// chooses a provider.
type UploadMode = storvia.UploadMode

const (
	// ModeDriveResumableSession uploads directly to a Drive resumable session URI.
	ModeDriveResumableSession UploadMode = storvia.ModeDriveResumableSession
	// ModeS3PresignedParts uploads parts directly with short-lived, scoped URLs.
	ModeS3PresignedParts UploadMode = storvia.ModeS3PresignedParts
)

// PresignedPart is one short-lived upload capability.
type PresignedPart = storvia.PresignedPart

// CreateUploadRequest describes a new upload.
type CreateUploadRequest = storvia.CreateUploadRequest

// CreateUploadResult is the server's routing decision plus transfer instructions.
type CreateUploadResult = storvia.CreateUploadResult

// DriveSession is a Drive resumable upload session.
type DriveSession = storvia.DriveSession

// UploadView is the state of an upload.
type UploadView = storvia.UploadView

// ReconcileResult is what the server found when it asked the provider.
type ReconcileResult = storvia.ReconcileResult

// CreateUpload starts a new upload on the given client.
//
// idempotencyKey must be stable across retries of the same logical request.
func CreateUpload(client *storvia.Client, ctx context.Context, idempotencyKey string, req CreateUploadRequest) (*CreateUploadResult, error) {
	return client.CreateUpload(ctx, idempotencyKey, req)
}

// GetUpload reads an upload's state.
func GetUpload(client *storvia.Client, ctx context.Context, uploadID string) (*UploadView, error) {
	return client.GetUpload(ctx, uploadID)
}

// CancelUpload abandons an upload that has not begun writing bytes.
func CancelUpload(client *storvia.Client, ctx context.Context, uploadID string) error {
	return client.CancelUpload(ctx, uploadID)
}

// ReconcileUpload asks the server what actually happened to an unfinished upload.
func ReconcileUpload(client *storvia.Client, ctx context.Context, uploadID string) (*ReconcileResult, error) {
	return client.ReconcileUpload(ctx, uploadID)
}

// CompleteUpload finalizes an upload. For Drive mode, driveFileID is the id
// Google returned in the final chunk response.
func CompleteUpload(client *storvia.Client, ctx context.Context, uploadID string, driveFileID string) (*storvia.Object, error) {
	if driveFileID == "" {
		return nil, &storvia.APIError{
			Kind:   storvia.KindValidation,
			Title:  "A Drive file id is required",
			Detail: "google returns the file id in the final chunk response of a resumable upload",
		}
	}
	body := map[string]string{"drive_file_id": driveFileID}

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("storvia: encoding the completion request: %w", err)
	}

	var response struct {
		Object          storvia.Object `json:"object"`
		AlreadyComplete bool           `json:"already_complete"`
	}
	path := "/v1/storage/uploads/" + uploadID + "/complete"
	if err := client.DoRequest(ctx, http.MethodPost, path, bytes.NewReader(encoded), &response); err != nil {
		return nil, err
	}
	return &response.Object, nil
}

// Upload handles both transfer modes (Drive resumable session and S3 presigned
// parts). Bytes go direct-to-provider; no file bytes pass through Storvia.
type Upload struct {
	client *storvia.Client
	result *CreateUploadResult
	size   int64
	offset int64
}

// New wraps a CreateUploadResult into an Upload handle for moving bytes.
// sizeBytes is the declared size of the upload, taken from the original request.
func New(client *storvia.Client, req CreateUploadRequest, result *CreateUploadResult) *Upload {
	return &Upload{
		client: client,
		result: result,
		size:   req.SizeBytes,
	}
}

// ID returns the upload identifier.
func (u *Upload) ID() string { return u.result.UploadID }

// ObjectID returns the logical object identifier.
func (u *Upload) ObjectID() string { return u.result.ObjectID }

// UploadedBytes returns how many bytes this client has successfully sent.
func (u *Upload) UploadedBytes() int64 { return u.offset }

// DriveMode reports whether this upload goes through a Drive resumable session.
func (u *Upload) DriveMode() bool {
	return u.result.Mode == ModeDriveResumableSession
}

// PresignedMode reports whether this upload goes through fallback presigned parts.
func (u *Upload) PresignedMode() bool {
	return u.result.Mode == ModeS3PresignedParts
}

// UploadAll sends every byte of r to the provider, in the mode the server chose.
func (u *Upload) UploadAll(ctx context.Context, r io.Reader) error {
	switch u.result.Mode {
	case ModeDriveResumableSession:
		return u.uploadDrive(ctx, r)
	case ModeS3PresignedParts:
		return u.uploadPresigned(ctx, r)
	default:
		return &storvia.APIError{
			Kind:   storvia.KindUnavailable,
			Title:  "Unsupported upload mode",
			Detail: fmt.Sprintf("the server selected %q, which this client does not implement", u.result.Mode),
		}
	}
}

func (u *Upload) uploadDrive(ctx context.Context, r io.Reader) error {
	if u.result.Session == nil || u.result.Session.URL == "" {
		return &storvia.APIError{
			Kind:   storvia.KindUnavailable,
			Title:  "No Drive session",
			Detail: "the server did not return a resumable session for this upload",
		}
	}

	chunkSize := u.result.ChunkSize
	if chunkSize <= 0 {
		chunkSize = u.result.Session.ChunkSize
	}
	if chunkSize <= 0 {
		chunkSize = 256 * 1024
	}

	buffer := make([]byte, chunkSize)
	sent := int64(0)

	for {
		n, readErr := io.ReadFull(r, buffer)
		if n > 0 {
			if err := u.putDriveChunk(ctx, buffer[:n], sent); err != nil {
				return err
			}
			sent += int64(n)
			u.offset = sent
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("storvia: reading the file: %w", readErr)
		}
	}
}

func (u *Upload) putDriveChunk(ctx context.Context, chunk []byte, offset int64) error {
	end := offset + int64(len(chunk)) - 1

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.result.Session.URL, bytes.NewReader(chunk))
	if err != nil {
		return fmt.Errorf("storvia: building the drive chunk request: %w", err)
	}
	req.ContentLength = int64(len(chunk))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, end, u.size))

	resp, err := u.client.HTTPClient().Do(req)
	if err != nil {
		return fmt.Errorf("storvia: sending a chunk to google drive: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusPermanentRedirect ||
		resp.StatusCode == http.StatusOK ||
		resp.StatusCode == http.StatusCreated {
		return nil
	}
	return &storvia.APIError{
		Kind:   storvia.KindUnavailable,
		Title:  "Drive rejected a chunk",
		Detail: fmt.Sprintf("google returned %d for the byte range %d-%d", resp.StatusCode, offset, end),
	}
}

func (u *Upload) uploadPresigned(ctx context.Context, r io.Reader) error {
	parts := u.result.Parts
	if len(parts) == 0 {
		return &storvia.APIError{
			Kind:   storvia.KindUnavailable,
			Title:  "No presigned parts",
			Detail: "the server returned no upload capabilities for this upload",
		}
	}

	buffer := make([]byte, u.result.ChunkSize)
	sent := int64(0)

	for _, part := range parts {
		n, readErr := io.ReadFull(r, buffer)
		if n == 0 {
			if readErr == io.EOF {
				break
			}
			return fmt.Errorf("storvia: reading the file: %w", readErr)
		}
		if err := u.putPresignedPart(ctx, part, buffer[:n]); err != nil {
			return err
		}
		sent += int64(n)
		u.offset = sent
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
	}
	return nil
}

func (u *Upload) putPresignedPart(ctx context.Context, part storvia.PresignedPart, chunk []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, part.URL, bytes.NewReader(chunk))
	if err != nil {
		return fmt.Errorf("storvia: building the part request: %w", err)
	}
	req.ContentLength = int64(len(chunk))
	for key, values := range part.Headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	resp, err := u.client.HTTPClient().Do(req)
	if err != nil {
		return fmt.Errorf("storvia: sending a part to the fallback provider: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode/100 != 2 {
		return &storvia.APIError{
			Kind:   storvia.KindUnavailable,
			Title:  "Fallback provider rejected a part",
			Detail: fmt.Sprintf("part %d returned %d", part.PartNumber, resp.StatusCode),
		}
	}
	return nil
}

// Complete finalizes a presigned upload and returns the stored object.
func (u *Upload) Complete(ctx context.Context) (*storvia.Object, error) {
	if u.result.Mode == ModeDriveResumableSession {
		return nil, &storvia.APIError{
			Kind:   storvia.KindValidation,
			Title:  "Drive uploads need a file id",
			Detail: "call CompleteDrive with the file id google returned from the final chunk",
		}
	}
	var object storvia.Object
	path := "/v1/storage/uploads/" + u.result.UploadID + "/complete"
	if err := u.client.DoRequest(ctx, http.MethodPost, path, nil, &object); err != nil {
		return nil, err
	}
	return &object, nil
}

// CompleteDrive finalizes a Drive upload.
func (u *Upload) CompleteDrive(ctx context.Context, driveFileID string) (*storvia.Object, error) {
	if driveFileID == "" {
		return nil, &storvia.APIError{
			Kind:   storvia.KindValidation,
			Title:  "A Drive file id is required",
			Detail: "google returns the file id in the final chunk response of a resumable upload",
		}
	}
	body := map[string]string{"drive_file_id": driveFileID}

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("storvia: encoding the completion request: %w", err)
	}

	var response struct {
		Object          storvia.Object `json:"object"`
		AlreadyComplete bool           `json:"already_complete"`
	}
	path := "/v1/storage/uploads/" + u.result.UploadID + "/complete"
	if err := u.client.DoRequest(ctx, http.MethodPost, path, bytes.NewReader(encoded), &response); err != nil {
		return nil, err
	}
	return &response.Object, nil
}

// Status returns the current upload state.
func (u *Upload) Status(ctx context.Context) (*UploadView, error) {
	return GetUpload(u.client, ctx, u.result.UploadID)
}

// Reconcile asks the server what actually happened to an unfinished upload.
func (u *Upload) Reconcile(ctx context.Context) (*ReconcileResult, error) {
	return ReconcileUpload(u.client, ctx, u.result.UploadID)
}

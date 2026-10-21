package drive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shovan04/storvia-project/internal/persistence"
	"github.com/shovan04/storvia-project/internal/storage/model"
	"github.com/shovan04/storvia-project/internal/storage/policy"
)

// DriveResumableChunkMultiple is Google's required chunk granularity.
//
// Drive rejects any chunk that is not a multiple of 256 KiB, except the final chunk,
// which may be any size. Announcing a chunk size that violates this produces a failure
// partway through an upload that has already moved bytes, which is the worst place to
// find out.
const DriveResumableChunkMultiple = 256 * 1024

// SessionTTL bounds how long a handed-out resumable session stays usable.
//
// A Drive session URI is a write capability: anyone holding it can write to that file
// until it expires or the upload completes. It is therefore never persisted and never
// handed out again, and the advertised expiry is deliberately short.
const SessionTTL = 15 * time.Minute

// FileMetadata is what Drive reports about a stored file.
type FileMetadata struct {
	ID         string
	Name       string
	MimeType   string
	SizeBytes  int64
	MD5        string
	Trashed    bool
	ParentID   string
	WebViewURL string
	// DriveID is the shared drive the file lives in. Empty for files in My Drive.
	DriveID string
}

// InitiateUploadRequest describes the file a client wants to store.
//
// Name is the display name. The parent is the app-managed folder, never a
// client-supplied identifier: a client must not be able to write into an arbitrary
// Drive location.
type InitiateUploadRequest struct {
	ConnectionID string
	FileName     string
	MimeType     string
	SizeBytes    int64
}

// InitiateUploadResult carries the resumable session to the client.
type InitiateUploadResult struct {
	// SessionURL is Drive's resumable session URI. It is a bearer capability for
	// this one file and must never be logged or persisted.
	SessionURL string
	// ChunkSize is the granularity the client must respect.
	ChunkSize int
	// ExpiresAt is when Storvia stops advertising the session.
	ExpiresAt time.Time
}

// InitiateUpload creates a Drive resumable upload session and returns its URI.
//
// This is Drive's equivalent of an S3 presigned URL. There is no signature to forge
// and no expiry to compute: Google mints the capability and Storvia only relays it.
// The client PUTs each chunk to SessionURL, and the final response carries the
// resulting file id.
//
// The session is created against the app-managed folder only. A caller cannot name a
// different parent, so this cannot be used to write anywhere in the user's Drive.
func (c *Client) InitiateUpload(ctx context.Context, scope persistence.Scope, req InitiateUploadRequest) (InitiateUploadResult, error) {
	connection, err := c.usableConnection(ctx, scope)
	if err != nil {
		return InitiateUploadResult{}, err
	}
	if req.ConnectionID != "" && req.ConnectionID != connection.ID {
		return InitiateUploadResult{}, &policy.ProviderError{
			Provider: c.Name(), Operation: "InitiateUpload", Class: policy.ClassValidation,
			SafeDetail: "the connection does not belong to this tenant",
		}
	}
	if strings.TrimSpace(req.FileName) == "" {
		return InitiateUploadResult{}, &policy.ProviderError{
			Provider: c.Name(), Operation: "InitiateUpload", Class: policy.ClassValidation,
			SafeDetail: "a file name is required",
		}
	}

	token, err := c.accessToken(ctx, connection)
	if err != nil {
		return InitiateUploadResult{}, err
	}

	// Only name, parents, and mimeType are writable on a Drive file at create time.
	//
	// When storing in appDataFolder, the parent is the special "appDataFolder"
	// ID, not a visible folder. Files there are invisible to the user in Drive.
	parents := []string{connection.RootFolderID}
	if c.cfg.StoreInAppDataFolder {
		parents = []string{"appDataFolder"}
	}
	// `size` is deliberately NOT here. It looks like the way to declare a length, and
	// the old comment claimed Drive required it, but `size` is a computed field on the
	// file resource: sending it makes every initiation fail with
	//   403 fieldNotWritable "The resource body includes fields which are not directly writable"
	// The declared length goes in the X-Upload-Content-Length header below, which is
	// how Drive is actually told how much to expect. This cost a full live round trip
	// to find, because the mocked tests assert on our own request rather than Drive's
	// opinion of it.
	metadata := map[string]any{
		"name":     req.FileName,
		"parents":  parents,
		"mimeType": req.MimeType,
	}
	body, err := json.Marshal(metadata)
	if err != nil {
		return InitiateUploadResult{}, &policy.ProviderError{
			Provider: c.Name(), Operation: "InitiateUpload", Class: policy.ClassValidation, Err: err,
		}
	}

	// driveUploadURL already ends in /files. Appending another "/files" produced
	// /upload/drive/v3/files/files, which Google answers with a 404 — the kind of
	// mistake that only shows up on the first live call.
	endpoint := driveUploadURL + "?uploadType=resumable&supportsAllDrives=true&fields=id,name,size"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return InitiateUploadResult{}, &policy.ProviderError{
			Provider: c.Name(), Operation: "InitiateUpload", Class: policy.ClassValidation, Err: err,
		}
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json; charset=UTF-8")
	httpReq.Header.Set("X-Upload-Content-Type", req.MimeType)
	if req.SizeBytes > 0 {
		httpReq.Header.Set("X-Upload-Content-Length", fmt.Sprintf("%d", req.SizeBytes))
	}

	resp, err := c.cfg.HTTPClient.Do(httpReq)
	if err != nil {
		return InitiateUploadResult{}, c.normalizeTransportError("InitiateUpload", err)
	}
	defer drainAndClose(resp)

	if resp.StatusCode >= 400 {
		return InitiateUploadResult{}, c.normalizeStatus("InitiateUpload", resp)
	}

	// The session URI is the only useful thing in the response, and it arrives in a
	// header rather than the body.
	sessionURL := resp.Header.Get("Location")
	if sessionURL == "" {
		return InitiateUploadResult{}, &policy.ProviderError{
			Provider: c.Name(), Operation: "InitiateUpload", Class: policy.ClassUnknown,
			SafeDetail: "drive accepted the session but returned no session URI",
		}
	}

	chunkSize := c.declaredChunkSize(req.SizeBytes)
	return InitiateUploadResult{
		SessionURL: sessionURL,
		ChunkSize:  chunkSize,
		ExpiresAt:  time.Now().UTC().Add(SessionTTL),
	}, nil
}

// declaredChunkSize returns the chunk granularity to advertise to a client.
//
// Drive requires every chunk except the last to be a 256 KiB multiple. Advertising a
// single safe value for all chunks is better than describing the exception and letting
// every client reimplement it, so a file that fits in one chunk advertises its own
// size and anything larger advertises the multiple.
func (c *Client) declaredChunkSize(totalSize int64) int {
	if totalSize > 0 && totalSize < DriveResumableChunkMultiple {
		return int(totalSize)
	}
	return DriveResumableChunkMultiple
}

// ChunkSizeIsValid reports whether a client-chosen chunk size would be accepted.
func ChunkSizeIsValid(size int) bool {
	return size > 0 && size%DriveResumableChunkMultiple == 0
}

// FetchFile reads a file's metadata.
//
// It is the verification step at completion: an upload is only committed once Drive
// confirms the stored size, because a client reporting "I finished" is not evidence
// that the bytes arrived.
func (c *Client) FetchFile(ctx context.Context, scope persistence.Scope, fileID string) (FileMetadata, error) {
	connection, err := c.usableConnection(ctx, scope)
	if err != nil {
		return FileMetadata{}, err
	}
	token, err := c.accessToken(ctx, connection)
	if err != nil {
		return FileMetadata{}, err
	}

	endpoint := fmt.Sprintf("%s/%s?fields=%s&supportsAllDrives=true",
		driveFilesURL, url.PathEscape(fileID), "id,name,mimeType,size,md5Checksum,trashed,parents,driveId,webViewLink")
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return FileMetadata{}, &policy.ProviderError{
			Provider: c.Name(), Operation: "FetchFile", Class: policy.ClassValidation, Err: err,
		}
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.cfg.HTTPClient.Do(httpReq)
	if err != nil {
		return FileMetadata{}, c.normalizeTransportError("FetchFile", err)
	}
	defer drainAndClose(resp)

	if resp.StatusCode >= 400 {
		return FileMetadata{}, c.normalizeStatus("FetchFile", resp)
	}

	var payload struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		MimeType    string   `json:"mimeType"`
		Size        string   `json:"size"`
		MD5         string   `json:"md5Checksum"`
		Trashed     bool     `json:"trashed"`
		Parents     []string `json:"parents"`
		DriveID     string   `json:"driveId"`
		WebViewLink string   `json:"webViewLink"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return FileMetadata{}, &policy.ProviderError{
			Provider: c.Name(), Operation: "FetchFile", Class: policy.ClassUnknown,
			SafeDetail: "drive returned a response Storvia could not read", Err: err,
		}
	}

	var size int64
	if payload.Size != "" {
		// Drive reports size as a JSON string because it exceeds float64 precision.
		fmt.Sscanf(payload.Size, "%d", &size)
	}

	var parent string
	if len(payload.Parents) > 0 {
		parent = payload.Parents[0]
	}

	return FileMetadata{
		ID:         payload.ID,
		Name:       payload.Name,
		MimeType:   payload.MimeType,
		SizeBytes:  size,
		MD5:        payload.MD5,
		Trashed:    payload.Trashed,
		ParentID:   parent,
		WebViewURL: payload.WebViewLink,
		DriveID:    payload.DriveID,
	}, nil
}

// DownloadFile streams a file's contents.
//
// alt=media makes Drive return the bytes rather than the metadata document.
func (c *Client) DownloadFile(ctx context.Context, scope persistence.Scope, fileID string) (io.ReadCloser, int64, string, error) {
	connection, err := c.usableConnection(ctx, scope)
	if err != nil {
		return nil, 0, "", err
	}
	token, err := c.accessToken(ctx, connection)
	if err != nil {
		return nil, 0, "", err
	}

	endpoint := fmt.Sprintf("%s/%s?alt=media&supportsAllDrives=true", driveFilesURL, url.PathEscape(fileID))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, "", &policy.ProviderError{
			Provider: c.Name(), Operation: "DownloadFile", Class: policy.ClassValidation, Err: err,
		}
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.cfg.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, 0, "", c.normalizeTransportError("DownloadFile", err)
	}
	if resp.StatusCode >= 400 {
		defer drainAndClose(resp)
		return nil, 0, "", c.normalizeStatus("DownloadFile", resp)
	}

	return resp.Body, resp.ContentLength, resp.Header.Get("Content-Type"), nil
}

// DeleteFile removes a file from Drive.
//
// Deletion is the one irreversible provider verb here, so it is never attempted on an
// ambiguous outcome: a transport failure returns an error and the caller decides,
// because a timed-out DELETE may or may not have happened.
func (c *Client) DeleteFile(ctx context.Context, scope persistence.Scope, fileID string) error {
	connection, err := c.usableConnection(ctx, scope)
	if err != nil {
		return err
	}
	token, err := c.accessToken(ctx, connection)
	if err != nil {
		return err
	}

	endpoint := fmt.Sprintf("%s/%s?supportsAllDrives=true", driveFilesURL, url.PathEscape(fileID))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return &policy.ProviderError{
			Provider: c.Name(), Operation: "DeleteFile", Class: policy.ClassValidation, Err: err,
		}
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.cfg.HTTPClient.Do(httpReq)
	if err != nil {
		return c.normalizeTransportError("DeleteFile", err)
	}
	defer drainAndClose(resp)

	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusNotFound {
		return c.normalizeStatus("DeleteFile", resp)
	}
	// 404 means it is already gone, which is the state the caller wanted.
	return nil
}

// UploadState reports where an in-flight resumable upload stands.
//
// It asks Drive how many bytes it has accepted, which is the only way to learn whether
// a failed upload partially committed. Guessing is how a duplicate write happens.
func (c *Client) UploadState(ctx context.Context, sessionURL string) (bytesReceived int64, done bool, err error) {
	if strings.TrimSpace(sessionURL) == "" {
		return 0, false, errors.New("drive: a session URL is required to query upload state")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, sessionURL, nil)
	if err != nil {
		return 0, false, err
	}
	req.Header.Set("Content-Range", "bytes */0")

	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return 0, false, err
	}
	defer drainAndClose(resp)

	// 308 is Drive's "resume from here" response. The Range header says how much
	// landed; it is absent when nothing has.
	if resp.StatusCode == http.StatusPermanentRedirect {
		if raw := resp.Header.Get("Range"); raw != "" {
			fmt.Sscanf(raw, "bytes=0-%d", &bytesReceived)
			bytesReceived++
		}
		return bytesReceived, false, nil
	}
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		return bytesReceived, true, nil
	}
	return 0, false, fmt.Errorf("drive: unexpected upload state response: %s", resp.Status)
}

// usableConnection resolves the tenant's connection or explains why it cannot be used.
func (c *Client) usableConnection(ctx context.Context, scope persistence.Scope) (Connection, error) {
	connection, err := c.conns.Connection(ctx, scope)
	if err != nil {
		return Connection{}, &policy.ProviderError{
			Provider: c.Name(), Operation: "DriveFile", Class: policy.ClassConsentRequired,
			SafeDetail: "no google drive connection exists for this account", Err: err,
		}
	}
	if connection.Status != StatusConnected {
		return Connection{}, &policy.ProviderError{
			Provider: c.Name(), Operation: "DriveFile", Class: policy.ClassConsentRequired,
			SafeDetail: "the google drive connection is not usable; reconnect it",
		}
	}
	// In app-data mode there is no visible folder to create or track, so a missing
	// RootFolderID is fine. appDataFolder is implicit and always available.
	if !c.cfg.StoreInAppDataFolder && connection.RootFolderID == "" {
		return Connection{}, &policy.ProviderError{
			Provider: c.Name(), Operation: "DriveFile", Class: policy.ClassConfiguration,
			SafeDetail: "the storvia drive folder is not known; reconnect the account",
		}
	}
	return connection, nil
}

// FileMetadataToModel converts Drive metadata into the provider-neutral form the
// service layer verifies against. appFolder is the app-managed folder the file must be
// inside for Storvia to adopt it.
func FileMetadataToModel(meta FileMetadata, appFolderID string) model.ObjectMetadata {
	algorithm, checksum := "", ""
	if meta.MD5 != "" {
		algorithm, checksum = "md5", meta.MD5
	}
	return model.ObjectMetadata{
		OriginalName:      meta.Name,
		MIMEType:          meta.MimeType,
		SizeBytes:         meta.SizeBytes,
		ChecksumAlgorithm: algorithm,
		Checksum:          checksum,
		InAppFolder:       meta.ParentID != "" && meta.ParentID == appFolderID,
		Trashed:           meta.Trashed,
	}
}

// AppFolderID returns the app-managed folder for a tenant, or "" when there is none.
func (c *Client) AppFolderID(ctx context.Context, scope persistence.Scope) string {
	connection, err := c.conns.Connection(ctx, scope)
	if err != nil {
		return ""
	}
	return connection.RootFolderID
}

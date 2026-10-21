package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/shovan04/storvia-project/internal/persistence"
	"github.com/shovan04/storvia-project/internal/storage/model"
	"github.com/shovan04/storvia-project/internal/storage/service"
)

// maxConsoleUploadBytes bounds a panel upload.
//
// It matches the product's own limit. The panel should exercise the same path a
// client would, including its limits, rather than a more permissive one that would let
// something pass in the console and fail in production.
const maxConsoleUploadBytes = 512 << 20

// consoleCategory is the category every panel upload is filed under.
//
// It is deliberately the most permissive configured category. The panel exists to
// exercise transport, and having an upload rejected for being an unusual file type
// would hide the thing the operator is actually trying to learn.
const consoleCategory = "other"

// UploadResult is what the panel shows after an upload.
type UploadResult struct {
	// Provider is where the bytes actually went.
	Provider string `json:"provider"`
	// ProviderRef is the provider-side identifier, for checking in Drive or R2.
	ProviderRef string `json:"provider_ref,omitempty"`
	// ObjectID and UploadID are the Storvia identifiers.
	ObjectID string `json:"object_id"`
	UploadID string `json:"upload_id"`
	// Name and Size echo what was received.
	Name string `json:"name"`
	Size int64  `json:"size"`
	// MIMEType is what was actually declared to the provider. It can differ from what
	// the client sent, and when it does the console says so rather than hiding it.
	MIMEType string `json:"mime_type,omitempty"`
	// Outcome is ready, failed, or unknown.
	Outcome string `json:"outcome"`
	// Elapsed is the wall time from receiving the file to committing it.
	Elapsed string `json:"elapsed"`
	// Detail is an operator-safe explanation when something went wrong.
	Detail string `json:"detail,omitempty"`
	// Bytes is what was actually transferred to the provider.
	Bytes int64 `json:"bytes_sent"`
}

// Upload runs a complete upload end to end.
//
// It performs the whole client sequence in one request -- create, transfer, complete
// -- because the point of the panel is to exercise that sequence. A panel that only
// created uploads would leave the interesting half untested.
//
// The Drive branch follows the real contract: Storvia returns a resumable session URL
// and the *client* writes to Google. Here the client is this handler, so it performs
// the PUT exactly as an SDK would. It is the same write; only the caller differs.
func (c *Console) Upload(w http.ResponseWriter, r *http.Request) {
	scope, ok := c.requireSession(w, r)
	if !ok {
		return
	}

	started := time.Now()

	// Bounded before anything else, so an oversized request is refused rather than
	// buffered.
	r.Body = http.MaxBytesReader(w, r.Body, maxConsoleUploadBytes)

	file, header, err := r.FormFile("file")
	if err != nil {
		c.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":  "No file was received.",
			"detail": "Choose a file and try again.",
		})
		return
	}
	defer file.Close()

	name := header.Filename
	if name == "" {
		name = "upload.bin"
	}
	size := header.Size
	if size <= 0 {
		// A stream of unknown length cannot be planned: the provider is told how much
		// to expect before the write, and the size is verified afterwards. Guessing
		// would make every upload a size mismatch.
		c.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":  "That file's size could not be determined.",
			"detail": "Storvia needs the declared size before it can plan an upload to a provider.",
		})
		return
	}

	// A browser usually sends a part Content-Type, but not every client does, and Go's
	// own form writer does not. Falling straight back to octet-stream would fail
	// validation on most deployments, so the type is inferred from the extension and
	// then from the configured allow-list.
	//
	// The result is reported back to the operator rather than silently substituted: a
	// declared type of application/octet-stream becoming text/plain without saying so
	// would make the panel's report untrustworthy.
	declaredType := header.Header.Get("Content-Type")
	mimeType := c.resolveMIMEType(declaredType, name)
	if mimeType != declaredType {
		c.log.InfoContext(r.Context(), "console inferred the content type",
			"declared", declaredType, "using", mimeType, "file", name)
	}

	category := c.consoleCategory(mimeType)

	created, err := c.uploads.CreateUpload(r.Context(), scope,
		fmt.Sprintf("console-%d", time.Now().UnixNano()),
		service.CreateUploadRequest{
			Name:      name,
			MIMEType:  mimeType,
			SizeBytes: size,
			Category:  category,
		})
	if err != nil {
		c.uploadFailed(w, r, name, size, started, "create", err)
		return
	}

	result := UploadResult{
		Provider: string(created.Provider),
		UploadID: created.UploadID,
		ObjectID: created.ObjectID,
		Name:     name,
		Size:     size,
		Elapsed:  time.Since(started).Round(time.Millisecond).String(),
		MIMEType: mimeType,
	}

	switch created.Provider {
	case model.ProviderGoogleDrive:
		err = c.runDriveUpload(r, scope, created, file, size, &result)
	case model.ProviderS3Compatible:
		err = c.runFallbackUpload(r, scope, created, file, size, &result)
	default:
		err = fmt.Errorf("the upload was planned for unknown provider %q", created.Provider)
	}
	if err != nil {
		c.uploadFailed(w, r, name, size, started, "transfer", err)
		return
	}

	c.writeJSON(w, http.StatusOK, result)
}

// categoryPreferences maps a MIME type prefix onto the categories Storvia defines, in
// order of preference.
//
// The panel files uploads under a real category rather than a synthetic one. A panel
// that always used a made-up category would pass while the category path -- size
// limits, provider content types, the UI grouping -- went untested.
var categoryPreferences = []struct {
	prefix     string
	categories []string
}{
	{"image/", []string{"image"}},
	{"text/", []string{"document", "report"}},
	{"application/pdf", []string{"document", "report"}},
	{"application/msword", []string{"document", "report"}},
	{"application/vnd.openxmlformats-officedocument", []string{"document", "report"}},
	{"application/vnd.ms-excel", []string{"report", "document"}},
	{"application/json", []string{"report", "document"}},
	{"text/csv", []string{"report", "document"}},
}

// extensionMIMETypes maps the extensions an operator is most likely to try into the
// types Storvia's allow-list uses.
var extensionMIMETypes = map[string]string{
	".txt":  "text/plain",
	".csv":  "text/csv",
	".pdf":  "application/pdf",
	".zip":  "application/zip",
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
}

// resolveMIMEType decides what type to declare for a console upload.
//
// The order is deliberate: trust what the client declared if the deployment allows it,
// then infer from the extension, then fall back to the first configured type. It never
// invents a type the deployment does not permit, because that would fail validation
// with a message about MIME types when the real problem is a missing header.
func (c *Console) resolveMIMEType(declared, name string) string {
	allowed := c.uploads.SortedMIMETypes()
	permitted := make(map[string]bool, len(allowed))
	for _, mime := range allowed {
		permitted[mime] = true
	}

	if declared != "" && permitted[declared] {
		return declared
	}
	if inferred, ok := extensionMIMETypes[strings.ToLower(path.Ext(name))]; ok && permitted[inferred] {
		return inferred
	}
	// A prefix match catches the common shapes: a text/* file, or an image/*.
	switch {
	case strings.HasPrefix(declared, "text/"), strings.HasPrefix(declared, "image/"):
		if permitted[declared] {
			return declared
		}
	}
	if len(allowed) > 0 {
		return allowed[0]
	}
	if declared != "" {
		return declared
	}
	return "application/octet-stream"
}

// consoleCategory picks a category this deployment allows for a file's media type.
//
// It reads the configured list rather than hardcoding one, because a deployment that
// does not allow the preferred category would otherwise reject every panel upload with
// a validation error that has nothing to do with what the operator is testing.
//
// If nothing matches, it falls back to the first configured category. Returning a
// category the deployment rejects would turn a testing tool into a source of
// unexplained failures.
func (c *Console) consoleCategory(mimeType string) string {
	allowed := c.uploads.AllowedCategories()
	if len(allowed) == 0 {
		return consoleCategory
	}
	permitted := make(map[string]bool, len(allowed))
	for _, category := range allowed {
		permitted[category] = true
	}

	for _, preference := range categoryPreferences {
		if !strings.HasPrefix(mimeType, preference.prefix) {
			continue
		}
		for _, category := range preference.categories {
			if permitted[category] {
				return category
			}
		}
	}
	return allowed[0]
}

// runDriveUpload performs the resumable upload an SDK performs.
//
// The session URL is a write capability. It is used once, here, and never persisted,
// logged, or replayed -- which is why it is read from the creation result and passed
// down rather than stored on a struct.
func (c *Console) runDriveUpload(r *http.Request, scope persistence.Scope, created service.CreateUploadResult,
	file io.Reader, size int64, result *UploadResult) error {
	if created.Session == nil || created.Session.URL == "" {
		return fmt.Errorf("drive upload returned no session url")
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPut,
		created.Session.URL, file)
	if err != nil {
		return fmt.Errorf("could not build the drive upload request: %w", err)
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Length", fmt.Sprint(size))

	resp, err := c.driveHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("the drive upload request failed: %w", err)
	}
	defer drainAndClose(resp)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The provider's own message is the only actionable part of a 403. It is
		// capped, but generously: truncating it mid-sentence turns a diagnosis into
		// a guess.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("drive refused the upload with %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	result.Bytes = size

	// Google answers a completed resumable upload with the file's metadata. The id in
	// it is the client's claim about what it created, and completion treats it as
	// untrusted: the service verifies it against Drive before believing it.
	fileID, err := parseDriveUploadResponse(resp.Body)
	if err != nil {
		return err
	}
	result.ProviderRef = fileID

	complete, err := c.uploads.CompleteDriveUpload(r.Context(), scope, created.UploadID,
		service.CompleteDriveRequest{DriveFileID: fileID})
	if err != nil {
		return fmt.Errorf("completion failed: %w", err)
	}
	result.Outcome = string(complete.Object.Status)
	result.ProviderRef = firstNonEmpty(
		deref(complete.Object.ProviderFileID), deref(complete.Object.ProviderObjectKey), fileID)
	return nil
}

// parseDriveUploadResponse reads the file id out of Google's response.
func parseDriveUploadResponse(body io.Reader) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("could not read drive's upload response: %w", err)
	}
	var payload struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("drive's upload response could not be read: %w", err)
	}
	if payload.ID == "" {
		return "", fmt.Errorf("drive accepted the upload but returned no file id")
	}
	return payload.ID, nil
}

// runFallbackUpload PUTs the bytes to each presigned part URL, then completes.
//
// Completion verifies with HeadObject rather than trusting a client-supplied ETag, so
// the console deliberately does not collect ETags: doing so would be a second,
// weaker contract that could pass while the real one failed.
func (c *Console) runFallbackUpload(r *http.Request, scope persistence.Scope, created service.CreateUploadResult,
	file io.Reader, size int64, result *UploadResult) error {
	if len(created.Parts) == 0 {
		return fmt.Errorf("the fallback upload returned no upload parts")
	}

	sent := int64(0)
	for _, part := range created.Parts {
		// The part's length is EndByte-StartByte+1. A range of zero is a
		// single-byte part, not "no range given", so the guard compares the two
		// bounds and not EndByte against zero.
		want := int64(-1)
		if part.EndByte >= part.StartByte {
			want = part.EndByte - part.StartByte + 1
		}

		// Always bound the reader to exactly this part's bytes.
		//
		// Handing the raw reader to the HTTP client for the final part would send it
		// chunked with no Content-Length. S3 presigned URLs are signed over the
		// headers they were issued with, so that request either fails the signature
		// or stalls -- and a stall here looks like an outage rather than a bug.
		var chunk io.Reader = file
		if want >= 0 {
			chunk = io.LimitReader(file, want)
		}

		req, err := http.NewRequestWithContext(r.Context(), http.MethodPut, part.URL, chunk)
		if err != nil {
			return fmt.Errorf("could not build the request for part %d: %w", part.PartNumber, err)
		}
		if want >= 0 {
			req.ContentLength = want
			req.Header.Set("Content-Length", fmt.Sprint(want))
		}
		for name, values := range part.Headers {
			for _, value := range values {
				req.Header.Add(name, value)
			}
		}

		resp, err := c.fallbackHTTPClient.Do(req)
		if err != nil {
			return fmt.Errorf("part %d failed: %w", part.PartNumber, err)
		}
		drainAndClose(resp)
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("part %d was refused with %d", part.PartNumber, resp.StatusCode)
		}

		if want >= 0 {
			sent += want
		} else {
			// A capability with no byte range means "the rest of the object".
			sent = size
			break
		}
	}
	result.Bytes = sent

	complete, err := c.uploads.CompleteUpload(r.Context(), scope, created.UploadID)
	if err != nil {
		return fmt.Errorf("completion failed: %w", err)
	}
	result.Outcome = string(complete.Object.Status)
	result.ProviderRef = firstNonEmpty(
		deref(complete.Object.ProviderObjectKey), deref(complete.Object.ProviderFileID))
	return nil
}

// uploadFailed reports a failed attempt.
func (c *Console) uploadFailed(w http.ResponseWriter, r *http.Request,
	name string, size int64, started time.Time, stage string, cause error) {
	c.log.WarnContext(r.Context(), "console upload failed",
		"stage", stage, "file", name, "size", size, "error", cause.Error())

	c.writeJSON(w, http.StatusBadGateway, UploadResult{
		Name:    name,
		Size:    size,
		Outcome: "failed",
		Elapsed: time.Since(started).Round(time.Millisecond).String(),
		Detail:  fmt.Sprintf("%s failed: %s", stage, safeConsoleDetail(cause)),
	})
}

// consoleFileLabel reduces a client-supplied name to something safe to display.
func consoleFileLabel(name string) string {
	cleaned := strings.TrimSpace(name)
	if cleaned == "" {
		return "upload"
	}
	// Only the final path element, so a crafted name cannot appear to be a path.
	cleaned = path.Base(strings.ReplaceAll(cleaned, "\\", "/"))
	if len(cleaned) > 120 {
		cleaned = cleaned[:120]
	}
	return cleaned
}

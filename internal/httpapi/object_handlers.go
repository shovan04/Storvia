package httpapi

import (
	"net/http"
	"strings"

	"github.com/shovan04/storvia-project/internal/apperr"
	"github.com/shovan04/storvia-project/internal/storage/model"
	"github.com/shovan04/storvia-project/internal/storage/service"
)

// ObjectHandler exposes read and delete over logical objects.
type ObjectHandler struct {
	svc *service.ObjectService
}

// NewObjectHandler builds the handler.
func NewObjectHandler(svc *service.ObjectService) *ObjectHandler {
	return &ObjectHandler{svc: svc}
}

// objectResponse describes a logical object.
//
// It carries Storvia's own identifiers only. A provider file ID or bucket key is
// never returned: handing one to a client turns a server-controlled pointer into
// a client-controlled one.
type objectResponse struct {
	ObjectID  string `json:"object_id"`
	Name      string `json:"name"`
	MIMEType  string `json:"mime_type"`
	SizeBytes int64  `json:"size_bytes"`
	Status    string `json:"status"`
	Provider  string `json:"provider"`
	Category  string `json:"category"`
	// FallbackReason is set when the object was stored on S3 during a Drive outage.
	FallbackReason string  `json:"fallback_reason,omitempty"`
	VerifiedAt     *string `json:"verified_at,omitempty"`
	CreatedAt      string  `json:"created_at"`
}

// newObjectResponse converts a domain object into its wire form.
func newObjectResponse(object model.LogicalObject) objectResponse {
	response := objectResponse{
		ObjectID:       object.ID,
		Name:           object.OriginalName,
		MIMEType:       object.MIMEType,
		SizeBytes:      object.VerifiedSize,
		Status:         string(object.Status),
		Provider:       string(object.Provider),
		Category:       object.Category,
		FallbackReason: string(object.FallbackReason),
		CreatedAt:      object.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
	if object.VerifiedAt != nil {
		formatted := object.VerifiedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
		response.VerifiedAt = &formatted
	}
	// Prefer the verified size, falling back to the expected one while pending so
	// a client can still show progress.
	if response.SizeBytes == 0 {
		response.SizeBytes = object.ExpectedSize
	}
	return response
}

// Show handles GET /v1/storage/objects/{object_id}.
func (h *ObjectHandler) Show(w http.ResponseWriter, r *http.Request) {
	scope, err := scopeFrom(r)
	if err != nil {
		WriteProblem(w, r, err)
		return
	}
	objectID := r.PathValue("object_id")
	if objectID == "" {
		WriteProblem(w, r, apperr.Validation("object_id is required"))
		return
	}

	object, err := h.svc.GetObject(r.Context(), scope, objectID)
	if err != nil {
		WriteProblem(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, newObjectResponse(object))
}

// Download handles GET /v1/storage/objects/{object_id}/content.
//
// The stream comes from the object's recorded provider and nowhere else. If that
// provider is currently down the request fails, because Storvia holds no second
// copy to serve from.
func (h *ObjectHandler) Download(w http.ResponseWriter, r *http.Request) {
	scope, err := scopeFrom(r)
	if err != nil {
		WriteProblem(w, r, err)
		return
	}
	objectID := r.PathValue("object_id")
	if objectID == "" {
		WriteProblem(w, r, apperr.Validation("object_id is required"))
		return
	}

	// Parse the range before authorizing against a provider, so a malformed
	// header cannot cause an outbound call.
	byteRange, err := model.ParseByteRange(r.Header.Get("Range"))
	if err != nil {
		WriteProblem(w, r, apperr.Validation("the Range header is not supported"))
		return
	}

	stream, err := h.svc.OpenContent(r.Context(), scope, objectID, byteRange)
	if err != nil {
		WriteProblem(w, r, err)
		return
	}
	defer stream.Body.Close()

	header := w.Header()
	header.Set("Content-Type", stream.MIMEType)
	// A quoted filename is escaped so a name containing a quote or newline cannot
	// inject a header.
	header.Set("Content-Disposition", contentDisposition(stream.Name))
	// User content must never be served from a shared cache, and a capability URL
	// or private object must never be indexed.
	header.Set("Cache-Control", "private, no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	if stream.ETag != "" {
		header.Set("ETag", stream.ETag)
	}
	if stream.SizeBytes >= 0 {
		header.Set("Content-Length", formatInt(stream.SizeBytes))
	}
	if stream.SizeBytes >= 0 && byteRange.Valid(stream.SizeBytes) && byteRange.HasRange {
		w.WriteHeader(http.StatusPartialContent)
		header.Set("Content-Range", contentRange(byteRange, stream.SizeBytes))
	}

	// Stream with a bounded copy: the object may be gigabytes and must never be
	// buffered in memory.
	if _, err := copyBounded(w, stream.Body, stream.SizeBytes); err != nil {
		// The status line is already sent, so the failure can only be logged.
		logStreamError(r, err)
	}
}

// Delete handles DELETE /v1/storage/objects/{object_id}.
func (h *ObjectHandler) Delete(w http.ResponseWriter, r *http.Request) {
	scope, err := scopeFrom(r)
	if err != nil {
		WriteProblem(w, r, err)
		return
	}
	objectID := r.PathValue("object_id")
	if objectID == "" {
		WriteProblem(w, r, apperr.Validation("object_id is required"))
		return
	}
	if err := h.svc.DeleteObject(r.Context(), scope, objectID); err != nil {
		WriteProblem(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// contentDisposition builds a safe Content-Disposition header.
//
// The filename is quoted and stripped of quotes, backslashes, and control
// characters. An unescaped filename is a header injection vector, and the name is
// user-controlled.
func contentDisposition(name string) string {
	sanitized := strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 || r == 0x7f:
			return -1
		case r == '"' || r == '\\':
			return '_'
		default:
			return r
		}
	}, name)
	if sanitized == "" {
		sanitized = "download"
	}
	return "attachment; filename=\"" + sanitized + "\""
}

// contentRange renders a Content-Range header value.
func contentRange(byteRange *model.ByteRange, total int64) string {
	end := total
	if byteRange.End > 0 && byteRange.End < total {
		end = byteRange.End
	}
	return "bytes " + formatInt(byteRange.Start) + "-" + formatInt(end-1) + "/" + formatInt(total)
}

package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/shovan04/storvia-project/internal/apperr"
	"github.com/shovan04/storvia-project/internal/cache"
	"github.com/shovan04/storvia-project/internal/config"
	"github.com/shovan04/storvia-project/internal/metrics"
	"github.com/shovan04/storvia-project/internal/storage/service"
)

// UploadHandler exposes the upload use cases over HTTP.
type UploadHandler struct {
	svc    *service.UploadService
	drive  service.DriveAvailability
	config config.UploadsConfig
}

// NewUploadHandler builds the handler.
func NewUploadHandler(svc *service.UploadService, drive service.DriveAvailability, cfg config.UploadsConfig) *UploadHandler {
	return &UploadHandler{svc: svc, drive: drive, config: cfg}
}

// createUploadRequest is the inbound JSON body.
//
// Every field is required and strictly decoded, so a misspelled field is an error
// rather than a silent zero. A zero size would otherwise mean an unlimited upload.
type createUploadRequest struct {
	Name      string `json:"name"`
	MIMEType  string `json:"mime_type"`
	SizeBytes int64  `json:"size_bytes"`
	Category  string `json:"category"`
}

// createUploadResponse is the transfer instruction returned to the client.
//
// The client never chooses a provider. The server has already routed, and this
// response reports the decision along with the mode needed to move bytes.
type createUploadResponse struct {
	UploadID string `json:"upload_id"`
	ObjectID string `json:"object_id"`
	// Mode is drive_resumable_session or s3_presigned_parts.
	Mode string `json:"mode"`
	// Provider is where the server will store this upload.
	Provider string `json:"provider"`
	State    string `json:"state"`
	// Replayed is true when this response is the answer to an idempotent repeat
	// rather than a new provider write.
	Replayed bool `json:"replayed"`
	// FallbackReason is non-empty only when Drive was unavailable.
	FallbackReason string `json:"fallback_reason,omitempty"`
	// ChunkSize is the byte size the client should use per chunk or part.
	ChunkSize int `json:"chunk_size"`
	// UploadedBytes is the confirmed offset, non-zero on a replay or a resume.
	UploadedBytes int64 `json:"uploaded_bytes"`
	// ExpiresAt bounds the capability URLs in presigned mode.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// Parts are short-lived, operation-scoped capabilities. They are returned
	// here and nowhere else.
	Parts []service.PresignedPartInfo `json:"parts,omitempty"`
	// Session carries the Drive resumable session in Drive mode.
	//
	// The URL is a write capability for this one file: anyone holding it can write to
	// that file until it expires. It is returned once, in this response, and is never
	// stored anywhere Storvia can replay it.
	Session *service.DriveSession `json:"session,omitempty"`
}

// Create handles POST /v1/storage/uploads.
func (h *UploadHandler) Create(w http.ResponseWriter, r *http.Request) {
	scope, err := scopeFrom(r)
	if err != nil {
		WriteProblem(w, r, err)
		return
	}

	var body createUploadRequest
	if err := DecodeJSON(w, r, &body); err != nil {
		WriteProblem(w, r, err)
		return
	}

	result, err := h.svc.CreateUpload(r.Context(), scope, IdempotencyKey(r.Context()),
		service.CreateUploadRequest{
			Name:      body.Name,
			MIMEType:  body.MIMEType,
			SizeBytes: body.SizeBytes,
			Category:  body.Category,
		})
	if err != nil {
		WriteProblem(w, r, err)
		return
	}

	response := createUploadResponse{
		UploadID:       result.UploadID,
		ObjectID:       result.ObjectID,
		Mode:           string(result.Mode),
		Provider:       string(result.Provider),
		State:          string(result.State),
		Replayed:       result.Replayed,
		FallbackReason: string(result.FallbackReason),
		ChunkSize:      result.ChunkSize,
		UploadedBytes:  result.UploadedBytes,
		Parts:          result.Parts,
		Session:        result.Session,
	}
	if !result.ExpiresAt.IsZero() {
		response.ExpiresAt = &result.ExpiresAt
	}

	// A replay returns 200 because nothing new was created; a fresh upload
	// returns 201 because it did.
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
	}
	WriteJSON(w, status, response)
}

// Get handles GET /v1/storage/uploads/{upload_id}.
func (h *UploadHandler) Get(w http.ResponseWriter, r *http.Request) {
	scope, err := scopeFrom(r)
	if err != nil {
		WriteProblem(w, r, err)
		return
	}
	uploadID := r.PathValue("upload_id")
	if uploadID == "" {
		WriteProblem(w, r, apperr.Validation("upload_id is required"))
		return
	}

	// The read is tenant-scoped, so an upload ID belonging to another tenant is
	// indistinguishable from one that does not exist.
	upload, err := h.svc.GetUpload(r.Context(), scope, uploadID)
	if err != nil {
		WriteProblem(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, upload)
}

// Complete handles POST /v1/storage/uploads/{upload_id}/complete.
//
// It is idempotent: a repeat returns the object that is already ready rather than
// writing again. Verification compares the provider's confirmed size against the
// declared size, so a truncated or substituted upload never becomes a ready
// location.
func (h *UploadHandler) Complete(w http.ResponseWriter, r *http.Request) {
	scope, err := scopeFrom(r)
	if err != nil {
		WriteProblem(w, r, err)
		return
	}
	uploadID := r.PathValue("upload_id")
	if uploadID == "" {
		WriteProblem(w, r, apperr.Validation("upload_id is required"))
		return
	}

	// A Drive upload is completed by reporting the file id Google returned from the
	// final chunk. That id is untrusted: the service re-reads the file from Drive and
	// checks its folder, size, and trashed state before adopting it.
	var body struct {
		DriveFileID string `json:"drive_file_id"`
	}
	if r.Body != nil {
		// An empty body is valid for the fallback path, so a decode failure here is
		// not an error; the provider-specific path reports the missing id instead.
		_ = json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body)
	}

	var result service.CompleteUploadResult
	if body.DriveFileID != "" {
		result, err = h.svc.CompleteDriveUpload(r.Context(), scope, uploadID,
			service.CompleteDriveRequest{DriveFileID: body.DriveFileID})
	} else {
		result, err = h.svc.CompleteUpload(r.Context(), scope, uploadID)
	}
	if err != nil {
		WriteProblem(w, r, err)
		return
	}

	// A repeat is not a new creation, so it answers 200 rather than 201.
	status := http.StatusCreated
	if result.AlreadyComplete {
		status = http.StatusOK
	}
	WriteJSON(w, status, map[string]any{
		"object":           newObjectResponse(result.Object),
		"already_complete": result.AlreadyComplete,
	})
}

// Reconcile handles POST /v1/storage/uploads/{upload_id}/reconcile.
//
// It answers "what actually happened?" for an upload that did not finish. It never
// retries and never re-uploads: deciding to write again is not something a reconcile
// endpoint may do on a client's behalf.
func (h *UploadHandler) Reconcile(w http.ResponseWriter, r *http.Request) {
	scope, err := scopeFrom(r)
	if err != nil {
		WriteProblem(w, r, err)
		return
	}
	uploadID := r.PathValue("upload_id")
	if uploadID == "" {
		WriteProblem(w, r, apperr.Validation("upload_id is required"))
		return
	}

	result, err := h.svc.Reconcile(r.Context(), scope, uploadID)
	if err != nil {
		WriteProblem(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, result)
}

// Cancel handles DELETE /v1/storage/uploads/{upload_id}.
func (h *UploadHandler) Cancel(w http.ResponseWriter, r *http.Request) {
	scope, err := scopeFrom(r)
	if err != nil {
		WriteProblem(w, r, err)
		return
	}
	uploadID := r.PathValue("upload_id")
	if err := h.svc.CancelUpload(r.Context(), scope, uploadID); err != nil {
		WriteProblem(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Status handles GET /v1/storage/status.
//
// It reports whether Storvia can accept an upload and why, without exposing any
// secret, provider URL, or credential.
func (h *UploadHandler) Status(w http.ResponseWriter, r *http.Request) {
	scope, err := scopeFrom(r)
	if err != nil {
		WriteProblem(w, r, err)
		return
	}

	status, err := h.svc.Status(r.Context(), scope)
	if err != nil {
		WriteProblem(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, status)
}

// HealthHandler serves liveness, readiness, and metrics.
type HealthHandler struct {
	// Ready is the dependency-aware check. It reports nil when the service can
	// serve traffic. Liveness deliberately does not call it: a database blip
	// must not get the container killed and restarted into the same blip.
	Ready func(ctx context.Context) error

	cacheBackend cache.Cache
	metrics      *metrics.Storvia
}

// WithMetrics attaches the metric set served at /metrics.
func (h *HealthHandler) WithMetrics(m *metrics.Storvia) *HealthHandler {
	h.metrics = m
	return h
}

// NewHealthHandler builds the handler.
func NewHealthHandler(ready func(ctx context.Context) error, cacheBackend cache.Cache) *HealthHandler {
	return &HealthHandler{Ready: ready, cacheBackend: cacheBackend}
}

// Livez reports process liveness.
func (h *HealthHandler) Livez(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Readyz reports whether the service can serve requests.
func (h *HealthHandler) Readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	if h.Ready != nil {
		if err := h.Ready(ctx); err != nil {
			WriteProblem(w, r, apperr.New(apperr.KindUnavailable, "Storage provider unavailable",
				"a required dependency is not reachable"))
			return
		}
	}
	WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// Metrics exposes counters in a plain text format.
//
// A dependency-free exposition is deliberate for this milestone: adding a metrics
// client is a decision about operational ownership, not a prerequisite for the
// routing policy to be correct.
func (h *HealthHandler) Metrics(w http.ResponseWriter, r *http.Request) {
	// The exposition format is a contract with the scraper. Content type and status
	// are part of it; a 200 with no body is worse than a 503, because it looks like a
	// healthy service with nothing to say.
	if h.metrics == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", metrics.ContentType())
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, h.metrics.Gather())
}

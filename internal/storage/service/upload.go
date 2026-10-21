// Package service holds the storage use cases.
//
// It owns policy decisions, state transitions, idempotency, and tenant checks. The
// provider adapters below it own HTTP; the transport above it owns encoding. This
// is the only layer allowed to write both a provider location and a state change,
// which is what keeps the single-location invariant enforceable in one place.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/shovan04/storvia-project/internal/apperr"
	"github.com/shovan04/storvia-project/internal/config"
	"github.com/shovan04/storvia-project/internal/idid"
	"github.com/shovan04/storvia-project/internal/metrics"
	"github.com/shovan04/storvia-project/internal/persistence"
	"github.com/shovan04/storvia-project/internal/storage/model"
	"github.com/shovan04/storvia-project/internal/storage/policy"
)

// DriveAvailability is the read-only Drive check the router depends on.
//
// It is an interface so the coordinator can be tested without a network, and so
// the absence of Drive credentials is an explicit wiring decision rather than a
// nil panic. The Drive adapter implements it; a deployment with no Drive
// credentials installs an implementation that reports not-configured, which routes
// nowhere rather than silently pretending Drive is up.
type DriveAvailability interface {
	// Availability performs one read-only check.
	Availability(ctx context.Context, scope model.AvailabilityScope) error
	// ConnectionUsable reports whether the user has a working Drive connection.
	ConnectionUsable(ctx context.Context, scope persistence.Scope) (bool, error)
	// ConnectionID returns the Drive connection to use for this tenant, if any.
	ConnectionID(ctx context.Context, scope persistence.Scope) (string, error)
}

// Clock is the time source, injected so tests do not sleep.
type Clock func() time.Time

// DriveSessionResult is the resumable session handed to a client.
//
// DriveSessionURL is a bearer capability for exactly one file: anyone holding it can
// write to that file until it expires. It is returned once, never persisted, and never
// logged.
type DriveSessionResult struct {
	SessionURL string
	ChunkSize  int
	ExpiresAt  time.Time
}

// DriveSessionPort creates and verifies Drive resumable uploads.
//
// It is separate from DriveAvailability because the coordinator must be able to route
// and plan without holding the ability to write to a user's Drive, and so a deployment
// that only ever uses the fallback never needs a Drive write capability at all.
type DriveSessionPort interface {
	// InitiateUpload asks Drive for a resumable session scoped to the app folder.
	InitiateUpload(ctx context.Context, scope persistence.Scope, req DriveUploadRequest) (DriveSessionResult, error)
	// FetchMetadata reports what Drive actually stored for a file. It is the only
	// evidence that an upload really finished, so the coordinator commits what it
	// returns rather than what the client declared.
	FetchMetadata(ctx context.Context, scope persistence.Scope, fileID string) (model.ObjectMetadata, error)
}

// DriveUploadRequest is what the coordinator asks Drive to create.
type DriveUploadRequest struct {
	ConnectionID string
	FileName     string
	MimeType     string
	SizeBytes    int64
}

// Dependencies are the collaborators the upload service needs.
type Dependencies struct {
	Objects  *persistence.ObjectRepository
	Uploads  *persistence.UploadRepository
	Attempts *persistence.AttemptRepository
	Backends *persistence.BackendRepository
	Drive    DriveAvailability
	// DriveSessions creates resumable Drive upload sessions. It is nil when the
	// deployment cannot write to Drive at all, which makes CreateUpload say so
	// instead of handing a client a mode that cannot work.
	DriveSessions DriveSessionPort
	// UploadLimits holds the validation bounds. It is named distinctly from the
	// upload repository because both are needed in the same methods.
	UploadLimits config.UploadsConfig
	DriveCfg     config.DriveConfig
	Fallback     config.FallbackConfig
	// ForceFallback reports whether an operator has deliberately switched routing
	// away from Drive. It is optional: nil means "follow the normal routing policy".
	//
	// It is a function rather than a bool so a runtime toggle in the admin panel takes
	// effect on the next upload without a restart, and so the service does not need to
	// know how the switch is stored.
	ForceFallback func() bool
	// Metrics is optional instrumentation. It is a pointer so the service does not
	// require it, and every recorder tolerates nil, so a deployment that has not
	// wired metrics loses observation rather than correctness.
	Metrics *metrics.Storvia
	// FallbackUploader plans and verifies uploads on the S3-compatible provider.
	// It is nil when the fallback is disabled, which is what makes completion
	// report honestly rather than pretending to work.
	FallbackUploader FallbackUploader
	IDs              *idid.Generator
	Logger           *slog.Logger
	Now              Clock
	// Jitter supplies the backoff seed. Tests pass a fixed value.
	Jitter func() float64
	// Sleep is the backoff wait, injected so tests do not sleep.
	Sleep func(ctx context.Context, d time.Duration) error
	// FallbackConsent reports whether a tenant accepted the outage-fallback
	// disclosure. Until it returns true, outage routing stays disabled even with
	// a configured backend.
	FallbackConsent func(ctx context.Context, scope persistence.Scope) (bool, error)
}

// UploadService implements the upload use cases.
type UploadService struct {
	deps Dependencies
}

// NewUploadService builds the service.
func NewUploadService(deps Dependencies) (*UploadService, error) {
	if deps.Objects == nil || deps.Uploads == nil || deps.Attempts == nil {
		return nil, errors.New("service: repositories are required")
	}
	if deps.Drive == nil {
		return nil, errors.New("service: a Drive availability implementation is required")
	}
	if deps.IDs == nil {
		deps.IDs = idid.NewGenerator("id")
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Jitter == nil {
		deps.Jitter = func() float64 {
			// Full jitter needs a real spread; a fixed value would synchronize
			// every retry in the fleet after a Drive blip.
			return float64(time.Now().UnixNano()%1000) / 1000.0
		}
	}
	if deps.Sleep == nil {
		deps.Sleep = sleepContext
	}
	if deps.FallbackConsent == nil {
		// Default to refusing outage uploads. Enabling this is a deliberate act.
		deps.FallbackConsent = func(context.Context, persistence.Scope) (bool, error) {
			return false, nil
		}
	}
	return &UploadService{deps: deps}, nil
}

// CreateUploadRequest is the validated inbound upload request.
type CreateUploadRequest struct {
	Name      string
	MIMEType  string
	SizeBytes int64
	Category  string
}

// CreateUploadResult tells the client how to transfer bytes.
type CreateUploadResult struct {
	UploadID string
	ObjectID string
	Mode     model.UploadMode
	Provider model.Provider
	State    model.ObjectStatus
	// Replayed is true when an existing idempotent upload was returned instead of
	// a new provider write. It tells the client the answer is already known.
	Replayed bool
	// FallbackReason is set when the outage fallback was selected.
	FallbackReason model.FallbackReason
	// ChunkSize is the Drive chunk size for the proxy mode.
	ChunkSize int
	// Parts are short-lived capabilities for the presigned mode.
	Parts []PresignedPartInfo
	// Session is set for the Drive resumable mode. It is nil for every other mode.
	//
	// The URL is a write capability for one file. It is deliberately not part of
	// any durable record, so a repeated request cannot recover it: a client that
	// loses it starts a new session rather than reusing a live one.
	Session *DriveSession
	// UploadedBytes is the confirmed offset on a replayed upload.
	UploadedBytes int64
	ExpiresAt     time.Time
}

// DriveSession is the resumable session a client uploads into.
type DriveSession struct {
	// URL is Drive's session URI. Treat it as a secret: it grants write access to
	// this one file until the session expires.
	URL       string    `json:"url"`
	ChunkSize int       `json:"chunk_size"`
	ExpiresAt time.Time `json:"expires_at"`
}

// PresignedPartInfo is one client-facing upload capability.
//
// The URL is a bearer token scoped to one part number. It is returned here and
// nowhere else, and it is never logged.
type PresignedPartInfo struct {
	PartNumber int                 `json:"part_number"`
	URL        string              `json:"url"`
	Headers    map[string][]string `json:"headers,omitempty"`
	ExpiresAt  time.Time           `json:"expires_at"`
	// StartByte and EndByte are this part's half-open byte range within the object,
	// with EndByte inclusive.
	//
	// They are here because a client cannot derive them otherwise. The server chose
	// the part size when it initiated the multipart upload, and S3 will only accept a
	// part that matches the boundaries it was created with. Without these, a client
	// has to guess a part size and be right, which is a contract that fails silently
	// at the provider.
	StartByte int64 `json:"start_byte"`
	EndByte   int64 `json:"end_byte"`
}

// Validate checks an inbound request against the configured limits.
//
// Client-declared MIME is untrusted metadata used for routing and download
// headers. It is not content validation and never grants trust in the content.
func (s *UploadService) Validate(req CreateUploadRequest) error {
	cfg := s.deps.UploadLimits

	if strings.TrimSpace(req.Name) == "" {
		return apperr.Validation("name is required").WithField("name", "required")
	}
	if len(req.Name) > 512 {
		return apperr.Validation("name is too long").WithField("name", "max 512 characters")
	}
	if req.SizeBytes <= 0 {
		return apperr.Validation("size_bytes must be positive").WithField("size_bytes", "must be positive")
	}
	if !cfg.CategoryAllowed(req.Category) {
		return apperr.Validation("category is not allowed").
			WithField("category", strings.Join(cfg.AllowedCategories, ", "))
	}
	if !cfg.MIMEAllowed(req.MIMEType) {
		// An unlisted type is rejected rather than sniffed. Silent acceptance is
		// how an unexpected archive payload reaches object storage.
		return apperr.Validation("mime_type is not allowed").
			WithField("mime_type", strings.Join(cfg.AllowedMIMETypes, ", "))
	}
	limit := cfg.EffectiveMaxSize(req.Category)
	if req.SizeBytes > limit {
		return apperr.Validation("size_bytes exceeds the limit for this category").
			WithField("size_bytes", fmt.Sprintf("maximum %d bytes for category %s", limit, req.Category))
	}
	return nil
}

// fingerprint is a stable hash of the immutable request metadata.
//
// Reusing an idempotency key with different metadata must conflict, or a client
// bug could silently retarget an in-flight upload at different content.
func fingerprint(req CreateUploadRequest) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		req.Name, req.MIMEType, fmt.Sprintf("%d", req.SizeBytes), req.Category,
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}

// CreateUpload runs the full new-upload path: validate, deduplicate, preflight,
// route, and persist the selection before any provider mutation.
func (s *UploadService) CreateUpload(ctx context.Context, scope persistence.Scope, idempotencyKey string, req CreateUploadRequest) (CreateUploadResult, error) {
	if err := s.Validate(req); err != nil {
		return CreateUploadResult{}, err
	}

	objectID, err := s.deps.IDs.New()
	if err != nil {
		return CreateUploadResult{}, apperr.Wrap(err, apperr.KindInternal, "Internal error", "could not allocate an identifier")
	}
	uploadID, err := s.deps.IDs.New()
	if err != nil {
		return CreateUploadResult{}, apperr.Wrap(err, apperr.KindInternal, "Internal error", "could not allocate an identifier")
	}

	// Idempotent replay: a repeat of the same key with the same metadata returns
	// the existing upload without touching a provider.
	existing, err := s.deps.Uploads.GetByIdempotencyKey(ctx, scope, idempotencyKey)
	if err == nil {
		return s.replay(ctx, scope, existing, fingerprint(req))
	}
	if !persistence.IsNotFound(err) {
		return CreateUploadResult{}, apperr.Wrap(err, apperr.KindInternal, "Internal error", "could not read the upload record")
	}

	logicalKey := objectID
	object := model.LogicalObject{
		ID:           objectID,
		TenantID:     scope.TenantID,
		UserID:       scope.UserID,
		LogicalKey:   logicalKey,
		OriginalName: req.Name,
		MIMEType:     req.MIMEType,
		Category:     req.Category,
		ExpectedSize: req.SizeBytes,
		Status:       model.ObjectPending,
	}
	if err := s.deps.Objects.Create(ctx, scope, object); err != nil {
		if persistence.IsConflict(err) {
			// Lost a creation race on the same logical key: treat it as a replay.
			return CreateUploadResult{}, apperr.Conflict("an object for this request already exists")
		}
		return CreateUploadResult{}, apperr.Wrap(err, apperr.KindInternal, "Internal error", "could not create the object record")
	}

	upload := model.Upload{
		ID:                 uploadID,
		ObjectID:           objectID,
		TenantID:           scope.TenantID,
		UserID:             scope.UserID,
		IdempotencyKey:     idempotencyKey,
		RequestFingerprint: fingerprint(req),
		State:              model.ObjectPending,
	}
	if err := s.deps.Uploads.Create(ctx, scope, upload); err != nil {
		if persistence.IsConflict(err) {
			found, findErr := s.deps.Uploads.GetByIdempotencyKey(ctx, scope, idempotencyKey)
			if findErr == nil {
				return s.replay(ctx, scope, found, fingerprint(req))
			}
			return CreateUploadResult{}, apperr.Conflict("this idempotency key is already in use")
		}
		return CreateUploadResult{}, apperr.Wrap(err, apperr.KindInternal, "Internal error", "could not create the upload record")
	}

	// Re-read the object so the row version used for the optimistic lock is the
	// one the database actually assigned. The in-memory struct still carries the
	// pre-insert version, and using it would make every selection fail its lock.
	fresh, err := s.deps.Objects.Get(ctx, scope, objectID)
	if err != nil {
		return CreateUploadResult{}, apperr.Wrap(err, apperr.KindInternal,
			"Internal error", "could not read back the object record")
	}

	return s.routeAndSelect(ctx, scope, fresh, upload)
}

// replay returns the existing upload for a repeated idempotent request.
func (s *UploadService) replay(ctx context.Context, scope persistence.Scope, upload model.Upload, requestedFingerprint string) (CreateUploadResult, error) {
	if upload.RequestFingerprint != requestedFingerprint {
		return CreateUploadResult{}, apperr.Conflict(
			"this Idempotency-Key was already used with different request metadata")
	}

	attempt, err := s.deps.Attempts.GetActiveForUpload(ctx, upload.ID)
	provider := model.ProviderUnset
	if err == nil {
		provider = attempt.Provider
	}
	// A replay reports the mode the upload was planned with. The capability itself is
	// never repeated: handing back a live Drive session or a live presigned URL on a
	// replay would extend the life of something meant to be single-use.
	mode := model.ModeDriveResumableSession
	if provider == model.ProviderS3Compatible {
		mode = model.ModeS3PresignedParts
	}

	offset := int64(0)
	if err == nil {
		offset = attempt.ConfirmedOffset
	}

	return CreateUploadResult{
		UploadID:      upload.ID,
		ObjectID:      upload.ObjectID,
		Mode:          mode,
		Provider:      provider,
		State:         upload.State,
		Replayed:      true,
		UploadedBytes: offset,
	}, nil
}

// routeAndSelect performs the bounded Drive preflight, applies the policy, and
// persists the chosen provider before any provider mutation.
func (s *UploadService) routeAndSelect(ctx context.Context, scope persistence.Scope, object model.LogicalObject, upload model.Upload) (CreateUploadResult, error) {
	connectionID, connUsable, err := s.driveConnection(ctx, scope)
	if err != nil {
		return CreateUploadResult{}, err
	}

	decision, err := s.decide(ctx, scope, connectionID, connUsable)
	if err != nil {
		return CreateUploadResult{}, err
	}

	// A reject or defer must stop here. Proceeding to the Drive path after the
	// policy said "this must not happen" is how a revoked grant or an
	// unclassified failure would silently become a Drive write.
	switch decision.Decision {
	case policy.DecisionReject:
		return CreateUploadResult{}, decisionToError(decision)
	case policy.DecisionDefer:
		return CreateUploadResult{}, apperr.Unavailable(
			"storage routing could not reach a decision; retry shortly")
	}

	selectedProvider := model.ProviderGoogleDrive
	var backendID *string
	var connectionRef *string
	fallbackReason := model.FallbackNone

	if decision.Decision == policy.DecisionUseFallback {
		selectedProvider = model.ProviderS3Compatible
		fallbackReason = model.FallbackReason(decision.Reason)

		backend, backendErr := s.deps.Backends.GetActive(ctx, scope.TenantID)
		if backendErr != nil {
			// The policy already said the fallback was eligible, so this is a
			// configuration race. Fail the upload rather than writing to a
			// destination the service cannot verify.
			s.deps.Logger.WarnContext(ctx, "fallback backend vanished between routing and selection",
				"backend_error", backendErr.Error())
			return CreateUploadResult{}, apperr.ProviderNotConfigured("the configured fallback backend is not available")
		}
		id := backend.ID
		backendID = &id
		s.deps.Metrics.RecordFallbackRouting(string(decision.Reason))
	} else {
		if connectionID == "" {
			return CreateUploadResult{}, apperr.ConsentRequired(
				"connect a Google Drive account before uploading")
		}
		ref := connectionID
		connectionRef = &ref
	}

	s.deps.Metrics.RecordUploadStarted(string(selectedProvider))
	if selectedProvider == model.ProviderS3Compatible {
		s.deps.Metrics.RecordFallbackRouting(string(fallbackReason))
	}

	// Generate the planned provider identity before the mutation. It is derived
	// from the opaque object ID, never from the client's file name.
	plannedIdentity := s.plannedIdentity(object, selectedProvider)

	if err := s.deps.Objects.SelectLocation(ctx, scope, object.ID, object.RowVersion,
		selectedProvider, connectionRef, backendID, plannedIdentity, fallbackReason); err != nil {
		return CreateUploadResult{}, apperr.Wrap(err, apperr.KindInternal,
			"Internal error", "could not record the provider selection")
	}

	attempt := model.UploadAttempt{
		ID:                      mustID(s.deps.IDs),
		UploadID:                upload.ID,
		Provider:                selectedProvider,
		DriveConnectionID:       connectionRef,
		BackendID:               backendID,
		PlannedProviderIdentity: &plannedIdentity,
		State:                   model.AttemptSelected,
		ExpectedSize:            object.ExpectedSize,
		OperationGeneration:     1,
	}
	if err := s.deps.Attempts.Create(ctx, attempt); err != nil {
		return CreateUploadResult{}, apperr.Wrap(err, apperr.KindInternal,
			"Internal error", "could not create the upload attempt")
	}
	if err := s.deps.Uploads.SetActiveAttempt(ctx, scope, upload.ID, attempt.ID, model.ObjectUploading); err != nil {
		return CreateUploadResult{}, apperr.Wrap(err, apperr.KindInternal,
			"Internal error", "could not bind the upload attempt")
	}

	result := CreateUploadResult{
		UploadID:       upload.ID,
		ObjectID:       object.ID,
		Provider:       selectedProvider,
		State:          model.ObjectUploading,
		FallbackReason: fallbackReason,
	}
	if selectedProvider == model.ProviderGoogleDrive {
		return s.planDriveUpload(ctx, scope, object, attempt, result)
	}

	result.Mode = model.ModeS3PresignedParts
	result.ChunkSize = s.deps.UploadLimits.S3PartSizeBytes
	result.ExpiresAt = s.deps.Now().Add(s.deps.UploadLimits.PresignedURLTTL.Duration())

	// The capabilities are planned only now: the policy has already selected the
	// fallback and the attempt row is durable, so a client cannot reach this path
	// for a Drive upload.
	parts, err := presignedPartsFor(s.deps.FallbackUploader, *backendID, plannedIdentity,
		object.ExpectedSize, s.deps.UploadLimits.PresignedURLTTL.Duration())
	if err != nil {
		return CreateUploadResult{}, err
	}
	result.Parts = parts
	return result, nil
}

// decisionToError maps a routing rejection onto the client-facing error.
//
// The mapping is per reason code rather than generic, because the user-visible
// actions differ completely: a revoked grant needs a reconnect, a quota needs
// "free up space", and an invalid request needs a corrected file.
func decisionToError(decision policy.RoutingDecision) error {
	switch decision.Reason {
	case policy.ReasonNotConnected:
		return apperr.ConsentRequired(decision.Message)
	case policy.ReasonQuotaBlocked:
		return apperr.Quota(decision.Message)
	case policy.ReasonInvalidRequest:
		return apperr.Validation(decision.Message)
	case policy.ReasonNotAuthorized:
		return apperr.Authorization(decision.Message)
	case policy.ReasonRateLimited:
		return apperr.RateLimited(decision.Message)
	case policy.ReasonNoFallbackConfigured:
		return apperr.ProviderNotConfigured(decision.Message)
	default:
		return apperr.Unavailable(decision.Message)
	}
}

// driveConnection resolves the Drive connection for the tenant.
func (s *UploadService) driveConnection(ctx context.Context, scope persistence.Scope) (string, bool, error) {
	connectionID, err := s.deps.Drive.ConnectionID(ctx, scope)
	if err != nil {
		return "", false, apperr.Wrap(err, apperr.KindInternal, "Internal error", "could not resolve the Drive connection")
	}
	if connectionID == "" {
		return "", false, nil
	}
	usable, err := s.deps.Drive.ConnectionUsable(ctx, scope)
	if err != nil {
		return "", false, apperr.Wrap(err, apperr.KindInternal, "Internal error", "could not read the Drive connection state")
	}
	return connectionID, usable, nil
}

// decide runs the bounded read-only preflight and evaluates the routing policy.
func (s *UploadService) decide(ctx context.Context, scope persistence.Scope, connectionID string, connectionUsable bool) (policy.RoutingDecision, error) {
	forced := s.deps.ForceFallback != nil && s.deps.ForceFallback()

	gates := policy.Gates{
		ConnectionUsable:        connectionUsable,
		Authorized:              true,
		RequestValid:            true,
		FallbackConfigured:      s.fallbackConfigured(ctx, scope),
		FallbackBudgetRemaining: true,
		OperatorForcedFallback:  forced,
	}

	consent, err := s.deps.FallbackConsent(ctx, scope)
	if err != nil {
		// A consent lookup failure is not consent. Defaulting to true here would
		// push a tenant's file into company-managed storage without consent.
		s.deps.Logger.WarnContext(ctx, "fallback consent lookup failed; treating as not consented",
			"error", err.Error())
		consent = false
	}
	gates.FallbackConsent = consent

	// With the fallback forced, the Drive preflight is not merely unnecessary -- it is
	// a wasted round trip on every upload during an incident, and one more request to
	// a provider that may already be struggling. Skipping it is what makes this
	// "route away from Drive" rather than "prefer not to".
	//
	// It is evaluated after the consent lookup because consent is still a gate: an
	// operator switching providers does not manufacture the tenant's agreement to the
	// fallback.
	if forced {
		s.deps.Logger.InfoContext(ctx, "routing to the fallback provider by operator request",
			"tenant_id", scope.TenantID)
		return policy.Evaluate(policy.RoutingRequest{Gates: gates}), nil
	}

	// Without a usable Drive connection there is nothing to preflight. The gates
	// already carry the reason, so the policy rejects without a provider call.
	if connectionID == "" || !connectionUsable {
		return policy.Evaluate(policy.RoutingRequest{
			Gates:       gates,
			CircuitOpen: false,
		}), nil
	}

	scopeInfo := model.AvailabilityScope{
		TenantID:          scope.TenantID,
		UserID:            scope.UserID,
		DriveConnectionID: connectionID,
		EgressContext:     "default",
	}

	preflightErr, err := s.runPreflight(ctx, scopeInfo)
	if err != nil {
		return policy.RoutingDecision{}, apperr.Wrap(err, apperr.KindInternal,
			"Internal error", "could not evaluate Drive availability")
	}

	decision := policy.Evaluate(policy.RoutingRequest{
		PreflightError: preflightErr,
		PreflightPhase: policy.PhasePreflight,
		Gates:          gates,
	})

	s.deps.Logger.InfoContext(ctx, "routing decision",
		"decision", string(decision.Decision),
		"reason", decision.Reason,
		"selected_provider", decision.Provider,
		"drive_available", preflightErr == nil,
		"eligible_for_fallback", decision.EligibleForFallback,
	)
	return decision, nil
}

// runPreflight performs the bounded, read-only Drive availability check.
//
// The coordinator owns the total budget and constrains each attempt to fit inside
// it. The check is read-only, so no byte can have been committed and a timeout
// here is genuine unavailability rather than the ambiguous case.
func (s *UploadService) runPreflight(ctx context.Context, scopeInfo model.AvailabilityScope) (*policy.ProviderError, error) {
	cfg := s.deps.DriveCfg
	budget := policy.RetryBudget{
		Total:       cfg.RoutingBudget.Duration(),
		MaxAttempts: cfg.PreflightAttempts,
	}

	var lastErr *policy.ProviderError
	for attempt := 0; ; attempt++ {
		remaining, allowed := budget.Allow(cfg.PreflightTimeout.Duration())
		if !allowed {
			break
		}

		callCtx, cancel := context.WithTimeout(ctx, minDuration(remaining, cfg.PreflightTimeout.Duration()))
		start := s.deps.Now()
		err := s.deps.Drive.Availability(callCtx, scopeInfo)
		budget = budget.Consume(s.deps.Now().Sub(start))
		cancel()

		if err == nil {
			return nil, nil
		}

		var providerErr *policy.ProviderError
		if !errors.As(err, &providerErr) {
			// A Drive adapter that returns an unclassified error has a bug. It is
			// treated as unknown rather than as an outage, so the fallback stays
			// closed.
			providerErr = &policy.ProviderError{
				Provider:   "google_drive",
				Operation:  "Availability",
				Class:      policy.ClassUnknown,
				SafeDetail: "the Drive adapter returned an unnormalized error",
				Err:        err,
			}
		}
		lastErr = providerErr

		// Only an eligible availability failure is worth retrying. Retrying a
		// quota or auth error wastes the routing budget and delays the real answer.
		if providerErr.Class != policy.ClassUnavailable {
			return providerErr, nil
		}
		if budget.Exhausted() {
			break
		}

		delay := policy.Backoff(attempt, 250*time.Millisecond, 2*time.Second, s.deps.Jitter())
		if remaining, allowed := budget.Allow(delay); allowed && delay < remaining {
			if err := s.deps.Sleep(ctx, delay); err != nil {
				break
			}
		} else {
			break
		}
	}

	if lastErr == nil {
		lastErr = &policy.ProviderError{
			Provider:   "google_drive",
			Operation:  "Availability",
			Class:      policy.ClassUnavailable,
			SafeDetail: "the Drive preflight budget was exhausted before any call completed",
		}
	}
	return lastErr, nil
}

// fallbackConfigured reports whether an enabled fallback backend exists for the
// tenant. It never checks credentials, only that one is configured.
func (s *UploadService) fallbackConfigured(ctx context.Context, scope persistence.Scope) bool {
	if !s.deps.Fallback.Enabled {
		return false
	}
	if len(s.deps.Fallback.ActiveBackends()) == 0 {
		return false
	}
	_, err := s.deps.Backends.GetActive(ctx, scope.TenantID)
	return err == nil
}

// plannedIdentity generates the provider-side identity for an object.
//
// It is derived from the opaque object ID and the attempt generation, so a client
// can neither choose the key nor collide with another tenant's namespace.
func (s *UploadService) plannedIdentity(object model.LogicalObject, provider model.Provider) string {
	if provider == model.ProviderS3Compatible {
		return fmt.Sprintf("storvia/tenants/%s/objects/%s", object.TenantID, object.ID)
	}
	// Drive file IDs are assigned by Google; Storvia tracks the intended file by
	// name inside its folder, which is why the folder ID is stored separately.
	return object.ID
}

// SortedMIMETypes returns the allow-list for diagnostics.
// AllowedCategories returns the categories this deployment accepts.
func (s *UploadService) AllowedCategories() []string {
	return append([]string(nil), s.deps.UploadLimits.AllowedCategories...)
}

func (s *UploadService) SortedMIMETypes() []string {
	out := append([]string(nil), s.deps.UploadLimits.AllowedMIMETypes...)
	sort.Strings(out)
	return out
}

// sleepContext waits for d or until ctx is done.
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if a <= 0 {
		return b
	}
	if b <= 0 {
		return a
	}
	if a < b {
		return a
	}
	return b
}

func mustID(gen *idid.Generator) string {
	id, err := gen.New()
	if err != nil {
		return ""
	}
	return id
}

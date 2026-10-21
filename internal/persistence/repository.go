package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shovan04/storvia-project/internal/storage/model"
)

// Scope is the authenticated tenant/user pair every repository call requires.
//
// Repositories accept a Scope rather than a tenant_id string. That signature is
// the mechanism behind the isolation requirement: a query without a verified scope
// does not compile, and the scope always appears in the WHERE clause alongside
// the row identifier.
type Scope struct {
	TenantID string
	UserID   string
}

// Valid reports whether the scope is usable.
func (s Scope) Valid() bool { return s.TenantID != "" && s.UserID != "" }

// ObjectRepository reads and writes logical objects.
type ObjectRepository struct{ store *Store }

// NewObjectRepository builds a repository.
func NewObjectRepository(store *Store) *ObjectRepository {
	return &ObjectRepository{store: store}
}

// Create inserts a new logical object.
func (r *ObjectRepository) Create(ctx context.Context, scope Scope, object model.LogicalObject) error {
	query := r.store.Rebind(`INSERT INTO storage_objects (
		id, tenant_id, user_id, logical_key, original_name, mime_type, category,
		expected_size, verified_size, checksum_algorithm, verified_checksum,
		provider, drive_connection_id, backend_id, provider_file_id, provider_object_key,
		status, fallback_reason, selected_at, verified_at, row_version, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)

	now := time.Now().UTC()
	_, err := r.store.db.ExecContext(ctx, query,
		object.ID, scope.TenantID, scope.UserID, object.LogicalKey, object.OriginalName,
		object.MIMEType, object.Category, object.ExpectedSize, object.VerifiedSize,
		nullable(object.ChecksumAlgorithm), nullable(object.VerifiedChecksum),
		nullable(string(object.Provider)), object.DriveConnectionID, object.BackendID,
		object.ProviderFileID, object.ProviderObjectKey, string(object.Status),
		nullable(string(object.FallbackReason)), object.SelectedAt, object.VerifiedAt,
		1, now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("persistence: create object: %w", err)
	}
	return nil
}

// GetByLogicalKey returns the existing object for a logical key, if any.
func (r *ObjectRepository) GetByLogicalKey(ctx context.Context, scope Scope, logicalKey string) (model.LogicalObject, error) {
	query := r.store.Rebind(`SELECT ` + objectColumns + ` FROM storage_objects
		WHERE tenant_id = ? AND user_id = ? AND logical_key = ?`)
	return r.scanOne(ctx, query, scope.TenantID, scope.UserID, logicalKey)
}

// Get returns an object by its opaque identifier, scoped to the tenant.
//
// The tenant predicate is not optional. A caller holding a valid object ID for a
// different tenant gets ErrNotFound, not the row, which is what turns an IDOR
// attempt into a 404 rather than a data leak.
func (r *ObjectRepository) Get(ctx context.Context, scope Scope, objectID string) (model.LogicalObject, error) {
	query := r.store.Rebind(`SELECT ` + objectColumns + ` FROM storage_objects
		WHERE id = ? AND tenant_id = ? AND user_id = ?`)
	return r.scanOne(ctx, query, objectID, scope.TenantID, scope.UserID)
}

const objectColumns = `id, tenant_id, user_id, logical_key, original_name, mime_type, category,
	expected_size, verified_size, checksum_algorithm, verified_checksum,
	provider, drive_connection_id, backend_id, provider_file_id, provider_object_key,
	status, fallback_reason, selected_at, verified_at, row_version, created_at, updated_at, deleted_at`

func (r *ObjectRepository) scanOne(ctx context.Context, query string, args ...any) (model.LogicalObject, error) {
	row := r.store.db.QueryRowContext(ctx, query, args...)

	var (
		object      model.LogicalObject
		provider    sql.NullString
		checksumAlg sql.NullString
		checksum    sql.NullString
		fallbackWhy sql.NullString
		deletedAt   sql.NullTime
		selectedAt  sql.NullTime
		verifiedAt  sql.NullTime
	)

	err := row.Scan(
		&object.ID, &object.TenantID, &object.UserID, &object.LogicalKey, &object.OriginalName,
		&object.MIMEType, &object.Category, &object.ExpectedSize, &object.VerifiedSize,
		&checksumAlg, &checksum, &provider, &object.DriveConnectionID, &object.BackendID,
		&object.ProviderFileID, &object.ProviderObjectKey, &object.Status, &fallbackWhy,
		&selectedAt, &verifiedAt, &object.RowVersion, &object.CreatedAt, &object.UpdatedAt, &deletedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return model.LogicalObject{}, ErrNotFound
	}
	if err != nil {
		return model.LogicalObject{}, fmt.Errorf("persistence: scan object: %w", err)
	}

	object.ChecksumAlgorithm = checksumAlg.String
	object.VerifiedChecksum = checksum.String
	object.FallbackReason = model.FallbackReason(fallbackWhy.String)
	object.Provider = model.Provider(provider.String)
	if selectedAt.Valid {
		value := selectedAt.Time
		object.SelectedAt = &value
	}
	if verifiedAt.Valid {
		value := verifiedAt.Time
		object.VerifiedAt = &value
	}
	if deletedAt.Valid {
		value := deletedAt.Time
		object.DeletedAt = &value
	}
	return object, nil
}

// SelectLocation records the chosen provider and its planned identity.
//
// It runs before the first provider mutation. Once written, the row is the
// routing evidence that the provider was chosen deliberately, which is what makes
// the pin auditable after an ambiguous write.
func (r *ObjectRepository) SelectLocation(ctx context.Context, scope Scope, objectID string, expectedRowVersion int64, provider model.Provider, connectionID, backendID *string, identity string, reason model.FallbackReason) error {
	query := r.store.Rebind(`UPDATE storage_objects SET
		provider = ?, drive_connection_id = ?, backend_id = ?,
		provider_file_id = CASE WHEN ? = 'google_drive' THEN ? ELSE provider_file_id END,
		provider_object_key = CASE WHEN ? = 's3_compatible' THEN ? ELSE provider_object_key END,
		fallback_reason = ?, selected_at = ?, status = 'uploading', row_version = row_version + 1, updated_at = ?
	WHERE id = ? AND tenant_id = ? AND user_id = ? AND row_version = ?`)

	now := time.Now().UTC()
	result, err := r.store.db.ExecContext(ctx, query,
		string(provider), connectionID, backendID,
		string(provider), identity, string(provider), identity,
		nullable(string(reason)), now, now,
		objectID, scope.TenantID, scope.UserID, expectedRowVersion)
	if err != nil {
		return fmt.Errorf("persistence: select location: %w", err)
	}
	return checkAffected(result, "select location")
}

// CommitReady records the single verified provider location.
//
// The WHERE clause requires the object's current row version and excludes rows
// that already carry a different provider location, so a late worker cannot
// overwrite one location with another.
func (r *ObjectRepository) CommitReady(ctx context.Context, scope Scope, objectID string, expectedRowVersion int64, provider model.Provider, connectionID, backendID *string, fileID, objectKey string, verifiedSize int64, checksumAlg, checksum string) error {
	// The guards below stop a late worker from replacing one provider location
	// with a different one. The provider must not have changed, and the mutual
	// exclusion between a Drive pointer and an S3 key is enforced by the table's
	// CHECK constraints.
	//
	// Note what is deliberately NOT compared: the previously stored Drive file ID.
	// SelectLocation stores a planned identity there, and Drive assigns the real file
	// ID on completion, so comparing them would reject every genuine Drive upload.
	// Overwriting the pointer is safe because the provider is unchanged, the row is
	// tenant-scoped, and the single-location CHECKs still hold.
	query := r.store.Rebind(`UPDATE storage_objects SET
		provider = ?, drive_connection_id = ?, backend_id = ?,
		provider_file_id = ?, provider_object_key = ?,
		verified_size = ?, checksum_algorithm = ?, verified_checksum = ?,
		status = 'ready', verified_at = ?, row_version = row_version + 1, updated_at = ?
	WHERE id = ? AND tenant_id = ? AND user_id = ? AND row_version = ? AND provider = ?`)

	now := time.Now().UTC()
	result, err := r.store.db.ExecContext(ctx, query,
		string(provider), connectionID, backendID, nullablePtr(fileID), nullablePtr(objectKey),
		verifiedSize, nullable(checksumAlg), nullable(checksum), now, now,
		objectID, scope.TenantID, scope.UserID, expectedRowVersion, string(provider))
	if err != nil {
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("persistence: commit ready: %w", err)
	}
	return checkAffected(result, "commit ready")
}

// UpdateStatus moves an object to a new lifecycle status.
func (r *ObjectRepository) UpdateStatus(ctx context.Context, scope Scope, objectID string, expectedRowVersion int64, status model.ObjectStatus) error {
	// deleted_at is stamped here rather than left for the reader to infer from
	// status. A soft delete that records only a status is indistinguishable from a
	// live row to every query that does not happen to repeat the check -- which is
	// how the console came to list deleted objects and diagnostics came to count
	// them as stored.
	//
	// `missing` is stamped too, because both it and `deleted` mean the object is no
	// longer part of the stored set: `missing` is what a delete records when the
	// provider says the file is already absent. They stay distinct in the row, so
	// the difference is still there to read, but neither is a stored object.
	//
	// It is deliberately not cleared on any other transition. `deleting` is not a
	// deletion, and a row that was deleted stays deleted.
	deletedAt := "NULL"
	if status == model.ObjectDeleted || status == model.ObjectMissing {
		deletedAt = "?"
	}

	args := []any{string(status), time.Now().UTC()}
	if deletedAt != "NULL" {
		args = append(args, time.Now().UTC())
	}
	args = append(args, objectID, scope.TenantID, scope.UserID, expectedRowVersion)

	query := r.store.Rebind(`UPDATE storage_objects SET status = ?, deleted_at = ` + deletedAt + `,
		row_version = row_version + 1, updated_at = ?
	WHERE id = ? AND tenant_id = ? AND user_id = ? AND row_version = ?`)
	result, err := r.store.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("persistence: update object status: %w", err)
	}
	return checkAffected(result, "update object status")
}

// UploadRepository reads and writes logical uploads.
type UploadRepository struct{ store *Store }

// NewUploadRepository builds a repository.
func NewUploadRepository(store *Store) *UploadRepository {
	return &UploadRepository{store: store}
}

// Create inserts an upload. A duplicate idempotency key returns ErrConflict,
// which the service resolves by returning the existing upload.
func (r *UploadRepository) Create(ctx context.Context, scope Scope, upload model.Upload) error {
	query := r.store.Rebind(`INSERT INTO storage_uploads (
		id, object_id, tenant_id, user_id, idempotency_key, request_fingerprint,
		active_attempt_id, state, row_version, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)

	now := time.Now().UTC()
	_, err := r.store.db.ExecContext(ctx, query,
		upload.ID, upload.ObjectID, scope.TenantID, scope.UserID,
		upload.IdempotencyKey, upload.RequestFingerprint, upload.ActiveAttemptID,
		string(upload.State), 1, now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("persistence: create upload: %w", err)
	}
	return nil
}

const uploadColumns = `id, object_id, tenant_id, user_id, idempotency_key, request_fingerprint,
	active_attempt_id, state, row_version, created_at, updated_at`

// GetByIdempotencyKey returns the upload bound to a key within the scope.
func (r *UploadRepository) GetByIdempotencyKey(ctx context.Context, scope Scope, key string) (model.Upload, error) {
	query := r.store.Rebind(`SELECT ` + uploadColumns + ` FROM storage_uploads
		WHERE tenant_id = ? AND user_id = ? AND idempotency_key = ?`)
	return r.scanOne(ctx, query, scope.TenantID, scope.UserID, key)
}

// Get returns an upload by identifier, scoped to the tenant.
func (r *UploadRepository) Get(ctx context.Context, scope Scope, uploadID string) (model.Upload, error) {
	query := r.store.Rebind(`SELECT ` + uploadColumns + ` FROM storage_uploads
		WHERE id = ? AND tenant_id = ? AND user_id = ?`)
	return r.scanOne(ctx, query, uploadID, scope.TenantID, scope.UserID)
}

func (r *UploadRepository) scanOne(ctx context.Context, query string, args ...any) (model.Upload, error) {
	row := r.store.db.QueryRowContext(ctx, query, args...)
	var upload model.Upload
	err := row.Scan(&upload.ID, &upload.ObjectID, &upload.TenantID, &upload.UserID,
		&upload.IdempotencyKey, &upload.RequestFingerprint, &upload.ActiveAttemptID,
		&upload.State, &upload.RowVersion, &upload.CreatedAt, &upload.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Upload{}, ErrNotFound
	}
	if err != nil {
		return model.Upload{}, fmt.Errorf("persistence: scan upload: %w", err)
	}
	return upload, nil
}

// AttemptRepository reads and writes provider upload attempts.
type AttemptRepository struct{ store *Store }

// NewAttemptRepository builds a repository.
func NewAttemptRepository(store *Store) *AttemptRepository {
	return &AttemptRepository{store: store}
}

// Create inserts an attempt.
func (r *AttemptRepository) Create(ctx context.Context, attempt model.UploadAttempt) error {
	query := r.store.Rebind(`INSERT INTO upload_attempts (
		id, upload_id, provider, drive_connection_id, backend_id,
		planned_provider_identity, encrypted_session_uri, multipart_upload_id, state,
		mutation_may_have_started, confirmed_offset, expected_size, parts_metadata,
		last_sanitized_error, operation_generation, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)

	now := time.Now().UTC()
	_, err := r.store.db.ExecContext(ctx, query,
		attempt.ID, attempt.UploadID, string(attempt.Provider), attempt.DriveConnectionID,
		attempt.BackendID, attempt.PlannedProviderIdentity, attempt.EncryptedSessionURI,
		attempt.MultipartUploadID, string(attempt.State),
		r.store.BoolValue(attempt.MutationMayHaveStarted), attempt.ConfirmedOffset,
		attempt.ExpectedSize, nullable(string(attempt.PartsMetadata)), attempt.LastSanitizedError,
		attempt.OperationGeneration, now, now)
	if err != nil {
		return fmt.Errorf("persistence: create attempt: %w", err)
	}
	return nil
}

const attemptColumns = `id, upload_id, provider, drive_connection_id, backend_id,
	planned_provider_identity, encrypted_session_uri, multipart_upload_id, state,
	mutation_may_have_started, confirmed_offset, expected_size, parts_metadata,
	last_sanitized_error, operation_generation, lease_owner, lease_expires_at, created_at, updated_at`

// Get returns an attempt by identifier.
func (r *AttemptRepository) Get(ctx context.Context, attemptID string) (model.UploadAttempt, error) {
	return r.scanOne(ctx, "SELECT "+attemptColumns+" FROM upload_attempts WHERE id = ?", attemptID)
}

// GetActiveForUpload returns the attempt currently resolving an upload, if any.
//
// The filter on unresolved states is what prevents a second attempt from being
// started while the first is still ambiguous.
func (r *AttemptRepository) GetActiveForUpload(ctx context.Context, uploadID string) (model.UploadAttempt, error) {
	query := r.store.Rebind(`SELECT ` + attemptColumns + ` FROM upload_attempts
		WHERE upload_id = ? AND state IN ('selected','initiating','active','verifying','outcome_unknown')
		ORDER BY operation_generation DESC`)
	return r.scanOne(ctx, query, uploadID)
}

// ListForUpload returns every attempt for an upload, newest generation first.
func (r *AttemptRepository) ListForUpload(ctx context.Context, uploadID string) ([]model.UploadAttempt, error) {
	query := r.store.Rebind(`SELECT ` + attemptColumns + ` FROM upload_attempts
		WHERE upload_id = ? ORDER BY operation_generation DESC`)
	rows, err := r.store.db.QueryContext(ctx, query, uploadID)
	if err != nil {
		return nil, fmt.Errorf("persistence: list attempts: %w", err)
	}
	defer rows.Close()
	return r.scanMany(rows)
}

func (r *AttemptRepository) scanOne(ctx context.Context, query string, args ...any) (model.UploadAttempt, error) {
	rows, err := r.store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return model.UploadAttempt{}, fmt.Errorf("persistence: query attempt: %w", err)
	}
	defer rows.Close()

	attempts, err := r.scanMany(rows)
	if err != nil {
		return model.UploadAttempt{}, err
	}
	if len(attempts) == 0 {
		return model.UploadAttempt{}, ErrNotFound
	}
	return attempts[0], nil
}

func (r *AttemptRepository) scanMany(rows *sql.Rows) ([]model.UploadAttempt, error) {
	var out []model.UploadAttempt
	for rows.Next() {
		var (
			attempt      model.UploadAttempt
			provider     string
			state        string
			mutationFlag any
			partsMeta    sql.NullString
			sessionURI   []byte
			leaseOwner   sql.NullString
			leaseExpiry  sql.NullTime
		)
		if err := rows.Scan(&attempt.ID, &attempt.UploadID, &provider, &attempt.DriveConnectionID,
			&attempt.BackendID, &attempt.PlannedProviderIdentity, &sessionURI, &attempt.MultipartUploadID,
			&state, &mutationFlag, &attempt.ConfirmedOffset, &attempt.ExpectedSize, &partsMeta,
			&attempt.LastSanitizedError, &attempt.OperationGeneration, &leaseOwner, &leaseExpiry,
			&attempt.CreatedAt, &attempt.UpdatedAt); err != nil {
			return nil, fmt.Errorf("persistence: scan attempt: %w", err)
		}
		attempt.Provider = model.Provider(provider)
		attempt.State = model.AttemptState(state)
		attempt.PartsMetadata = []byte(partsMeta.String)
		attempt.MutationMayHaveStarted = coerceBool(mutationFlag)
		if len(sessionURI) > 0 {
			value := string(sessionURI)
			attempt.EncryptedSessionURI = &value
		}
		if leaseOwner.Valid {
			value := leaseOwner.String
			attempt.LeaseOwner = &value
		}
		if leaseExpiry.Valid {
			value := leaseExpiry.Time
			attempt.LeaseExpiresAt = &value
		}
		out = append(out, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("persistence: iterate attempts: %w", err)
	}
	return out, nil
}

// MarkMutationStarted flips the pin that forbids cross-provider failover.
//
// It only ever moves false to true. The WHERE clause requires the flag to still
// be false, so two concurrent initiations cannot both believe they own the right
// to proceed.
func (r *AttemptRepository) MarkMutationStarted(ctx context.Context, attemptID string, expectedSize int64) error {
	query := r.store.Rebind(`UPDATE upload_attempts SET
		mutation_may_have_started = ?, expected_size = ?, updated_at = ?
	WHERE id = ? AND mutation_may_have_started = ?`)
	result, err := r.store.db.ExecContext(ctx, query,
		r.store.BoolValue(true), expectedSize, time.Now().UTC(), attemptID, r.store.BoolValue(false))
	if err != nil {
		return fmt.Errorf("persistence: mark mutation started: %w", err)
	}
	return checkAffected(result, "mark mutation started")
}

// SetState moves an attempt to a new state.
func (r *AttemptRepository) SetState(ctx context.Context, attemptID string, state model.AttemptState, sanitizedError string) error {
	query := r.store.Rebind(`UPDATE upload_attempts SET
		state = ?, last_sanitized_error = ?, updated_at = ? WHERE id = ?`)
	_, err := r.store.db.ExecContext(ctx, query, string(state), nullable(sanitizedError),
		time.Now().UTC(), attemptID)
	if err != nil {
		return fmt.Errorf("persistence: set attempt state: %w", err)
	}
	return nil
}

// SetConfirmedOffset records provider-confirmed progress.
func (r *AttemptRepository) SetConfirmedOffset(ctx context.Context, attemptID string, offset int64) error {
	query := r.store.Rebind(`UPDATE upload_attempts SET confirmed_offset = ?, updated_at = ? WHERE id = ?`)
	_, err := r.store.db.ExecContext(ctx, query, offset, time.Now().UTC(), attemptID)
	if err != nil {
		return fmt.Errorf("persistence: set confirmed offset: %w", err)
	}
	return nil
}

// SetActiveAttempt binds an upload to its current attempt.
func (r *UploadRepository) SetActiveAttempt(ctx context.Context, scope Scope, uploadID string, attemptID string, state model.ObjectStatus) error {
	query := r.store.Rebind(`UPDATE storage_uploads SET
		active_attempt_id = ?, state = ?, row_version = row_version + 1, updated_at = ?
	WHERE id = ? AND tenant_id = ? AND user_id = ?`)
	result, err := r.store.db.ExecContext(ctx, query, attemptID, string(state),
		time.Now().UTC(), uploadID, scope.TenantID, scope.UserID)
	if err != nil {
		return fmt.Errorf("persistence: set active attempt: %w", err)
	}
	return checkAffected(result, "set active attempt")
}

// BackendRepository reads fallback backend configuration.
type BackendRepository struct{ store *Store }

// Backend is an S3-compatible destination record.
type Backend struct {
	ID           string
	Scope        string
	TenantID     *string
	Vendor       string
	Endpoint     string
	Region       string
	Bucket       string
	Prefix       string
	Status       string
	Capabilities string
}

// NewBackendRepository builds a repository.
func NewBackendRepository(store *Store) *BackendRepository { return &BackendRepository{store: store} }

// GetActive returns the active deployment-scoped fallback backend, if one exists.
//
// It returns ErrNotFound rather than a zero value, because "no backend" is the
// signal that stops new uploads from being routed to S3, not a configuration to
// work around.
func (r *BackendRepository) GetActive(ctx context.Context, tenantID string) (Backend, error) {
	query := r.store.Rebind(`SELECT id, scope, tenant_id, vendor, endpoint, region, bucket, prefix, status, capabilities
		FROM storage_backends
		WHERE status = 'active' AND (scope = 'deployment' OR tenant_id = ?)
		ORDER BY CASE WHEN scope = 'tenant' THEN 0 ELSE 1 END`)

	row := r.store.db.QueryRowContext(ctx, query, tenantID)
	var backend Backend
	var tenant sql.NullString
	err := row.Scan(&backend.ID, &backend.Scope, &tenant, &backend.Vendor, &backend.Endpoint,
		&backend.Region, &backend.Bucket, &backend.Prefix, &backend.Status, &backend.Capabilities)
	if errors.Is(err, sql.ErrNoRows) {
		return Backend{}, ErrNotFound
	}
	if err != nil {
		return Backend{}, fmt.Errorf("persistence: get backend: %w", err)
	}
	backend.TenantID = nullableString(tenant)
	return backend, nil
}

// Upsert records backend configuration.
func (r *BackendRepository) Upsert(ctx context.Context, backend Backend) error {
	query := r.store.Rebind(`INSERT INTO storage_backends (
		id, scope, tenant_id, provider, vendor, endpoint, region, bucket, prefix,
		credential_reference, status, capabilities, created_at, updated_at
	) VALUES (?, ?, ?, 's3_compatible', ?, ?, ?, ?, ?, 'operator-managed', ?, ?, ?, ?)
	ON CONFLICT (id) DO UPDATE SET
		vendor = ?, endpoint = ?, region = ?, bucket = ?, prefix = ?,
		status = ?, capabilities = ?, updated_at = ?`)

	now := time.Now().UTC()
	_, err := r.store.db.ExecContext(ctx, query,
		backend.ID, backend.Scope, backend.TenantID, backend.Vendor, backend.Endpoint,
		backend.Region, backend.Bucket, backend.Prefix, backend.Status, backend.Capabilities,
		now, now,
		backend.Vendor, backend.Endpoint, backend.Region, backend.Bucket, backend.Prefix,
		backend.Status, backend.Capabilities, now)
	if err != nil {
		return fmt.Errorf("persistence: upsert backend: %w", err)
	}
	return nil
}

// helper functions

// nullable renders the empty string as SQL NULL so optional columns stay NULL
// rather than becoming indistinguishable from a real empty value.
func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// nullablePtr renders an empty string as SQL NULL for pointer columns.
//
// Without this, writing "" into a provider pointer would make the column
// non-NULL-but-empty, which the single-location CHECK constraints correctly treat
// as a populated location of the wrong kind.
func nullablePtr(value string) *string {
	if value == "" {
		return nil
	}
	out := value
	return &out
}

func nullableString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	out := value.String
	return &out
}

// checkAffected converts a zero-row update into ErrConflict, which is how an
// optimistic row-version loss is surfaced.
func checkAffected(result sql.Result, operation string) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("persistence: %s: %w", operation, err)
	}
	if affected == 0 {
		return ErrConflict
	}
	return nil
}

// coerceBool reads a dialect boolean into a Go bool.
func coerceBool(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case int64:
		return v != 0
	case int:
		return v != 0
	case []byte:
		return len(v) > 0 && (v[0] == '1' || v[0] == 't' || v[0] == 'T')
	case string:
		return strings.EqualFold(v, "1") || strings.EqualFold(v, "t") || strings.EqualFold(v, "true")
	default:
		return false
	}
}

// isUniqueViolation reports whether err is a unique-constraint failure in either
// dialect. The message text is the only portable signal across both.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "unique constraint") ||
		strings.Contains(text, "duplicate key value") ||
		strings.Contains(text, "unique_violation")
}

// ExistsByProviderKey reports whether any object row claims a provider key.
//
// It exists for orphan detection. The sweep lists what is at a provider and asks this
// for each key; a false result is evidence for a person to look at, never permission to
// delete, because the commit for a good object may still be in flight.
func (r *ObjectRepository) ExistsByProviderKey(ctx context.Context, objectKey string) (bool, error) {
	if objectKey == "" {
		return false, nil
	}
	var found bool
	err := r.store.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM storage_objects WHERE provider_object_key = ?)`,
		objectKey).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("persistence: check provider key: %w", err)
	}
	return found, nil
}

// ListObjects returns an account's objects, newest first.
//
// It exists for the testing console and for the object API. The tenant scope is
// mandatory and is applied in the query rather than filtered afterwards: a listing
// that fetched rows across tenants and discarded some would still have read another
// tenant's rows.
func (r *ObjectRepository) ListObjects(ctx context.Context, scope Scope, limit int) ([]model.LogicalObject, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	rows, err := r.store.db.QueryContext(ctx, r.store.Rebind(`
		SELECT id, logical_key, original_name, mime_type, category, expected_size,
		       provider, status, provider_object_key, provider_file_id, backend_id,
		       created_at, updated_at
		FROM storage_objects
		WHERE tenant_id = ? AND deleted_at IS NULL
		ORDER BY created_at DESC, id DESC
		LIMIT ?`), scope.TenantID, limit)
	if err != nil {
		return nil, fmt.Errorf("persistence: list objects: %w", err)
	}
	defer rows.Close()

	var out []model.LogicalObject
	for rows.Next() {
		var (
			object   model.LogicalObject
			size     sql.NullInt64
			provider sql.NullString
			key      sql.NullString
			fileID   sql.NullString
			backend  sql.NullString
		)
		if err := rows.Scan(&object.ID, &object.LogicalKey, &object.OriginalName,
			&object.MIMEType, &object.Category, &size, &provider, &object.Status,
			&key, &fileID, &backend, &object.CreatedAt, &object.UpdatedAt); err != nil {
			return nil, fmt.Errorf("persistence: scan object: %w", err)
		}
		object.ExpectedSize = size.Int64
		object.Provider = model.Provider(provider.String)
		object.ProviderObjectKey = nullableString(key)
		object.ProviderFileID = nullableString(fileID)
		object.BackendID = nullableString(backend)
		out = append(out, object)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("persistence: list objects: %w", err)
	}
	return out, nil
}

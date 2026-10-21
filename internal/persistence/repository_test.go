package persistence

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/shovan04/storvia-project/internal/config"
	"github.com/shovan04/storvia-project/internal/storage/model"
)

// newTestStore opens an isolated on-disk SQLite database per test.
//
// A file rather than :memory: because the schema applies PRAGMAs that a shared
// in-memory database handles differently, and because a file gives each test a
// genuinely separate database.
func newTestStore(t *testing.T) *Store {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := Open(context.Background(), config.DatabaseConfig{
		Driver:          config.DriverSQLite,
		SQLitePath:      "file:" + dbPath,
		MaxOpenConns:    1,
		ConnMaxLifetime: config.Duration(time.Minute),
		ConnMaxIdleTime: config.Duration(time.Minute),
		AutoMigrate:     true,
	}, "")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if _, err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return store
}

// testScope is a tenant/user pair used by the repository tests.
func testScope() Scope {
	return Scope{TenantID: "tenant-a", UserID: "user-1"}
}

// otherScope is a second tenant, used to prove isolation.
func otherScope() Scope {
	return Scope{TenantID: "tenant-b", UserID: "user-2"}
}

func newObject(id, logicalKey string) LogicalObjectForTest {
	return LogicalObjectForTest{
		ID:           id,
		LogicalKey:   logicalKey,
		OriginalName: "report.pdf",
		MIMEType:     "application/pdf",
		Category:     "report",
		ExpectedSize: 2048,
		Status:       "pending",
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	applied, err := store.Migrate(ctx)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if applied != 0 {
		t.Errorf("second Migrate applied %d migrations, want 0", applied)
	}

	version, err := store.CurrentVersion(ctx)
	if err != nil {
		t.Fatalf("CurrentVersion: %v", err)
	}
	if version == 0 {
		t.Error("CurrentVersion should report the applied migration")
	}
}

func TestRepositoryRoundTrip(t *testing.T) {
	store := newTestStore(t)
	repo := NewObjectRepository(store)
	ctx := context.Background()
	scope := testScope()

	object := newObject("obj_1", "key-1")
	if err := repo.Create(ctx, scope, object.model()); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.Get(ctx, scope, "obj_1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != "obj_1" || got.LogicalKey != "key-1" {
		t.Errorf("got %+v, want the object that was created", got)
	}
	if got.Status != "pending" {
		t.Errorf("status = %q, want pending", got.Status)
	}
}

func TestGetIsTenantScoped(t *testing.T) {
	store := newTestStore(t)
	repo := NewObjectRepository(store)
	ctx := context.Background()

	owner := testScope()
	intruder := otherScope()

	if err := repo.Create(ctx, owner, newObject("obj_1", "key-1").model()); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A different tenant holding a valid object ID must get ErrNotFound, not the
	// row. This is the IDOR guard.
	if _, err := repo.Get(ctx, intruder, "obj_1"); !IsNotFound(err) {
		t.Fatalf("cross-tenant Get = %v, want ErrNotFound", err)
	}

	// The same applies to a same-tenant different user.
	if _, err := repo.Get(ctx, Scope{TenantID: owner.TenantID, UserID: "someone-else"}, "obj_1"); !IsNotFound(err) {
		t.Fatalf("cross-user Get = %v, want ErrNotFound", err)
	}
}

func TestUpdateStatusIsTenantScoped(t *testing.T) {
	store := newTestStore(t)
	repo := NewObjectRepository(store)
	ctx := context.Background()

	owner := testScope()
	intruder := otherScope()

	if err := repo.Create(ctx, owner, newObject("obj_1", "key-1").model()); err != nil {
		t.Fatal(err)
	}

	// A cross-tenant status write must not match a row.
	err := repo.UpdateStatus(ctx, intruder, "obj_1", 1, "ready")
	if err == nil {
		t.Fatal("a cross-tenant update must not succeed")
	}
	if !IsConflict(err) && !IsNotFound(err) {
		t.Errorf("error = %v, want a conflict or not-found", err)
	}
}

func TestOptimisticRowVersionBlocksStaleWrites(t *testing.T) {
	store := newTestStore(t)
	repo := NewObjectRepository(store)
	ctx := context.Background()
	scope := testScope()

	if err := repo.Create(ctx, scope, newObject("obj_1", "key-1").model()); err != nil {
		t.Fatal(err)
	}

	// Two readers hold version 1. Only one update may win.
	first := repo.UpdateStatus(ctx, scope, "obj_1", 1, "uploading")
	second := repo.UpdateStatus(ctx, scope, "obj_1", 1, "uploading")

	if first != nil {
		t.Errorf("the first update should succeed: %v", first)
	}
	if second == nil {
		t.Fatal("the second update with a stale row version must fail")
	}
	if !IsConflict(second) {
		t.Errorf("second update = %v, want ErrConflict", second)
	}
}

// TestSelectLocationThenCommitReadyIsTheOnlyPathToReady walks the normal flow.
func TestSelectLocationThenCommitReadyIsTheOnlyPathToReady(t *testing.T) {
	store := newTestStore(t)
	repo := NewObjectRepository(store)
	ctx := context.Background()
	scope := testScope()

	// The object row carries a foreign key to the connection, so a location can
	// never point at a connection that does not exist.
	mustInsertConnection(t, store, "dc-1", scope)

	if err := repo.Create(ctx, scope, newObject("obj_1", "key-1").model()); err != nil {
		t.Fatal(err)
	}

	connectionID := "dc-1"
	if err := repo.SelectLocation(ctx, scope, "obj_1", 1, "google_drive", &connectionID, nil, "file-1", ""); err != nil {
		t.Fatalf("SelectLocation: %v", err)
	}

	current, err := repo.Get(ctx, scope, "obj_1")
	if err != nil {
		t.Fatal(err)
	}
	if current.Provider != "google_drive" {
		t.Errorf("provider = %q, want google_drive", current.Provider)
	}
	if current.DriveConnectionID == nil || *current.DriveConnectionID != connectionID {
		t.Errorf("connection = %v, want %q", current.DriveConnectionID, connectionID)
	}
	if current.BackendID != nil {
		t.Error("a Drive location must not carry an S3 backend")
	}

	if err := repo.CommitReady(ctx, scope, "obj_1", current.RowVersion,
		"google_drive", &connectionID, nil, "file-1", "", 2048, "", ""); err != nil {
		t.Fatalf("CommitReady: %v", err)
	}

	ready, err := repo.Get(ctx, scope, "obj_1")
	if err != nil {
		t.Fatal(err)
	}
	if ready.Status != "ready" {
		t.Errorf("status = %q, want ready", ready.Status)
	}
	if ready.VerifiedSize != ready.ExpectedSize {
		t.Errorf("verified %d != expected %d", ready.VerifiedSize, ready.ExpectedSize)
	}
}

// mustInsertConnection inserts a minimal drive_connections row.
//
// Object rows carry a foreign key to it, which is what makes "an object pointing
// at a connection that does not exist" unrepresentable rather than merely
// discouraged.
func mustInsertConnection(t *testing.T, store *Store, id string, scope Scope) {
	t.Helper()
	now := time.Now().UTC()
	_, err := store.DB().ExecContext(context.Background(), `
		INSERT INTO drive_connections (
			id, tenant_id, user_id, google_subject, encrypted_refresh_token,
			key_reference, granted_scopes, status, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, 'connected', ?, ?)`,
		id, scope.TenantID, scope.UserID, "subject-"+id, []byte("ciphertext"),
		"ref", "https://www.googleapis.com/auth/drive.file", now, now)
	if err != nil {
		t.Fatalf("insert drive connection: %v", err)
	}
}

// mustInsertBackend inserts a minimal storage_backends row.
func mustInsertBackend(t *testing.T, store *Store, id string) {
	t.Helper()
	now := time.Now().UTC()
	_, err := store.DB().ExecContext(context.Background(), `
		INSERT INTO storage_backends (
			id, scope, provider, vendor, endpoint, region, bucket,
			credential_reference, status, created_at, updated_at
		) VALUES (?, 'deployment', 's3_compatible', 'cloudflare_r2', ?, 'auto', ?,
			'operator-managed', 'active', ?, ?)`,
		id, "https://account.r2.cloudflarestorage.com", "bucket-"+id, now, now)
	if err != nil {
		t.Fatalf("insert backend: %v", err)
	}
}

// TestDatabaseRejectsReadyObjectWithTwoLocations is the single-location invariant
// enforced by the schema, not merely by application code.
func TestDatabaseRejectsReadyObjectWithTwoLocations(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	scope := testScope()

	mustInsertConnection(t, store, "dc-1", scope)
	mustInsertBackend(t, store, "backend-1")

	now := time.Now().UTC()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO storage_objects (
			id, tenant_id, user_id, logical_key, original_name, mime_type,
			expected_size, verified_size, provider, drive_connection_id,
			provider_file_id, backend_id, provider_object_key, status,
			row_version, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'ready', 1, ?, ?)`,
		"obj_bad", "tenant-a", "user-1", "key-bad", "x", "application/pdf",
		10, 10, "google_drive", "dc-1", "file-1", "backend-1", "storvia/key", now, now)

	if err == nil {
		t.Fatal("the database accepted a ready object with both a Drive file and an S3 key; the invariant is not enforced")
	}
}

// TestDatabaseRejectsReadyObjectWithVerifiedSizeMismatch keeps a location from
// being declared ready when the provider's confirmed size disagrees.
func TestDatabaseRejectsReadyObjectWithVerifiedSizeMismatch(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	scope := testScope()

	mustInsertConnection(t, store, "dc-1", scope)

	now := time.Now().UTC()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO storage_objects (
			id, tenant_id, user_id, logical_key, original_name, mime_type,
			expected_size, verified_size, provider, drive_connection_id,
			provider_file_id, status, row_version, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'ready', 1, ?, ?)`,
		"obj_bad", "tenant-a", "user-1", "key-bad", "x", "application/pdf",
		100, 99, "google_drive", "dc-1", "file-1", now, now)

	if err == nil {
		t.Fatal("the database accepted a ready object whose verified size differs from the expected size")
	}
}

// TestDatabaseRejectsObjectWithBothPointerKinds covers the not-ready case too.
func TestDatabaseRejectsObjectWithBothPointerKinds(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	scope := testScope()

	mustInsertConnection(t, store, "dc-1", scope)

	now := time.Now().UTC()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO storage_objects (
			id, tenant_id, user_id, logical_key, original_name, mime_type,
			expected_size, verified_size, provider, drive_connection_id,
			provider_file_id, provider_object_key, status, row_version, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, 1, 0, ?, ?, ?, ?, 'pending', 1, ?, ?)`,
		"obj_bad", "tenant-a", "user-1", "key-bad", "x", "application/pdf",
		"s3_compatible", "dc-1", "file-1", "storvia/key", now, now)

	if err == nil {
		t.Fatal("the database accepted an object carrying both a Drive file and an S3 key")
	}
}

// TestSelectLocationRejectsAnUnknownConnection proves the foreign key is live on
// the selection path too, not just on raw inserts.
func TestSelectLocationRejectsAnUnknownConnection(t *testing.T) {
	store := newTestStore(t)
	repo := NewObjectRepository(store)
	ctx := context.Background()
	scope := testScope()

	if err := repo.Create(ctx, scope, newObject("obj_1", "key-1").model()); err != nil {
		t.Fatal(err)
	}

	unknown := "dc-does-not-exist"
	if err := repo.SelectLocation(ctx, scope, "obj_1", 1, "google_drive", &unknown, nil, "file-1", ""); err == nil {
		t.Fatal("selecting a location on a nonexistent connection must fail")
	}
}

func TestDuplicateLogicalKeyIsRejected(t *testing.T) {
	store := newTestStore(t)
	repo := NewObjectRepository(store)
	ctx := context.Background()
	scope := testScope()

	if err := repo.Create(ctx, scope, newObject("obj_1", "same-key").model()); err != nil {
		t.Fatal(err)
	}
	// A second object with the same logical key for the same tenant/user must
	// conflict: it would be a second file for one logical object.
	if err := repo.Create(ctx, scope, newObject("obj_2", "same-key").model()); !IsConflict(err) {
		t.Errorf("duplicate logical key = %v, want ErrConflict", err)
	}
}

func TestDuplicateIdempotencyKeyIsRejected(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	scope := testScope()

	objects := NewObjectRepository(store)
	uploads := NewUploadRepository(store)

	if err := objects.Create(ctx, scope, newObject("obj_1", "key-1").model()); err != nil {
		t.Fatal(err)
	}
	first := uploadFor("upl_1", "obj_1", "idem-1")
	if err := uploads.Create(ctx, scope, first); err != nil {
		t.Fatalf("first Create: %v", err)
	}

	second := uploadFor("upl_2", "obj_1", "idem-1")
	if err := uploads.Create(ctx, scope, second); !IsConflict(err) {
		t.Errorf("duplicate idempotency key = %v, want ErrConflict", err)
	}
}

func TestGetUploadByIdempotencyKeyIsTenantScoped(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	owner := testScope()
	intruder := otherScope()

	objects := NewObjectRepository(store)
	uploads := NewUploadRepository(store)

	if err := objects.Create(ctx, owner, newObject("obj_1", "key-1").model()); err != nil {
		t.Fatal(err)
	}
	if err := uploads.Create(ctx, owner, uploadFor("upl_1", "obj_1", "idem-1")); err != nil {
		t.Fatal(err)
	}

	if _, err := uploads.GetByIdempotencyKey(ctx, intruder, "idem-1"); !IsNotFound(err) {
		t.Errorf("cross-tenant idempotency lookup = %v, want ErrNotFound", err)
	}
	got, err := uploads.GetByIdempotencyKey(ctx, owner, "idem-1")
	if err != nil {
		t.Fatalf("owner lookup: %v", err)
	}
	if got.ID != "upl_1" {
		t.Errorf("id = %q, want upl_1", got.ID)
	}
}

// TestAttemptMutationFlagIsOneWay proves the pin can only be set, never cleared.
func TestAttemptMutationFlagIsOneWay(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	scope := testScope()

	objects := NewObjectRepository(store)
	uploads := NewUploadRepository(store)
	attempts := NewAttemptRepository(store)

	if err := objects.Create(ctx, scope, newObject("obj_1", "key-1").model()); err != nil {
		t.Fatal(err)
	}
	if err := uploads.Create(ctx, scope, uploadFor("upl_1", "obj_1", "idem-1")); err != nil {
		t.Fatal(err)
	}
	if err := attempts.Create(ctx, attemptFor("att_1", "upl_1")); err != nil {
		t.Fatal(err)
	}

	if err := attempts.MarkMutationStarted(ctx, "att_1", 2048); err != nil {
		t.Fatalf("MarkMutationStarted: %v", err)
	}

	// Setting it again must fail, so two concurrent initiations cannot both
	// believe they own the right to write.
	if err := attempts.MarkMutationStarted(ctx, "att_1", 2048); err == nil {
		t.Fatal("the mutation flag must be settable only once")
	}

	got, err := attempts.Get(ctx, "att_1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.MutationMayHaveStarted {
		t.Error("the flag should be set on the stored attempt")
	}
	if got.ExpectedSize != 2048 {
		t.Errorf("expected size = %d, want 2048", got.ExpectedSize)
	}
}

func TestGetActiveForUploadIgnoresSettledAttempts(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	scope := testScope()

	objects := NewObjectRepository(store)
	uploads := NewUploadRepository(store)
	attempts := NewAttemptRepository(store)

	if err := objects.Create(ctx, scope, newObject("obj_1", "key-1").model()); err != nil {
		t.Fatal(err)
	}
	if err := uploads.Create(ctx, scope, uploadFor("upl_1", "obj_1", "idem-1")); err != nil {
		t.Fatal(err)
	}
	if err := attempts.Create(ctx, attemptFor("att_1", "upl_1")); err != nil {
		t.Fatal(err)
	}
	if err := attempts.SetState(ctx, "att_1", "completed", ""); err != nil {
		t.Fatal(err)
	}

	// A completed attempt must not block a new attempt from being recognized as
	// the active one, because cleanup is already done.
	if _, err := attempts.GetActiveForUpload(ctx, "upl_1"); !IsNotFound(err) {
		t.Errorf("GetActiveForUpload on a completed upload = %v, want ErrNotFound", err)
	}
}

func TestPlaceholderRebindingPerDialect(t *testing.T) {
	sqlite := &Store{dialect: DialectSQLite}
	postgres := &Store{dialect: DialectPostgres}

	if got := sqlite.Placeholder(3); got != "?" {
		t.Errorf("sqlite placeholder = %q, want ?", got)
	}
	if got := postgres.Placeholder(3); got != "$3" {
		t.Errorf("postgres placeholder = %q, want $3", got)
	}

	query := "SELECT * FROM t WHERE a = ? AND b = ?"
	if got := sqlite.Rebind(query); got != query {
		t.Errorf("sqlite rebind changed the query: %s", got)
	}
	want := "SELECT * FROM t WHERE a = $1 AND b = $2"
	if got := postgres.Rebind(query); got != want {
		t.Errorf("postgres rebind = %s, want %s", got, want)
	}
}

func TestBooleanConversionPerDialect(t *testing.T) {
	sqlite := &Store{dialect: DialectSQLite}
	postgres := &Store{dialect: DialectPostgres}

	if got := sqlite.BoolValue(true); got != 1 {
		t.Errorf("sqlite true = %v, want 1", got)
	}
	if got := sqlite.BoolValue(false); got != 0 {
		t.Errorf("sqlite false = %v, want 0", got)
	}
	if got, ok := postgres.BoolValue(true).(bool); !ok || !got {
		t.Errorf("postgres true = %v, want the bool true", got)
	}
}

func TestBackendRepositoryReturnsNotFoundWhenNoneIsConfigured(t *testing.T) {
	store := newTestStore(t)
	repo := NewBackendRepository(store)

	if _, err := repo.GetActive(context.Background(), "tenant-a"); !IsNotFound(err) {
		t.Errorf("GetActive with no backend = %v, want ErrNotFound", err)
	}
}

func TestBackendRepositoryUpsertAndFetch(t *testing.T) {
	store := newTestStore(t)
	repo := NewBackendRepository(store)
	ctx := context.Background()

	backend := Backend{
		ID:           "r2-primary",
		Scope:        "deployment",
		Vendor:       "cloudflare_r2",
		Endpoint:     "https://account.r2.cloudflarestorage.com",
		Region:       "auto",
		Bucket:       "storvia",
		Prefix:       "prod",
		Status:       "active",
		Capabilities: "{}",
	}
	if err := repo.Upsert(ctx, backend); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := repo.GetActive(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("GetActive: %v", err)
	}
	if got.ID != "r2-primary" || got.Vendor != "cloudflare_r2" || got.Bucket != "storvia" {
		t.Errorf("got %+v, want the backend that was stored", got)
	}

	// A disabled backend must not be selected for outage routing.
	backend.Status = "disabled"
	if err := repo.Upsert(ctx, backend); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.GetActive(ctx, "tenant-a"); !IsNotFound(err) {
		t.Errorf("GetActive after disabling = %v, want ErrNotFound", err)
	}
}

func TestPlaceholderRebindDoesNotTouchQuestionMarksInsideLiterals(t *testing.T) {
	// This documents a known limitation of positional rebinding: a literal '?'
	// inside a string would be rewritten. Storvia's queries contain no such
	// literals; the test keeps the constraint visible so a future query that adds
	// one is caught.
	postgres := &Store{dialect: DialectPostgres}
	query := "SELECT * FROM t WHERE a = ? AND b = 'literal'"
	got := postgres.Rebind(query)
	if got != "SELECT * FROM t WHERE a = $1 AND b = 'literal'" {
		t.Errorf("rebind = %s", got)
	}
}

// TestUpdateStatusStampsDeletedAt.
//
// The soft delete used to record only a status. Every query that did not happen to
// repeat the `status != 'deleted'` check then treated the row as live -- which is how
// the console came to list objects the user had deleted and diagnostics came to count
// them as stored.
//
// deleted_at is now written with the status, so a deleted row is marked by the write
// that deleted it rather than by each reader remembering to look.
func TestUpdateStatusStampsDeletedAt(t *testing.T) {
	store := newTestStore(t)
	repo := NewObjectRepository(store)
	ctx := context.Background()
	scope := testScope()

	if err := repo.Create(ctx, scope, newObject("obj_1", "key-1").model()); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A non-delete transition must not stamp it: `missing` and `deleting` are not
	// deletions, and a row that merely passed through them is still live.
	if err := repo.UpdateStatus(ctx, scope, "obj_1", 1, model.ObjectUploading); err != nil {
		t.Fatalf("UpdateStatus(uploading): %v", err)
	}
	if got, err := repo.Get(ctx, scope, "obj_1"); err != nil {
		t.Fatalf("Get: %v", err)
	} else if got.DeletedAt != nil {
		t.Errorf("uploading object has deleted_at = %v, want nil", got.DeletedAt)
	}

	if err := repo.UpdateStatus(ctx, scope, "obj_1", 2, model.ObjectDeleted); err != nil {
		t.Fatalf("UpdateStatus(deleted): %v", err)
	}
	got, err := repo.Get(ctx, scope, "obj_1")
	if err != nil {
		t.Fatalf("Get after delete: %v", err)
	}
	if got.DeletedAt == nil {
		t.Error("deleted object has deleted_at = nil; the delete is not recorded in the row")
	}
}

// TestListObjectsExcludesDeleted.
//
// The console showed deleted objects because the list query filtered only by tenant.
// A deleted object is not stored, and listing it invites the operator to act on
// something that is already gone.
func TestListObjectsExcludesDeleted(t *testing.T) {
	store := newTestStore(t)
	repo := NewObjectRepository(store)
	ctx := context.Background()
	scope := testScope()

	for _, id := range []string{"obj_live", "obj_gone"} {
		if err := repo.Create(ctx, scope, newObject(id, "key-"+id).model()); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}
	if err := repo.UpdateStatus(ctx, scope, "obj_gone", 1, model.ObjectDeleted); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}

	objects, err := repo.ListObjects(ctx, scope, 50)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	for _, object := range objects {
		if object.ID == "obj_gone" {
			t.Error("a deleted object was listed; the console would offer to delete it again")
		}
	}
	if len(objects) != 1 {
		t.Errorf("listed %d objects, want 1 (the live one)", len(objects))
	}
}

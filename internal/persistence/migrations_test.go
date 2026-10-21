package persistence

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/shovan04/storvia-project/internal/config"
)

// TestBackfillMigrationStampsPreviouslyDeletedObjects.
//
// The read path filters on `deleted_at IS NULL`, and that filter is only honest if every
// removed object carries the marker. Objects deleted before UpdateStatus wrote it do not,
// so this migration is what makes the filter correct for existing data rather than only
// for rows created after the fix.
//
// The store is migrated up to version 3 first and the dead row is inserted afterwards, so
// the row genuinely predates the backfill. Inserting it after a full migration would test
// nothing.
func TestBackfillMigrationStampsPreviouslyDeletedObjects(t *testing.T) {
	store := openStoreAtVersion(t, 3)
	ctx := context.Background()

	// Two rows that were deleted the old way: terminal status, no marker. One of them is
	// `missing`, which also means "no longer in the stored set".
	for _, id := range []string{"obj_deleted", "obj_missing"} {
		exec(t, store, `
			INSERT INTO storage_objects
			 (id, tenant_id, user_id, logical_key, original_name, mime_type, category,
			  expected_size, status, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, 'text/plain', 'document', 10, ?, ?, ?)`,
			id, testScope().TenantID, testScope().UserID, id, id+".txt",
			idStatusFor(id), time.Now().UTC(), time.Now().UTC())
	}

	// A live row must be left alone. Without this assertion a migration that stamped
	// everything would pass the test above and destroy the stored set. It is 'pending'
	// rather than 'ready' only to keep the fixture clear of the provider-location CHECK;
	// the backfill keys on status, so any non-terminal status proves the same thing.
	exec(t, store, `
		INSERT INTO storage_objects
		 (id, tenant_id, user_id, logical_key, original_name, mime_type, category,
		  expected_size, status, created_at, updated_at)
		VALUES ('obj_live', ?, ?, 'obj_live', 'live.txt', 'text/plain', 'document',
		        10, 'pending', ?, ?)`,
		testScope().TenantID, testScope().UserID, time.Now().UTC(), time.Now().UTC())

	if stamped := countStamped(t, store); stamped != 0 {
		t.Fatalf("the fixture already has %d stamped rows; it is not testing the migration", stamped)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	if stamped := countStamped(t, store); stamped != 2 {
		t.Errorf("%d rows stamped deleted_at after the backfill, want 2", stamped)
	}

	// And the live object is still in the stored set.
	var deletedAt any
	scanOne(t, store, `SELECT deleted_at FROM storage_objects WHERE id = 'obj_live'`, &deletedAt)
	if deletedAt != nil {
		t.Errorf("the backfill stamped a live object: deleted_at = %v", deletedAt)
	}

	listed, err := NewObjectRepository(store).ListObjects(ctx, testScope(), 50)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != "obj_live" {
		t.Errorf("listed %+v, want only the live object", listed)
	}
}

// openStoreAtVersion migrates only up to and including the given version, so a row
// inserted afterwards is genuinely older than the migrations above it.
func openStoreAtVersion(t *testing.T, version int) *Store {
	t.Helper()

	store, err := Open(context.Background(), config.DatabaseConfig{
		Driver:          config.DriverSQLite,
		SQLitePath:      "file:" + filepath.Join(t.TempDir(), "backfill.db"),
		MaxOpenConns:    1,
		ConnMaxLifetime: config.Duration(time.Minute),
		ConnMaxIdleTime: config.Duration(time.Minute),
		AutoMigrate:     true,
	}, "")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := store.ensureMigrationTable(context.Background()); err != nil {
		t.Fatalf("ensureMigrationTable: %v", err)
	}

	migrations, err := LoadMigrations(DialectSQLite)
	if err != nil {
		t.Fatalf("LoadMigrations: %v", err)
	}
	for _, migration := range migrations {
		if migration.Version > version {
			continue
		}
		if err := store.applyMigration(context.Background(), migration); err != nil {
			t.Fatalf("apply %04d: %v", migration.Version, err)
		}
	}
	return store
}

func idStatusFor(id string) string {
	if id == "obj_missing" {
		return "missing"
	}
	return "deleted"
}

func countStamped(t *testing.T, store *Store) int {
	t.Helper()
	var count int
	scanOne(t, store,
		`SELECT COUNT(*) FROM storage_objects WHERE deleted_at IS NOT NULL`, &count)
	return count
}

func exec(t *testing.T, store *Store, query string, args ...any) {
	t.Helper()
	if _, err := store.db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("exec: %v", err)
	}
}

func scanOne(t *testing.T, store *Store, query string, dest ...any) {
	t.Helper()
	if err := store.db.QueryRowContext(context.Background(), query).Scan(dest...); err != nil {
		t.Fatalf("scan (%s): %v", query, err)
	}
}

package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shovan04/storvia/internal/config"
	"github.com/shovan04/storvia/internal/persistence"
)

// resolveDevDBPath points the loaded config at the repository's development database.
//
// The configured SQLite path is relative to the *process* working directory and `go test`
// runs from this package directory. Left alone, this file would open a different, empty
// database, find no connection, and skip — a green run that proves nothing. The same
// guard and the same reasoning are in TestLiveDefaultUploadGoesToDrive.
func resolveDevDBPath(t *testing.T, cfg *config.Config, repoRoot string) {
	t.Helper()
	if repoRoot == "" {
		return
	}
	configured := strings.TrimPrefix(cfg.Database.SQLitePath, "file:")
	if configured == "" || strings.Contains(configured, ":memory:") || filepath.IsAbs(configured) {
		return
	}
	absolute := filepath.Join(repoRoot, configured)
	if _, err := os.Stat(absolute); err != nil {
		t.Skipf("the development database is not at %s", absolute)
	}
	cfg.Database.SQLitePath = "file:" + absolute
}

// TestLiveWhereDoTheFilesActuallyLive answers an operator's question directly: they say
// they cannot see Storvia's files in their Drive app, so where are they?
//
// Read-only. It calls files.get and nothing else: no create, no update, no delete.
func TestLiveWhereDoTheFilesActuallyLive(t *testing.T) {
	requireLiveDrive(t)

	cfg := loadDevConfig(t)
	resolveDevDBPath(t, cfg, findRepoRoot())

	built := buildLiveApp(t, cfg)
	t.Cleanup(func() {
		built.StopBackground()
		_ = built.Cache.Close()
		_ = built.Store.Close()
	})

	scope := persistence.Scope{TenantID: "tenant_dev_admin", UserID: "user_dev_admin"}
	ctx := context.Background()

	var folderID string
	if err := built.Store.DB().QueryRowContext(ctx, `
		SELECT root_folder_id FROM drive_connections WHERE tenant_id = ? AND user_id = ?`,
		scope.TenantID, scope.UserID).Scan(&folderID); err != nil {
		t.Fatalf("read root_folder_id: %v", err)
	}
	t.Logf("STORVIA ROOT FOLDER ID: %s", folderID)

	folder, err := built.Drive.FetchFile(ctx, scope, folderID)
	if err != nil {
		t.Fatalf("read the Storvia folder back from Drive: %v", err)
	}
	t.Logf("FOLDER  name=%q mime=%q trashed=%v", folder.Name, folder.MimeType, folder.Trashed)
	t.Logf("FOLDER  parentID=%q  (empty means it sits in the account root)", folder.ParentID)
	t.Logf("FOLDER  url=%s", folder.WebViewURL)

	type stored struct{ name, fileID, status string }
	var items []stored

	rows, err := built.Store.DB().QueryContext(ctx, `
		SELECT original_name, provider_file_id, status
		  FROM storage_objects
		 WHERE provider_file_id IS NOT NULL
		   AND provider_file_id NOT LIKE 'upl_%'
		 ORDER BY created_at DESC`)
	if err != nil {
		t.Fatalf("list stored objects: %v", err)
	}
	// Drain fully and close before any Drive call. SQLite is opened with MaxOpenConns=1
	// (all open pools are, via the config), so holding a rows cursor while FetchFile asks
	// the same pool for a connection deadlocks: the cursor waits for nothing, and the
	// query waits for the cursor's connection. This exact mistake hung the first attempt
	// of this diagnostic for 90s.
	for rows.Next() {
		var it stored
		if err := rows.Scan(&it.name, &it.fileID, &it.status); err != nil {
			t.Fatalf("scan: %v", err)
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	rows.Close()

	for _, it := range items {
		meta, err := built.Drive.FetchFile(ctx, scope, it.fileID)
		if err != nil {
			t.Logf("FILE %-34s status=%-8s id=%s -> NOT READABLE: %v", it.name, it.status, it.fileID, err)
			continue
		}
		inFolder := "NOT in the Storvia folder"
		if meta.ParentID == folderID {
			inFolder = "inside the Storvia folder"
		}
		t.Logf("FILE %-34s status=%-8s id=%s", it.name, it.status, meta.ID)
		t.Logf("     drive name=%q trashed=%v %s", meta.Name, meta.Trashed, inFolder)
		t.Logf("     parentID=%q", meta.ParentID)
		t.Logf("     url=%s", meta.WebViewURL)
	}
}

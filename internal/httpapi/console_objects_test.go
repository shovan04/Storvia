package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shovan04/storvia-project/internal/auth"
	"github.com/shovan04/storvia-project/internal/config"
	"github.com/shovan04/storvia-project/internal/persistence"
	"github.com/shovan04/storvia-project/internal/storage/model"
	"github.com/shovan04/storvia-project/internal/storage/service"
)

// stubContentSource satisfies the object service's provider port without a provider.
//
// Nothing in this file reaches a network. An object with no provider reference is deleted
// locally, which is one behaviour under test; the other uses an object that has one, and
// this records the call instead of making it.
type stubContentSource struct{ deletes int }

func (s *stubContentSource) Open(context.Context, model.LogicalObject, *model.ByteRange) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (s *stubContentSource) Delete(context.Context, model.LogicalObject) error {
	s.deletes++
	return nil
}

// newObjectConsole wires a console whose object routes use a real store.
//
// The routes are the point: a handler that returns 200 while deleting nothing is exactly
// the failure this covers, and it cannot be caught below the HTTP layer.
func newObjectConsole(t *testing.T, objects *service.ObjectService, repo *persistence.ObjectRepository) *Console {
	t.Helper()

	console := newTestConsole(t, auth.NewMemorySessions(10), &stubRouting{})
	console.objects = objects
	console.objects_ = repo
	return console
}

func newConsoleStore(t *testing.T) (*persistence.ObjectRepository, *service.ObjectService) {
	t.Helper()

	store, err := persistence.Open(context.Background(), config.DatabaseConfig{
		Driver:          config.DriverSQLite,
		SQLitePath:      "file:" + filepath.Join(t.TempDir(), "objects.db"),
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

	now := time.Now().UTC()
	if _, err := store.DB().Exec(
		`INSERT INTO tenants (id, name, created_at, updated_at) VALUES (?,?,?,?)`,
		"tenant-dev", "tenant-dev", now, now); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	// A ready object must reference a connection, and the foreign key enforces it. That
	// is the constraint doing its job, so the fixture satisfies it rather than working
	// around it.
	if _, err := store.DB().Exec(
		`INSERT INTO drive_connections
		 (id, tenant_id, user_id, google_subject, encrypted_refresh_token,
		  key_reference, granted_scopes, root_folder_id, status, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		"dc-1", "tenant-dev", "user-1", "subject-1", []byte("unused"),
		"STORVIA_ENCRYPTION_KEY", "https://www.googleapis.com/auth/drive.file",
		"folder-1", "connected", now, now); err != nil {
		t.Fatalf("seed drive connection: %v", err)
	}

	repo := persistence.NewObjectRepository(store)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	objectService := service.NewObjectService(
		repo, persistence.NewUploadRepository(store), &stubContentSource{},
	).WithJobs(persistence.NewJobRepository(store), logger)
	return repo, objectService
}

// seedObject inserts one row directly. It is a fixture, not a shortcut through the
// pipeline: the states under test are ones the pipeline leaves behind, not ones it
// produces on a happy path.
func seedObject(t *testing.T, repo *persistence.ObjectRepository, object model.LogicalObject) {
	t.Helper()

	object.TenantID = "tenant-dev"
	object.UserID = "user-1"
	if object.LogicalKey == "" {
		object.LogicalKey = object.ID
	}
	if object.OriginalName == "" {
		object.OriginalName = object.ID + ".txt"
	}
	if object.MIMEType == "" {
		object.MIMEType = "text/plain"
	}
	if object.Category == "" {
		object.Category = "document"
	}
	if object.ExpectedSize == 0 {
		object.ExpectedSize = 12
	}
	// A ready object must have a verified size equal to the expected one; the database
	// enforces it, so the fixture records a completed upload rather than a half one.
	if object.Status == model.ObjectReady && object.VerifiedSize == 0 {
		object.VerifiedSize = object.ExpectedSize
	}
	if err := repo.Create(context.Background(),
		persistence.Scope{TenantID: object.TenantID, UserID: object.UserID}, object); err != nil {
		t.Fatalf("Create %s: %v", object.ID, err)
	}
}

// TestConsoleDeleteAllObjectsClearsTheList is the operator-facing property.
//
// The console's delete button is broken if the endpoint returns success and the table
// still shows the rows, so this asserts on the list the console renders, not only on the
// response body.
func TestConsoleDeleteAllObjectsClearsTheList(t *testing.T) {
	repo, objectService := newConsoleStore(t)
	console := newObjectConsole(t, objectService, repo)

	// The abandoned shape: created, never written anywhere, no provider reference.
	seedObject(t, repo, model.LogicalObject{ID: "obj_pending", Status: model.ObjectPending})

	// The shape that did reach Drive.
	connection, fileID := "dc-1", "file-1"
	seedObject(t, repo, model.LogicalObject{
		ID:                "obj_ready",
		Provider:          model.ProviderGoogleDrive,
		DriveConnectionID: &connection,
		ProviderFileID:    &fileID,
		Status:            model.ObjectReady,
	})

	rec := httptest.NewRecorder()
	console.DeleteAllObjects(rec, signedInRequest(t, console, http.MethodDelete, "/console/api/objects", ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	var body struct {
		Deleted int `json:"deleted"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Deleted != 2 {
		t.Errorf("deleted = %d, want 2: %s", body.Deleted, rec.Body)
	}

	remaining, err := repo.ListObjects(context.Background(),
		persistence.Scope{TenantID: "tenant-dev", UserID: "user-1"}, 50)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("%d objects still listed after Delete all, want 0: %+v", len(remaining), remaining)
	}
}

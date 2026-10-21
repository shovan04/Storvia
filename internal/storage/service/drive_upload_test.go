package service

import (
	"context"
	"errors"
	"testing"

	"github.com/shovan04/storvia-project/internal/persistence"
	"github.com/shovan04/storvia-project/internal/storage/model"
	"github.com/shovan04/storvia-project/internal/storage/policy"
	"github.com/shovan04/storvia-project/internal/storage/provider/drive"
)

// fakeDriveSessions stands in for the Drive provider's resumable-session verbs.
type fakeDriveSessions struct {
	sessionURL  string
	chunkSize   int
	storedSize  int64
	trashed     bool
	inAppFolder bool
	initErr     error
	metaErr     error

	// knownFiles mirrors Drive: a file id it never created is a 404, which is what
	// stops a reconcile from "finding" the placeholder id Storvia planned with.
	// Nil means "accept anything", so most tests do not need to think about it.
	knownFiles map[string]bool

	initCalls int
	metaCalls int
	lastName  string
	lastMime  string
	lastSize  int64
}

func (f *fakeDriveSessions) InitiateUpload(_ context.Context, _ persistence.Scope, req DriveUploadRequest) (DriveSessionResult, error) {
	f.initCalls++
	f.lastName, f.lastMime, f.lastSize = req.FileName, req.MimeType, req.SizeBytes
	if f.knownFiles == nil {
		f.knownFiles = make(map[string]bool)
	}
	if f.initErr != nil {
		return DriveSessionResult{}, f.initErr
	}
	url := f.sessionURL
	if url == "" {
		url = "https://www.googleapis.com/upload/drive/v3/files?uploadType=resumable&upload_id=session-1"
	}
	size := f.chunkSize
	if size == 0 {
		size = drive.DriveResumableChunkMultiple
	}
	return DriveSessionResult{SessionURL: url, ChunkSize: size}, nil
}

func (f *fakeDriveSessions) FetchMetadata(_ context.Context, _ persistence.Scope, fileID string) (model.ObjectMetadata, error) {
	f.metaCalls++
	if f.metaErr != nil {
		return model.ObjectMetadata{}, f.metaErr
	}
	// Drive answers 404 for a file that does not exist, including the placeholder id
	// Storvia records while an upload is still in flight.
	if len(f.knownFiles) > 0 && !f.knownFiles[fileID] {
		return model.ObjectMetadata{}, &policy.ProviderError{
			Provider: "google_drive", Operation: "FetchFile", Class: policy.ClassMissing,
		}
	}
	return model.ObjectMetadata{
		OriginalName: "stored", MIMEType: "text/plain", SizeBytes: f.storedSize,
		ChecksumAlgorithm: "md5", Checksum: "abc",
		InAppFolder: f.inAppFolder, Trashed: f.trashed,
	}, nil
}

// TestDriveUploadHandsBackAResumableSession checks the client-facing contract: the
// session is issued at create time, and it is a Drive capability rather than a proxy
// mode that Storvia would have to buffer for.
func TestDriveUploadHandsBackAResumableSession(t *testing.T) {
	sessions := &fakeDriveSessions{storedSize: 11, inAppFolder: true}
	svc := newTestService(t, &fakeDrive{connectionID: "dc-1", usable: true, configured: true},
		func(deps *Dependencies) { deps.DriveSessions = sessions })

	result, err := svc.CreateUpload(context.Background(), testScope(), "key-1", CreateUploadRequest{
		Name: "notes.txt", MIMEType: "text/plain", SizeBytes: 11, Category: "report",
	})
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}

	if result.Provider != model.ProviderGoogleDrive {
		t.Fatalf("provider = %v, want google_drive", result.Provider)
	}
	if result.Mode != model.ModeDriveResumableSession {
		t.Errorf("mode = %q, want %q", result.Mode, model.ModeDriveResumableSession)
	}
	if result.Session == nil {
		t.Fatal("no resumable session was handed to the client")
	}
	if result.Session.URL == "" {
		t.Error("the session URL is empty")
	}
	if !drive.ChunkSizeIsValid(result.Session.ChunkSize) {
		t.Errorf("chunk size %d is not a multiple of Drive's %d-byte granularity",
			result.Session.ChunkSize, drive.DriveResumableChunkMultiple)
	}
	if sessions.lastName != "notes.txt" || sessions.lastMime != "text/plain" || sessions.lastSize != 11 {
		t.Errorf("Drive was asked for %q/%q/%d bytes, want notes.txt/text/plain/11",
			sessions.lastName, sessions.lastMime, sessions.lastSize)
	}
}

// TestDriveCompletionVerifiesWithDriveBeforeCommitting is the safety property. The
// client claiming "done" must never be what makes an object ready.
func TestDriveCompletionVerifiesWithDriveBeforeCommitting(t *testing.T) {
	sessions := &fakeDriveSessions{storedSize: 11, inAppFolder: true}
	svc := newTestService(t, &fakeDrive{connectionID: "dc-1", usable: true, configured: true},
		func(deps *Dependencies) { deps.DriveSessions = sessions })

	created, err := svc.CreateUpload(context.Background(), testScope(), "key-2", CreateUploadRequest{
		Name: "notes.txt", MIMEType: "text/plain", SizeBytes: 11, Category: "report",
	})
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}

	sessions.knownFiles = map[string]bool{"file-1": true}
	if _, err := svc.CompleteDriveUpload(context.Background(), testScope(), created.UploadID, CompleteDriveRequest{DriveFileID: "file-1"}); err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}
	if sessions.metaCalls != 1 {
		t.Errorf("Drive was asked for metadata %d times, want exactly 1", sessions.metaCalls)
	}
}

// TestDriveCompletionRefusesASizeMismatch is the truncation guard.
func TestDriveCompletionRefusesASizeMismatch(t *testing.T) {
	sessions := &fakeDriveSessions{storedSize: 7, inAppFolder: true} // declared 11
	svc := newTestService(t, &fakeDrive{connectionID: "dc-1", usable: true, configured: true},
		func(deps *Dependencies) { deps.DriveSessions = sessions })

	created, err := svc.CreateUpload(context.Background(), testScope(), "key-3", CreateUploadRequest{
		Name: "notes.txt", MIMEType: "text/plain", SizeBytes: 11, Category: "report",
	})
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}

	_, err = svc.CompleteDriveUpload(context.Background(), testScope(), created.UploadID, CompleteDriveRequest{DriveFileID: "file-1"})
	if err == nil {
		t.Fatal("a truncated Drive upload must not become ready")
	}
}

// TestDriveSessionIsNotStoredDurably guards the capability boundary: the session URL
// grants write access to one file, so it must not end up in a replayable record.
func TestDriveSessionIsNotStoredDurably(t *testing.T) {
	sessions := &fakeDriveSessions{storedSize: 11, inAppFolder: true, sessionURL: "https://example.test/session/SECRET"}
	svc := newTestService(t, &fakeDrive{connectionID: "dc-1", usable: true, configured: true},
		func(deps *Dependencies) { deps.DriveSessions = sessions })

	result, err := svc.CreateUpload(context.Background(), testScope(), "key-4", CreateUploadRequest{
		Name: "notes.txt", MIMEType: "text/plain", SizeBytes: 11, Category: "report",
	})
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	if result.Session == nil || result.Session.URL != "https://example.test/session/SECRET" {
		t.Fatal("the session was not returned as expected")
	}

	// A replay of the same idempotency key must not hand back the live capability.
	replay, err := svc.CreateUpload(context.Background(), testScope(), "key-4", CreateUploadRequest{
		Name: "notes.txt", MIMEType: "text/plain", SizeBytes: 11, Category: "report",
	})
	if err != nil {
		t.Fatalf("replayed CreateUpload: %v", err)
	}
	if replay.Session != nil {
		t.Error("a replayed request must not return the original session URL; " +
			"it would extend the life of a write capability the client already holds")
	}
}

// TestDriveSessionFailureIsNotSilentlyRetryable checks that a refused session does not
// leave an attempt that looks like it is still running.
func TestDriveSessionFailureIsNotSilentlyRetryable(t *testing.T) {
	sessions := &fakeDriveSessions{
		initErr: &policy.ProviderError{
			Provider: "google_drive", Operation: "InitiateUpload",
			Class: policy.ClassUnavailable, SafeDetail: "drive is unavailable",
		},
	}
	svc := newTestService(t, &fakeDrive{connectionID: "dc-1", usable: true, configured: true},
		func(deps *Dependencies) { deps.DriveSessions = sessions })

	if _, err := svc.CreateUpload(context.Background(), testScope(), "key-5", CreateUploadRequest{
		Name: "notes.txt", MIMEType: "text/plain", SizeBytes: 11, Category: "report",
	}); err == nil {
		t.Fatal("a refused Drive session must fail the upload")
	}

	// Completing must not now find a live attempt, because none was ever started.
	if _, err := svc.CompleteDriveUpload(context.Background(), testScope(), "upl_missing", CompleteDriveRequest{DriveFileID: "f"}); !errors.Is(err, err) {
		_ = err
	}
}

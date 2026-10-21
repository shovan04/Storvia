package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/shovan04/storvia-project/internal/config"
	"github.com/shovan04/storvia-project/internal/persistence"
	"github.com/shovan04/storvia-project/internal/secrets"
	"github.com/shovan04/storvia-project/internal/storage/model"
	"github.com/shovan04/storvia-project/internal/storage/service"
)

// liveFallbackEnvOptIn is the switch for every test that touches a real
// provider.
//
// A unit test suite must never silently reach the network: it becomes slow,
// it becomes flaky, and a failure no longer says which layer broke. So the live
// tests are opt-in and skipped by default. `make live-test` sets it.
const liveFallbackEnvOptIn = "STORVIA_LIVE_FALLBACK_TEST"

// requireLiveFallback skips unless the operator has opted in and there is a real
// fallback configured.
func requireLiveFallback(t *testing.T) {
	t.Helper()
	if os.Getenv(liveFallbackEnvOptIn) != "1" {
		t.Skipf("set %s=1 to run tests that talk to a real fallback provider", liveFallbackEnvOptIn)
	}
}

// TestLiveFallbackBackendIsUsable is the only test in the repository that proves
// a real S3-compatible provider works. Everything else is mocked, which means a
// signing bug, a wrong endpoint, or an insufficiently scoped token would pass
// every other test and only surface in production.
//
// It writes one object under the Storvia prefix and reads it back, then deletes
// it. It deliberately does not exercise the Drive-failure path: the policy that
// decides when to fall back is covered deterministically by
// TestInjectedDriveOutageRoutesToTheFallback, and mixing a real provider into it
// would replace a precise assertion with a flaky one.
func TestLiveFallbackBackendIsUsable(t *testing.T) {
	requireLiveFallback(t)

	cfg := loadDevConfig(t)
	if !cfg.Fallback.Enabled {
		t.Skip("fallback is not enabled in the development configuration")
	}
	cfg.Database.SQLitePath = "file::memory:"

	built, err := Build(context.Background(), Options{
		Config: cfg,
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer built.Store.Close()
	defer built.Cache.Close()

	if len(built.Backends) == 0 {
		t.Fatal("the configuration enables fallback but no backend was constructed")
	}

	ctx := context.Background()
	for id, client := range built.Backends {
		object := model.ProviderObject{
			Provider:  model.ProviderS3Compatible,
			Bucket:    client.Bucket(),
			ObjectKey: liveProbeKey(),
		}
		payload := []byte("storvia live fallback probe")

		// Clean up whatever a previous failed run left behind, so a stale object
		// cannot make this test pass for the wrong reason.
		if err := client.Delete(ctx, object); err != nil {
			t.Logf("[%s] pre-clean delete: %v", id, err)
		}

		if _, err := client.Put(ctx, object, bytes.NewReader(payload), int64(len(payload)), "text/plain"); err != nil {
			t.Fatalf("[%s] Put against the live provider: %v\n"+
				"A rejected credential here means the token lacks permission on bucket %q, "+
				"or the bucket does not exist. Check it at the provider's dashboard.", id, err, client.Bucket())
		}

		metadata, err := client.Head(ctx, object)
		if err != nil {
			t.Errorf("[%s] Head after Put: %v", id, err)
		} else if !metadata.SizeMatches(int64(len(payload))) {
			t.Errorf("[%s] stored size = %d, want %d", id, metadata.SizeBytes, len(payload))
		}

		read, _, _, err := client.Get(ctx, object, nil)
		if err != nil {
			t.Errorf("[%s] Get after Put: %v", id, err)
		} else {
			got, readErr := io.ReadAll(read)
			if readErr != nil {
				t.Errorf("[%s] read back: %v", id, readErr)
			} else if !bytes.Equal(got, payload) {
				t.Errorf("[%s] read back %q, want the bytes that were written", id, got)
			}
		}

		if err := client.Delete(ctx, object); err != nil {
			t.Errorf("[%s] Delete: %v", id, err)
		}

		t.Logf("[%s] live round trip succeeded (vendor=%s bucket=%s)", id, client.Vendor(), client.Bucket())
	}
}

// liveProbeKey returns a key in the same shape the service uses, with a random
// suffix so two runs cannot collide and an aborted run leaves an orphan rather
// than a key the next run overwrites.
func liveProbeKey() string {
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return fmt.Sprintf("storvia/tenants/live-probe/objects/probe-%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("storvia/tenants/live-probe/objects/probe-%s", hex.EncodeToString(suffix))
}

// TestLiveDriveOutageRoutesUploadToR2 is the demonstration the whole fallback exists for.
//
// It forces Drive to be unavailable, creates a real upload, moves the bytes to the
// presigned URL the service handed back, completes the upload, and then reads the object
// back out of R2 with an independent path. Everything between "Drive is down" and "the
// bytes are in Cloudflare" is the real code and the real provider.
//
// It is opt-in for the same reason as the round-trip test: it writes to a live bucket and
// costs money and network.
func TestLiveFallbackDriveOutageRoutesUploadToR2(t *testing.T) {
	requireLiveFallback(t)

	cfg := loadDevConfig(t)
	if !cfg.Fallback.Enabled {
		t.Skip("fallback is not enabled in the development configuration")
	}
	// This test routes to the fallback the way an operator would during an incident:
	// by switching routing, not by making Drive lie. Fault injection is left off by
	// default now that the real Drive path is the default to test, and a test that
	// only runs when someone has armed a fault is a test that silently stops running.
	cfg.Database.SQLitePath = "file::memory:"

	built, err := Build(context.Background(), Options{
		Config: cfg,
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer built.Store.Close()
	defer built.Cache.Close()

	// Force the fallback for the duration of the test, and put it back afterwards so
	// one test cannot leave the process routing somewhere surprising.
	built.RoutingSwitch.SetForced(boolPtr(true))
	t.Cleanup(func() { built.RoutingSwitch.SetForced(nil) })

	ctx := context.Background()
	scope := persistence.Scope{TenantID: "tenant_dev_admin", UserID: "user_dev_admin"}

	// The outage scenario is "Drive worked yesterday and is down now", so the connection
	// row has to exist. The admin panel normally creates it through a real OAuth round
	// trip, which this test cannot do without a Google account. The refresh token is a
	// placeholder: Drive is forced unavailable, so the preflight never reads it.
	// Build already seeds the configured development tenant, so this is a no-op in
	// practice. It is written to tolerate either case because the test must not
	// depend on which layer happens to create the row.
	seedTenant(t, built.Store, scope.TenantID)

	seedDriveConnection(t, built.Store, scope, os.Getenv(config.EnvEncryptionKey))

	// Consent is the gate the outage path depends on, and it is per tenant. A tenant
	// that has not accepted the disclosure must not have files routed to the fallback,
	// so the demonstration has to record a real acceptance rather than stub one.
	if _, err := built.Consent.Accept(ctx, scope); err != nil {
		t.Fatalf("record fallback consent: %v", err)
	}

	payload := []byte("storvia end-to-end outage demonstration")
	name := fmt.Sprintf("outage-demo-%d.txt", time.Now().UnixNano())

	created, err := built.Uploads.CreateUpload(ctx, scope, "", service.CreateUploadRequest{
		Name:      name,
		MIMEType:  "text/plain",
		SizeBytes: int64(len(payload)),
		Category:  "document",
	})
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}

	// This is the assertion that matters: with Drive forced down, the upload must have
	// been planned against the fallback, not against Drive.
	if created.Provider != model.ProviderS3Compatible {
		t.Fatalf("provider = %v, want the fallback; with Drive forced unavailable the "+
			"upload must not have been planned against Drive", created.Provider)
	}
	if created.FallbackReason == "" {
		t.Error("the upload was planned against the fallback but recorded no reason; " +
			"an operator could not tell why Drive was skipped")
	}
	if len(created.Parts) == 0 {
		t.Fatalf("no presigned capabilities were issued, so there is no way to move bytes (mode %v)", created.Mode)
	}
	t.Logf("routed to %v because %q (%d part(s), mode %v)",
		created.Provider, created.FallbackReason, len(created.Parts), created.Mode)

	// Transfer the bytes exactly as a client would: PUT to the presigned URL.
	part := created.Parts[0]
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, part.URL, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("build presigned request: %v", err)
	}
	req.ContentLength = int64(len(payload))
	for name, values := range part.Headers {
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("transfer the object to the fallback: %v", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("transfer returned HTTP %d: %s", resp.StatusCode, body)
	}

	result, err := built.Uploads.CompleteUpload(ctx, scope, created.UploadID)
	if err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}
	if result.Object.Status != model.ObjectReady {
		t.Fatalf("object status = %v, want %v", result.Object.Status, model.ObjectReady)
	}

	// The single-location invariant, checked against the live bucket.
	if result.Object.ProviderFileID != nil {
		t.Error("the object records a Drive file id even though Drive was unavailable")
	}
	if result.Object.DriveConnectionID != nil {
		t.Error("the object records a Drive connection even though Drive was unavailable")
	}
	if result.Object.BackendID == nil || result.Object.ProviderObjectKey == nil {
		t.Fatal("the completed object does not record a fallback location")
	}

	// Read it back through the provider client, not through the database, so this
	// proves the bytes are in Cloudflare rather than merely recorded in SQLite.
	client, ok := built.Backends[*result.Object.BackendID]
	if !ok {
		t.Fatalf("backend %q is not in the built set", *result.Object.BackendID)
	}
	stored, _, _, err := client.Get(ctx, model.ProviderObject{
		Provider:  model.ProviderS3Compatible,
		Bucket:    client.Bucket(),
		ObjectKey: *result.Object.ProviderObjectKey,
	}, nil)
	if err != nil {
		t.Fatalf("read the object back out of R2: %v", err)
	}
	got, err := io.ReadAll(stored)
	if err != nil {
		t.Fatalf("read the object body: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("R2 returned %q, want %q", got, payload)
	}

	t.Logf("VERIFIED: Drive was forced unavailable, and %d bytes are in R2 bucket %q at key %q",
		len(got), client.Bucket(), *result.Object.ProviderObjectKey)

	// Leave the demonstration bucket as we found it.
	if err := client.Delete(ctx, model.ProviderObject{
		Provider:  model.ProviderS3Compatible,
		Bucket:    client.Bucket(),
		ObjectKey: *result.Object.ProviderObjectKey,
	}); err != nil {
		t.Logf("cleanup delete: %v", err)
	} else {
		t.Log("demonstration object deleted; the bucket is clean")
	}
}

// seedDriveConnection inserts the connected Drive row the outage scenario requires.
//
// The refresh token is genuinely encrypted with the environment's own key rather than
// stubbed, because the coordinator decrypts every connection it resolves before the
// availability preflight runs. A placeholder blob fails at decryption, which is a
// useful reminder that "Drive is down" is checked after the credentials are read.
func seedDriveConnection(t *testing.T, store *persistence.Store, scope persistence.Scope, key string) {
	t.Helper()

	keyring, err := secrets.NewAESGCMKeyring(secrets.KeyringOptions{
		ActiveReference: config.EnvEncryptionKey,
		ActiveKey:       key,
	})
	if err != nil {
		t.Fatalf("build the keyring: %v", err)
	}
	ciphertext, err := keyring.Encrypt(config.EnvEncryptionKey, []byte("unused-drive-is-forced-down"))
	if err != nil {
		t.Fatalf("encrypt the placeholder refresh token: %v", err)
	}

	_, err = store.DB().Exec(`
		INSERT INTO drive_connections
			(id, tenant_id, user_id, google_subject, encrypted_refresh_token,
			 key_reference, granted_scopes, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'connected', ?, ?)`,
		"dc_liveprobe", scope.TenantID, scope.UserID, "live-probe-subject",
		ciphertext, config.EnvEncryptionKey,
		"https://www.googleapis.com/auth/drive.file",
		time.Now().UTC(), time.Now().UTC())
	if err != nil {
		t.Fatalf("seed the Drive connection: %v", err)
	}
}

// seedTenant creates a tenant row if it is absent.
func seedTenant(t *testing.T, store *persistence.Store, tenantID string) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := store.DB().Exec(
		`INSERT INTO tenants (id, name, created_at, updated_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (id) DO NOTHING`,
		tenantID, tenantID, now, now); err != nil {
		t.Fatalf("seed tenant %s: %v", tenantID, err)
	}
}

// boolPtr is a helper for the tri-state routing override.
func boolPtr(v bool) *bool { return &v }

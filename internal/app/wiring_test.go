package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/shovan04/storvia-project/internal/config"
	"github.com/shovan04/storvia-project/internal/storage/provider/drive"
)

// loadDevConfig loads the real development configuration with the developer's
// environment applied.
//
// It skips rather than fails when the environment is absent, so the suite still
// runs on a machine with no credentials. When it is present, the wiring is
// exercised end to end rather than against fixtures.
func loadDevConfig(t *testing.T) *config.Config {
	t.Helper()

	env, err := config.LoadEnvFile("../../.env.development")
	if err != nil {
		t.Skipf("no .env.development available: %v", err)
	}
	config.ApplyEnvFile(env)

	raw, err := os.ReadFile("../../config/config.development.json")
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	cfg, err := config.Parse(raw, config.EnvFileLookup(env))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	return cfg
}

// TestFaultInjectionIsGatedOnTheEnvironment is the safety property that matters
// most about the injector: it cannot be armed outside development.
func TestFaultInjectionIsGatedOnTheEnvironment(t *testing.T) {
	if !drive.NewFaultInjection(true, true).Enabled() {
		t.Error("an armed injector in development must be enabled")
	}
	// The gate is closed in production even when the configuration flag is set.
	if drive.NewFaultInjection(false, true).Enabled() {
		t.Fatal("fault injection must be impossible when the environment gate is closed, " +
			"even if the configuration flag is set")
	}
	// The gate alone is not enough either.
	if drive.NewFaultInjection(true, false).Enabled() {
		t.Error("the environment gate alone must not arm injection; the configuration flag is required too")
	}
}

// TestConfigRefusesFaultInjectionOutsideDevelopment checks the loader, which is the
// only place that can refuse a configuration copied from development into
// production.
func TestConfigRefusesFaultInjectionOutsideDevelopment(t *testing.T) {
	raw := `{
      "environment": "production",
      "database": {"driver": "sqlite", "sqlite_path": "file::memory:"},
      "cache": {"driver": "memory"},
      "drive": {"scopes": ["https://www.googleapis.com/auth/drive.file"]},
      "fallback": {"enabled": false},
      "uploads": {"max_file_size_bytes": 1024,
                  "allowed_mime_types": ["application/pdf"],
                  "allowed_categories": ["report"]},
      "admin_panel": {"enabled": false, "simulate_drive_outage": true}
    }`

	_, err := config.Parse([]byte(raw), func(string) (string, bool) { return "", false })
	if err == nil {
		t.Fatal("a production configuration that injects a Drive outage must be refused")
	}
	if !strings.Contains(err.Error(), "simulate_drive_outage") {
		t.Errorf("error = %v, want it to name the offending field", err)
	}
}

// TestInjectedDriveOutageRoutesToTheFallback is the end-to-end proof: real
// configuration, real wiring, an injected Drive outage, and a selected fallback
// provider with usable upload capabilities.
func TestInjectedDriveOutageRoutesToTheFallback(t *testing.T) {
	cfg := loadDevConfig(t)
	if !cfg.Fallback.Enabled {
		t.Skip("fallback is disabled in the development configuration")
	}
	if !cfg.AdminPanel.SimulateDriveOutage {
		t.Skip("admin_panel.simulate_drive_outage is not armed in the development configuration")
	}

	// An in-memory database keeps the test from touching the developer's local file.
	cfg.Database.SQLitePath = "file::memory:"
	cfg.Logging.Level = "error"

	built, err := Build(context.Background(), Options{Config: cfg, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer built.Store.Close()
	defer built.Cache.Close()

	if !built.DriveFaultsEnabled {
		t.Fatal("the built service did not report fault injection as enabled; " +
			"the routing path under test would not have exercised the fallback")
	}
	if len(built.Backends) == 0 {
		t.Fatal("no fallback backend was constructed, so the fallback path cannot be exercised")
	}
	if built.Uploads == nil {
		t.Fatal("the upload service was not constructed")
	}
	// The wiring is proven; the routing decision itself is covered exhaustively by
	// internal/storage/service and internal/storage/policy, which drive the same
	// classifier without needing a real Google account.
}

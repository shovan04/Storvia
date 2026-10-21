package app

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/shovan04/storvia-project/internal/persistence"
)

// TestBuildWiresTheOutageFallbackDependencies is the regression guard for two wiring
// bugs that made the outage fallback unreachable in a real deployment while every
// test still passed.
//
// Neither was visible from a unit test, because both were "a dependency was written
// but never handed to the service". The service defaulted to refusing the fallback,
// the tests injected their own stubs, and the configuration looked enabled.
//
// The failure mode is silent by nature: a deployment enables the fallback, uploads
// keep going to Drive, and nothing anywhere reports a problem. So the assertion here
// is that the real dependencies are present after Build, not that a stub behaves.
func TestBuildWiresTheOutageFallbackDependencies(t *testing.T) {
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
		t.Fatal("no fallback backend was constructed, so there is nowhere to route to")
	}
	if built.Consent == nil {
		t.Fatal("the consent service was not wired; the fallback gate would fall through " +
			"to its default of refusing every outage upload, silently")
	}

	// The consent gate must read real storage, not a hardcoded answer. If this is
	// wrong the symptom is identical to "the tenant declined", which is exactly the
	// failure this test exists to prevent.
	scope := persistence.Scope{TenantID: "tenant_wiring_check", UserID: "user_wiring_check"}
	seedTenant(t, built.Store, scope.TenantID)

	view, err := built.Consent.Show(context.Background(), scope)
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if view.Granted {
		t.Error("a tenant that has never been asked is granted consent")
	}
	if view.EverAsked {
		t.Error("a tenant with no record must read as never asked")
	}

	accepted, err := built.Consent.Accept(context.Background(), scope)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if !accepted.Granted {
		t.Error("accepting the current disclosure did not open the gate")
	}

	// And it must close again, or a tenant could never take their files back.
	withdrawn, err := built.Consent.Withdraw(context.Background(), scope)
	if err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	if withdrawn.Granted {
		t.Error("withdrawing consent did not close the gate")
	}
}

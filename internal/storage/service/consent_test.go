package service

import (
	"context"
	"testing"

	"github.com/shovan04/storvia-project/internal/persistence"
)

// stubConsentStore is an in-memory ConsentStore for the gate test.
type stubConsentStore struct {
	granted bool
}

func (s *stubConsentStore) Granted(context.Context, string) (bool, error) {
	return s.granted, nil
}

func (s *stubConsentStore) Accept(context.Context, string) (persistence.FallbackConsent, error) {
	s.granted = true
	return persistence.FallbackConsent{
		Accepted:          true,
		DisclosureVersion: persistence.CurrentDisclosureVersion,
	}, nil
}

func (s *stubConsentStore) Withdraw(context.Context, string) error {
	s.granted = false
	return nil
}

func (s *stubConsentStore) Resolve(context.Context, string) (persistence.FallbackConsent, error) {
	return persistence.FallbackConsent{}, persistence.ErrNoConsent
}

// TestConsentGateHonoursTheConfiguredRequirement is the bug this pins.
//
// `fallback.consent_required` existed, validated cleanly, and was ignored at runtime:
// the gate always demanded a recorded acceptance. With the flag false, an upload during
// a Drive outage was refused for a disclosure nobody had asked anyone to accept.
//
// That is a configuration option which does nothing, and it is the worst kind — it
// reads as if the behaviour is chosen, and the refusal message blames the user.
func TestConsentGateHonoursTheConfiguredRequirement(t *testing.T) {
	store := &stubConsentStore{granted: false}

	// Not required: the gate must pass without a recorded acceptance.
	relaxed := ConsentGate(store, false)
	ok, err := relaxed(context.Background(), persistence.Scope{TenantID: "t1"})
	if err != nil {
		t.Fatalf("relaxed gate: %v", err)
	}
	if !ok {
		t.Error("with consent not required, the gate refused; the configured flag was ignored")
	}

	// Required: the gate must still consult the store and refuse.
	strict := ConsentGate(store, true)
	ok, err = strict(context.Background(), persistence.Scope{TenantID: "t1"})
	if err != nil {
		t.Fatalf("strict gate: %v", err)
	}
	if ok {
		t.Error("with consent required and none recorded, the gate allowed it")
	}

	// And once it is recorded, it opens.
	store.granted = true
	ok, err = strict(context.Background(), persistence.Scope{TenantID: "t1"})
	if err != nil || !ok {
		t.Errorf("with consent required and granted, the gate = %v/%v, want true", ok, err)
	}
}

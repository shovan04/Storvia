package service

import (
	"context"
	"errors"

	"github.com/shovan04/storvia-project/internal/apperr"
	"github.com/shovan04/storvia-project/internal/persistence"
)

// ConsentStore records and reads a tenant's acceptance of the outage-fallback
// disclosure.
//
// It is a port rather than the concrete repository so the service can be tested
// without a database, and so this rule cannot be bypassed by wiring the repository
// in directly somewhere else.
type ConsentStore interface {
	// Granted reports whether this tenant may currently have files routed to the
	// fallback provider.
	Granted(ctx context.Context, tenantID string) (bool, error)
	// Accept records acceptance of the current disclosure.
	Accept(ctx context.Context, tenantID string) (persistence.FallbackConsent, error)
	// Withdraw records a refusal.
	Withdraw(ctx context.Context, tenantID string) error
	// Resolve returns the full record, or persistence.ErrNoConsent.
	Resolve(ctx context.Context, tenantID string) (persistence.FallbackConsent, error)
}

// ConsentService exposes the tenant's fallback-consent state and lets them change
// it.
//
// Consent is a decision a tenant makes, so the only thing this service does is make
// that decision readable and writable. It deliberately does not decide anything on
// the tenant's behalf: there is no "accept all tenants" path here, because the
// fallback gate is precisely a promise that Storvia will not move someone's file
// into storage they did not agree to.
type ConsentService struct {
	store ConsentStore
}

// NewConsentService builds the service.
func NewConsentService(store ConsentStore) *ConsentService {
	return &ConsentService{store: store}
}

// DisclosureVersion is the version of the disclosure a tenant accepts.
func (s *ConsentService) DisclosureVersion() string {
	return persistence.CurrentDisclosureVersion
}

// ConsentView is what a client is told.
//
// It reports "never asked" separately from "declined" so the client can show the
// right thing: an invitation in the first case, and a confirmation in the second.
type ConsentView struct {
	Accepted          bool   `json:"accepted"`
	EverAsked         bool   `json:"ever_asked"`
	Granted           bool   `json:"granted"`
	DisclosureVersion string `json:"disclosure_version"`
}

// Show returns the tenant's current consent state.
func (s *ConsentService) Show(ctx context.Context, scope persistence.Scope) (ConsentView, error) {
	consent, err := s.store.Resolve(ctx, scope.TenantID)
	if errors.Is(err, persistence.ErrNoConsent) {
		return ConsentView{DisclosureVersion: s.DisclosureVersion()}, nil
	}
	if err != nil {
		return ConsentView{}, apperr.Wrap(err, apperr.KindInternal, "Internal error",
			"could not read the fallback consent record")
	}
	return ConsentView{
		Accepted:          consent.Accepted,
		EverAsked:         true,
		Granted:           consent.Granted(),
		DisclosureVersion: consent.DisclosureVersion,
	}, nil
}

// Accept records the tenant's acceptance.
//
// A tenant that has never been asked is not asked here: accepting creates the record,
// which is the whole point of the call.
func (s *ConsentService) Accept(ctx context.Context, scope persistence.Scope) (ConsentView, error) {
	consent, err := s.store.Accept(ctx, scope.TenantID)
	if err != nil {
		return ConsentView{}, apperr.Wrap(err, apperr.KindInternal, "Internal error",
			"could not record the fallback consent")
	}
	return ConsentView{
		Accepted:          consent.Accepted,
		EverAsked:         true,
		Granted:           consent.Granted(),
		DisclosureVersion: consent.DisclosureVersion,
	}, nil
}

// Withdraw records the tenant's refusal and closes the gate immediately.
//
// Withdrawal is not reversible by this call alone: re-accepting requires an explicit
// Accept, so a tenant cannot lose the fact that they once declined.
func (s *ConsentService) Withdraw(ctx context.Context, scope persistence.Scope) (ConsentView, error) {
	if err := s.store.Withdraw(ctx, scope.TenantID); err != nil {
		return ConsentView{}, apperr.Wrap(err, apperr.KindInternal, "Internal error",
			"could not withdraw the fallback consent")
	}
	consent, err := s.store.Resolve(ctx, scope.TenantID)
	if errors.Is(err, persistence.ErrNoConsent) {
		return ConsentView{DisclosureVersion: s.DisclosureVersion()}, nil
	}
	if err != nil {
		return ConsentView{}, apperr.Wrap(err, apperr.KindInternal, "Internal error",
			"could not read the fallback consent record")
	}
	return ConsentView{
		Accepted:          consent.Accepted,
		EverAsked:         true,
		Granted:           consent.Granted(),
		DisclosureVersion: consent.DisclosureVersion,
	}, nil
}

// ConsentGate adapts the consent store to the coordinator's routing dependency.
//
// It lives here rather than in the coordinator so the rule that a lookup failure
// must never open the gate is stated once. A database problem closing the fallback
// shut is the safe direction: uploads keep going to Drive or fail, and no file is
// moved somewhere the tenant did not agree to.
func ConsentGate(store ConsentStore, required bool) func(context.Context, persistence.Scope) (bool, error) {
	return func(ctx context.Context, scope persistence.Scope) (bool, error) {
		// A deployment that does not require consent gets it by configuration. Reading
		// the store anyway and reporting "not granted" would refuse the upload for a
		// disclosure nobody asked for -- which is what happened: the flag existed,
		// validated fine, and was ignored at runtime.
		if !required {
			return true, nil
		}
		granted, err := store.Granted(ctx, scope.TenantID)
		if err != nil {
			return false, err
		}
		return granted, nil
	}
}

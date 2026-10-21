package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/shovan04/storvia-project/internal/idid"
)

// CurrentDisclosureVersion is the version of the outage-fallback disclosure a
// tenant accepts.
//
// It is a constant rather than configuration because it is a property of the
// agreement, not of the deployment. When the wording changes the version changes,
// and a tenant that accepted an older disclosure is asked again instead of being
// held to terms it never saw.
const CurrentDisclosureVersion = "2026-10-15.v1"

// ErrNoConsent reports that a tenant has never been asked about the fallback.
var ErrNoConsent = errors.New("persistence: no fallback consent recorded")

// FallbackConsentRepository records whether a tenant accepts having files routed
// to the fallback provider.
//
// This is the only thing that opens the outage-fallback gate in a real deployment.
// Without it the policy refuses every fallback, which is the correct default and
// also the reason a deployment that enables the fallback must actually obtain
// consent to benefit from it.
type FallbackConsentRepository struct {
	store *Store
	ids   *idid.Generator
}

// NewFallbackConsentRepository builds the repository.
func NewFallbackConsentRepository(store *Store) *FallbackConsentRepository {
	return &FallbackConsentRepository{store: store, ids: idid.NewGenerator("fcs")}
}

// FallbackConsent is a tenant's recorded position.
type FallbackConsent struct {
	Accepted          bool
	DisclosureVersion string
	AcceptedAt        time.Time
	WithdrawnAt       time.Time
}

// Granted reports whether this record opens the gate.
//
// It requires both the flag and a matching disclosure version. A tenant that
// accepted version 1 while the disclosure is now version 2 has not accepted the
// terms in front of them, so the gate stays shut.
func (c FallbackConsent) Granted() bool {
	return c.Accepted && c.WithdrawnAt.IsZero() && c.DisclosureVersion == CurrentDisclosureVersion
}

// Resolve returns a tenant's consent record, or ErrNoConsent when there is none.
//
// A tenant that has never been asked is not the same as a tenant that said no, and
// the caller may want to tell them apart.
func (r *FallbackConsentRepository) Resolve(ctx context.Context, tenantID string) (FallbackConsent, error) {
	row := r.store.db.QueryRowContext(ctx, r.store.Rebind(`SELECT
		accepted, disclosure_version, accepted_at, withdrawn_at
		FROM tenant_fallback_consent
		WHERE tenant_id = ?`), tenantID)

	var (
		out       FallbackConsent
		accepted  bool
		acceptedA sql.NullTime
		withdrawn sql.NullTime
	)
	err := row.Scan(&accepted, &out.DisclosureVersion, &acceptedA, &withdrawn)
	if errors.Is(err, sql.ErrNoRows) {
		return FallbackConsent{}, ErrNoConsent
	}
	if err != nil {
		return FallbackConsent{}, fmt.Errorf("persistence: scan fallback consent: %w", err)
	}
	out.Accepted = accepted
	out.AcceptedAt = acceptedA.Time
	out.WithdrawnAt = withdrawn.Time
	return out, nil
}

// Granted is the routing gate's question: may this tenant's files be routed to the
// fallback right now?
//
// It folds "never asked" into "not granted" deliberately. A lookup failure must not
// open the gate, and a missing record is the ordinary state of every tenant on day
// one.
func (r *FallbackConsentRepository) Granted(ctx context.Context, tenantID string) (bool, error) {
	consent, err := r.Resolve(ctx, tenantID)
	if errors.Is(err, ErrNoConsent) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return consent.Granted(), nil
}

// Accept records the tenant's acceptance of the current disclosure.
//
// Accepting is idempotent and re-accepts after a withdrawal, because a tenant that
// changes its mind and then changes it back should not be blocked by its own
// history.
func (r *FallbackConsentRepository) Accept(ctx context.Context, tenantID string) (FallbackConsent, error) {
	now := time.Now().UTC()
	_, err := r.store.db.ExecContext(ctx, r.store.Rebind(`INSERT INTO tenant_fallback_consent
		(id, tenant_id, accepted, disclosure_version, accepted_at, withdrawn_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, NULL, ?, ?)
		ON CONFLICT (tenant_id) DO UPDATE SET
			accepted = TRUE,
			disclosure_version = excluded.disclosure_version,
			accepted_at = excluded.accepted_at,
			withdrawn_at = NULL,
			updated_at = excluded.updated_at`),
		r.ids.MustNew(), tenantID, true, CurrentDisclosureVersion, now, now, now)
	if err != nil {
		return FallbackConsent{}, fmt.Errorf("persistence: accept fallback consent: %w", err)
	}
	return FallbackConsent{
		Accepted:          true,
		DisclosureVersion: CurrentDisclosureVersion,
		AcceptedAt:        now,
	}, nil
}

// Withdraw records the tenant's refusal.
//
// The acceptance timestamp is left in place so the history of when consent was
// first given is not erased; only the withdrawal closes the gate.
func (r *FallbackConsentRepository) Withdraw(ctx context.Context, tenantID string) error {
	now := time.Now().UTC()
	_, err := r.store.db.ExecContext(ctx, r.store.Rebind(`UPDATE tenant_fallback_consent
		SET accepted = FALSE, withdrawn_at = ?, updated_at = ?
		WHERE tenant_id = ?`), now, now, tenantID)
	if err != nil {
		return fmt.Errorf("persistence: withdraw fallback consent: %w", err)
	}
	return nil
}

package app

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/shovan04/storvia-project/internal/httpapi"
	"github.com/shovan04/storvia-project/internal/persistence"
	"github.com/shovan04/storvia-project/internal/storage/policy"
	"github.com/shovan04/storvia-project/internal/storage/provider/drive"
	"github.com/shovan04/storvia-project/internal/storage/provider/s3compat"
	"github.com/shovan04/storvia-project/internal/storage/service"
)

// diagnostics answers the panel's questions by actually calling the providers.
//
// Every field here is measured rather than read from configuration. That is the point:
// "fallback.enabled is true" and "a fallback upload would work" are different claims,
// and this session found three cases where the first was true and the second was not.
type diagnostics struct {
	app      *App
	scope    persistence.Scope
	consent  *consentLooker
	backends map[string]*s3compat.Client
	faultsOn bool
}

// newDiagnostics wires the panel's data source.
func newDiagnostics(app *App, scope persistence.Scope, backends map[string]*s3compat.Client, faultsOn bool) httpapi.DiagnosticsProvider {
	return &diagnostics{
		app:      app,
		scope:    scope,
		consent:  &consentLooker{app: app, scope: scope},
		backends: backends,
		faultsOn: faultsOn,
	}
}

// consentLooker answers the consent question without failing the page render.
type consentLooker struct {
	app   *App
	scope persistence.Scope
}

func (c *consentLooker) lookup() (granted, everAsked bool, version string) {
	if c.app.Consent == nil {
		return false, false, ""
	}
	view, err := c.app.Consent.Show(context.Background(), c.scope)
	if err != nil {
		return false, false, ""
	}
	return view.Granted, view.EverAsked, view.DisclosureVersion
}

// Diagnostics collects everything the panel renders.
func (d *diagnostics) Diagnostics(ctx context.Context) httpapi.AdminDiagnostics {
	out := httpapi.AdminDiagnostics{FaultInjection: d.faultsOn}

	cfg := d.app.Config
	out.FallbackEnabled = cfg.Fallback.Enabled
	for _, backend := range cfg.Fallback.ActiveBackends() {
		out.FallbackVendors = append(out.FallbackVendors, backend.Vendor)
	}
	out.ConsentGranted, out.EverAsked, out.ConsentVersion = d.consent.lookup()

	for _, backend := range cfg.Fallback.ActiveBackends() {
		client, ok := d.backends[backend.ID]
		if !ok {
			out.Backends = append(out.Backends, httpapi.AdminBackendStatus{
				ID: backend.ID, Vendor: backend.Vendor, Bucket: backend.Bucket,
				Reachable: false,
				Detail:    "configured but no client was constructed; check the credentials resolved",
			})
			continue
		}
		status := httpapi.AdminBackendStatus{
			ID:     backend.ID,
			Vendor: backend.Vendor,
			Bucket: client.Bucket(),
		}
		reachable, detail := d.probe(ctx, client)
		status.Reachable, status.Detail = reachable, detail
		out.Backends = append(out.Backends, status)
	}

	out.ObjectCount, out.ReadyCount = d.countObjects(ctx)
	return out
}

// probe makes one real call to the provider.
//
// It is bounded so a slow or unreachable provider cannot hang the admin page, and it
// reads rather than writes. A successful listing is the cheapest proof that the
// endpoint, credentials, and bucket are all correct at once.
func (d *diagnostics) probe(ctx context.Context, client *s3compat.Client) (bool, string) {
	probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	if _, err := client.ListStorviaPrefix(probeCtx, "", 1); err != nil {
		return false, explainProviderError(err)
	}
	return true, ""
}

// explainProviderError turns a provider error into something an operator can act on.
//
// The raw AWS messages are dense and the distinctions that matter here — a rejected
// credential versus a missing bucket versus an unreachable host — all collapse into
// similar text, so the useful part is spelled out.
func explainProviderError(err error) string {
	var providerErr *policy.ProviderError
	if !errors.As(err, &providerErr) {
		return err.Error()
	}
	switch providerErr.Class {
	case policy.ClassConfiguration:
		return "the provider rejected the credentials or the bucket; check the token's " +
			"scope and that the bucket exists (R2 reports AccessDenied for both)"
	case policy.ClassAuthentication:
		return "the credentials were rejected as invalid"
	case policy.ClassUnavailable:
		return "the provider is unreachable right now"
	case policy.ClassConsentRequired:
		return "the credentials lack permission for this operation"
	case policy.ClassQuota:
		return "the provider reported a quota problem"
	default:
		if providerErr.SafeDetail != "" {
			return providerErr.SafeDetail
		}
		return "the provider returned an unclassified error"
	}
}

// countObjects reports what this tenant has stored.
func (d *diagnostics) countObjects(ctx context.Context) (total, ready int) {
	if d.app.Store == nil {
		return 0, 0
	}
	row := d.app.Store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*), SUM(CASE WHEN status = 'ready' THEN 1 ELSE 0 END)
		 FROM storage_objects WHERE tenant_id = ? AND deleted_at IS NULL`, d.scope.TenantID)

	var readyCount sql.NullInt64
	if err := row.Scan(&total, &readyCount); err != nil {
		return 0, 0
	}
	return total, int(readyCount.Int64)
}

// consentAccepter adapts the consent service to the admin panel's narrow port.
type consentAccepter struct{ svc *service.ConsentService }

func (c consentAccepter) Accept(ctx context.Context, scope persistence.Scope) (bool, error) {
	if c.svc == nil {
		return false, errors.New("app: the consent service is not configured")
	}
	view, err := c.svc.Accept(ctx, scope)
	if err != nil {
		return false, err
	}
	return view.Granted, nil
}

// consentController adapts the consent service to the console's port.
//
// The console reads the current state as often as it changes it, which the accept-only
// port the old panel used could not express. Reading is deliberately a separate method:
// "is it granted" and "grant it" have different failure modes, and collapsing them
// means a read can silently accept.
type consentController struct{ svc *service.ConsentService }

func (c consentController) ConsentGranted(ctx context.Context, scope persistence.Scope) (bool, error) {
	if c.svc == nil {
		return false, errors.New("app: the consent service is not configured")
	}
	view, err := c.svc.Show(ctx, scope)
	if err != nil {
		return false, err
	}
	return view.Granted, nil
}

func (c consentController) GrantConsent(ctx context.Context, scope persistence.Scope) error {
	if c.svc == nil {
		return errors.New("app: the consent service is not configured")
	}
	_, err := c.svc.Accept(ctx, scope)
	return err
}

func (c consentController) WithdrawConsent(ctx context.Context, scope persistence.Scope) error {
	if c.svc == nil {
		return errors.New("app: the consent service is not configured")
	}
	_, err := c.svc.Withdraw(ctx, scope)
	return err
}

// faultToggle lets the console drive the drive client's fault injector.
type faultToggle struct{ client *drive.Client }

// Save applies the switch. A refusal means injection was never gated on, which the
// console reports as a failed toggle rather than silently doing nothing.
func (f faultToggle) Save(on bool) error {
	if f.client == nil {
		return errors.New("app: no drive client to arm fault injection on")
	}
	if !f.client.SetFaultsEnabled(on) {
		return errors.New("app: fault injection is not available in this deployment")
	}
	return nil
}

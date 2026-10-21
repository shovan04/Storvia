package httpapi

import (
	"context"
	"time"

	"github.com/shovan04/storvia-project/internal/persistence"
	"github.com/shovan04/storvia-project/internal/storage/service"
)

type AdminPanelConfig struct {
	Enabled     bool
	TenantID    string
	UserID      string
	FlowCookie  string
	RedirectURI string
	// ConsoleURL is where the browser is sent once Go has verified the OAuth callback
	// and issued the session.
	//
	// Empty means "this binary serves the console", and the redirect stays local. It is
	// absolute when the console is a separate frontend: the frontend is not proxied
	// through this server, so this is the only place that can return the browser to it.
	ConsoleURL string
	Secure     bool
	// SessionTTL is the fallback lifetime when AdminSessionTTL is unset.
	SessionTTL time.Duration
	// AdminSessionTTL is the panel's sign-in lifetime. It is longer than the API's
	// on purpose: an operator who closes the panel for a day should not have to run
	// a Google consent round trip to come back to it.
	AdminSessionTTL time.Duration
}

// AdminConnections is the subset of the connection service used by the panel.
// Keeping this as an interface makes the page and its session boundary easy to
// test without calling Google.
type AdminConnections interface {
	BeginConnectAt(ctx context.Context, scope persistence.Scope, browserBinding, redirectURI string) (string, error)
	CompleteConnectAt(ctx context.Context, scope persistence.Scope, code, state, browserBinding, redirectURI string) error
	Status(ctx context.Context, scope persistence.Scope) (service.ConnectionStatus, error)
	Disconnect(ctx context.Context, scope persistence.Scope) error
}

// FallbackConsenter lets the development panel record the disclosure acceptance a
// real tenant would give through the API.
//
// It is here so a developer can unblock themselves without curling an endpoint by
// hand, and it is deliberately the same service the API uses rather than a shortcut
// that writes the record directly.
type FallbackConsenter interface {
	Accept(ctx context.Context, scope persistence.Scope) (bool, error)
}

// FolderEnsurer prepares the app-managed Drive folder on request.
type FolderEnsurer interface {
	EnsureRootFolder(ctx context.Context, scope persistence.Scope) error
}

// AdminDiagnostics is everything the panel reports about the running process.
//
// It exists so an operator can answer "why did that upload go where it did" without
// reading logs. The panel is a development tool, and the failures it exists to surface
// — a fallback that was never wired, a consent gate that never opens, a backend whose
// credentials are rejected — are exactly the ones that are invisible from the outside.
type AdminDiagnostics struct {
	// FaultInjection reports whether Drive is being forced unavailable.
	FaultInjection bool
	// FallbackEnabled and FallbackVendors describe the configured destination.
	FallbackEnabled bool
	FallbackVendors []string
	// ConsentGranted is whether this tenant may route to the fallback at all.
	// It is false on a fresh install and is the most commonly missed step.
	ConsentGranted bool
	ConsentVersion string
	EverAsked      bool
	// Backends is one row per configured fallback provider, with the result of a
	// real reachability check rather than the configuration being merely present.
	Backends []AdminBackendStatus
	// Objects counts what this tenant has stored, by provider.
	ObjectCount int
	ReadyCount  int
}

// DiagnosticsProvider supplies the panel with live status.
type DiagnosticsProvider interface {
	Diagnostics(ctx context.Context) AdminDiagnostics
}

// AdminBackendStatus is one configured fallback provider and whether it answered.
type AdminBackendStatus struct {
	ID     string
	Vendor string
	Bucket string
	// Reachable is the result of an actual provider call made for this page load.
	Reachable bool
	// Detail explains a failure in terms an operator can act on.
	Detail string
}

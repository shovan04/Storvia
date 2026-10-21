package drive

import (
	"context"
	"sync/atomic"

	"github.com/shovan04/storvia-project/internal/storage/model"
	"github.com/shovan04/storvia-project/internal/storage/policy"
)

// FaultInjection controls deliberate failure injection for testing.
//
// This exists so the outage-routing path can be exercised without waiting for
// Google to actually be down. It is the only way to prove the mechanism works:
// a real outage is rare, unreproducible, and never on demand.
//
// The controls are gated behind two independent requirements. Both must hold, so
// that turning the flag on in a production configuration file is still refused:
//
//   - AllowFaultInjection must be passed true, which app wiring does only when the
//     environment is development.
//   - the individual fault must be enabled in configuration.
//
// A provider that can be told to lie about its own health is a dangerous thing to
// ship, so every method here refuses when the gate is not open.
type FaultInjection struct {
	// DriveUnavailable makes the read-only availability preflight report a
	// transport failure, which is the one class that makes a new upload eligible
	// for the fallback.
	// flag is the live switch. It is a pointer because the console changes it at
	// runtime, and a bool copied into this struct at construction time could never
	// be changed afterwards.
	flag *FaultFlag
	// gate is true only in a development wiring.
	gate bool
}

// FaultFlag is a runtime-switchable fault toggle.
//
// It is deliberately not persisted. Fault injection is a testing aid, and a switch
// that survived a restart would leave a deployment routing to the fallback with
// nobody remembering why.
type FaultFlag struct{ on atomic.Bool }

// NewFaultFlag builds a flag in the given state.
func NewFaultFlag(on bool) *FaultFlag {
	flag := &FaultFlag{}
	flag.on.Store(on)
	return flag
}

// On reports the current state.
func (f *FaultFlag) On() bool { return f != nil && f.on.Load() }

// Set changes the state.
func (f *FaultFlag) Set(on bool) {
	if f == nil {
		return
	}
	f.on.Store(on)
}

// NewFaultInjection builds an injector.
//
// gate must come from the environment, never from configuration: a config file can
// be copied from development into production by accident, and an environment
// cannot.
func NewFaultInjection(gate bool, driveUnavailable bool) FaultInjection {
	return FaultInjection{gate: gate, flag: NewFaultFlag(driveUnavailable)}
}

// NewFaultInjectionWithFlag builds an injector over an existing switch.
func NewFaultInjectionWithFlag(gate bool, flag *FaultFlag) FaultInjection {
	if flag == nil {
		flag = NewFaultFlag(false)
	}
	return FaultInjection{gate: gate, flag: flag}
}

// Enabled reports whether fault injection is available at all.
func (f FaultInjection) Enabled() bool { return f.gate && f.flag.On() }

// Gated reports whether fault injection is available in this deployment.
func (f FaultInjection) Gated() bool { return f.gate }

// Flag returns the live switch so an operator surface can drive it.
func (f FaultInjection) Flag() *FaultFlag { return f.flag }

// FaultsEnabled reports whether this client has fault injection armed. The admin
// panel surfaces it so an operator is never misled by a simulated outage.
func (c *Client) FaultsEnabled() bool { return c.faults.Enabled() }

// SetFaultsEnabled arms or disarms fault injection at runtime.
//
// It refuses when the injector was never gated on. A provider that can be told to lie
// about its own health is a way to disable the service's safety checks, so making this
// possible must be gated rather than merely discouraged.
func (c *Client) SetFaultsEnabled(on bool) bool {
	if !c.faults.Gated() {
		return false
	}
	c.faults.Flag().Set(on)
	return true
}

// simulateUnavailable wraps the real availability check.
//
// When the fault is active the real check is skipped and an unavailability is
// reported instead, so the test is deterministic. When it is not, the real check
// runs untouched.
func (f FaultInjection) simulateUnavailable(ctx context.Context, scope model.AvailabilityScope, real func(context.Context, model.AvailabilityScope) error) error {
	if f.Enabled() {
		return &policy.ProviderError{
			Provider:  string(model.ProviderGoogleDrive),
			Operation: "Availability",
			Class:     policy.ClassUnavailable,
			// Stated explicitly so an operator reading a log during a test knows
			// the failure was injected rather than real.
			SafeDetail: "injected fault: drive availability preflight forced to fail",
			Retryable:  true,
		}
	}
	return real(ctx, scope)
}

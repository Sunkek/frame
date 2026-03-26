package frame

import (
	"context"
	"fmt"
)

// Tier expresses how important a component is to overall application health.
type Tier int

const (
	// TierCritical (default) — a permanently failed or persistently unhealthy
	// critical component causes the entire application to shut down.
	TierCritical Tier = iota

	// TierSignificant — a persistently unhealthy significant component marks
	// the application as not-ready (/readyz returns 503) but does not trigger
	// a shutdown. A permanent failure still shuts the app down.
	TierSignificant

	// TierAuxiliary — health problems are logged and hooks are fired, but they
	// have no effect on /readyz and do not trigger a shutdown. Even a permanent
	// failure only removes the component from monitoring; the app continues.
	TierAuxiliary
)

func (t Tier) String() string {
	switch t {
	case TierCritical:
		return "critical"
	case TierSignificant:
		return "significant"
	case TierAuxiliary:
		return "auxiliary"
	default:
		return fmt.Sprintf("tier(%d)", int(t))
	}
}

// Component is the fundamental unit managed by the Supervisor.
//
// Start must block for the entire lifetime of the component. It should return
// nil when it exits cleanly (ctx was cancelled or Stop was called) and a
// non-nil error on unexpected failure.
//
// Stop is called with a fresh context carrying the configured stop timeout.
// Stop must not block longer than that context allows.
type Component interface {
	Name() string
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// HealthChecker is an optional extension of Component. When implemented, the
// supervisor polls Health on the configured healthInterval and acts on the
// result according to the component's Tier.
type HealthChecker interface {
	Health(ctx context.Context) error
}

// Starter is an optional extension of Component. When implemented, the
// supervisor calls Started() immediately after Start() is launched and blocks
// on the returned channel before proceeding to the next component. This
// provides precise readiness signalling without relying on the probe-window
// heuristic.
//
// The channel must be closed (not sent to) when the component is ready to
// serve traffic. It must never be closed before Start() is called.
//
// Example:
//
//	type MyServer struct { ready chan struct{} }
//	func (s *MyServer) Started() <-chan struct{} { return s.ready }
//	func (s *MyServer) Start(ctx context.Context) error {
//	    ln, err := net.Listen("tcp", s.addr)
//	    if err != nil { return err }
//	    close(s.ready)          // signal ready — supervisor proceeds
//	    return s.srv.Serve(ln)  // blocks
//	}
type Starter interface {
	Started() <-chan struct{}
}

// ComponentOption configures a managedComponent at registration time.
type ComponentOption func(*componentConfig)

type componentConfig struct {
	tier          Tier
	restartPolicy RestartPolicy
	deps          []string
}

// WithTier sets the importance tier of a component. Defaults to TierCritical.
func WithTier(t Tier) ComponentOption {
	return func(c *componentConfig) { c.tier = t }
}

// WithRestartPolicy sets the restart policy. Defaults to NeverRestart().
func WithRestartPolicy(p RestartPolicy) ComponentOption {
	return func(c *componentConfig) { c.restartPolicy = p }
}

// WithDependencies declares that this component must not be started until all
// named dependencies are running. Names must match Component.Name() of other
// registered components.
func WithDependencies(names ...string) ComponentOption {
	return func(c *componentConfig) { c.deps = append(c.deps, names...) }
}

// managedComponent pairs a Component with its supervisor metadata.
type managedComponent struct {
	component     Component
	tier          Tier
	restartPolicy RestartPolicy
	deps          []string
}

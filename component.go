package frame

import (
	"context"
	"fmt"
)

// Tier expresses how important a component is to overall application health.
// It controls both the readiness signal exposed by HealthServer and whether a
// persistent failure triggers a full application shutdown.
type Tier int

const (
	// TierCritical (default) — a permanently failed or persistently unhealthy
	// critical component causes the entire application to shut down.
	TierCritical Tier = iota

	// TierSignificant — a persistently unhealthy significant component marks
	// the application as not-ready (/readyz returns 503) but does not trigger
	// a shutdown. A permanent failure (retries exhausted) still shuts the app down.
	TierSignificant

	// TierAuxiliary — health problems are logged and hooks are fired, but they
	// have no effect on /readyz and do not trigger a shutdown. Even a permanent
	// failure only removes the component from the supervisor; the app continues.
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
// nil when it exits cleanly (e.g. because ctx was cancelled) and a non-nil
// error on unexpected failure. The supervisor calls Stop to request a graceful
// exit; the component must honour that by unblocking Start promptly.
//
// Stop is called with a fresh context that carries the configured stop timeout,
// giving the component a bounded window to flush state and release resources.
// Stop must not block longer than that context allows.
type Component interface {
	// Name returns a stable, unique identifier for this component. It is used
	// in logs, hooks, dependency declarations, and health responses.
	Name() string

	// Start runs the component. It must block until the component has exited.
	Start(ctx context.Context) error

	// Stop signals the component to exit and waits for it to do so.
	Stop(ctx context.Context) error
}

// HealthChecker is an optional extension of Component. When a component
// implements this interface the supervisor polls it on the configured
// healthInterval and acts on the result according to the component's Tier.
type HealthChecker interface {
	Health(ctx context.Context) error
}

// ComponentOption configures a managedComponent at registration time.
type ComponentOption func(*componentConfig)

type componentConfig struct {
	tier          Tier
	restartPolicy RestartPolicy
	deps          []string
}

// WithTier sets the importance tier of a component.
// Defaults to TierCritical when not specified.
func WithTier(t Tier) ComponentOption {
	return func(c *componentConfig) { c.tier = t }
}

// WithRestartPolicy sets the restart policy applied when a component's Start
// returns an error or its Health check fails persistently.
// Defaults to NeverRestart() when not specified.
func WithRestartPolicy(p RestartPolicy) ComponentOption {
	return func(c *componentConfig) { c.restartPolicy = p }
}

// WithDependencies declares that this component must not be started until all
// named dependencies are running and healthy (if they implement HealthChecker).
// Names must match Component.Name() of other registered components.
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

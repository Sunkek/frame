package samsara

import (
	"context"
	"fmt"
	"time"
)

// Tier expresses how important a component is to overall application health.
type Tier int

const (
	// TierCritical (default) — a permanently failed or persistently unhealthy
	// critical component causes the entire application to shut down.
	TierCritical Tier = iota

	// TierSignificant — while a significant component is transiently unhealthy
	// the application is marked not-ready (/readyz returns 503) but keeps
	// running. A permanent failure (restart policy exhausted) triggers a full
	// shutdown, identical to TierCritical.
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
// # Lifecycle contract
//
// Start must block for the entire lifetime of the component. The ready
// function must be called exactly once, as soon as the component is ready to
// serve traffic — not before, and never more than once (the supervisor wraps
// it in sync.Once so double-calls are safe, but semantically wrong). The
// supervisor will not start the next component until ready is called. Start
// should return nil on a clean exit (ctx cancelled, Stop called) and a non-nil
// error on unexpected failure.
//
// If ready is never called, the supervisor will wait up to startTimeout and
// then treat the attempt as a failure.
//
// Stop is called with a context carrying the configured stop timeout. It must
// not block longer than that context allows. Stop must be idempotent — the
// supervisor may call it more than once in some shutdown paths. Stop must also
// be safe to call concurrently with a still-running Start (e.g. before a port
// is bound), so components must guard shared state accordingly.
//
// If ctx is cancelled during Start (clean shutdown), Start should return nil.
// Only return a non-nil error when an abnormal failure occurs that the
// supervisor should treat as a crash.
//
// # Background goroutines
//
// Start is allowed to spawn background goroutines, but those goroutines must
// exit when ctx is cancelled or Stop is called — whichever comes first. A
// component that leaks goroutines after Stop returns will cause resource leaks
// on restart. The supervisor has no way to detect or recover from this.
//
// Example — an HTTP server:
//
//	func (s *Server) Start(ctx context.Context, ready func()) error {
//	    ln, err := net.Listen("tcp", s.addr)
//	    if err != nil { return err }
//	    ready()                       // port is bound — supervisor proceeds
//	    return s.srv.Serve(ln)        // blocks until Stop calls Shutdown
//	}
//
// Example — a DB pool (no run loop needed):
//
//	func (p *Pool) Start(ctx context.Context, ready func()) error {
//	    p.stop = make(chan struct{})
//	    pool, err := pgxpool.New(ctx, p.dsn)
//	    if err != nil { return err }
//	    p.pool = pool
//	    ready()                       // pool is up — supervisor proceeds
//	    select {
//	    case <-p.stop:
//	    case <-ctx.Done():
//	    }
//	    return nil
//	}
type Component interface {
	Name() string
	Start(ctx context.Context, ready func()) error
	Stop(ctx context.Context) error
}

// HealthChecker is an optional extension of Component. When implemented, the
// supervisor polls Health on the configured healthInterval and acts on the
// result according to the component's Tier.
type HealthChecker interface {
	Health(ctx context.Context) error
}

// ComponentOption configures a managedComponent at registration time.
type ComponentOption func(*componentConfig)

type componentConfig struct {
	tier          Tier
	restartPolicy RestartPolicy
	deps          []string

	health healthTuning
}

// healthTuning holds the five per-component health knobs. Every field is
// optional: a zero value means "use the supervisor default", so the same type
// carries both the caller's declared overrides and, after resolve, the values
// the health loop actually runs with.
type healthTuning struct {
	interval         time.Duration // 0 = use supervisor default
	timeout          time.Duration // 0 = use supervisor default
	failThreshold    int           // consecutive failures before unhealthy; 0/1 = act immediately
	recoverThreshold int           // consecutive successes before recovered; 0/1 = act immediately
	jitter           float64       // ±fraction applied to the interval; 0 = none
}

// resolve returns the tuning with every unset knob filled in from the
// supervisor's defaults.
func (h healthTuning) resolve(s *Supervisor) healthTuning {
	if h.interval <= 0 {
		h.interval = s.healthInterval
	}
	if h.timeout <= 0 {
		h.timeout = s.healthTimeout
	}
	if h.failThreshold < 1 {
		h.failThreshold = 1
	}
	if h.recoverThreshold < 1 {
		h.recoverThreshold = 1
	}
	return h
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

// WithComponentHealthInterval overrides the supervisor's global health-poll
// interval for this component. Zero (the default) uses the supervisor value.
func WithComponentHealthInterval(d time.Duration) ComponentOption {
	return func(c *componentConfig) { c.health.interval = d }
}

// WithComponentHealthTimeout overrides the supervisor's global per-probe Health
// timeout for this component. Zero (the default) uses the supervisor value.
func WithComponentHealthTimeout(d time.Duration) ComponentOption {
	return func(c *componentConfig) { c.health.timeout = d }
}

// WithHealthThreshold sets how many consecutive failed Health probes mark the
// component unhealthy (fail), and how many consecutive successes mark it
// recovered again (recover). This debounces transient blips: a single failed
// probe no longer flips /readyz or triggers a restart. Values < 1 are treated
// as 1 (act on the first probe, the historical behaviour).
func WithHealthThreshold(fail, recover int) ComponentOption {
	return func(c *componentConfig) {
		c.health.failThreshold = fail
		c.health.recoverThreshold = recover
	}
}

// WithHealthJitter applies a random ±fraction to this component's health-poll
// interval, de-synchronising probes across components and instances. fraction
// is clamped to [0, 1]; 0 (the default) disables jitter. E.g. 0.1 spreads a 10s
// interval over [9s, 11s].
func WithHealthJitter(fraction float64) ComponentOption {
	return func(c *componentConfig) { c.health.jitter = fraction }
}

// managedComponent pairs a Component with its supervisor metadata.
type managedComponent struct {
	component     Component
	tier          Tier
	restartPolicy RestartPolicy
	deps          []string

	health healthTuning
}

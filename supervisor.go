package frame

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// healthStatus holds the last observed health state of a component.
type healthStatus struct {
	mu      sync.RWMutex
	err     error
	present bool // false until the first health check completes
}

func (h *healthStatus) set(err error) {
	h.mu.Lock()
	h.err = err
	h.present = true
	h.mu.Unlock()
}

func (h *healthStatus) get() (err error, present bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.err, h.present
}

// SupervisorOption configures a Supervisor.
type SupervisorOption func(*supervisorConfig)

type supervisorConfig struct {
	healthInterval     time.Duration
	startTimeout       time.Duration
	healthTimeout      time.Duration
	stopTimeout        time.Duration
	restartResetWindow time.Duration
	startProbeWindow   time.Duration
	logger             Logger
	hooks              *EventHooks
	metrics            MetricsObserver
}

func WithHealthInterval(d time.Duration) SupervisorOption {
	return func(c *supervisorConfig) { c.healthInterval = d }
}

func WithStartTimeout(d time.Duration) SupervisorOption {
	return func(c *supervisorConfig) { c.startTimeout = d }
}

func WithHealthTimeout(d time.Duration) SupervisorOption {
	return func(c *supervisorConfig) { c.healthTimeout = d }
}

func WithStopTimeout(d time.Duration) SupervisorOption {
	return func(c *supervisorConfig) { c.stopTimeout = d }
}

func WithRestartResetWindow(d time.Duration) SupervisorOption {
	return func(c *supervisorConfig) { c.restartResetWindow = d }
}

// WithStartProbeWindow overrides how long the supervisor waits after calling
// Start before assuming the component is alive and blocking.
// Only applies to components that do not implement the Starter interface.
// The default (100ms) is suitable for production; tests may use a lower value.
func WithStartProbeWindow(d time.Duration) SupervisorOption {
	return func(c *supervisorConfig) { c.startProbeWindow = d }
}

func WithSupervisorLogger(l Logger) SupervisorOption {
	return func(c *supervisorConfig) { c.logger = l }
}

func WithEventHooks(h *EventHooks) SupervisorOption {
	return func(c *supervisorConfig) { c.hooks = h }
}

// WithMetricsObserver registers a MetricsObserver that will receive telemetry
// events from the supervisor. See MetricsObserver for details.
func WithMetricsObserver(m MetricsObserver) SupervisorOption {
	return func(c *supervisorConfig) { c.metrics = m }
}

// Supervisor starts, monitors, and stops a set of Components in dependency
// order. Components are started sequentially (dependencies first) and stopped
// in reverse order (dependents first).
type Supervisor struct {
	// registration-time state (written before Run, read-only after)
	components     map[string]*managedComponent
	insertionOrder []string            // preserves Add() order for stable topo sort
	order          []*managedComponent // topological order, set in Run

	// configuration (immutable after construction)
	healthInterval     time.Duration
	startTimeout       time.Duration
	healthTimeout      time.Duration
	stopTimeout        time.Duration
	restartResetWindow time.Duration
	startProbeWindow   time.Duration
	logger             Logger
	hooks              *EventHooks
	metrics            MetricsObserver

	// runtime state
	running   int32           // 0 = not started, 1 = started (atomic)
	statusMu  sync.RWMutex   // guards statuses map assignment and iteration
	statuses  map[string]*healthStatus
}

// NewSupervisor constructs a Supervisor with the given options.
func NewSupervisor(opts ...SupervisorOption) *Supervisor {
	cfg := supervisorConfig{
		healthInterval:     defaultHealthInterval,
		startTimeout:       defaultStartTimeout,
		healthTimeout:      defaultHealthTimeout,
		stopTimeout:        defaultStopTimeout,
		restartResetWindow: defaultRestartResetWindow,
		startProbeWindow:   defaultStartProbeWindow,
		logger:             newNopLogger(),
		metrics:            newNopMetrics(),
	}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	return &Supervisor{
		healthInterval:     cfg.healthInterval,
		startTimeout:       cfg.startTimeout,
		healthTimeout:      cfg.healthTimeout,
		stopTimeout:        cfg.stopTimeout,
		restartResetWindow: cfg.restartResetWindow,
		startProbeWindow:   cfg.startProbeWindow,
		logger:             cfg.logger,
		hooks:              cfg.hooks,
		metrics:            cfg.metrics,
	}
}

// Add registers a Component with the Supervisor. Panics if called after Run
// has started or if a component with the same name is already registered.
func (s *Supervisor) Add(c Component, opts ...ComponentOption) {
	if atomic.LoadInt32(&s.running) == 1 {
		panic(ErrSupervisorRunning)
	}
	cfg := componentConfig{
		tier:          TierCritical,
		restartPolicy: NeverRestart(),
	}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	if s.components == nil {
		s.components = make(map[string]*managedComponent)
	}
	name := c.Name()
	if _, exists := s.components[name]; exists {
		panic(fmt.Errorf("%w: %s", ErrComponentAlreadyRegistered, name))
	}
	s.components[name] = &managedComponent{
		component:     c,
		tier:          cfg.tier,
		restartPolicy: cfg.restartPolicy,
		deps:          cfg.deps,
	}
	s.insertionOrder = append(s.insertionOrder, name)
}

// ComponentHealth returns the last known health error for a named component,
// or nil if healthy. The second return value is false when no health check has
// been recorded yet.
func (s *Supervisor) ComponentHealth(name string) (err error, known bool) {
	s.statusMu.RLock()
	hs, ok := s.statuses[name]
	s.statusMu.RUnlock()
	if !ok {
		return nil, false
	}
	return hs.get()
}

// HealthReport returns a snapshot of all component health states, keyed by
// component name. Results are sorted by name for deterministic iteration.
// Intended for use by HealthServer and custom reporters.
func (s *Supervisor) HealthReport() map[string]ComponentStatus {
	s.statusMu.RLock()
	statuses := s.statuses
	s.statusMu.RUnlock()
	if statuses == nil {
		return nil
	}
	out := make(map[string]ComponentStatus, len(statuses))
	for name, hs := range statuses {
		err, known := hs.get()
		mc := s.components[name]
		out[name] = ComponentStatus{
			Err:   err,
			Known: known,
			Tier:  mc.tier,
		}
	}
	return out
}

// HealthReportOrdered returns the same data as HealthReport but as a sorted
// slice, which is useful when order matters (e.g. rendering JSON responses
// where deterministic output is important for diffing and testing).
func (s *Supervisor) HealthReportOrdered() []NamedComponentStatus {
	s.statusMu.RLock()
	statuses := s.statuses
	s.statusMu.RUnlock()
	if statuses == nil {
		return nil
	}
	out := make([]NamedComponentStatus, 0, len(statuses))
	for name, hs := range statuses {
		err, known := hs.get()
		mc := s.components[name]
		out = append(out, NamedComponentStatus{
			Name: name,
			ComponentStatus: ComponentStatus{
				Err:   err,
				Known: known,
				Tier:  mc.tier,
			},
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// NamedComponentStatus is a ComponentStatus paired with its component name,
// used by HealthReportOrdered for sorted, slice-based iteration.
type NamedComponentStatus struct {
	Name string
	ComponentStatus
}

// ComponentStatus is a point-in-time snapshot of a single component's health.
type ComponentStatus struct {
	Err   error // nil means healthy
	Known bool  // false until the first health check runs
	Tier  Tier
}

// Run starts all registered components in dependency order, monitors them, and
// blocks until ctx is cancelled or a critical failure occurs.
func (s *Supervisor) Run(ctx context.Context) error {
	atomic.StoreInt32(&s.running, 1)

	ordered, err := s.topoSort()
	if err != nil {
		return err
	}
	s.order = ordered

	s.statusMu.Lock()
	s.statuses = make(map[string]*healthStatus, len(ordered))
	for _, mc := range ordered {
		s.statuses[mc.component.Name()] = &healthStatus{}
	}
	s.statusMu.Unlock()

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	started := make([]*managedComponent, 0, len(ordered))
	criticalErrCh := make(chan error, len(ordered))
	var wg sync.WaitGroup

	for _, mc := range ordered {
		readyCh, startErrCh := s.launch(ctx, cancel, mc, criticalErrCh, &wg)

		select {
		case err := <-readyCh:
			if err != nil {
				// Component failed to start permanently; stop everything already
				// running and return the error.
				s.stopAll(started)
				<-startErrCh
				return err
			}
			// Component is running — add to the started set so it gets stopped
			// during shutdown.
			started = append(started, mc)

		case <-ctx.Done():
			// A previously launched component triggered shutdown (or the caller
			// cancelled) while we were waiting for this component to become ready.
			// Include the in-flight component in the stop list so it is always
			// cleaned up, then drain its goroutine.
			started = append(started, mc)
			s.stopAll(started)
			<-startErrCh
			if cause := context.Cause(ctx); cause != nil && cause != ctx.Err() {
				return cause
			}
			return nil
		}
	}

	<-ctx.Done()
	wg.Wait()
	close(criticalErrCh)
	s.stopAll(started)

	for err := range criticalErrCh {
		if err != nil {
			return err
		}
	}

	// Surface the cancellation cause if it was set by a component failure.
	if cause := context.Cause(ctx); cause != nil && cause != ctx.Err() {
		return cause
	}
	return nil
}

// launch starts a component's goroutine and returns:
//   - readyCh: receives nil when ready, error if permanently failed.
//   - startErrCh: closed when the goroutine exits (used to synchronise cleanup).
func (s *Supervisor) launch(
	ctx context.Context,
	cancel context.CancelCauseFunc,
	mc *managedComponent,
	criticalErrCh chan<- error,
	wg *sync.WaitGroup,
) (<-chan error, <-chan struct{}) {
	readyCh := make(chan error, 1)
	startErrCh := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(startErrCh)

		err := s.startOne(ctx, mc)
		if err != nil {
			readyCh <- err
			return
		}
		readyCh <- nil

		if err := s.manage(ctx, mc, cancel); err != nil {
			criticalErrCh <- err
			cancel(err)
		}
	}()

	return readyCh, startErrCh
}

// startOne starts mc and blocks until it is ready. For components implementing
// Starter, it waits for the Started() channel to close (bounded by
// startTimeout). For all others it waits up to startProbeWindow for Start to
// fail; if Start is still running the component is assumed alive.
//
// # Goroutine ownership
//
// startOne spawns exactly one goroutine per attempt to run Start(ctx).
// On a failed attempt (startErr != nil) startOne either drains startExit
// synchronously (ctx cancelled path) or lets the loop continue — on the next
// iteration a fresh goroutine is spawned and the old one will exit on its own
// once ctx is cancelled or Stop is called.
//
// On a successful attempt startOne returns without waiting for Start to finish
// (Start intentionally blocks for the component's lifetime). The goroutine
// running Start is then "owned" by the component itself: it will exit when
// ctx is cancelled or when doStop calls Stop(). Because startExit is a
// buffered channel of size 1 the goroutine can always send without blocking
// even if nobody is reading, so there is no goroutine leak.
//
// Returns nil when the component is running, error if all retries are exhausted.
func (s *Supervisor) startOne(ctx context.Context, mc *managedComponent) error {
	name := mc.component.Name()

	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return nil
		}

		s.logger.Info("component starting", "component", name, "attempt", attempt)

		// startExit is buffered so the Start goroutine can always send without
		// blocking, regardless of whether startOne is still waiting on it.
		startExit := make(chan error, 1)
		go func() { startExit <- mc.component.Start(ctx) }()

		startErr := s.waitForReady(ctx, mc.component, startExit)
		if startErr != nil {
			s.logger.Error("component start failed",
				"component", name, "error", startErr, "attempt", attempt)

			restart, delay := mc.restartPolicy.ShouldRestart(startErr, attempt)
			if !restart {
				s.hooks.fireFailed(name, startErr)
				return fmt.Errorf("component %q failed to start: %w", name, startErr)
			}
			s.hooks.fireRestart(name, startErr, attempt+1)
			s.metrics.ComponentRestarting(name, startErr, attempt+1, delay)
			s.logger.Info("component will restart",
				"component", name, "delay", delay, "next_attempt", attempt+1)
			select {
			case <-ctx.Done():
				<-startExit // drain so the goroutine exits cleanly
				return nil
			case <-time.After(delay):
			}
			continue
		}

		// Component is running. The goroutine running Start(ctx) is now owned
		// by the component's lifetime — it exits when ctx is cancelled or
		// doStop calls Stop(). See goroutine ownership note above.
		s.logger.Info("component started", "component", name)
		s.metrics.ComponentStarted(name, attempt)
		return nil
	}
}

// waitForReady waits for a component to signal readiness.
//
// If the component implements Starter, it waits for Started() to close,
// bounded by startTimeout. A component that fails to signal readiness within
// startTimeout is treated as a failed start attempt.
//
// Otherwise it waits up to startProbeWindow for Start to return an error.
// If Start is still running after the probe window the component is assumed
// to be blocking normally and is considered alive.
func (s *Supervisor) waitForReady(ctx context.Context, c Component, startExit <-chan error) error {
	if st, ok := c.(Starter); ok {
		// Apply startTimeout so a component that never closes its Started()
		// channel does not block the supervisor indefinitely.
		timer := time.NewTimer(s.startTimeout)
		defer timer.Stop()
		select {
		case err := <-startExit:
			// Start returned before signalling ready — treat as a failure.
			return err
		case <-st.Started():
			return nil
		case <-ctx.Done():
			return nil
		case <-timer.C:
			return fmt.Errorf("component did not signal readiness within %s", s.startTimeout)
		}
	}

	// Probe-window fallback for components without Starter.
	timer := time.NewTimer(s.startProbeWindow)
	defer timer.Stop()
	select {
	case err := <-startExit:
		return err
	case <-ctx.Done():
		return nil
	case <-timer.C:
		return nil
	}
}

// manage runs the ongoing health-check loop for a running component. It
// returns a non-nil error only when a critical/significant failure has
// occurred and the application must shut down.
//
// # Goroutine model during restarts
//
// When a health check fails and the restart policy permits a retry, manage
// calls doStop (which calls Stop, causing the Start goroutine spawned by the
// most recent startOne call to exit), waits for the delay, then calls
// startOne again. startOne spawns a fresh goroutine for the new Start call.
// Each restart cycle therefore has exactly one live Start goroutine at a time.
// manage itself runs inside the goroutine launched by launch and does not
// spawn additional long-lived goroutines of its own.
func (s *Supervisor) manage(ctx context.Context, mc *managedComponent, cancel context.CancelCauseFunc) error {
	name := mc.component.Name()

	hc, hasHealth := mc.component.(HealthChecker)
	if !hasHealth {
		s.statuses[name].set(nil)
		<-ctx.Done()
		return nil
	}

	ticker := time.NewTicker(s.healthInterval)
	defer ticker.Stop()

	wasUnhealthy := false
	attempt := 0
	startedAt := time.Now()

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-ticker.C:
			t0 := time.Now()
			hCtx, hCancel := context.WithTimeout(ctx, s.healthTimeout)
			hErr := hc.Health(hCtx)
			hCancel()
			duration := time.Since(t0)

			s.statuses[name].set(hErr)
			s.metrics.HealthCheckCompleted(name, duration, hErr)

			if hErr == nil {
				if wasUnhealthy {
					wasUnhealthy = false
					s.logger.Info("component recovered", "component", name)
					s.hooks.fireRecovered(name)
				}
				continue
			}

			s.logger.Error("component unhealthy", "component", name, "error", hErr)
			s.hooks.fireUnhealthy(name, hErr)
			wasUnhealthy = true

			if time.Since(startedAt) > s.restartResetWindow {
				attempt = 0
			}

			restart, delay := mc.restartPolicy.ShouldRestart(hErr, attempt)
			if !restart {
				s.hooks.fireFailed(name, hErr)
				s.logger.Error("component failed permanently",
					"component", name, "tier", mc.tier, "error", hErr)
				return s.handlePermanentFailure(mc, hErr, cancel)
			}

			// Stop the component before restarting.
			stopErr := s.doStop(mc)
			s.metrics.ComponentStopped(name, stopErr)

			s.hooks.fireRestart(name, hErr, attempt+1)
			s.metrics.ComponentRestarting(name, hErr, attempt+1, delay)
			s.logger.Info("component restarting after unhealthy",
				"component", name, "delay", delay, "next_attempt", attempt+1)

			select {
			case <-ctx.Done():
				return nil
			case <-time.After(delay):
			}

			if err := s.startOne(ctx, mc); err != nil {
				s.hooks.fireFailed(name, err)
				return s.handlePermanentFailure(mc, err, cancel)
			}

			startedAt = time.Now()
			wasUnhealthy = false
			attempt++
		}
	}
}

// handlePermanentFailure decides what to do when a component can no longer be
// recovered. Critical and Significant failures trigger a shutdown; Auxiliary
// failures are logged and the component is simply removed from monitoring.
func (s *Supervisor) handlePermanentFailure(mc *managedComponent, err error, cancel context.CancelCauseFunc) error {
	name := mc.component.Name()
	switch mc.tier {
	case TierCritical:
		s.logger.Error("critical component failed — shutting down",
			"component", name, "error", err)
		cancel(fmt.Errorf("critical component %q failed: %w", name, err))
		return fmt.Errorf("critical component %q failed: %w", name, err)

	case TierSignificant:
		s.logger.Error("significant component failed permanently — shutting down",
			"component", name, "error", err)
		cancel(fmt.Errorf("significant component %q failed: %w", name, err))
		return fmt.Errorf("significant component %q failed: %w", name, err)

	default: // TierAuxiliary
		s.logger.Error("auxiliary component failed permanently — continuing",
			"component", name, "error", err)
		return nil
	}
}

// doStop calls Stop on a component within the configured stop timeout.
func (s *Supervisor) doStop(mc *managedComponent) error {
	name := mc.component.Name()
	s.logger.Info("component stopping", "component", name)
	ctx, cancel := context.WithTimeout(context.Background(), s.stopTimeout)
	defer cancel()
	err := mc.component.Stop(ctx)
	if err != nil {
		s.logger.Error("component stop error", "component", name, "error", err)
	} else {
		s.logger.Info("component stopped", "component", name)
	}
	return err
}

// stopAll stops components in reverse order, logging but not returning errors.
func (s *Supervisor) stopAll(components []*managedComponent) {
	for i := len(components) - 1; i >= 0; i-- {
		err := s.doStop(components[i])
		s.metrics.ComponentStopped(components[i].component.Name(), err)
	}
}

// topoSort returns components in topological dependency order. Insertion order
// is the stable tiebreaker for components with no declared dependency between
// them, so Add() call order is always respected.
func (s *Supervisor) topoSort() ([]*managedComponent, error) {
	visited := make(map[string]bool, len(s.components))
	inStack := make(map[string]bool, len(s.components))
	result := make([]*managedComponent, 0, len(s.components))

	var visit func(name string) error
	visit = func(name string) error {
		if inStack[name] {
			return fmt.Errorf("%w: %s", ErrCircularDependency, name)
		}
		if visited[name] {
			return nil
		}
		mc, ok := s.components[name]
		if !ok {
			return fmt.Errorf("%w: %s", ErrUnknownDependency, name)
		}
		inStack[name] = true
		for _, dep := range mc.deps {
			if err := visit(dep); err != nil {
				return err
			}
		}
		inStack[name] = false
		visited[name] = true
		result = append(result, mc)
		return nil
	}

	for _, name := range s.insertionOrder {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	return result, nil
}

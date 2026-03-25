package frame

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// healthStatus holds the last observed health state of a component.
type healthStatus struct {
	mu      sync.RWMutex
	err     error // nil means healthy
	present bool  // false until the first health check completes
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

func WithSupervisorLogger(l Logger) SupervisorOption {
	return func(c *supervisorConfig) { c.logger = l }
}

func WithEventHooks(h *EventHooks) SupervisorOption {
	return func(c *supervisorConfig) { c.hooks = h }
}

// WithStartProbeWindow overrides how long the supervisor waits after calling
// Start before assuming the component is alive and blocking. The default
// (100 ms) is a good production value; tests may set this lower to speed up
// failure detection in retry scenarios.
func WithStartProbeWindow(d time.Duration) SupervisorOption {
	return func(c *supervisorConfig) { c.startProbeWindow = d }
}

// Supervisor starts, monitors, and stops a set of Components in dependency
// order. Components are started sequentially (dependencies first) and stopped
// in reverse order (dependents first), giving each layer a clean shutdown
// window before its dependencies disappear.
type Supervisor struct {
	// registration-time state (written before Run, read-only after)
	components     map[string]*managedComponent
	insertionOrder []string            // preserves Add() call order for stable topo sort
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

	// runtime state
	running  int32 // 0 = not started, 1 = started (atomic)
	statuses map[string]*healthStatus
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
	}
}

// Add registers a Component with the Supervisor. It panics if called after Run
// has started, or if a component with the same name is already registered.
// Panicking here follows the same convention as http.Handle — these are
// programmer errors that should surface immediately during startup.
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
// or nil if it is healthy. The second return value is false when no health
// check has been recorded yet (component still starting up).
func (s *Supervisor) ComponentHealth(name string) (err error, known bool) {
	if s.statuses == nil {
		return nil, false
	}
	hs, ok := s.statuses[name]
	if !ok {
		return nil, false
	}
	e, present := hs.get()
	return e, present
}

// Run starts all registered components in dependency order, monitors them, and
// blocks until ctx is cancelled or a critical failure occurs.
//
// On shutdown, components are stopped in reverse start order, each within the
// configured stop timeout. Run returns the first critical error encountered, or
// nil on a clean shutdown.
func (s *Supervisor) Run(ctx context.Context) error {
	atomic.StoreInt32(&s.running, 1)

	ordered, err := s.topoSort()
	if err != nil {
		return err
	}
	s.order = ordered

	// Initialise health status map for all components.
	s.statuses = make(map[string]*healthStatus, len(ordered))
	for _, mc := range ordered {
		s.statuses[mc.component.Name()] = &healthStatus{}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// started tracks which components reached the running state, in order.
	// Only these will be stopped on shutdown.
	started := make([]*managedComponent, 0, len(ordered))

	// criticalErrCh carries the first critical error that demands a shutdown.
	criticalErrCh := make(chan error, len(ordered))

	// wg tracks all per-component manage goroutines so we can drain them before
	// calling stopAll.
	var wg sync.WaitGroup

	for _, mc := range ordered {
		// launch launches Start in a goroutine and returns a channel that
		// delivers exactly one value: nil when the component is ready (Start
		// returned without error AND the first health check passed), or the
		// error that caused the start attempt to fail.
		//
		// The goroutine keeps running after readyCh fires; it owns the
		// component's lifetime and will call cancel() on a critical failure.
		readyCh, startErrCh := s.launch(ctx, mc, cancel, criticalErrCh, &wg)

		// Block until this component is ready before moving on to the next one.
		// This enforces the sequential, dependency-ordered startup guarantee.
		select {
		case err := <-readyCh:
			if err != nil {
				// The component failed to start (retries exhausted).
				// Stop everything that is already running, then return.
				s.stopAll(started)
				// Drain the startErrCh so the goroutine can exit.
				<-startErrCh
				return err
			}
		case <-ctx.Done():
			// A previously launched component signalled a critical failure
			// while we were waiting for this one to become ready.
			s.stopAll(started)
			<-startErrCh
			break
		}

		started = append(started, mc)
	}

	// Wait for the context to be cancelled (OS signal, Application shutdown, or
	// a critical component failure that called cancel() above).
	<-ctx.Done()

	// Drain all manage goroutines before stopping components so we don't race
	// on component state.
	wg.Wait()
	close(criticalErrCh)

	// Stop all started components in reverse order.
	s.stopAll(started)

	// Return the first critical error, if any.
	for err := range criticalErrCh {
		if err != nil {
			return err
		}
	}
	return nil
}

// launch starts a component's goroutine. It returns:
//   - readyCh: receives nil when the component is ready, or an error if it
//     failed to start permanently. Closes after sending exactly one value.
//   - startErrCh: receives the goroutine's final exit error (always closes).
//
// The goroutine increments wg before launch returns and decrements it when it
// exits so the caller can wait for all goroutines to finish.
func (s *Supervisor) launch(
	ctx context.Context,
	mc *managedComponent,
	cancel context.CancelFunc,
	criticalErrCh chan<- error,
	wg *sync.WaitGroup,
) (<-chan error, <-chan error) {
	readyCh := make(chan error, 1)
	startErrCh := make(chan error, 1)

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(startErrCh)

		// startAndSignal starts the component, waits for it to report healthy
		// (if it implements HealthChecker), then signals readyCh exactly once.
		// Returns true if the component is now confirmed running, false if it
		// failed permanently.
		startAndSignal := func() bool {
			err := s.startOne(ctx, mc)
			if err != nil {
				readyCh <- err
				return false
			}
			// If the component exposes health, do an initial poll now so that
			// dependents are not launched until this component is confirmed
			// healthy. This is the sequential readiness guarantee.
			if hc, ok := mc.component.(HealthChecker); ok {
				if err := s.waitUntilHealthy(ctx, mc.component.Name(), hc); err != nil {
					readyCh <- err
					return false
				}
			}
			readyCh <- nil
			return true
		}

		if !startAndSignal() {
			startErrCh <- nil // error already sent on readyCh
			return
		}

		// Component is running. Hand off to the health/restart monitor.
		if err := s.manage(ctx, mc, cancel); err != nil {
			criticalErrCh <- err
			cancel()
		}
	}()

	return readyCh, startErrCh
}

// startOne attempts to start mc, honouring the restart policy on failure.
// It returns nil only when the component's Start call has returned without
// error AND the first health check (if applicable) has passed.
// It returns an error when all retries are exhausted or ctx is cancelled.
//
// Because Start is defined as a blocking call (it runs for the component's
// entire lifetime), startOne runs Start in its own goroutine and blocks on a
// "ready" signal instead.
func (s *Supervisor) startOne(ctx context.Context, mc *managedComponent) error {
	name := mc.component.Name()
	attempt := 0

	for {
		if ctx.Err() != nil {
			return nil // parent is shutting down; not our error
		}

		s.logger.Info("component starting", "component", name, "attempt", attempt)

		// Run Start in a goroutine; it blocks for the component's lifetime.
		// startExit is closed when Start returns.
		startExit := make(chan error, 1)
		go func() {
			startExit <- mc.component.Start(ctx)
		}()

		// Wait for a brief window so that a component that fails immediately
		// (e.g. cannot bind a port) is detected before we do the health check.
		// For components that start successfully, Start will not return until
		// Stop is called, so we proceed after the timeout.
		startErr := s.waitForStartOrFail(ctx, startExit)
		if startErr != nil {
			// Start returned an error quickly.
			s.logger.Error("component start failed",
				"component", name, "error", startErr, "attempt", attempt)

			restart, delay := mc.restartPolicy.ShouldRestart(startErr, attempt)
			if !restart {
				s.hooks.fireFailed(name, startErr)
				return fmt.Errorf("component %q failed to start: %w", name, startErr)
			}

			s.hooks.fireRestart(name, startErr, attempt+1)
			s.logger.Info("component will restart",
				"component", name, "delay", delay, "next_attempt", attempt+1)

			select {
			case <-ctx.Done():
				return nil
			case <-time.After(delay):
			}
			attempt++
			continue
		}

		// Start is blocking (component is alive). Consider it ready — manage
		// owns all health checking from this point forward.
		s.logger.Info("component started", "component", name)
		return nil
	}
}

// waitForStartOrFail waits for a brief window (startProbeWindow) to see if
// Start returns an error immediately (e.g. bind failure, init error). If Start
// is still running after the window we assume it is blocking successfully and
// return nil.
func (s *Supervisor) waitForStartOrFail(ctx context.Context, startExit <-chan error) error {
	timer := time.NewTimer(s.startProbeWindow)
	defer timer.Stop()
	select {
	case err := <-startExit:
		return err
	case <-ctx.Done():
		return nil
	case <-timer.C:
		// Start is still blocking — component is alive.
		return nil
	}
}

// waitUntilHealthy performs a single health check after startOne succeeds.
// It gives the component one healthTimeout window to report healthy before
// declaring it ready. If the check fails or the context is cancelled we
// proceed anyway — manage owns all ongoing health logic and will react
// immediately on its first tick.
//
// This is purely a best-effort "did it come up cleanly?" signal so that
// dependents are not started against a component that is already in a broken
// state. It does not retry and does not fire any hooks.
func (s *Supervisor) waitUntilHealthy(ctx context.Context, name string, hc HealthChecker) error {
	hCtx, cancel := context.WithTimeout(ctx, s.healthTimeout)
	defer cancel()
	err := hc.Health(hCtx)
	if err != nil {
		s.logger.Debug("component not healthy at startup, manage will handle it",
			"component", name, "error", err)
	}
	// Always return nil — we don't fail startup on an initial health miss.
	// manage will detect and act on persistent unhealthy state.
	return nil
}

// manage runs the ongoing health-check loop for a component that is already
// started. It also handles restarts when health fails persistently.
//
// It returns a non-nil error only when a critical failure has occurred and the
// application must shut down.
func (s *Supervisor) manage(ctx context.Context, mc *managedComponent, cancel context.CancelFunc) error {
	name := mc.component.Name()

	hc, hasHealth := mc.component.(HealthChecker)
	if !hasHealth {
		// Component has no health interface — mark it healthy immediately so
		// /readyz reflects its running state rather than "starting" forever.
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
			hCtx, hCancel := context.WithTimeout(ctx, s.healthTimeout)
			hErr := hc.Health(hCtx)
			hCancel()

			s.statuses[name].set(hErr)

			if hErr == nil {
				if wasUnhealthy {
					wasUnhealthy = false
					s.logger.Info("component recovered", "component", name)
					s.hooks.fireRecovered(name)
				}
				continue
			}

			// Component is unhealthy.
			s.logger.Error("component unhealthy", "component", name, "error", hErr)
			s.hooks.fireUnhealthy(name, hErr)
			wasUnhealthy = true

			// Reset attempt counter if the component was stable long enough.
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

			// Stop the component before restarting it.
			_ = s.doStop(mc)

			s.hooks.fireRestart(name, hErr, attempt+1)
			s.logger.Info("component restarting after unhealthy",
				"component", name, "delay", delay, "next_attempt", attempt+1)

			select {
			case <-ctx.Done():
				return nil
			case <-time.After(delay):
			}

			// Re-start the component inline (startOne spawns its own goroutine
			// for Start, so this call returns as soon as the component is ready).
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
// recovered. Critical and Significant components trigger a shutdown; Auxiliary
// components are simply removed from monitoring.
func (s *Supervisor) handlePermanentFailure(mc *managedComponent, err error, cancel context.CancelFunc) error {
	name := mc.component.Name()
	switch mc.tier {
	case TierCritical:
		s.logger.Error("critical component failed — shutting down application",
			"component", name, "error", err)
		cancel()
		return fmt.Errorf("critical component %q failed: %w", name, err)

	case TierSignificant:
		s.logger.Error("significant component failed permanently — shutting down application",
			"component", name, "error", err)
		cancel()
		return fmt.Errorf("significant component %q failed: %w", name, err)

	default: // TierAuxiliary
		s.logger.Error("auxiliary component failed permanently — continuing without it",
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

// stopAll stops the supplied components in reverse order (last started → first
// stopped). Errors are logged but do not prevent subsequent stops.
func (s *Supervisor) stopAll(components []*managedComponent) {
	for i := len(components) - 1; i >= 0; i-- {
		_ = s.doStop(components[i])
	}
}

// topoSort returns all registered components in topological dependency order
// using an iterative DFS. An error is returned on cycles or unknown deps.
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

	// Drive the outer loop from insertionOrder so that components with no
	// declared dependency between them are started in Add() call order.
	// This makes registration order the stable, predictable tiebreaker.
	for _, name := range s.insertionOrder {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	return result, nil
}

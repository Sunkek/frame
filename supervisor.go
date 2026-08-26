package samsara

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// healthStatus holds the last observed health state of a component.
// tier is set once at Run() time before any goroutine can read it and is
// never mutated again, so it is safe to read without the mu.
type healthStatus struct {
	tier         Tier
	mu           sync.RWMutex
	err          error
	present      bool
	restartCount int
}

func (h *healthStatus) set(err error) {
	h.mu.Lock()
	h.err = err
	h.present = true
	h.mu.Unlock()
}

func (h *healthStatus) incRestarts() {
	h.mu.Lock()
	h.restartCount++
	h.mu.Unlock()
}

func (h *healthStatus) get() (present bool, restartCount int, err error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.present, h.restartCount, h.err
}

// SupervisorOption configures a Supervisor.
type SupervisorOption func(*supervisorConfig)

type supervisorConfig struct {
	healthInterval     time.Duration
	startTimeout       time.Duration
	healthTimeout      time.Duration
	stopTimeout        time.Duration
	restartResetWindow time.Duration
	shutdownGrace      time.Duration
	logger             Logger
	hooks              *EventHooks
	metrics            MetricsObserver
	parallel           bool
}

// WithShutdownGrace bounds the total wall-clock time spent stopping all
// components during a shutdown. When set, each component's Stop context is
// capped so the sum of stop work cannot outlast the grace period: no matter how
// many components hang in Stop, graceful shutdown returns within this budget.
//
// A component's individual Stop is still additionally bounded by WithStopTimeout
// (whichever is smaller applies). When left at zero (the default) there is no
// overall bound and each component receives the full WithStopTimeout — this
// preserves the historical behaviour. When a Supervisor is driven by an
// Application, the Application propagates its WithShutdownTimeout here unless an
// explicit grace was set.
func WithShutdownGrace(d time.Duration) SupervisorOption {
	return func(c *supervisorConfig) { c.shutdownGrace = d }
}

// WithHealthInterval sets how often the supervisor polls each component's
// Health method. Defaults to 10s.
func WithHealthInterval(d time.Duration) SupervisorOption {
	return func(c *supervisorConfig) { c.healthInterval = d }
}

// WithStartTimeout sets how long the supervisor waits for a component to call
// ready() after Start is launched. Defaults to 15s.
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

// WithMetricsObserver registers a MetricsObserver for telemetry events.
func WithMetricsObserver(m MetricsObserver) SupervisorOption {
	return func(c *supervisorConfig) { c.metrics = m }
}

// WithParallelStartStop enables concurrent startup and shutdown of components
// that have no dependency relationship.
//
// By default the supervisor starts components strictly sequentially in
// registration order and stops them in exact reverse order. With this option,
// the only ordering constraint is the dependency graph declared via
// WithDependencies: a component starts as soon as all of its dependencies have
// signalled ready(), and stops only after all of its dependents have stopped.
// Components with no edge between them start and stop concurrently, so their
// relative order is no longer deterministic.
func WithParallelStartStop() SupervisorOption {
	return func(c *supervisorConfig) { c.parallel = true }
}

// Supervisor starts, monitors, and stops a set of Components in dependency
// order. Components are started sequentially (dependencies first) and stopped
// in reverse order (dependents first).
type Supervisor struct {
	components     map[string]*managedComponent
	insertionOrder []string
	order          []*managedComponent

	healthInterval     time.Duration
	startTimeout       time.Duration
	healthTimeout      time.Duration
	stopTimeout        time.Duration
	restartResetWindow time.Duration
	shutdownGrace      time.Duration
	logger             Logger
	hooks              *EventHooks
	metrics            MetricsObserver

	running      int32
	failing      int32        // set to 1 when a Critical/Significant component fails permanently
	stopDeadline atomic.Int64 // unix-nanos hard deadline for all Stop work; 0 = unbounded
	statusMu     sync.RWMutex
	statuses     map[string]*healthStatus
	parallel     bool
}

// Alive reports whether the supervisor is still in a healthy operating state.
// It returns false once a Critical or Significant component has failed
// permanently and the supervisor has begun a failure-driven shutdown. A
// HealthServer consults this to make /livez reflect supervision state rather
// than merely "the health port is open" (see LivenessReporter).
func (s *Supervisor) Alive() bool {
	return atomic.LoadInt32(&s.failing) == 0
}

// NewSupervisor constructs a Supervisor with the given options.
func NewSupervisor(opts ...SupervisorOption) *Supervisor {
	cfg := supervisorConfig{
		healthInterval:     defaultHealthInterval,
		startTimeout:       defaultStartTimeout,
		healthTimeout:      defaultHealthTimeout,
		stopTimeout:        defaultStopTimeout,
		restartResetWindow: defaultRestartResetWindow,
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
		shutdownGrace:      cfg.shutdownGrace,
		logger:             cfg.logger,
		hooks:              cfg.hooks,
		metrics:            cfg.metrics,
		parallel:           cfg.parallel,
	}
}

// setDefaultShutdownGrace sets the overall stop budget only if none was
// configured via WithShutdownGrace. Called by Application before Run so the
// supervisor's total shutdown time is bounded by the application's
// WithShutdownTimeout. A margin is left so the supervisor finishes stopping
// just inside the application's own timeout, avoiding an ErrShutdownTimeout
// race. Must be called before Run.
func (s *Supervisor) setDefaultShutdownGrace(d time.Duration) {
	if s.shutdownGrace <= 0 && d > 0 {
		s.shutdownGrace = d - d/10 // 90% of the app budget
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
		component:        c,
		tier:             cfg.tier,
		restartPolicy:    cfg.restartPolicy,
		deps:             cfg.deps,
		healthInterval:   cfg.healthInterval,
		healthTimeout:    cfg.healthTimeout,
		failThreshold:    cfg.failThreshold,
		recoverThreshold: cfg.recoverThreshold,
		healthJitter:     cfg.healthJitter,
	}
	s.insertionOrder = append(s.insertionOrder, name)
}

// ComponentHealth returns the last known health error for a named component.
func (s *Supervisor) ComponentHealth(name string) (err error, known bool) {
	s.statusMu.RLock()
	hs, ok := s.statuses[name]
	s.statusMu.RUnlock()
	if !ok {
		return nil, false
	}
	known, _, err = hs.get()
	return
}

// HealthReport returns a snapshot of all component health states keyed by name.
func (s *Supervisor) HealthReport() map[string]ComponentStatus {
	s.statusMu.RLock()
	statuses := s.statuses
	s.statusMu.RUnlock()
	if statuses == nil {
		return nil
	}
	out := make(map[string]ComponentStatus, len(statuses))
	for name, hs := range statuses {
		known, restarts, err := hs.get()
		out[name] = ComponentStatus{Err: err, Known: known, Tier: hs.tier, RestartCount: restarts}
	}
	return out
}

// HealthReportOrdered returns a name-sorted slice of component health states.
func (s *Supervisor) HealthReportOrdered() []NamedComponentStatus {
	s.statusMu.RLock()
	statuses := s.statuses
	s.statusMu.RUnlock()
	if statuses == nil {
		return nil
	}
	out := make([]NamedComponentStatus, 0, len(statuses))
	for name, hs := range statuses {
		known, restarts, err := hs.get()
		out = append(out, NamedComponentStatus{
			Name:            name,
			ComponentStatus: ComponentStatus{Err: err, Known: known, Tier: hs.tier, RestartCount: restarts},
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// NamedComponentStatus is a ComponentStatus with its component name.
type NamedComponentStatus struct {
	Name string
	ComponentStatus
}

// ComponentStatus is a point-in-time snapshot of a single component's health.
type ComponentStatus struct {
	Err          error // nil means healthy; non-nil means last health check failed
	Known        bool  // false until the first health check completes
	Tier         Tier
	RestartCount int // number of times the component has been restarted by the supervisor
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
		s.statuses[mc.component.Name()] = &healthStatus{tier: mc.tier}
	}
	s.statusMu.Unlock()

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	if s.parallel {
		return s.runParallel(ctx, cancel, ordered)
	}
	return s.runSequential(ctx, cancel, ordered)
}

// runSequential starts components one at a time in dependency order, waiting
// for each to become ready before starting the next, and stops them in exact
// reverse order. This is the default behaviour.
func (s *Supervisor) runSequential(ctx context.Context, cancel context.CancelCauseFunc, ordered []*managedComponent) error {
	started := make([]*managedComponent, 0, len(ordered))
	criticalErrCh := make(chan error, len(ordered))
	var wg sync.WaitGroup

	for _, mc := range ordered {
		readyCh, startErrCh := s.launch(ctx, cancel, mc, criticalErrCh, &wg)

		select {
		case err := <-readyCh:
			if err != nil {
				// Component failed to start permanently. Stop everything
				// already running and return the error.
				s.stopAll(started)
				<-startErrCh
				return err
			}
			// Component is running — register it for cleanup on shutdown.
			started = append(started, mc)

		case <-ctx.Done():
			// Shutdown fired while waiting for this component to become ready.
			// Include mc in the stop list regardless of whether it managed to
			// call ready() — the goroutine is running and must be cleaned up.
			// stopAll calls Stop on each component (including mc), which will
			// cause any in-progress Start to unblock and return.
			started = append(started, mc)
			s.beginShutdown()
			s.stopAll(started)
			<-startErrCh
			wg.Wait()
			close(criticalErrCh)
			for err := range criticalErrCh {
				if err != nil {
					return err
				}
			}
			if cause := context.Cause(ctx); cause != nil && !isContextError(ctx.Err()) {
				return cause
			}
			return nil
		}
	}

	<-ctx.Done()
	s.beginShutdown()
	s.stopAll(started)
	wg.Wait()
	close(criticalErrCh)

	for err := range criticalErrCh {
		if err != nil {
			return err
		}
	}
	// Surface the cause only if it was set by a component failure. If the
	// context was cancelled by its parent (e.g. an OS signal via
	// signal.NotifyContext), the cause is a context error — that is a clean
	// shutdown and must not be returned as an error.
	if cause := context.Cause(ctx); cause != nil && !isContextError(ctx.Err()) {
		return cause
	}
	return nil
}

// runParallel starts components concurrently, constrained only by the
// dependency graph: each component waits for all of its dependencies to signal
// ready() before its own Start is attempted. On shutdown, each component waits
// for all of its dependents to finish stopping before it is stopped. Components
// with no edge between them run concurrently.
func (s *Supervisor) runParallel(ctx context.Context, cancel context.CancelCauseFunc, ordered []*managedComponent) error {
	criticalErrCh := make(chan error, len(ordered))

	// One ready channel per component, closed when that component has signalled
	// ready(). Built once before any goroutine starts so reads need no lock.
	readyChs := make(map[string]chan struct{}, len(ordered))
	for _, mc := range ordered {
		readyChs[mc.component.Name()] = make(chan struct{})
	}

	var (
		lifeWg    sync.WaitGroup // tracks each component's full lifetime goroutine
		startWg   sync.WaitGroup // tracks completion of the startup phase only
		startedMu sync.Mutex
		started   = make([]*managedComponent, 0, len(ordered))
	)

	for _, mc := range ordered {
		name := mc.component.Name()

		lifeWg.Add(1)
		startWg.Add(1)
		go func() {
			defer lifeWg.Done()

			// Wait for every dependency to become ready, or abort on shutdown.
			for _, dep := range mc.deps {
				select {
				case <-readyChs[dep]:
				case <-ctx.Done():
					startWg.Done()
					return
				}
			}
			if ctx.Err() != nil {
				startWg.Done()
				return
			}

			startExit, err := s.startOne(ctx, mc)
			if err != nil {
				criticalErrCh <- err
				cancel(err)
				startWg.Done()
				return
			}
			// startOne returned nil: either the component signalled ready, or ctx
			// was cancelled mid-start. In both cases its Start goroutine is live
			// and must be stopped, so register it (Stop is idempotent). This
			// mirrors the sequential path, which also adds in-flight components.
			startedMu.Lock()
			started = append(started, mc)
			startedMu.Unlock()
			close(readyChs[name]) // unblock dependents
			startWg.Done()

			if err := s.manage(ctx, mc, cancel, startExit); err != nil {
				criticalErrCh <- err
				cancel(err)
			}
		}()
	}

	<-ctx.Done()
	s.beginShutdown()

	// Let the startup phase settle so the started set is stable before we stop.
	startWg.Wait()
	s.stopAllParallel(started)
	lifeWg.Wait()
	close(criticalErrCh)

	for err := range criticalErrCh {
		if err != nil {
			return err
		}
	}
	if cause := context.Cause(ctx); cause != nil && !isContextError(ctx.Err()) {
		return cause
	}
	return nil
}

// stopAllParallel stops every started component concurrently, ensuring a
// component is stopped only after all of its (started) dependents have stopped.
func (s *Supervisor) stopAllParallel(started []*managedComponent) {
	startedSet := make(map[string]bool, len(started))
	for _, mc := range started {
		startedSet[mc.component.Name()] = true
	}

	// dependents[x] = started components that declared x as a dependency.
	dependents := make(map[string][]string, len(started))
	for _, mc := range started {
		for _, dep := range mc.deps {
			if startedSet[dep] {
				dependents[dep] = append(dependents[dep], mc.component.Name())
			}
		}
	}

	stoppedChs := make(map[string]chan struct{}, len(started))
	for _, mc := range started {
		stoppedChs[mc.component.Name()] = make(chan struct{})
	}

	var wg sync.WaitGroup
	for _, mc := range started {
		name := mc.component.Name()
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Wait for all dependents to stop before stopping this component.
			for _, dep := range dependents[name] {
				<-stoppedChs[dep]
			}
			err := s.doStop(mc)
			s.metrics.ComponentStopped(name, err)
			close(stoppedChs[name])
		}()
	}
	wg.Wait()
}

// launch starts a component's goroutine. Returns:
//   - readyCh: receives nil when the component calls ready(), error if it fails permanently.
//   - startErrCh: closed when the goroutine exits.
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

		startExit, err := s.startOne(ctx, mc)
		if err != nil {
			readyCh <- err
			return
		}
		readyCh <- nil

		if err := s.manage(ctx, mc, cancel, startExit); err != nil {
			criticalErrCh <- err
			cancel(err)
		}
	}()

	return readyCh, startErrCh
}

// startOne attempts to start mc, retrying according to the restart policy.
//
// The supervisor passes a ready() function into Component.Start. When the
// component calls ready(), startOne unblocks and returns nil — the component
// is considered running. If ready() is never called within startTimeout, the
// attempt is treated as a failure.
//
// On a successful start the goroutine running Start(ctx, ready) continues to
// run for the component's lifetime. It exits when ctx is cancelled or Stop is
// called. The startExit channel (buffered, size 1) ensures the goroutine never
// blocks on send after startOne returns.
//
// On success startOne returns the running goroutine's startExit channel so the
// caller (manage) can keep watching for an unexpected post-ready exit. On a
// clean ctx-cancellation mid-start it returns (nil, nil); on permanent start
// failure it returns (nil, err).
func (s *Supervisor) startOne(ctx context.Context, mc *managedComponent) (<-chan error, error) {
	name := mc.component.Name()

	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return nil, nil
		}

		s.logger.Info("component starting", "component", name, "attempt", attempt)

		// readyOnce ensures ready() is safe to call multiple times even if a
		// component implementation accidentally calls it more than once.
		readySignal := make(chan struct{})
		var readyOnce sync.Once
		ready := func() {
			readyOnce.Do(func() { close(readySignal) })
		}

		// startExit is buffered so the Start goroutine can always send without
		// blocking once startOne has returned.
		startExit := make(chan error, 1)
		go func() { startExit <- mc.component.Start(ctx, ready) }()

		// Wait for ready(), a start failure, timeout, or shutdown.
		timer := time.NewTimer(s.startTimeout)
		var startErr error
		select {
		case <-readySignal:
			// Component signalled ready — it is now running.
			timer.Stop()
		case err := <-startExit:
			timer.Stop()
			select {
			case <-readySignal:
				// Start signalled ready() and then returned almost immediately —
				// both channels were ready and select picked startExit. This is a
				// post-ready exit, not a startup failure: hand the exit back so
				// the caller's manage loop applies post-ready crash semantics.
				// startExit is buffered (cap 1) and was just drained, so the
				// re-send cannot block.
				startExit <- err
			default:
				// Start returned before calling ready() — treat as failure.
				startErr = err
			}
		case <-ctx.Done():
			timer.Stop()
			// Drain startExit so the Start goroutine can exit cleanly.
			// Stop will be called by stopAll in Run — not here — because
			// the in-flight component was already added to the started slice.
			<-startExit
			return nil, nil
		case <-timer.C:
			startErr = fmt.Errorf("component did not call ready() within %s", s.startTimeout)
		}

		if startErr == nil {
			// Component is running. Hand the live startExit back so the caller
			// can watch for an unexpected exit after ready().
			s.logger.Info("component started", "component", name)
			s.metrics.ComponentStarted(name, attempt)
			s.hooks.fireReady(name)
			return startExit, nil
		}

		s.logger.Error("component start failed",
			"component", name, "error", startErr, "attempt", attempt)

		restart, delay := mc.restartPolicy.ShouldRestart(startErr, attempt)
		if !restart {
			s.hooks.fireFailed(name, startErr)
			return nil, fmt.Errorf("component %q failed to start: %w", name, startErr)
		}
		s.hooks.fireRestart(name, startErr, attempt+1)
		s.metrics.ComponentRestarting(name, startErr, attempt+1, delay)
		s.logger.Info("component will restart",
			"component", name, "delay", delay, "next_attempt", attempt+1)

		if !sleepCtx(ctx, delay) {
			return nil, nil
		}
	}
}

// sleepCtx waits for d or until ctx is cancelled, reporting true if the full
// delay elapsed and false if ctx was cancelled first. It uses an explicit timer
// (stopped on the cancel path) so the ctx-cancel branch does not leak a pending
// time.After timer until it fires.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// jittered returns d perturbed by ±fraction of its value, spreading health
// probes across components and instances. fraction is clamped to [0, 1]; a
// non-positive fraction returns d unchanged.
func jittered(d time.Duration, fraction float64) time.Duration {
	if fraction <= 0 || d <= 0 {
		return d
	}
	if fraction > 1 {
		fraction = 1
	}
	delta := float64(d) * fraction * (2*rand.Float64() - 1)
	return d + time.Duration(delta)
}

// manage runs the ongoing health-check loop for a running component.
// It accesses s.statuses[name] directly (without statusMu) because the map
// is written exactly once in Run() before any goroutine starts, and is never
// structurally modified afterwards. Only the per-entry healthStatus values are
// mutated, each under their own mu.
//
// manage watches two fault sources for a running component:
//
//   - startExit: the component's Start goroutine returning while ctx is still
//     live. That is an unexpected post-ready exit (a background server/worker
//     crash) and is treated as a fault subject to the restart policy — closing
//     the A1 hole where such crashes were silently lost.
//   - Health(): for components implementing HealthChecker, a failing probe.
//
// Both faults funnel through the same restart-policy / tier handling.
func (s *Supervisor) manage(ctx context.Context, mc *managedComponent, cancel context.CancelCauseFunc, startExit <-chan error) error {
	name := mc.component.Name()
	hc, hasHealth := mc.component.(HealthChecker)

	// Component is running and considered healthy until proven otherwise.
	s.statuses[name].set(nil)

	// Resolve per-component health tuning, falling back to supervisor defaults.
	interval := mc.healthInterval
	if interval <= 0 {
		interval = s.healthInterval
	}
	hTimeout := mc.healthTimeout
	if hTimeout <= 0 {
		hTimeout = s.healthTimeout
	}
	failThreshold := mc.failThreshold
	if failThreshold < 1 {
		failThreshold = 1
	}
	recoverThreshold := mc.recoverThreshold
	if recoverThreshold < 1 {
		recoverThreshold = 1
	}

	var (
		timer  *time.Timer
		timerC <-chan time.Time
	)
	if hasHealth {
		timer = time.NewTimer(jittered(interval, mc.healthJitter))
		defer timer.Stop()
		timerC = timer.C
	}

	unhealthy := false // confirmed-unhealthy state (threshold breached)
	failCount := 0     // consecutive failed probes
	okCount := 0       // consecutive healthy probes while unhealthy
	attempt := 0
	startedAt := time.Now()

	// onFault applies the restart policy after a detected fault (a failed health
	// probe or an unexpected Start exit). It returns done=true with the terminal
	// error when the component must not be restarted; otherwise it restarts the
	// component, rebinds startExit to the new Start goroutine, and returns
	// done=false so the caller keeps monitoring.
	onFault := func(fErr error) (termErr error, done bool) {
		s.statuses[name].set(fErr)
		s.logger.Warn("component unhealthy", "component", name, "error", fErr)
		s.hooks.fireUnhealthy(name, fErr)

		if time.Since(startedAt) > s.restartResetWindow {
			attempt = 0
		}

		restart, delay := mc.restartPolicy.ShouldRestart(fErr, attempt)
		if !restart {
			s.hooks.fireFailed(name, fErr)
			s.logger.Error("component failed permanently",
				"component", name, "tier", mc.tier, "error", fErr)
			return s.handlePermanentFailure(mc, fErr, cancel), true
		}

		// Stop before restarting. Idempotent and safe even if the component has
		// already exited on its own (the post-ready-crash path).
		stopErr := s.doStop(mc)
		s.metrics.ComponentStopped(name, stopErr)

		s.hooks.fireRestart(name, fErr, attempt+1)
		s.metrics.ComponentRestarting(name, fErr, attempt+1, delay)
		s.logger.Info("component restarting",
			"component", name, "delay", delay, "next_attempt", attempt+1)
		s.statuses[name].incRestarts()

		if !sleepCtx(ctx, delay) {
			return nil, true
		}

		newExit, err := s.startOne(ctx, mc)
		if err != nil {
			s.hooks.fireFailed(name, err)
			return s.handlePermanentFailure(mc, err, cancel), true
		}
		if ctx.Err() != nil {
			return nil, true
		}
		startExit = newExit
		s.statuses[name].set(nil)
		startedAt = time.Now()
		attempt++
		return nil, false
	}

	for {
		select {
		case <-ctx.Done():
			return nil

		case exitErr := <-startExit:
			// The Start goroutine returned. If ctx is already cancelled this is a
			// clean shutdown; otherwise it is an unexpected post-ready exit.
			if ctx.Err() != nil {
				return nil
			}
			fErr := exitErr
			if fErr == nil {
				fErr = fmt.Errorf("component %q Start returned unexpectedly after ready", name)
			}
			s.logger.Error("component start goroutine exited unexpectedly",
				"component", name, "error", fErr)
			// A post-ready exit is a confirmed fault, so it enters the unhealthy
			// state exactly as a threshold-breaching probe does. A component with
			// Health then leaves it through the usual sustained-recovery path;
			// one without probes simply has no way back, as before.
			unhealthy = true
			failCount = 0
			okCount = 0
			if termErr, done := onFault(fErr); done {
				return termErr
			}

		case <-timerC:
			// Schedule the next probe up front so the cadence holds regardless of
			// which branch below is taken (transient, recovery, or restart).
			timer.Reset(jittered(interval, mc.healthJitter))

			t0 := time.Now()
			hCtx, hCancel := context.WithTimeout(ctx, hTimeout)
			hErr := hc.Health(hCtx)
			hCancel()
			duration := time.Since(t0)

			// Report every raw probe for observability, but only let the
			// *confirmed* state (after threshold) drive status and restarts.
			s.metrics.HealthCheckCompleted(name, duration, hErr)

			if hErr == nil {
				failCount = 0
				if unhealthy {
					okCount++
					if okCount >= recoverThreshold {
						unhealthy = false
						okCount = 0
						s.statuses[name].set(nil)
						s.logger.Info("component recovered", "component", name)
						s.hooks.fireRecovered(name)
					}
				}
				continue
			}

			okCount = 0
			failCount++
			if failCount < failThreshold {
				// Transient blip: below threshold, do not flip readiness or restart.
				s.logger.Debug("component health probe failed (below threshold)",
					"component", name, "error", hErr, "fails", failCount, "threshold", failThreshold)
				continue
			}

			// Threshold breached — a confirmed fault.
			failCount = 0
			unhealthy = true
			if termErr, done := onFault(hErr); done {
				return termErr
			}
			// Restarted: the component is fresh again. Keep unhealthy=true so a
			// sustained recovery still fires OnRecovered.
			okCount = 0
		}
	}
}

func (s *Supervisor) handlePermanentFailure(mc *managedComponent, err error, cancel context.CancelCauseFunc) error {
	name := mc.component.Name()
	switch mc.tier {
	case TierCritical:
		atomic.StoreInt32(&s.failing, 1)
		s.logger.Error("critical component failed — shutting down", "component", name, "error", err)
		cancel(fmt.Errorf("critical component %q failed: %w", name, err))
		return fmt.Errorf("critical component %q failed: %w", name, err)
	case TierSignificant:
		atomic.StoreInt32(&s.failing, 1)
		s.logger.Error("significant component failed permanently — shutting down", "component", name, "error", err)
		cancel(fmt.Errorf("significant component %q failed: %w", name, err))
		return fmt.Errorf("significant component %q failed: %w", name, err)
	default:
		s.logger.Warn("auxiliary component failed permanently — continuing", "component", name, "error", err)
		return nil
	}
}

// beginShutdown records the hard deadline for all remaining Stop work. It is
// called once when shutdown starts (ctx cancelled). With no shutdownGrace
// configured, no deadline is set and each component keeps its full stopTimeout.
func (s *Supervisor) beginShutdown() {
	if s.shutdownGrace <= 0 {
		return
	}
	s.stopDeadline.CompareAndSwap(0, time.Now().Add(s.shutdownGrace).UnixNano())
}

// stopBudget returns the context timeout to grant a component's Stop: the
// smaller of stopTimeout and the time remaining until the overall shutdown
// deadline. ok is false when the deadline has already passed, signalling that
// this Stop should be abandoned rather than allowed to block the rest.
func (s *Supervisor) stopBudget() (budget time.Duration, ok bool) {
	budget = s.stopTimeout
	if dl := s.stopDeadline.Load(); dl != 0 {
		remaining := time.Until(time.Unix(0, dl))
		if remaining <= 0 {
			return 0, false
		}
		if remaining < budget {
			budget = remaining
		}
	}
	return budget, true
}

func (s *Supervisor) doStop(mc *managedComponent) (err error) {
	name := mc.component.Name()
	s.logger.Info("component stopping", "component", name)
	s.hooks.fireBeforeStop(name)
	defer func() { s.hooks.fireStopped(name, err) }()

	budget, ok := s.stopBudget()
	if !ok {
		// Overall shutdown deadline exceeded: abandon this Stop with an
		// already-cancelled context so a well-behaved Stop returns immediately
		// and cannot block the remaining components past the deadline.
		s.logger.Warn("shutdown deadline exceeded — abandoning stop", "component", name)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err = mc.component.Stop(ctx)
		if err != nil {
			s.logger.Error("component stop error", "component", name, "error", err)
		}
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	err = mc.component.Stop(ctx)
	if err != nil {
		s.logger.Error("component stop error", "component", name, "error", err)
	} else {
		s.logger.Info("component stopped", "component", name)
	}
	return err
}

func (s *Supervisor) stopAll(components []*managedComponent) {
	for i := len(components) - 1; i >= 0; i-- {
		err := s.doStop(components[i])
		s.metrics.ComponentStopped(components[i].component.Name(), err)
	}
}

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

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
// tier is set once at Run() time before any goroutine can read it and is
// never mutated again, so it is safe to read without the mu.
type healthStatus struct {
	tier    Tier
	mu      sync.RWMutex
	err     error
	present bool
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
	logger             Logger
	hooks              *EventHooks
	metrics            MetricsObserver
}

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
	logger             Logger
	hooks              *EventHooks
	metrics            MetricsObserver

	running  int32
	statusMu sync.RWMutex
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

// ComponentHealth returns the last known health error for a named component.
func (s *Supervisor) ComponentHealth(name string) (err error, known bool) {
	s.statusMu.RLock()
	hs, ok := s.statuses[name]
	s.statusMu.RUnlock()
	if !ok {
		return nil, false
	}
	return hs.get()
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
		err, known := hs.get()
		out[name] = ComponentStatus{Err: err, Known: known, Tier: hs.tier}
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
		err, known := hs.get()
		out = append(out, NamedComponentStatus{
			Name:            name,
			ComponentStatus: ComponentStatus{Err: err, Known: known, Tier: hs.tier},
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
	Err   error
	Known bool
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
		s.statuses[mc.component.Name()] = &healthStatus{tier: mc.tier}
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
				s.stopAll(started)
				<-startErrCh
				return err
			}
			started = append(started, mc)

		case <-ctx.Done():
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
	if cause := context.Cause(ctx); cause != nil && cause != ctx.Err() {
		return cause
	}
	return nil
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
func (s *Supervisor) startOne(ctx context.Context, mc *managedComponent) error {
	name := mc.component.Name()

	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return nil
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
			// Start returned before calling ready() — treat as failure.
			timer.Stop()
			startErr = err
		case <-ctx.Done():
			timer.Stop()
			// The outer stopAll will call Stop on this component. Calling it
			// here too would be a double-Stop and may leave the Start goroutine
			// permanently blocked if the component's Stop is not idempotent.
			// Just drain the goroutine so wg never stalls.
			<-startExit
			return nil
		case <-timer.C:
			startErr = fmt.Errorf("component did not call ready() within %s", s.startTimeout)
		}

		if startErr == nil {
			// Component is running.
			s.logger.Info("component started", "component", name)
			s.metrics.ComponentStarted(name, attempt)
			return nil
		}

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
			return nil
		case <-time.After(delay):
		}
	}
}

// manage runs the ongoing health-check loop for a running component.
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

func (s *Supervisor) handlePermanentFailure(mc *managedComponent, err error, cancel context.CancelCauseFunc) error {
	name := mc.component.Name()
	switch mc.tier {
	case TierCritical:
		s.logger.Error("critical component failed — shutting down", "component", name, "error", err)
		cancel(fmt.Errorf("critical component %q failed: %w", name, err))
		return fmt.Errorf("critical component %q failed: %w", name, err)
	case TierSignificant:
		s.logger.Error("significant component failed permanently — shutting down", "component", name, "error", err)
		cancel(fmt.Errorf("significant component %q failed: %w", name, err))
		return fmt.Errorf("significant component %q failed: %w", name, err)
	default:
		s.logger.Error("auxiliary component failed permanently — continuing", "component", name, "error", err)
		return nil
	}
}

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

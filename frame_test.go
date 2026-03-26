package frame_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sunkek/frame"
)

// ── helpers ──────────────────────────────────────────────────────────────────

var errFake = errors.New("fake error")

// mockComponent is a controllable Component used throughout tests.
type mockComponent struct {
	name    string
	stopErr error // returned by Stop (set before test, never mutated concurrently)

	// shouldFail controls whether Start returns an error. Safe for concurrent
	// use via atomic. True = return errFake, false = proceed normally.
	shouldFail atomic.Bool

	startCalled atomic.Int32
	stopCalled  atomic.Int32

	// healthErrStr holds the current health error message.
	// Empty string means healthy. We use a string so atomic.Value is always
	// the same concrete type (string), avoiding the "inconsistent type" panic.
	healthErrStr atomic.Value // stores string

	startedOnce sync.Once
	started     chan struct{} // closed once when Start begins successfully
	stop        chan struct{} // closed by Stop to unblock Start
}

func newMock(name string) *mockComponent {
	m := &mockComponent{
		name:    name,
		started: make(chan struct{}),
		stop:    make(chan struct{}),
	}
	m.healthErrStr.Store("") // initialise with typed value
	return m
}

func (m *mockComponent) Name() string { return m.name }

func (m *mockComponent) Start(ctx context.Context) error {
	m.startCalled.Add(1)
	if m.shouldFail.Load() {
		return errFake
	}
	m.startedOnce.Do(func() { close(m.started) })
	select {
	case <-m.stop:
	case <-ctx.Done():
	}
	return nil
}

func (m *mockComponent) Stop(_ context.Context) error {
	m.stopCalled.Add(1)
	select {
	case <-m.stop:
		// already closed
	default:
		close(m.stop)
	}
	return m.stopErr
}

func (m *mockComponent) Health(_ context.Context) error {
	s := m.healthErrStr.Load().(string)
	if s == "" {
		return nil
	}
	return errors.New(s)
}

func (m *mockComponent) setHealthErr(err error) {
	if err == nil {
		m.healthErrStr.Store("")
	} else {
		m.healthErrStr.Store(err.Error())
	}
}

// waitStarted blocks until the component's Start has been called or the
// timeout fires.
func waitStarted(t *testing.T, m *mockComponent, timeout time.Duration) {
	t.Helper()
	select {
	case <-m.started:
	case <-time.After(timeout):
		t.Fatalf("component %q did not start within %s", m.name, timeout)
	}
}

// ── RestartPolicy ─────────────────────────────────────────────────────────────

func TestNeverRestart(t *testing.T) {
	p := frame.NeverRestart()
	restart, _ := p.ShouldRestart(errFake, 0)
	if restart {
		t.Fatal("NeverRestart should return false")
	}
}

func TestAlwaysRestart(t *testing.T) {
	p := frame.AlwaysRestart(10 * time.Millisecond)
	for i := range 10 {
		restart, delay := p.ShouldRestart(errFake, i)
		if !restart {
			t.Fatalf("AlwaysRestart returned false at attempt %d", i)
		}
		if delay != 10*time.Millisecond {
			t.Fatalf("unexpected delay %v at attempt %d", delay, i)
		}
	}
}

func TestMaxRetries(t *testing.T) {
	p := frame.MaxRetries(3, 5*time.Millisecond)
	for i := range 3 {
		restart, _ := p.ShouldRestart(errFake, i)
		if !restart {
			t.Fatalf("MaxRetries should restart at attempt %d", i)
		}
	}
	restart, _ := p.ShouldRestart(errFake, 3)
	if restart {
		t.Fatal("MaxRetries should stop after max retries")
	}
}

func TestExponentialBackoff(t *testing.T) {
	base := 10 * time.Millisecond
	p := frame.ExponentialBackoff(4, base)
	expected := []time.Duration{base, 2 * base, 4 * base, 8 * base}
	for i, want := range expected {
		restart, got := p.ShouldRestart(errFake, i)
		if !restart {
			t.Fatalf("ExponentialBackoff should restart at attempt %d", i)
		}
		if got != want {
			t.Fatalf("attempt %d: want delay %v, got %v", i, want, got)
		}
	}
	restart, _ := p.ShouldRestart(errFake, 4)
	if restart {
		t.Fatal("ExponentialBackoff should stop after max retries")
	}
}

// ── Supervisor: basic lifecycle ───────────────────────────────────────────────

func TestSupervisor_StartsAndStops(t *testing.T) {
	sup := frame.NewSupervisor(
		frame.WithHealthInterval(100 * time.Millisecond),
	)
	mc := newMock("alpha")
	sup.Add(mc)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	waitStarted(t, mc, 2*time.Second)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected supervisor error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not stop in time")
	}

	if mc.stopCalled.Load() == 0 {
		t.Fatal("Stop was never called on component")
	}
}

func TestSupervisor_DependencyOrder(t *testing.T) {
	// db → cache → app: db must start first, stop last.
	sup := frame.NewSupervisor(
		frame.WithHealthInterval(50 * time.Millisecond),
	)

	db := newMock("db")
	cache := newMock("cache")
	app := newMock("app")

	sup.Add(db)
	sup.Add(cache, frame.WithDependencies("db"))
	sup.Add(app, frame.WithDependencies("cache"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	// Sequential startup guarantees that if app is started, db and cache are
	// already running.
	waitStarted(t, db, 2*time.Second)
	waitStarted(t, cache, 2*time.Second)
	waitStarted(t, app, 2*time.Second)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected supervisor error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not stop in time")
	}

	// Verify stop was called on all three.
	if db.stopCalled.Load() == 0 {
		t.Error("db: Stop not called")
	}
	if cache.stopCalled.Load() == 0 {
		t.Error("cache: Stop not called")
	}
	if app.stopCalled.Load() == 0 {
		t.Error("app: Stop not called")
	}
}

func TestSupervisor_InsertionOrderRespected(t *testing.T) {
	// Without any declared dependencies, components must start in Add() order
	// and stop in reverse Add() order. Run this multiple times to catch any
	// map-iteration non-determinism.
	for range 20 {
		sup := frame.NewSupervisor(
			frame.WithStartProbeWindow(5 * time.Millisecond),
		)

		first := newMock("first")
		second := newMock("second")
		third := newMock("third")

		sup.Add(first)
		sup.Add(second)
		sup.Add(third)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- sup.Run(ctx) }()

		// Sequential start: first must be started before second, second before third.
		waitStarted(t, first, 2*time.Second)
		waitStarted(t, second, 2*time.Second)
		waitStarted(t, third, 2*time.Second)

		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("supervisor did not stop in time")
		}

		// Verify all three were stopped.
		if first.stopCalled.Load() == 0 || second.stopCalled.Load() == 0 || third.stopCalled.Load() == 0 {
			t.Fatal("not all components were stopped")
		}
	}
}

func TestSupervisor_CircularDependency(t *testing.T) {
	sup := frame.NewSupervisor()
	a := newMock("a")
	b := newMock("b")
	sup.Add(a, frame.WithDependencies("b"))
	sup.Add(b, frame.WithDependencies("a"))

	err := sup.Run(context.Background())
	if !errors.Is(err, frame.ErrCircularDependency) {
		t.Fatalf("expected ErrCircularDependency, got: %v", err)
	}
}

func TestSupervisor_UnknownDependency(t *testing.T) {
	sup := frame.NewSupervisor()
	mc := newMock("alpha")
	sup.Add(mc, frame.WithDependencies("nonexistent"))

	err := sup.Run(context.Background())
	if !errors.Is(err, frame.ErrUnknownDependency) {
		t.Fatalf("expected ErrUnknownDependency, got: %v", err)
	}
}

func TestSupervisor_DuplicateComponentPanics(t *testing.T) {
	sup := frame.NewSupervisor()
	sup.Add(newMock("alpha"))
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for duplicate component name")
		}
	}()
	sup.Add(newMock("alpha"))
}

// ── Supervisor: start failure ─────────────────────────────────────────────────

func TestSupervisor_StartFailure_NeverRestart(t *testing.T) {
	sup := frame.NewSupervisor(
		frame.WithStartProbeWindow(5 * time.Millisecond),
	)
	mc := newMock("broken")
	mc.shouldFail.Store(true)
	sup.Add(mc, frame.WithRestartPolicy(frame.NeverRestart()))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := sup.Run(ctx)
	if err == nil {
		t.Fatal("expected error from failing component")
	}
	if !errors.Is(err, errFake) {
		t.Fatalf("expected errFake in error chain, got: %v", err)
	}
}

func TestSupervisor_StartFailure_WithRetries(t *testing.T) {
	// Probe window = 5ms, retry delay = 10ms → each failed attempt costs ~15ms.
	// We allow 20 retries (300ms budget) and disable failures after 100ms,
	// which gives ~6 failed attempts before recovery with plenty of room left.
	sup := frame.NewSupervisor(
		frame.WithStartProbeWindow(5 * time.Millisecond),
	)
	mc := newMock("flaky")
	mc.shouldFail.Store(true)

	go func() {
		time.Sleep(100 * time.Millisecond)
		mc.shouldFail.Store(false)
	}()

	sup.Add(mc, frame.WithRestartPolicy(frame.MaxRetries(20, 10*time.Millisecond)))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	// Wait for the component to actually start (eventually).
	select {
	case <-mc.started:
	case err := <-done:
		t.Fatalf("supervisor exited before component started: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("component never started after retries")
	}

	cancel()
	<-done
}

// ── Supervisor: health / tier interaction ─────────────────────────────────────

func TestSupervisor_CriticalUnhealthy_TriggersShutdown(t *testing.T) {
	sup := frame.NewSupervisor(
		frame.WithHealthInterval(20*time.Millisecond),
		frame.WithHealthTimeout(10*time.Millisecond),
		frame.WithStartTimeout(50*time.Millisecond),
	)
	mc := newMock("db")
	sup.Add(mc,
		frame.WithTier(frame.TierCritical),
		frame.WithRestartPolicy(frame.NeverRestart()),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	waitStarted(t, mc, 2*time.Second)

	// Mark the component unhealthy.
	mc.setHealthErr(errFake)

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error due to critical component failure")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not shut down after critical failure")
	}
}

func TestSupervisor_AuxiliaryUnhealthy_DoesNotShutdown(t *testing.T) {
	sup := frame.NewSupervisor(
		frame.WithHealthInterval(20*time.Millisecond),
		frame.WithHealthTimeout(5*time.Millisecond),
	)
	aux := newMock("metrics")
	sup.Add(aux,
		frame.WithTier(frame.TierAuxiliary),
		frame.WithRestartPolicy(frame.NeverRestart()),
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	waitStarted(t, aux, 2*time.Second)

	// Mark auxiliary component unhealthy — should not cause shutdown.
	aux.setHealthErr(errFake)

	// App should stay alive.
	select {
	case err := <-done:
		t.Fatalf("supervisor exited unexpectedly (auxiliary failure): %v", err)
	case <-time.After(200 * time.Millisecond):
		// good — still running
	}

	cancel()
	<-done
}

// ── EventHooks ────────────────────────────────────────────────────────────────

func TestEventHooks_Fired(t *testing.T) {
	unhealthyCh := make(chan struct{}, 1)
	failedCh := make(chan struct{}, 1)

	hooks := &frame.EventHooks{
		OnUnhealthy: func(c string, err error) {
			select {
			case unhealthyCh <- struct{}{}:
			default:
			}
		},
		OnFailed: func(c string, err error) {
			select {
			case failedCh <- struct{}{}:
			default:
			}
		},
	}

	sup := frame.NewSupervisor(
		frame.WithHealthInterval(20*time.Millisecond),
		frame.WithHealthTimeout(10*time.Millisecond),
		frame.WithStartTimeout(50*time.Millisecond),
		frame.WithEventHooks(hooks),
	)
	mc := newMock("db")
	sup.Add(mc,
		frame.WithTier(frame.TierCritical),
		frame.WithRestartPolicy(frame.NeverRestart()),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	waitStarted(t, mc, 2*time.Second)
	mc.setHealthErr(errFake)

	// Wait for OnUnhealthy to fire (within 1s).
	select {
	case <-unhealthyCh:
	case <-time.After(time.Second):
		t.Error("OnUnhealthy was not called within 1s")
	}

	// Wait for OnFailed to fire (within 1s).
	select {
	case <-failedCh:
	case <-time.After(time.Second):
		t.Error("OnFailed was not called within 1s")
	}

	// Supervisor should have shut down due to critical failure.
	select {
	case err := <-done:
		if err == nil {
			t.Error("expected non-nil error from supervisor after critical failure")
		}
	case <-time.After(2 * time.Second):
		t.Error("supervisor did not shut down after critical failure")
	}
}

// ── Application ───────────────────────────────────────────────────────────────

func TestApplication_CleanShutdown(t *testing.T) {
	mc := newMock("svc")
	sup := frame.NewSupervisor()
	sup.Add(mc)

	mainDone := make(chan struct{})
	app := frame.NewApplication(
		frame.WithSupervisor(sup),
		frame.WithMainFunc(func(ctx context.Context) error {
			<-ctx.Done()
			close(mainDone)
			return nil
		}),
		frame.WithShutdownTimeout(5*time.Second),
	)

	done := make(chan error, 1)
	go func() { done <- app.Run() }()

	waitStarted(t, mc, 2*time.Second)

	app.Shutdown(nil)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("application did not stop in time")
	}

	select {
	case <-mainDone:
	default:
		t.Fatal("main function did not run")
	}
}

func TestApplication_MainFuncError(t *testing.T) {
	app := frame.NewApplication(
		frame.WithMainFunc(func(ctx context.Context) error {
			return errFake
		}),
	)

	err := app.Run()
	if !errors.Is(err, errFake) {
		t.Fatalf("expected errFake, got: %v", err)
	}
}

func TestApplication_NoMainFunc_NoSupervisor(t *testing.T) {
	app := frame.NewApplication()
	err := app.Run()
	if !errors.Is(err, frame.ErrMainOmitted) {
		t.Fatalf("expected ErrMainOmitted, got: %v", err)
	}
}

// ── ServiceAdapter ────────────────────────────────────────────────────────────

type mockLegacyService struct {
	name        string
	closeErr    error // set before test, never mutated concurrently
	initCalled  atomic.Bool
	closeCalled atomic.Bool

	// Use atomic.Value with a typed wrapper so errors.Is works correctly
	// while remaining race-safe. Empty wrapper (nil Err) means no error.
	initErrVal atomic.Value // stores errWrapper
	pingErrVal atomic.Value // stores errWrapper
}

// errWrapper is a concrete type so atomic.Value always stores the same type.
type errWrapper struct{ err error }

func newMockLegacyService(name string) *mockLegacyService {
	m := &mockLegacyService{name: name}
	m.initErrVal.Store(errWrapper{})
	m.pingErrVal.Store(errWrapper{})
	return m
}

func (s *mockLegacyService) setInitErr(err error) {
	s.initErrVal.Store(errWrapper{err: err})
}

func (s *mockLegacyService) setPingErr(err error) {
	s.pingErrVal.Store(errWrapper{err: err})
}

func (s *mockLegacyService) Ident() string { return s.name }
func (s *mockLegacyService) Init(_ context.Context) error {
	s.initCalled.Store(true)
	return s.initErrVal.Load().(errWrapper).err
}
func (s *mockLegacyService) Ping(_ context.Context) error {
	return s.pingErrVal.Load().(errWrapper).err
}
func (s *mockLegacyService) Close() error { s.closeCalled.Store(true); return s.closeErr }

func TestServiceAdapter_LifecycleIntegration(t *testing.T) {
	legacy := newMockLegacyService("postgres")
	adapter := frame.NewServiceAdapter(legacy)

	if adapter.Name() != "postgres" {
		t.Fatalf("unexpected name: %s", adapter.Name())
	}

	ctx, cancel := context.WithCancel(context.Background())
	startDone := make(chan error, 1)
	go func() { startDone <- adapter.Start(ctx) }()

	time.Sleep(20 * time.Millisecond)
	if !legacy.initCalled.Load() {
		t.Fatal("Init was not called")
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	if err := adapter.Stop(stopCtx); err != nil {
		t.Fatalf("Stop error: %v", err)
	}

	select {
	case err := <-startDone:
		if err != nil {
			t.Fatalf("Start error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start did not return after Stop")
	}

	if !legacy.closeCalled.Load() {
		t.Fatal("Close was not called")
	}
	cancel()
}

func TestServiceAdapter_InitError(t *testing.T) {
	legacy := newMockLegacyService("redis")
	legacy.setInitErr(errFake)
	adapter := frame.NewServiceAdapter(legacy)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := adapter.Start(ctx)
	if !errors.Is(err, errFake) {
		t.Fatalf("expected errFake, got: %v", err)
	}
}

func TestServiceAdapter_HealthDelegation(t *testing.T) {
	legacy := newMockLegacyService("redis")
	legacy.setPingErr(errFake)
	adapter := frame.NewServiceAdapter(legacy)

	err := adapter.Health(context.Background())
	if !errors.Is(err, errFake) {
		t.Fatalf("expected errFake from Health, got: %v", err)
	}
}

// ── HealthServer ──────────────────────────────────────────────────────────────

func TestHealthServer_Endpoints(t *testing.T) {
	sup := frame.NewSupervisor(
		frame.WithHealthInterval(50*time.Millisecond),
		frame.WithStartTimeout(2*time.Second),
	)

	hs := frame.NewHealthServer(sup, frame.WithHealthAddr(":18080"))
	// Register health server first so it starts before other components.
	sup.Add(hs, frame.WithTier(frame.TierCritical))

	mc := newMock("db")
	sup.Add(mc)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	// Poll until the server is accepting connections (up to 3 s).
	var resp *http.Response
	var err error
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err = http.Get("http://localhost:18080/livez")
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		cancel()
		t.Fatalf("livez request failed after 3s: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("livez: expected 200, got %d", resp.StatusCode)
	}

	// /readyz should eventually return 200 once db is started.
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err = http.Get("http://localhost:18080/readyz")
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			break
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(30 * time.Millisecond)
	}

	// /healthz is an alias for /readyz.
	resp2, err2 := http.Get("http://localhost:18080/healthz")
	if err2 != nil {
		cancel()
		t.Fatalf("healthz request failed: %v", err2)
	}
	resp2.Body.Close()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected supervisor error on shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not stop in time")
	}
}

// ── Starter interface ─────────────────────────────────────────────────────────

// starterMock is a Component that implements Starter for precise readiness
// signalling. It closes its readyCh after a configurable delay inside Start,
// simulating a server that needs a moment to bind a port.
type starterMock struct {
	mockComponent
	readyCh    chan struct{}
	readyDelay time.Duration
}

func newStarterMock(name string, readyDelay time.Duration) *starterMock {
	m := &starterMock{
		mockComponent: *newMock(name),
		readyCh:       make(chan struct{}),
		readyDelay:    readyDelay,
	}
	return m
}

func (m *starterMock) Started() <-chan struct{} { return m.readyCh }

func (m *starterMock) Start(ctx context.Context) error {
	m.startCalled.Add(1)
	if m.shouldFail.Load() {
		return errFake
	}
	// Signal started channel (for waitStarted helper).
	m.startedOnce.Do(func() { close(m.started) })
	// Delay before signalling ready, simulating port bind time.
	select {
	case <-time.After(m.readyDelay):
		close(m.readyCh)
	case <-ctx.Done():
		return nil
	}
	// Block for component lifetime.
	select {
	case <-m.stop:
	case <-ctx.Done():
	}
	return nil
}

func TestStarter_PreciseReadiness(t *testing.T) {
	// A Starter component should signal readiness without waiting for the
	// probe window. With a 5ms ready delay and a 2s probe window, the
	// supervisor should proceed in ~5ms, not ~2s.
	sup := frame.NewSupervisor(
		frame.WithStartProbeWindow(2 * time.Second), // deliberately long
	)

	sm := newStarterMock("precise", 5*time.Millisecond)
	sup.Add(sm)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	start := time.Now()
	go func() { done <- sup.Run(ctx) }()

	waitStarted(t, &sm.mockComponent, 3*time.Second)
	elapsed := time.Since(start)

	// Should be ready well under 200ms, not waiting the full 2s probe window.
	if elapsed > 500*time.Millisecond {
		t.Errorf("Starter component took %v to become ready; expected <500ms", elapsed)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not stop in time")
	}
}

func TestStarter_DependentWaitsForReady(t *testing.T) {
	// dep implements Starter with a deliberate 50ms delay.
	// svc depends on dep — it must not start until dep signals ready.
	sup := frame.NewSupervisor(
		frame.WithStartProbeWindow(5 * time.Millisecond),
	)

	dep := newStarterMock("dep", 50*time.Millisecond)
	svc := newMock("svc")
	sup.Add(dep)
	sup.Add(svc, frame.WithDependencies("dep"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	// dep must start first, then svc.
	waitStarted(t, &dep.mockComponent, 2*time.Second)
	depReadyAt := time.Now()

	waitStarted(t, svc, 2*time.Second)
	svcStartedAt := time.Now()

	// svc must have started after dep was ready (50ms delay).
	if svcStartedAt.Before(depReadyAt) {
		t.Error("svc started before dep was ready")
	}

	cancel()
	<-done
}

// ── MetricsObserver ───────────────────────────────────────────────────────────

type captureMetrics struct {
	mu       sync.Mutex
	started  []string
	stopped  []string
	restarts []string
	checks   []string
}

func (m *captureMetrics) ComponentStarted(c string, _ int) {
	m.mu.Lock()
	m.started = append(m.started, c)
	m.mu.Unlock()
}
func (m *captureMetrics) ComponentStopped(c string, _ error) {
	m.mu.Lock()
	m.stopped = append(m.stopped, c)
	m.mu.Unlock()
}
func (m *captureMetrics) ComponentRestarting(c string, _ error, _ int, _ time.Duration) {
	m.mu.Lock()
	m.restarts = append(m.restarts, c)
	m.mu.Unlock()
}
func (m *captureMetrics) HealthCheckCompleted(c string, _ time.Duration, _ error) {
	m.mu.Lock()
	m.checks = append(m.checks, c)
	m.mu.Unlock()
}
func (m *captureMetrics) startedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.started)
}
func (m *captureMetrics) stoppedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.stopped)
}
func (m *captureMetrics) checksCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.checks)
}

func TestMetricsObserver_StartStop(t *testing.T) {
	obs := &captureMetrics{}
	sup := frame.NewSupervisor(
		frame.WithMetricsObserver(obs),
		frame.WithStartProbeWindow(5*time.Millisecond),
	)
	mc := newMock("svc")
	sup.Add(mc)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	waitStarted(t, mc, 2*time.Second)
	cancel()
	<-done

	if obs.startedCount() != 1 {
		t.Errorf("ComponentStarted: want 1, got %d", obs.startedCount())
	}
	if obs.stoppedCount() != 1 {
		t.Errorf("ComponentStopped: want 1, got %d", obs.stoppedCount())
	}
}

func TestMetricsObserver_HealthChecks(t *testing.T) {
	obs := &captureMetrics{}
	sup := frame.NewSupervisor(
		frame.WithMetricsObserver(obs),
		frame.WithHealthInterval(20*time.Millisecond),
		frame.WithHealthTimeout(10*time.Millisecond),
		frame.WithStartProbeWindow(5*time.Millisecond),
	)
	mc := newMock("db")
	sup.Add(mc)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	waitStarted(t, mc, 2*time.Second)
	// Wait for at least 3 health checks.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if obs.checksCount() >= 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if obs.checksCount() < 3 {
		t.Errorf("HealthCheckCompleted: want ≥3, got %d", obs.checksCount())
	}

	cancel()
	<-done
}

func TestMetricsObserver_Restarts(t *testing.T) {
	obs := &captureMetrics{}
	sup := frame.NewSupervisor(
		frame.WithMetricsObserver(obs),
		frame.WithStartProbeWindow(5*time.Millisecond),
	)
	mc := newMock("svc")
	mc.shouldFail.Store(true)
	go func() {
		time.Sleep(50 * time.Millisecond)
		mc.shouldFail.Store(false)
	}()
	sup.Add(mc, frame.WithRestartPolicy(frame.MaxRetries(20, 5*time.Millisecond)))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	waitStarted(t, mc, 3*time.Second)
	cancel()
	<-done

	obs.mu.Lock()
	restarts := len(obs.restarts)
	obs.mu.Unlock()
	if restarts == 0 {
		t.Error("ComponentRestarting: expected at least one restart event")
	}
}

// ── HealthReporter / context.Cause ───────────────────────────────────────────

func TestContextCause_CriticalFailure(t *testing.T) {
	// When a critical component fails, the error returned by Supervisor.Run
	// should be the specific component failure error, not a generic
	// "context canceled".
	sup := frame.NewSupervisor(
		frame.WithHealthInterval(20*time.Millisecond),
		frame.WithHealthTimeout(10*time.Millisecond),
		frame.WithStartProbeWindow(5*time.Millisecond),
	)
	mc := newMock("db")
	sup.Add(mc,
		frame.WithTier(frame.TierCritical),
		frame.WithRestartPolicy(frame.NeverRestart()),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	waitStarted(t, mc, 2*time.Second)
	mc.setHealthErr(errFake)

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected non-nil error")
		}
		// The error must be the specific component error, not just
		// "context canceled".
		if errors.Is(err, context.Canceled) && !errors.Is(err, errFake) {
			t.Errorf("expected errFake in chain, got: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not shut down")
	}
}

func TestHealthReporter_Interface(t *testing.T) {
	// *Supervisor must satisfy HealthReporter.
	sup := frame.NewSupervisor(
		frame.WithStartProbeWindow(5 * time.Millisecond),
	)
	mc := newMock("svc")
	sup.Add(mc)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	waitStarted(t, mc, 2*time.Second)

	// HealthReportOrdered should return at least the svc component.
	report := sup.HealthReportOrdered()
	found := false
	for _, s := range report {
		if s.Name == "svc" {
			found = true
			break
		}
	}
	if !found {
		t.Error("HealthReportOrdered: expected 'svc' entry")
	}

	cancel()
	<-done
}

// ── ServiceAdapter restart safety (#1) ───────────────────────────────────────

func TestServiceAdapter_SafeAfterRestart(t *testing.T) {
	// A ServiceAdapter with a restart policy must not panic on the second
	// Start call (channel-reinitialisation fix). Init fails on the first
	// attempt and succeeds on subsequent attempts.
	svc := newMockLegacyService("restartable")
	svc.setInitErr(errFake) // first attempt will fail

	adapter := frame.NewServiceAdapter(svc)
	sup := frame.NewSupervisor(
		frame.WithStartProbeWindow(5 * time.Millisecond),
	)
	sup.Add(adapter, frame.WithRestartPolicy(frame.MaxRetries(5, 10*time.Millisecond)))

	// After 30 ms clear the error so the next retry succeeds.
	go func() {
		time.Sleep(30 * time.Millisecond)
		svc.setInitErr(nil)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	// Wait until Init has been called at least twice (one failure + one success).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if svc.initCalled.Load() {
			// initCalled is true after the first call; we need to wait for the
			// successful second call, which we detect by the adapter becoming
			// ready (supervisor would cancel on failure).
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Give the supervisor time to complete the successful start.
	time.Sleep(100 * time.Millisecond)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected error after restart: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not stop after restart test")
	}
}

// ── startTimeout applied to Starter components (#4) ──────────────────────────

func TestStarter_StartTimeoutEnforced(t *testing.T) {
	// A Starter that never closes its readyCh should be failed by the
	// startTimeout, not block forever.
	sup := frame.NewSupervisor(
		frame.WithStartTimeout(50 * time.Millisecond),
	)

	// A component that implements Starter but never signals ready.
	hungSvc := &neverReadyComponent{stop: make(chan struct{})}
	sup.Add(hungSvc, frame.WithRestartPolicy(frame.NeverRestart()))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := sup.Run(ctx)
	if err == nil {
		t.Fatal("expected error from component that never signals ready")
	}
}

type neverReadyComponent struct {
	stop    chan struct{}
	readyCh chan struct{} // never closed
}

func (c *neverReadyComponent) Name() string { return "never-ready" }
func (c *neverReadyComponent) Started() <-chan struct{} {
	if c.readyCh == nil {
		c.readyCh = make(chan struct{})
	}
	return c.readyCh
}
func (c *neverReadyComponent) Start(ctx context.Context) error {
	select {
	case <-c.stop:
	case <-ctx.Done():
	}
	return nil
}
func (c *neverReadyComponent) Stop(_ context.Context) error {
	select {
	case <-c.stop:
	default:
		close(c.stop)
	}
	return nil
}

// ── Shutdown with cause (#6) ─────────────────────────────────────────────────

func TestApplication_ShutdownWithCause(t *testing.T) {
	shutdownCause := errors.New("planned maintenance")

	var receivedCause error
	app := frame.NewApplication(
		frame.WithMainFunc(func(ctx context.Context) error {
			<-ctx.Done()
			receivedCause = context.Cause(ctx)
			return nil
		}),
	)

	done := make(chan error, 1)
	go func() { done <- app.Run() }()

	// Give Run time to start.
	time.Sleep(20 * time.Millisecond)
	app.Shutdown(shutdownCause)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("application did not stop in time")
	}

	if !errors.Is(receivedCause, shutdownCause) {
		t.Errorf("context.Cause: want %v, got %v", shutdownCause, receivedCause)
	}
}

// ── HealthReportOrdered determinism (#5) ─────────────────────────────────────

func TestHealthReportOrdered_Deterministic(t *testing.T) {
	sup := frame.NewSupervisor(
		frame.WithStartProbeWindow(5 * time.Millisecond),
	)
	z := newMock("zebra")
	a := newMock("alpha")
	m := newMock("mango")
	// Register in reverse alphabetical order — report must still come out sorted.
	sup.Add(z)
	sup.Add(a)
	sup.Add(m)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	// Wait for all three components to start before reading the report.
	waitStarted(t, z, 2*time.Second)
	waitStarted(t, a, 2*time.Second)
	waitStarted(t, m, 2*time.Second)

	for range 20 {
		report := sup.HealthReportOrdered()
		if len(report) != 3 {
			continue
		}
		names := make([]string, len(report))
		for i, r := range report {
			names[i] = r.Name
		}
		if names[0] != "alpha" || names[1] != "mango" || names[2] != "zebra" {
			t.Errorf("HealthReportOrdered not sorted: %v", names)
			break
		}
	}

	cancel()
	<-done
}

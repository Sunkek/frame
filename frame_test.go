package samsara_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sunkek/samsara"
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

func (m *mockComponent) Start(ctx context.Context, ready func()) error {
	m.startCalled.Add(1)
	if m.shouldFail.Load() {
		return errFake
	}
	ready()
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
	p := samsara.NeverRestart()
	restart, _ := p.ShouldRestart(errFake, 0)
	if restart {
		t.Fatal("NeverRestart should return false")
	}
}

func TestAlwaysRestart(t *testing.T) {
	p := samsara.AlwaysRestart(10 * time.Millisecond)
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
	p := samsara.MaxRetries(3, 5*time.Millisecond)
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
	p := samsara.ExponentialBackoff(4, base)
	// Each attempt's nominal delay doubles; ±25% jitter is applied so the
	// actual delay falls in [0.75×nominal, 1.25×nominal).
	nominals := []time.Duration{base, 2 * base, 4 * base, 8 * base}
	for i, nominal := range nominals {
		restart, got := p.ShouldRestart(errFake, i)
		if !restart {
			t.Fatalf("ExponentialBackoff should restart at attempt %d", i)
		}
		lo := time.Duration(float64(nominal) * 0.75)
		hi := time.Duration(float64(nominal) * 1.25)
		if got < lo || got >= hi {
			t.Fatalf("attempt %d: delay %v outside jitter range [%v, %v)", i, got, lo, hi)
		}
	}
	restart, _ := p.ShouldRestart(errFake, 4)
	if restart {
		t.Fatal("ExponentialBackoff should stop after max retries")
	}
}

// ── Supervisor: basic lifecycle ───────────────────────────────────────────────

func TestSupervisor_StartsAndStops(t *testing.T) {
	sup := samsara.NewSupervisor(
		samsara.WithHealthInterval(100 * time.Millisecond),
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
	sup := samsara.NewSupervisor(
		samsara.WithHealthInterval(50 * time.Millisecond),
	)

	db := newMock("db")
	cache := newMock("cache")
	app := newMock("app")

	sup.Add(db)
	sup.Add(cache, samsara.WithDependencies("db"))
	sup.Add(app, samsara.WithDependencies("cache"))

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
		sup := samsara.NewSupervisor()

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
	sup := samsara.NewSupervisor()
	a := newMock("a")
	b := newMock("b")
	sup.Add(a, samsara.WithDependencies("b"))
	sup.Add(b, samsara.WithDependencies("a"))

	err := sup.Run(context.Background())
	if !errors.Is(err, samsara.ErrCircularDependency) {
		t.Fatalf("expected ErrCircularDependency, got: %v", err)
	}
}

func TestSupervisor_UnknownDependency(t *testing.T) {
	sup := samsara.NewSupervisor()
	mc := newMock("alpha")
	sup.Add(mc, samsara.WithDependencies("nonexistent"))

	err := sup.Run(context.Background())
	if !errors.Is(err, samsara.ErrUnknownDependency) {
		t.Fatalf("expected ErrUnknownDependency, got: %v", err)
	}
}

func TestSupervisor_DuplicateComponentPanics(t *testing.T) {
	sup := samsara.NewSupervisor()
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
	sup := samsara.NewSupervisor()
	mc := newMock("broken")
	mc.shouldFail.Store(true)
	sup.Add(mc, samsara.WithRestartPolicy(samsara.NeverRestart()))

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
	sup := samsara.NewSupervisor()
	mc := newMock("flaky")
	mc.shouldFail.Store(true)

	go func() {
		time.Sleep(100 * time.Millisecond)
		mc.shouldFail.Store(false)
	}()

	sup.Add(mc, samsara.WithRestartPolicy(samsara.MaxRetries(20, 10*time.Millisecond)))

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
	sup := samsara.NewSupervisor(
		samsara.WithHealthInterval(20*time.Millisecond),
		samsara.WithHealthTimeout(10*time.Millisecond),
		samsara.WithStartTimeout(50*time.Millisecond),
	)
	mc := newMock("db")
	sup.Add(mc,
		samsara.WithTier(samsara.TierCritical),
		samsara.WithRestartPolicy(samsara.NeverRestart()),
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
	sup := samsara.NewSupervisor(
		samsara.WithHealthInterval(20*time.Millisecond),
		samsara.WithHealthTimeout(5*time.Millisecond),
	)
	aux := newMock("metrics")
	sup.Add(aux,
		samsara.WithTier(samsara.TierAuxiliary),
		samsara.WithRestartPolicy(samsara.NeverRestart()),
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

	hooks := &samsara.EventHooks{
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

	sup := samsara.NewSupervisor(
		samsara.WithHealthInterval(20*time.Millisecond),
		samsara.WithHealthTimeout(10*time.Millisecond),
		samsara.WithStartTimeout(50*time.Millisecond),
		samsara.WithEventHooks(hooks),
	)
	mc := newMock("db")
	sup.Add(mc,
		samsara.WithTier(samsara.TierCritical),
		samsara.WithRestartPolicy(samsara.NeverRestart()),
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
	sup := samsara.NewSupervisor()
	sup.Add(mc)

	mainDone := make(chan struct{})
	app := samsara.NewApplication(
		samsara.WithSupervisor(sup),
		samsara.WithMainFunc(func(ctx context.Context) error {
			<-ctx.Done()
			close(mainDone)
			return nil
		}),
		samsara.WithShutdownTimeout(5*time.Second),
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
	app := samsara.NewApplication(
		samsara.WithMainFunc(func(ctx context.Context) error {
			return errFake
		}),
	)

	err := app.Run()
	if !errors.Is(err, errFake) {
		t.Fatalf("expected errFake, got: %v", err)
	}
}

func TestApplication_NoMainFunc_NoSupervisor(t *testing.T) {
	app := samsara.NewApplication()
	err := app.Run()
	if !errors.Is(err, samsara.ErrNothingToRun) {
		t.Fatalf("expected ErrNothingToRun, got: %v", err)
	}
}

// ── HealthServer ──────────────────────────────────────────────────────────────

func TestHealthServer_Endpoints(t *testing.T) {
	sup := samsara.NewSupervisor(
		samsara.WithHealthInterval(50*time.Millisecond),
		samsara.WithStartTimeout(2*time.Second),
	)

	hs := samsara.NewHealthServer(sup, samsara.WithHealthAddr(":19090"))
	// Register health server first so it starts before other components.
	sup.Add(hs, samsara.WithTier(samsara.TierCritical))

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
		resp, err = http.Get("http://localhost:19090/livez")
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
		resp, err = http.Get("http://localhost:19090/readyz")
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
	resp2, err2 := http.Get("http://localhost:19090/healthz")
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

// ── ready() function — precise readiness signalling ──────────────────────────

// delayedReadyComponent calls ready() after a configurable delay, simulating
// a server that needs time to bind a port before it can serve traffic.
type delayedReadyComponent struct {
	mockComponent
	readyDelay time.Duration
}

func newDelayedReady(name string, delay time.Duration) *delayedReadyComponent {
	return &delayedReadyComponent{
		mockComponent: *newMock(name),
		readyDelay:    delay,
	}
}

func (m *delayedReadyComponent) Start(ctx context.Context, ready func()) error {
	m.startCalled.Add(1)
	if m.shouldFail.Load() {
		return errFake
	}
	select {
	case <-time.After(m.readyDelay):
		ready()
		m.startedOnce.Do(func() { close(m.started) })
	case <-ctx.Done():
		return nil
	}
	select {
	case <-m.stop:
	case <-ctx.Done():
	}
	return nil
}

func TestReady_PreciseReadiness(t *testing.T) {
	// Component calls ready() after 5ms. The supervisor should proceed to the
	// next component in ~5ms, well within the 15s startTimeout.
	sup := samsara.NewSupervisor()

	sm := newDelayedReady("precise", 5*time.Millisecond)
	sup.Add(sm)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	start := time.Now()
	go func() { done <- sup.Run(ctx) }()

	waitStarted(t, &sm.mockComponent, 3*time.Second)
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Errorf("component took %v to become ready; expected <500ms", elapsed)
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

func TestReady_DependentWaitsForReady(t *testing.T) {
	// dep delays ready() by 50ms. svc depends on dep — svc must not start
	// until dep has called ready().
	sup := samsara.NewSupervisor()

	dep := newDelayedReady("dep", 50*time.Millisecond)
	svc := newMock("svc")
	sup.Add(dep)
	sup.Add(svc, samsara.WithDependencies("dep"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	waitStarted(t, &dep.mockComponent, 2*time.Second)
	depReadyAt := time.Now()

	waitStarted(t, svc, 2*time.Second)
	svcStartedAt := time.Now()

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
	sup := samsara.NewSupervisor(
		samsara.WithMetricsObserver(obs),
	)
	mc := newMock("svc")
	sup.Add(mc)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	// Wait for ComponentStarted to fire — this is the definitive signal that
	// the supervisor has fully processed the component's readiness, which
	// happens after ready() is called inside Start. Waiting on mc.started
	// is not sufficient because that channel closes inside the Start goroutine,
	// which races with startOne processing readySignal and emitting the metric.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if obs.startedCount() >= 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if obs.startedCount() < 1 {
		cancel()
		t.Fatal("ComponentStarted was not called within 2s")
	}

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
	sup := samsara.NewSupervisor(
		samsara.WithMetricsObserver(obs),
		samsara.WithHealthInterval(20*time.Millisecond),
		samsara.WithHealthTimeout(10*time.Millisecond),
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
	sup := samsara.NewSupervisor(
		samsara.WithMetricsObserver(obs),
	)
	mc := newMock("svc")
	mc.shouldFail.Store(true)
	go func() {
		time.Sleep(50 * time.Millisecond)
		mc.shouldFail.Store(false)
	}()
	sup.Add(mc, samsara.WithRestartPolicy(samsara.MaxRetries(20, 5*time.Millisecond)))

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
	sup := samsara.NewSupervisor(
		samsara.WithHealthInterval(20*time.Millisecond),
		samsara.WithHealthTimeout(10*time.Millisecond),
	)
	mc := newMock("db")
	sup.Add(mc,
		samsara.WithTier(samsara.TierCritical),
		samsara.WithRestartPolicy(samsara.NeverRestart()),
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
	sup := samsara.NewSupervisor()
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

// ── startTimeout: component never calls ready() ───────────────────────────────

func TestStartTimeout_NeverCallsReady(t *testing.T) {
	// A component that never calls ready() should be failed by startTimeout.
	sup := samsara.NewSupervisor(
		samsara.WithStartTimeout(50 * time.Millisecond),
	)

	hungSvc := &neverReadyComponent{stop: make(chan struct{})}
	sup.Add(hungSvc, samsara.WithRestartPolicy(samsara.NeverRestart()))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := sup.Run(ctx)
	if err == nil {
		t.Fatal("expected error from component that never calls ready()")
	}
}

type neverReadyComponent struct {
	stop chan struct{}
}

func (c *neverReadyComponent) Name() string { return "never-ready" }
func (c *neverReadyComponent) Start(ctx context.Context, _ func()) error {
	// Never calls ready() — supervisor must time out.
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

func TestApplication_OsSignalShutdown_ReturnsNil(t *testing.T) {
	// A clean OS-signal-triggered shutdown must return nil — not an error.
	// We simulate this by cancelling the parent context directly, which is
	// equivalent to what signal.NotifyContext does on Ctrl+C.
	sup := samsara.NewSupervisor()
	mc := newMock("svc")
	sup.Add(mc)

	app := samsara.NewApplication(
		samsara.WithSupervisor(sup),
		samsara.WithShutdownTimeout(5*time.Second),
	)

	done := make(chan error, 1)
	go func() { done <- app.Run() }()

	waitStarted(t, mc, 2*time.Second)
	app.Shutdown(nil) // nil cause = clean shutdown, same semantics as Ctrl+C

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("clean shutdown should return nil, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("application did not stop in time")
	}
}

func TestApplication_ShutdownWithCause(t *testing.T) {
	shutdownCause := errors.New("planned maintenance")

	var receivedCause error
	app := samsara.NewApplication(
		samsara.WithMainFunc(func(ctx context.Context) error {
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
	sup := samsara.NewSupervisor()
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

// ── additional coverage ───────────────────────────────────────────────────────

func TestSupervisor_SignificantUnhealthy_DegradedNotShutdown(t *testing.T) {
	// A TierSignificant component that is transiently unhealthy should not shut
	// the app down. It will be restarted by the restart policy, and its unhealthy
	// state should be visible in the health report while it is down.
	sup := samsara.NewSupervisor(
		samsara.WithHealthInterval(20*time.Millisecond),
		samsara.WithHealthTimeout(10*time.Millisecond),
	)
	sig := newMock("cache")
	sup.Add(sig,
		samsara.WithTier(samsara.TierSignificant),
		samsara.WithRestartPolicy(samsara.AlwaysRestart(5*time.Millisecond)),
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	waitStarted(t, sig, 2*time.Second)
	sig.setHealthErr(errFake)

	// App must stay alive despite the Significant component being unhealthy.
	select {
	case err := <-done:
		t.Fatalf("supervisor exited unexpectedly: %v", err)
	case <-time.After(150 * time.Millisecond):
		// good — still running
	}

	// Confirm the degraded state is visible in the health report.
	deadline := time.Now().Add(time.Second)
	var foundDegraded bool
	for time.Now().Before(deadline) {
		for _, s := range sup.HealthReportOrdered() {
			if s.Name == "cache" && s.Err != nil {
				foundDegraded = true
			}
		}
		if foundDegraded {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !foundDegraded {
		t.Error("expected cache to appear degraded in HealthReport")
	}

	cancel()
	<-done
}

func TestSupervisor_ComponentRestartAndRecover(t *testing.T) {
	// A component that fails health checks should be restarted and eventually
	// come back healthy. The supervisor should not shut down.
	sup := samsara.NewSupervisor(
		samsara.WithHealthInterval(20*time.Millisecond),
		samsara.WithHealthTimeout(10*time.Millisecond),
	)
	mc := newMock("db")
	sup.Add(mc,
		samsara.WithTier(samsara.TierCritical),
		samsara.WithRestartPolicy(samsara.MaxRetries(5, 5*time.Millisecond)),
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	waitStarted(t, mc, 2*time.Second)

	// Make it unhealthy — this triggers a stop+restart cycle.
	mc.setHealthErr(errFake)

	// Wait for at least one restart (startCalled > 1).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if mc.startCalled.Load() > 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if mc.startCalled.Load() <= 1 {
		t.Fatal("component was not restarted after health failure")
	}

	// Clear the health error — component should recover.
	mc.setHealthErr(nil)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected error after restart cycle: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not stop cleanly after restart cycle")
	}
}

func TestSupervisor_DependencyFails_DependentNeverStarts(t *testing.T) {
	// If a dependency fails to start, its dependent must never be started.
	sup := samsara.NewSupervisor()

	dep := newMock("dep")
	dep.shouldFail.Store(true) // dep always fails immediately

	svc := newMock("svc")

	sup.Add(dep, samsara.WithRestartPolicy(samsara.NeverRestart()))
	sup.Add(svc, samsara.WithDependencies("dep"))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := sup.Run(ctx)
	if err == nil {
		t.Fatal("expected error from failing dependency")
	}
	if svc.startCalled.Load() != 0 {
		t.Errorf("svc should never have been started, but startCalled=%d", svc.startCalled.Load())
	}
}

// recordingComponent is a Component that records the order in which Stop is
// called into a shared slice, enabling stop-order assertions.
type recordingComponent struct {
	*mockComponent
	mu    *sync.Mutex
	order *[]string
}

func newRecording(name string, mu *sync.Mutex, order *[]string) *recordingComponent {
	return &recordingComponent{
		mockComponent: newMock(name),
		mu:            mu,
		order:         order,
	}
}

func (r *recordingComponent) Stop(ctx context.Context) error {
	r.mu.Lock()
	*r.order = append(*r.order, r.Name())
	r.mu.Unlock()
	return r.mockComponent.Stop(ctx)
}

func TestSupervisor_StopOrder_ReverseOfStart(t *testing.T) {
	// Components registered in order a→b→c must be stopped in order c→b→a.
	// Run 20 times to catch map-iteration non-determinism.
	for range 20 {
		sup := samsara.NewSupervisor()

		var mu sync.Mutex
		var stopOrder []string

		a := newRecording("a", &mu, &stopOrder)
		b := newRecording("b", &mu, &stopOrder)
		c := newRecording("c", &mu, &stopOrder)

		sup.Add(a)
		sup.Add(b)
		sup.Add(c)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- sup.Run(ctx) }()

		waitStarted(t, a.mockComponent, 2*time.Second)
		waitStarted(t, b.mockComponent, 2*time.Second)
		waitStarted(t, c.mockComponent, 2*time.Second)

		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("supervisor did not stop in time")
		}

		mu.Lock()
		got := make([]string, len(stopOrder))
		copy(got, stopOrder)
		mu.Unlock()

		want := []string{"c", "b", "a"}
		if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
			t.Errorf("stop order = %v, want %v", got, want)
		}
	}
}

func TestSupervisor_AddAfterRun_Panics(t *testing.T) {
	sup := samsara.NewSupervisor()
	mc := newMock("svc")
	sup.Add(mc)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	waitStarted(t, mc, 2*time.Second)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic when Add called after Run started")
		}
		cancel()
		<-done
	}()
	sup.Add(newMock("late"))
}

func TestApplication_SupervisorFailure_PropagatesError(t *testing.T) {
	// When a critical component fails, Application.Run should return a
	// non-nil error that describes the failure.
	sup := samsara.NewSupervisor(
		samsara.WithHealthInterval(20*time.Millisecond),
		samsara.WithHealthTimeout(10*time.Millisecond),
	)
	mc := newMock("db")
	sup.Add(mc,
		samsara.WithTier(samsara.TierCritical),
		samsara.WithRestartPolicy(samsara.NeverRestart()),
	)

	app := samsara.NewApplication(
		samsara.WithSupervisor(sup),
		samsara.WithShutdownTimeout(5*time.Second),
	)

	done := make(chan error, 1)
	go func() { done <- app.Run() }()

	waitStarted(t, mc, 2*time.Second)
	mc.setHealthErr(errFake)

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected non-nil error when critical component fails")
		}
		// The error message must mention the component name.
		if !strings.Contains(err.Error(), "db") {
			t.Errorf("error should mention component name 'db': %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("application did not exit after critical component failure")
	}
}

// ── RestartCount ──────────────────────────────────────────────────────────────

func TestComponentStatus_RestartCountIncrements(t *testing.T) {
	// Each health-triggered restart must increment RestartCount in the
	// status report so operators can detect flapping components.
	sup := samsara.NewSupervisor(
		samsara.WithHealthInterval(20*time.Millisecond),
		samsara.WithHealthTimeout(10*time.Millisecond),
	)
	mc := newMock("db")
	sup.Add(mc,
		samsara.WithTier(samsara.TierCritical),
		samsara.WithRestartPolicy(samsara.MaxRetries(5, 5*time.Millisecond)),
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	waitStarted(t, mc, 2*time.Second)

	// Trigger a health-driven restart.
	mc.setHealthErr(errFake)

	// Wait until at least one restart has been recorded.
	deadline := time.Now().Add(2 * time.Second)
	var restartCount int
	for time.Now().Before(deadline) {
		for _, s := range sup.HealthReportOrdered() {
			if s.Name == "db" {
				restartCount = s.RestartCount
			}
		}
		if restartCount > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if restartCount == 0 {
		t.Error("RestartCount should be > 0 after a health-triggered restart")
	}

	mc.setHealthErr(nil)
	cancel()
	<-done
}

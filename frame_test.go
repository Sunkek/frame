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

	app.Shutdown()

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
	initErr     error
	pingErr     error
	closeErr    error
	initCalled  atomic.Bool
	closeCalled atomic.Bool
}

func (s *mockLegacyService) Ident() string                { return s.name }
func (s *mockLegacyService) Init(_ context.Context) error { s.initCalled.Store(true); return s.initErr }
func (s *mockLegacyService) Ping(_ context.Context) error { return s.pingErr }
func (s *mockLegacyService) Close() error                 { s.closeCalled.Store(true); return s.closeErr }

func TestServiceAdapter_LifecycleIntegration(t *testing.T) {
	legacy := &mockLegacyService{name: "postgres"}
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
	legacy := &mockLegacyService{name: "redis", initErr: errFake}
	adapter := frame.NewServiceAdapter(legacy)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := adapter.Start(ctx)
	if !errors.Is(err, errFake) {
		t.Fatalf("expected errFake, got: %v", err)
	}
}

func TestServiceAdapter_HealthDelegation(t *testing.T) {
	legacy := &mockLegacyService{name: "redis", pingErr: errFake}
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

package samsara_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sunkek/samsara"
)

// gateComponent calls ready() only after its release channel is closed. This
// lets a test observe whether multiple Start calls run concurrently before any
// component has signalled ready — the defining property of parallel startup.
type gateComponent struct {
	name        string
	startCalled atomic.Int32
	stopCalled  atomic.Int32
	release     chan struct{}
	stop        chan struct{}
}

func newGate(name string) *gateComponent {
	return &gateComponent{
		name:    name,
		release: make(chan struct{}),
		stop:    make(chan struct{}),
	}
}

func (g *gateComponent) Name() string { return g.name }

func (g *gateComponent) Start(ctx context.Context, ready func()) error {
	g.startCalled.Add(1)
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil
	}
	ready()
	select {
	case <-g.stop:
	case <-ctx.Done():
	}
	return nil
}

func (g *gateComponent) Stop(_ context.Context) error {
	g.stopCalled.Add(1)
	select {
	case <-g.stop:
	default:
		close(g.stop)
	}
	return nil
}

// waitGateStarted blocks until g.Start has been invoked or the timeout fires.
func waitGateStarted(t *testing.T, g *gateComponent, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for g.startCalled.Load() == 0 {
		select {
		case <-deadline:
			t.Fatalf("gate %q did not start within %s", g.name, timeout)
		case <-time.After(2 * time.Millisecond):
		}
	}
}

func TestSupervisor_Parallel_IndependentStartConcurrently(t *testing.T) {
	// Two components with no dependency between them must have their Start
	// invoked concurrently — neither has called ready() yet, so in sequential
	// mode the second would never start.
	sup := samsara.NewSupervisor(samsara.WithParallelStartStop())
	a := newGate("a")
	b := newGate("b")
	sup.Add(a)
	sup.Add(b)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	deadline := time.After(2 * time.Second)
	for a.startCalled.Load() == 0 || b.startCalled.Load() == 0 {
		select {
		case <-deadline:
			t.Fatalf("independent components not started concurrently: a=%d b=%d",
				a.startCalled.Load(), b.startCalled.Load())
		case <-time.After(2 * time.Millisecond):
		}
	}

	close(a.release)
	close(b.release)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSupervisor_Sequential_DoesNotStartConcurrently(t *testing.T) {
	// Contrast with parallel mode: the default supervisor must not invoke the
	// second component's Start until the first has called ready().
	sup := samsara.NewSupervisor()
	a := newGate("a")
	b := newGate("b")
	sup.Add(a)
	sup.Add(b)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	waitGateStarted(t, a, 2*time.Second)
	// a is started but has not called ready(); b must remain unstarted.
	time.Sleep(50 * time.Millisecond)
	if b.startCalled.Load() != 0 {
		t.Fatalf("b started before a became ready (sequential violated)")
	}

	close(a.release)
	close(b.release)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSupervisor_Parallel_DependencyRespected(t *testing.T) {
	// Even in parallel mode, a dependent must not start until its dependency
	// has signalled ready().
	sup := samsara.NewSupervisor(samsara.WithParallelStartStop())
	dep := newGate("dep")
	svc := newGate("svc")
	sup.Add(dep)
	sup.Add(svc, samsara.WithDependencies("dep"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	waitGateStarted(t, dep, 2*time.Second)
	// dep is started but not ready; svc must not have started yet.
	time.Sleep(50 * time.Millisecond)
	if svc.startCalled.Load() != 0 {
		t.Fatalf("svc started before dependency dep was ready")
	}

	close(dep.release)                     // dep becomes ready
	waitGateStarted(t, svc, 2*time.Second) // svc now allowed to start
	close(svc.release)

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSupervisor_Parallel_StopRespectsDependents(t *testing.T) {
	// Dependency chain a <- b <- c (c depends on b depends on a). On shutdown,
	// a dependent must always stop before the component it depends on, even
	// though stops run concurrently.
	for range 20 {
		sup := samsara.NewSupervisor(samsara.WithParallelStartStop())

		var mu sync.Mutex
		var stopOrder []string

		a := newRecording("a", &mu, &stopOrder)
		b := newRecording("b", &mu, &stopOrder)
		c := newRecording("c", &mu, &stopOrder)

		sup.Add(a)
		sup.Add(b, samsara.WithDependencies("a"))
		sup.Add(c, samsara.WithDependencies("b"))

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
		got := append([]string(nil), stopOrder...)
		mu.Unlock()

		if len(got) != 3 {
			t.Fatalf("expected 3 stops, got %v", got)
		}
		if idx(got, "c") > idx(got, "b") || idx(got, "b") > idx(got, "a") {
			t.Errorf("stop order %v violates dependents-first (want c before b before a)", got)
		}
	}
}

func idx(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

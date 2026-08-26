package samsara_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sunkek/samsara"
)

// ── A1: post-ready crash of a non-Health component ────────────────────────────

// postReadyCrash is a Component WITHOUT a Health method. Its Start signals
// ready(), publishes its attempt number on readyCh (handshake — the receive
// guarantees the supervisor's manage loop is already watching startExit), then
// returns errFake for the first `crashUntil` lives and blocks until shutdown
// thereafter.
type postReadyCrash struct {
	name       string
	starts     atomic.Int32
	stops      atomic.Int32
	readyCh    chan int
	crashUntil int32
}

func (c *postReadyCrash) Name() string { return c.name }

func (c *postReadyCrash) Start(ctx context.Context, ready func()) error {
	n := c.starts.Add(1)
	ready()
	select {
	case c.readyCh <- int(n):
	case <-ctx.Done():
		return nil
	}
	if n <= c.crashUntil {
		return errFake // unexpected exit AFTER ready — the A1 hole
	}
	<-ctx.Done()
	return nil
}

func (c *postReadyCrash) Stop(context.Context) error {
	c.stops.Add(1)
	return nil
}

func recvReady(t *testing.T, ch chan int, want int) {
	t.Helper()
	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("expected life %d, got %d", want, got)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for life %d (post-ready crash not supervised?)", want)
	}
}

func TestPostReadyCrash_NonHealthComponent_Restarts(t *testing.T) {
	c := &postReadyCrash{name: "worker", readyCh: make(chan int), crashUntil: 1}

	sup := samsara.NewSupervisor()
	sup.Add(c,
		samsara.WithTier(samsara.TierAuxiliary),
		samsara.WithRestartPolicy(samsara.AlwaysRestart(1*time.Millisecond)),
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	// Life 1 starts, then crashes after ready — must be restarted (life 2).
	recvReady(t, c.readyCh, 1)
	recvReady(t, c.readyCh, 2) // proves the restart policy applied post-ready

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not stop in time")
	}

	if got := c.starts.Load(); got < 2 {
		t.Fatalf("expected component started >= 2 times, got %d", got)
	}
	if _, restarts, _ := reportFor(t, sup, "worker"); restarts < 1 {
		t.Fatalf("expected RestartCount >= 1, got %d", restarts)
	}
}

func reportFor(t *testing.T, sup *samsara.Supervisor, name string) (bool, int, error) {
	t.Helper()
	for _, s := range sup.HealthReportOrdered() {
		if s.Name == name {
			return s.Known, s.RestartCount, s.Err
		}
	}
	t.Fatalf("no status for %q", name)
	return false, 0, nil
}

// A critical (default tier) non-Health component crashing post-ready with
// NeverRestart must bring the whole supervisor down — previously silent.
func TestPostReadyCrash_Critical_ShutsDown(t *testing.T) {
	c := &postReadyCrash{name: "server", readyCh: make(chan int), crashUntil: 1}

	sup := samsara.NewSupervisor()
	sup.Add(c) // default: TierCritical, NeverRestart

	done := make(chan error, 1)
	go func() { done <- sup.Run(context.Background()) }()

	recvReady(t, c.readyCh, 1)

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected non-nil error from critical post-ready crash")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not shut down after critical post-ready crash")
	}
	if sup.Alive() {
		t.Fatal("supervisor should report not-alive after critical permanent failure")
	}
}

// ── A2: /livez reflects supervisor liveness ───────────────────────────────────

type fakeLivenessReporter struct {
	alive atomic.Bool
}

func (f *fakeLivenessReporter) HealthReportOrdered() []samsara.NamedComponentStatus {
	return nil
}
func (f *fakeLivenessReporter) Alive() bool { return f.alive.Load() }

func TestLivez_ReflectsSupervisorLiveness(t *testing.T) {
	rep := &fakeLivenessReporter{}
	rep.alive.Store(true)

	addr := freeLoopbackAddr(t)
	hs := samsara.NewHealthServer(rep, samsara.WithHealthAddr(addr))

	sup := samsara.NewSupervisor()
	sup.Add(hs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sup.Run(ctx) }()

	url := "http://" + addr + "/livez"
	waitCode(t, url, http.StatusOK)

	// Supervisor enters failure-driven shutdown → /livez must fail even though
	// the health port is still bound.
	rep.alive.Store(false)
	waitCode(t, url, http.StatusServiceUnavailable)
}

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func waitCode(t *testing.T, url string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last int
	for time.Now().Before(deadline) {
		resp, err := http.Get(url) //nolint:noctx
		if err == nil {
			last = resp.StatusCode
			_ = resp.Body.Close()
			if last == want {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("GET %s: expected status %d, last got %d", url, want, last)
}

// ── A3: total shutdown bounded by grace ───────────────────────────────────────

// slowStop starts cleanly and hangs in Stop until its (supervisor-capped)
// context expires — a well-behaved-but-slow shutdown.
type slowStop struct {
	name    string
	started chan struct{}
	once    sync.Once
}

func (s *slowStop) Name() string { return s.name }
func (s *slowStop) Start(ctx context.Context, ready func()) error {
	ready()
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	return nil
}
func (s *slowStop) Stop(ctx context.Context) error {
	<-ctx.Done() // block until the budget granted by the supervisor runs out
	return nil
}

func TestShutdownGrace_BoundsTotalStopTime(t *testing.T) {
	const grace = 200 * time.Millisecond
	sup := samsara.NewSupervisor(
		samsara.WithShutdownGrace(grace),
		samsara.WithStopTimeout(2*time.Second), // per-component; grace must win
	)

	comps := make([]*slowStop, 4)
	for i := range comps {
		comps[i] = &slowStop{name: fmt.Sprintf("c%d", i), started: make(chan struct{})}
		sup.Add(comps[i], samsara.WithTier(samsara.TierAuxiliary))
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	for _, c := range comps {
		select {
		case <-c.started:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s did not start", c.name)
		}
	}

	cancel()
	start := time.Now()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not shut down")
	}
	elapsed := time.Since(start)

	// Without the fix, 4 components × 2s stopTimeout serial ≈ 8s. Bounded by
	// grace it must be well under a second.
	if elapsed > time.Second {
		t.Fatalf("shutdown took %v, expected bounded by grace ~%v", elapsed, grace)
	}
}

// postReadyCrashHealth is a postReadyCrash that also implements HealthChecker,
// always probing healthy. It exists to prove that a post-ready crash enters the
// unhealthy state, so the restarted component recovers out of it.
type postReadyCrashHealth struct {
	postReadyCrash
}

func (c *postReadyCrashHealth) Health(context.Context) error { return nil }

// A post-ready crash is a confirmed fault like any other, so the component
// announces OnUnhealthy, restarts, and — once its probes are sustainedly
// healthy again — announces OnRecovered.
func TestPostReadyCrash_RecoversAfterRestart(t *testing.T) {
	c := &postReadyCrashHealth{postReadyCrash{name: "worker", readyCh: make(chan int), crashUntil: 1}}

	var mu sync.Mutex
	var events []string
	recovered := make(chan struct{}, 1)
	hooks := &samsara.EventHooks{
		OnUnhealthy: func(string, error) {
			mu.Lock()
			events = append(events, "unhealthy")
			mu.Unlock()
		},
		OnRecovered: func(string) {
			mu.Lock()
			events = append(events, "recovered")
			mu.Unlock()
			select {
			case recovered <- struct{}{}:
			default:
			}
		},
	}

	sup := samsara.NewSupervisor(
		samsara.WithEventHooks(hooks),
		samsara.WithHealthInterval(5*time.Millisecond),
	)
	sup.Add(c,
		samsara.WithTier(samsara.TierAuxiliary),
		samsara.WithRestartPolicy(samsara.AlwaysRestart(1*time.Millisecond)),
		samsara.WithHealthRecoverThreshold(2),
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	recvReady(t, c.readyCh, 1)
	recvReady(t, c.readyCh, 2)

	select {
	case <-recovered:
	case <-time.After(3 * time.Second):
		t.Fatal("component never recovered after a post-ready crash")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not stop in time")
	}

	mu.Lock()
	got := append([]string(nil), events...)
	mu.Unlock()
	if len(got) < 2 || got[0] != "unhealthy" || got[1] != "recovered" {
		t.Fatalf("expected unhealthy then recovered, got %v", got)
	}
}

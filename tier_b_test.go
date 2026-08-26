package samsara_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sunkek/samsara"
)

// ── B1: lifecycle hooks ───────────────────────────────────────────────────────

func TestLifecycleHooks_ReadyBeforeStopStopped(t *testing.T) {
	var (
		mu     sync.Mutex
		events []string
	)
	record := func(e string) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	}

	readyCh := make(chan struct{})
	var readyOnce sync.Once
	hooks := &samsara.EventHooks{
		OnReady: func(c string) {
			record("ready:" + c)
			readyOnce.Do(func() { close(readyCh) })
		},
		BeforeStop: func(c string) { record("beforestop:" + c) },
		OnStopped:  func(c string, _ error) { record("stopped:" + c) },
	}

	sup := samsara.NewSupervisor(samsara.WithEventHooks(hooks))
	mc := newMock("alpha")
	sup.Add(mc)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	waitStarted(t, mc, 2*time.Second)
	// Wait for OnReady to be observed before triggering shutdown so the ordering
	// assertion is deterministic (OnReady races the shutdown path otherwise).
	select {
	case <-readyCh:
	case <-time.After(2 * time.Second):
		t.Fatal("OnReady never fired")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not stop")
	}

	mu.Lock()
	got := append([]string(nil), events...)
	mu.Unlock()

	// Expect ready before beforestop before stopped.
	idx := map[string]int{}
	for i, e := range got {
		idx[e] = i
	}
	for _, e := range []string{"ready:alpha", "beforestop:alpha", "stopped:alpha"} {
		if _, ok := idx[e]; !ok {
			t.Fatalf("missing lifecycle event %q; got %v", e, got)
		}
	}
	if !(idx["ready:alpha"] < idx["beforestop:alpha"] && idx["beforestop:alpha"] < idx["stopped:alpha"]) {
		t.Fatalf("lifecycle events out of order: %v", got)
	}
}

// ── B2: per-component health tuning ───────────────────────────────────────────

// TestHealthThreshold_DebouncesTransientBlip: with failThreshold=3 and
// AlwaysRestart, two consecutive failures followed by a recovery must NOT
// trigger a restart — the blip is below threshold.
func TestHealthThreshold_DebouncesTransientBlip(t *testing.T) {
	var restarts atomic.Int32
	hooks := &samsara.EventHooks{
		OnRestart: func(string, error, int) { restarts.Add(1) },
	}

	sup := samsara.NewSupervisor(
		samsara.WithHealthInterval(15*time.Millisecond),
		samsara.WithEventHooks(hooks),
	)
	mc := newMock("flaky")
	sup.Add(mc,
		samsara.WithRestartPolicy(samsara.AlwaysRestart(1*time.Millisecond)),
		samsara.WithHealthFailThreshold(3),
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()
	waitStarted(t, mc, 2*time.Second)

	// Two failed probes (below the threshold of 3), then healthy again.
	mc.setHealthErr(errFake)
	time.Sleep(40 * time.Millisecond) // ~2 probes
	mc.setHealthErr(nil)
	time.Sleep(40 * time.Millisecond)

	cancel()
	<-done

	if n := restarts.Load(); n != 0 {
		t.Fatalf("transient blip below threshold should not restart; got %d restarts", n)
	}
	// readiness must have stayed healthy throughout (status never set to error).
	if err, known := sup.ComponentHealth("flaky"); known && err != nil {
		t.Fatalf("status should stay healthy below threshold; got %v", err)
	}
}

// TestHealthThreshold_FiresAfterConsecutiveFailures: sustained failure past the
// threshold does trigger a restart.
func TestHealthThreshold_FiresAfterConsecutiveFailures(t *testing.T) {
	var restarts atomic.Int32
	hooks := &samsara.EventHooks{
		OnRestart: func(string, error, int) { restarts.Add(1) },
	}

	sup := samsara.NewSupervisor(
		samsara.WithHealthInterval(10*time.Millisecond),
		samsara.WithEventHooks(hooks),
	)
	mc := newMock("sick")
	sup.Add(mc,
		samsara.WithRestartPolicy(samsara.AlwaysRestart(1*time.Millisecond)),
		samsara.WithHealthFailThreshold(2),
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()
	waitStarted(t, mc, 2*time.Second)

	mc.setHealthErr(errFake) // stays unhealthy → threshold breached → restart(s)

	deadline := time.Now().Add(2 * time.Second)
	for restarts.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	if restarts.Load() == 0 {
		t.Fatal("sustained failure past threshold should trigger a restart")
	}
}

// TestPerComponentHealthTimeout: a component-level health timeout shorter than
// the probe duration surfaces a deadline error via the restart policy.
func TestPerComponentHealthInterval_Override(t *testing.T) {
	var probes atomic.Int32
	hc := &countingHealth{name: "fast", probes: &probes}

	sup := samsara.NewSupervisor(
		samsara.WithHealthInterval(10 * time.Second), // slow global default
	)
	sup.Add(hc,
		samsara.WithTier(samsara.TierAuxiliary),
		samsara.WithComponentHealthInterval(15*time.Millisecond), // fast override
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	// With the override, many probes should land within ~300ms. With the 10s
	// global default, we would see ~0.
	deadline := time.Now().Add(1 * time.Second)
	for probes.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	if probes.Load() < 3 {
		t.Fatalf("expected >=3 probes with 15ms override, got %d", probes.Load())
	}
}

type countingHealth struct {
	name   string
	probes *atomic.Int32
}

func (c *countingHealth) Name() string { return c.name }
func (c *countingHealth) Start(ctx context.Context, ready func()) error {
	ready()
	<-ctx.Done()
	return nil
}
func (c *countingHealth) Stop(context.Context) error { return nil }
func (c *countingHealth) Health(context.Context) error {
	c.probes.Add(1)
	return nil
}

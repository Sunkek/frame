package testutil_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sunkek/samsara"
	"github.com/sunkek/samsara/testutil"
)

// Compile-time proof that FakeComponent satisfies the samsara contracts.
var (
	_ samsara.Component     = (*testutil.FakeComponent)(nil)
	_ samsara.HealthChecker = (*testutil.FakeComponent)(nil)
)

func TestFakeComponent_LifecycleUnderSupervisor(t *testing.T) {
	f := testutil.NewFakeComponent("db")

	sup := samsara.NewSupervisor(samsara.WithHealthInterval(10 * time.Millisecond))
	sup.Add(f, samsara.WithTier(samsara.TierAuxiliary))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	if !f.WaitReady(2 * time.Second) {
		t.Fatal("component never became ready")
	}
	// Let a few health probes land.
	deadline := time.Now().Add(1 * time.Second)
	for f.HealthCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	<-done

	if f.StartCount() != 1 {
		t.Fatalf("StartCount = %d, want 1", f.StartCount())
	}
	if f.StopCount() == 0 {
		t.Fatal("Stop was never called")
	}
	if f.HealthCount() < 2 {
		t.Fatalf("HealthCount = %d, want >= 2", f.HealthCount())
	}
}

func TestFakeComponent_CrashTriggersRestart(t *testing.T) {
	f := testutil.NewFakeComponent("worker")

	sup := samsara.NewSupervisor()
	sup.Add(f,
		samsara.WithTier(samsara.TierAuxiliary),
		samsara.WithRestartPolicy(samsara.AlwaysRestart(1*time.Millisecond)),
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	if !f.WaitReady(2 * time.Second) {
		t.Fatal("component never became ready")
	}
	f.Crash(errors.New("boom"))

	deadline := time.Now().Add(2 * time.Second)
	for f.StartCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	if f.StartCount() < 2 {
		t.Fatalf("crash should have restarted; StartCount = %d", f.StartCount())
	}
}

func TestFakeComponent_CrashIsPerLife(t *testing.T) {
	f := testutil.NewFakeComponent("worker")

	// First life: ready, then crash.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	first := make(chan error, 1)
	go func() { first <- f.Start(ctx, func() {}) }()

	if !f.WaitReady(2 * time.Second) {
		t.Fatal("first life never became ready")
	}

	boom := errors.New("boom")
	f.Crash(boom)

	select {
	case err := <-first:
		if !errors.Is(err, boom) {
			t.Fatalf("first life returned %v, want %v", err, boom)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("crash did not unblock the first life")
	}

	// The crash belonged to that life only: readiness is gone with it.
	if f.WaitReady(50 * time.Millisecond) {
		t.Fatal("WaitReady reported ready after the life that signalled it crashed")
	}

	// Second life: the fresh Start must block like a healthy component.
	second := make(chan error, 1)
	go func() { second <- f.Start(ctx, func() {}) }()

	if !f.WaitReady(2 * time.Second) {
		t.Fatal("second life never became ready")
	}
	select {
	case err := <-second:
		t.Fatalf("second life returned %v immediately, want it to block", err)
	case <-time.After(50 * time.Millisecond):
	}

	// Cancellation ends the second life cleanly.
	cancel()
	select {
	case err := <-second:
		if err != nil {
			t.Fatalf("second life returned %v on cancellation, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not unblock the second life")
	}

	if f.StartCount() != 2 {
		t.Fatalf("StartCount = %d, want 2", f.StartCount())
	}
}

func TestFakeComponent_CrashRestartsUnderSupervisorBudget(t *testing.T) {
	f := testutil.NewFakeComponent("worker")

	sup := samsara.NewSupervisor()
	sup.Add(f,
		samsara.WithTier(samsara.TierAuxiliary),
		samsara.WithRestartPolicy(samsara.AlwaysRestart(1*time.Millisecond)),
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	if !f.WaitReady(2 * time.Second) {
		t.Fatal("component never became ready")
	}
	f.Crash(errors.New("boom"))

	// The restarted life must become ready on its own, rather than burning
	// through the restart budget by crashing again immediately.
	if !f.WaitReady(2 * time.Second) {
		t.Fatal("restarted component never became ready")
	}
	if got := f.StartCount(); got != 2 {
		t.Fatalf("StartCount = %d, want 2 (one crash, one restart)", got)
	}

	cancel()
	<-done
}

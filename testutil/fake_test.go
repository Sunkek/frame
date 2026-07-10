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

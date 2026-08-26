package samsara_test

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sunkek/samsara"
	"github.com/sunkek/samsara/testutil"
)

// ExampleApplication demonstrates wiring a HealthServer, a couple of components
// with different tiers and restart policies, and a main function into a single
// Application.Run call.
func ExampleApplication() {
	sup := samsara.NewSupervisor(
		samsara.WithHealthInterval(5 * time.Second),
	)

	// HealthServer is registered first so it starts first and stops last.
	hs := samsara.NewHealthServer(sup, samsara.WithHealthAddr("127.0.0.1:0"))
	sup.Add(hs)

	db := testutil.NewFakeComponent("db")
	cache := testutil.NewFakeComponent("cache")

	sup.Add(db) // TierCritical by default
	sup.Add(cache,
		samsara.WithTier(samsara.TierSignificant),
		samsara.WithDependencies("db"),
		samsara.WithRestartPolicy(samsara.ExponentialBackoff(5, 100*time.Millisecond)),
		samsara.WithHealthFailThreshold(3), // debounce transient blips
	)

	app := samsara.NewApplication(
		samsara.WithSupervisor(sup),
		samsara.WithMainFunc(func(ctx context.Context) error {
			// Real work would run here until ctx is cancelled.
			<-ctx.Done()
			return nil
		}),
		samsara.WithShutdownTimeout(20*time.Second),
	)

	// Trigger a shutdown shortly after start so the example terminates.
	go func() {
		time.Sleep(50 * time.Millisecond)
		app.Shutdown(nil)
	}()

	if err := app.Run(); err != nil {
		fmt.Println("run error:", err)
		return
	}
	fmt.Println("stopped cleanly")
	// Output: stopped cleanly
}

// ExampleEventHooks shows draining and lifecycle telemetry via hooks.
func ExampleEventHooks() {
	hooks := &samsara.EventHooks{
		OnReady:    func(c string) { fmt.Println("ready:", c) },
		BeforeStop: func(c string) { fmt.Println("draining:", c) },
		OnStopped:  func(c string, _ error) { fmt.Println("stopped:", c) },
		OnUnhealthy: func(c string, err error) {
			fmt.Printf("unhealthy: %s (%v)\n", c, err)
		},
	}

	sup := samsara.NewSupervisor(samsara.WithEventHooks(hooks))
	sup.Add(testutil.NewFakeComponent("api"), samsara.WithTier(samsara.TierAuxiliary))

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = sup.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	time.Sleep(50 * time.Millisecond)
	// Unordered output:
	// ready: api
	// draining: api
	// stopped: api
}

// ExampleWithHealthFailThreshold shows configuring a component so a single failed
// probe does not immediately flip readiness or trigger a restart.
func ExampleWithHealthFailThreshold() {
	f := testutil.NewFakeComponent("flaky", testutil.WithInitialHealthError(errors.New("warming up")))
	sup := samsara.NewSupervisor()
	sup.Add(f,
		samsara.WithTier(samsara.TierAuxiliary),
		samsara.WithHealthFailThreshold(3),    // unhealthy after 3 consecutive fails
		samsara.WithHealthRecoverThreshold(2), // recovered after 2 consecutive oks
		samsara.WithHealthJitter(0.1),         // de-synchronise probes
	)
	fmt.Println("configured")
	// Output: configured
}

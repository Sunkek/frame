package samsara_test

import (
	"context"
	"testing"
	"time"

	"github.com/sunkek/samsara"
)

// C3: Shutdown called before Run is honoured — Run starts then shuts down
// immediately instead of silently ignoring the request.
func TestShutdownBeforeRun_HonouredNotIgnored(t *testing.T) {
	ranMain := make(chan struct{})
	app := samsara.NewApplication(
		samsara.WithMainFunc(func(ctx context.Context) error {
			close(ranMain)
			<-ctx.Done() // would block forever if the pre-Run Shutdown were ignored
			return nil
		}),
		samsara.WithShutdownTimeout(2*time.Second),
	)

	app.Shutdown(nil) // before Run

	done := make(chan error, 1)
	go func() { done <- app.Run() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected clean shutdown, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return; pre-Run Shutdown was ignored")
	}

	select {
	case <-ranMain:
	default:
		t.Fatal("main func should still have started before the immediate shutdown")
	}
}

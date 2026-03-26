package frame

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// ApplicationOption configures an Application.
type ApplicationOption func(*applicationConfig)

type applicationConfig struct {
	mainFunc        func(ctx context.Context) error
	supervisor      *Supervisor
	shutdownTimeout time.Duration
	logger          Logger
}

// WithMainFunc sets the primary function that runs as the application's main
// goroutine. The context passed to f is cancelled when an OS shutdown signal
// is received or when the Supervisor encounters a critical failure.
// Returning a non-nil error from f is treated as an application-level failure.
func WithMainFunc(f func(ctx context.Context) error) ApplicationOption {
	return func(c *applicationConfig) { c.mainFunc = f }
}

// WithSupervisor attaches a Supervisor to the application. The supervisor is
// started alongside the main function and both receive the same root context.
func WithSupervisor(s *Supervisor) ApplicationOption {
	return func(c *applicationConfig) { c.supervisor = s }
}

// WithShutdownTimeout sets how long the application waits for the main
// function and supervisor to exit after the root context is cancelled.
// Defaults to 15 s. If the timeout is exceeded, ErrShutdownTimeout is joined
// into the returned error.
func WithShutdownTimeout(d time.Duration) ApplicationOption {
	return func(c *applicationConfig) { c.shutdownTimeout = d }
}

// WithLogger sets the logger used by the Application itself (not the
// Supervisor — pass WithSupervisorLogger to NewSupervisor for that).
func WithLogger(l Logger) ApplicationOption {
	return func(c *applicationConfig) { c.logger = l }
}

// Application is the top-level entry point for a service. It wires together
// signal handling, an optional Supervisor, and a main function into a single
// blocking Run call.
//
// Typical usage:
//
//	sup := frame.NewSupervisor(...)
//	sup.Add(myDB, frame.WithTier(frame.TierCritical))
//	sup.Add(myCache, frame.WithTier(frame.TierSignificant))
//
//	app := frame.NewApplication(
//	    frame.WithSupervisor(sup),
//	    frame.WithMainFunc(server.Run),
//	    frame.WithShutdownTimeout(20*time.Second),
//	)
//	if err := app.Run(); err != nil {
//	    log.Fatal(err)
//	}
type Application struct {
	main            func(ctx context.Context) error
	supervisor      *Supervisor
	shutdownTimeout time.Duration
	logger          Logger

	mu         sync.Mutex
	cancelRoot context.CancelCauseFunc
}

// NewApplication constructs an Application with the supplied options.
func NewApplication(opts ...ApplicationOption) *Application {
	cfg := applicationConfig{
		shutdownTimeout: defaultShutdownTimeout,
		logger:          newNopLogger(),
	}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	return &Application{
		main:            cfg.mainFunc,
		supervisor:      cfg.supervisor,
		shutdownTimeout: cfg.shutdownTimeout,
		logger:          cfg.logger,
	}
}

// Shutdown cancels the application's root context, triggering a graceful
// shutdown. The optional cause is attached to the context so that components
// and the main function can inspect it via context.Cause if needed.
//
// It is safe to call from any goroutine. Calling Shutdown before Run is a
// no-op. Calling it multiple times is safe; only the first cause is recorded.
func (a *Application) Shutdown(cause error) {
	a.mu.Lock()
	cancel := a.cancelRoot
	a.mu.Unlock()
	if cancel != nil {
		cancel(cause)
	}
}

// Run starts the application and blocks until it exits.
//
// Startup order:
//  1. Root context is created and wired to OS signals (SIGINT, SIGTERM,
//     SIGHUP, SIGQUIT).
//  2. Supervisor.Run is launched in a goroutine (if a Supervisor was provided).
//  3. The main function is launched in a goroutine (if one was provided).
//
// Shutdown is triggered by any of:
//   - An OS signal.
//   - A call to Application.Shutdown(cause).
//   - The main function returning (with or without an error).
//   - The Supervisor encountering a critical failure.
//
// After the shutdown signal, Run waits up to ShutdownTimeout for both
// goroutines to finish. If they do not, ErrShutdownTimeout is joined into
// the returned error.
func (a *Application) Run() error {
	if a.main == nil && a.supervisor == nil {
		return ErrMainOmitted
	}

	sigCtx, stopSig := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
		syscall.SIGHUP,
		syscall.SIGQUIT,
	)
	defer stopSig()

	// WithCancelCause lets Shutdown() and internal failures attach a reason to
	// the context, which components can inspect via context.Cause(ctx).
	rootCtx, cancelRoot := context.WithCancelCause(sigCtx)
	defer cancelRoot(nil)

	a.mu.Lock()
	a.cancelRoot = cancelRoot
	a.mu.Unlock()

	a.logger.Info("application starting")

	errCh := make(chan error, 2)
	var wg sync.WaitGroup

	if a.supervisor != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := a.supervisor.Run(rootCtx); err != nil {
				a.logger.Error("supervisor exited with error", "error", err)
				errCh <- err
				cancelRoot(err)
			}
		}()
	}

	if a.main != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := a.main(rootCtx); err != nil {
				a.logger.Error("main function exited with error", "error", err)
				errCh <- err
			}
			// Always cancel on main exit so supervisor and other goroutines
			// are notified, whether or not main returned an error.
			cancelRoot(nil)
		}()
	}

	<-rootCtx.Done()
	a.logger.Info("application shutting down")

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	var timeoutErr error
	select {
	case <-done:
	case <-time.After(a.shutdownTimeout):
		a.logger.Error("shutdown timeout exceeded", "timeout", a.shutdownTimeout)
		timeoutErr = ErrShutdownTimeout
	}

	close(errCh)

	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}
	if timeoutErr != nil {
		errs = append(errs, timeoutErr)
	}

	if len(errs) == 0 {
		a.logger.Info("application stopped cleanly")
		return nil
	}
	return errors.Join(errs...)
}

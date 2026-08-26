package samsara

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

// WithLogger sets the logger used by the Application. The Supervisor attached
// with WithSupervisor inherits it, and in turn passes it on to the components
// it manages, so a single WithLogger call gives the whole tree logging. A
// logger set explicitly with WithSupervisorLogger or WithHealthLogger takes
// precedence over the inherited one. A nil logger is ignored.
func WithLogger(l Logger) ApplicationOption {
	return func(c *applicationConfig) {
		if l != nil {
			c.logger = l
		}
	}
}

// Application is the top-level entry point for a service. It wires together
// signal handling, an optional Supervisor, and a main function into a single
// blocking Run call.
//
// Typical usage:
//
//	sup := samsara.NewSupervisor(...)
//	sup.Add(myDB, samsara.WithTier(samsara.TierCritical))
//	sup.Add(myCache, samsara.WithTier(samsara.TierSignificant))
//
//	app := samsara.NewApplication(
//	    samsara.WithSupervisor(sup),
//	    samsara.WithMainFunc(server.Run),
//	    samsara.WithShutdownTimeout(20*time.Second),
//	)
//	if err := app.Run(); err != nil {
//	    log.Fatal(err)
//	}
type Application struct {
	main            func(ctx context.Context) error
	supervisor      *Supervisor
	shutdownTimeout time.Duration
	logger          Logger
	loggerSet       bool // a logger was supplied via WithLogger

	mu              sync.Mutex
	cancelRoot      context.CancelCauseFunc
	pendingShutdown bool  // Shutdown was called before Run wired cancelRoot
	pendingCause    error // cause recorded by a pre-Run Shutdown
}

// NewApplication constructs an Application with the supplied options.
func NewApplication(opts ...ApplicationOption) *Application {
	cfg := applicationConfig{
		shutdownTimeout: defaultShutdownTimeout,
	}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	logger := cfg.logger
	if logger == nil {
		logger = newNopLogger()
	}
	return &Application{
		main:            cfg.mainFunc,
		supervisor:      cfg.supervisor,
		shutdownTimeout: cfg.shutdownTimeout,
		logger:          logger,
		loggerSet:       cfg.logger != nil,
	}
}

// isContextError reports whether err is (or wraps) a standard context
// cancellation or deadline error. These indicate an externally-triggered
// shutdown, not a component failure, and should be treated as clean exits.
func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// Shutdown cancels the application's root context, triggering a graceful
// shutdown. The optional cause is attached to the context so that components
// and the main function can inspect it via context.Cause if needed.
//
// It is safe to call from any goroutine. Calling Shutdown before Run records
// the request: the subsequent Run starts and then immediately begins a graceful
// shutdown with the recorded cause, rather than silently ignoring the call.
// Calling it multiple times is safe; only the first cause is recorded.
func (a *Application) Shutdown(cause error) {
	a.mu.Lock()
	cancel := a.cancelRoot
	if cancel == nil && !a.pendingShutdown {
		// Run has not wired the root context yet — remember the request so Run
		// can honour it as soon as it starts.
		a.pendingShutdown = true
		a.pendingCause = cause
	}
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
		return ErrNothingToRun
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
	pending, pendingCause := a.pendingShutdown, a.pendingCause
	a.mu.Unlock()

	a.logger.Info("application starting")

	// A Shutdown call that arrived before Run wired the context is honoured now:
	// start normally, then trigger graceful shutdown immediately.
	if pending {
		a.logger.Info("shutdown requested before run — shutting down immediately")
		cancelRoot(pendingCause)
	}

	errCh := make(chan error, 2)
	var wg sync.WaitGroup

	if a.supervisor != nil {
		// Bound the supervisor's total shutdown work by the application's
		// shutdown budget (A3) unless the caller set an explicit grace.
		a.supervisor.setDefaultShutdownGrace(a.shutdownTimeout)
		if a.loggerSet {
			a.supervisor.setDefaultLogger(a.logger)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := a.supervisor.Run(rootCtx); err != nil {
				// An OS signal cancels the root context, which propagates into
				// the supervisor as a context error. That is a clean shutdown,
				// not a failure — filter it out so we don't log it as an error
				// or return a non-zero exit code.
				if isContextError(rootCtx.Err()) {
					return
				}
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
	// Re-enable normal signal delivery immediately. Until stopSig is called,
	// signal.NotifyContext intercepts every signal — so repeated Ctrl+C presses
	// during shutdown are silently swallowed. Calling it here means the second
	// signal kills the process instead of being ignored.
	stopSig()
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
		a.logger.Warn("shutdown timeout exceeded", "timeout", a.shutdownTimeout)
		timeoutErr = ErrShutdownTimeout
	}

	// Drain errCh without closing it. On the timeout path the main and
	// supervisor goroutines may still be alive and could send after we return;
	// closing here would turn that late send into a send-on-closed-channel
	// panic during shutdown. errCh is buffered to cap(errCh) with at most that
	// many senders, so every send fits the buffer and a non-blocking drain
	// collects all errors already reported.
	var errs []error
	for i := 0; i < cap(errCh); i++ {
		select {
		case err := <-errCh:
			if err != nil {
				errs = append(errs, err)
			}
		default:
		}
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

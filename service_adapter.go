package frame

import (
	"context"
	"fmt"
	"sync"
)

// LegacyService is the interface implemented by services written for the
// previous version of the framework. Wrap them with NewServiceAdapter to use
// them as Components without rewriting.
type LegacyService interface {
	Ident() string
	Init(ctx context.Context) error
	Ping(ctx context.Context) error
	Close() error
}

// ServiceAdapter wraps a LegacyService and exposes it as a Component.
// It implements Starter so the supervisor receives a precise ready signal the
// moment Init completes, with no probe-window guessing.
//
// ServiceAdapter is safe for reuse across restarts: each call to Start
// reinitialises the internal channels so a fresh restart cycle is clean.
//
// Example:
//
//	pgAdapter := frame.NewServiceAdapter(myPostgresService)
//	sup.Add(pgAdapter, frame.WithTier(frame.TierCritical),
//	    frame.WithRestartPolicy(frame.ExponentialBackoff(5, time.Second)))
type ServiceAdapter struct {
	svc LegacyService

	mu      sync.Mutex
	readyCh chan struct{}
	stopCh  chan struct{}
	doneCh  chan struct{}
}

// NewServiceAdapter wraps svc as a Component.
func NewServiceAdapter(svc LegacyService) *ServiceAdapter {
	a := &ServiceAdapter{svc: svc}
	a.resetChannels()
	return a
}

// resetChannels reinitialises the three lifecycle channels. Called at the
// start of every Start invocation so that restart cycles begin with a clean
// state and do not close already-closed channels.
func (a *ServiceAdapter) resetChannels() {
	a.readyCh = make(chan struct{})
	a.stopCh = make(chan struct{})
	a.doneCh = make(chan struct{})
}

func (a *ServiceAdapter) Name() string { return a.svc.Ident() }

// Started implements Starter. The returned channel is closed once Init
// completes successfully, giving the supervisor a precise readiness signal.
// The channel is replaced on every restart, so callers must call Started()
// after each Start invocation begins — the supervisor does this correctly.
func (a *ServiceAdapter) Started() <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.readyCh
}

// Start initialises the underlying service and blocks until Stop is called or
// ctx is cancelled. It is safe to call Start again after a previous Start has
// returned — channels are reinitialised at the top of each call.
func (a *ServiceAdapter) Start(ctx context.Context) error {
	// Reinitialise channels so this Start call is independent of any previous
	// one. This is what makes restarts safe: each cycle gets fresh channels.
	a.mu.Lock()
	a.resetChannels()
	readyCh := a.readyCh
	stopCh := a.stopCh
	doneCh := a.doneCh
	a.mu.Unlock()

	if err := a.svc.Init(ctx); err != nil {
		return fmt.Errorf("%s: init: %w", a.svc.Ident(), err)
	}
	close(readyCh) // signal readiness to the supervisor

	select {
	case <-stopCh:
	case <-ctx.Done():
	}
	close(doneCh)
	return nil
}

// Stop signals Start to unblock and then closes the underlying service.
// It respects the context deadline when waiting for Start to return, ensuring
// it does not block beyond the supervisor's configured stop timeout.
func (a *ServiceAdapter) Stop(ctx context.Context) error {
	a.mu.Lock()
	stopCh := a.stopCh
	doneCh := a.doneCh
	a.mu.Unlock()

	select {
	case <-stopCh:
		// already closed — another goroutine beat us here
	default:
		close(stopCh)
	}
	// Wait for Start to return before calling Close so we don't race on the
	// underlying resource. Honour the caller's context deadline.
	select {
	case <-doneCh:
	case <-ctx.Done():
	}
	return a.svc.Close()
}

// Health delegates to the underlying service's Ping method.
func (a *ServiceAdapter) Health(ctx context.Context) error {
	return a.svc.Ping(ctx)
}

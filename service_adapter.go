package frame

import (
	"context"
	"fmt"
	"time"
)

// LegacyService is the interface implemented by services written for the
// previous version of the framework (ServiceKeeper). Wrap them with
// NewServiceAdapter to use them as Components without rewriting.
type LegacyService interface {
	// Ident returns a stable unique name for this service.
	Ident() string
	// Init initialises the service (connects to DB, establishes pool, etc.).
	Init(ctx context.Context) error
	// Ping checks whether the service is still healthy.
	Ping(ctx context.Context) error
	// Close releases all resources held by the service.
	Close() error
}

// ServiceAdapter wraps a LegacyService and exposes it as a Component.
//
// The adapter's Start method calls Init and then blocks until ctx is cancelled
// or Stop is called. Health is delegated to Ping. Stop closes the underlying
// service.
//
// Example:
//
//	pgAdapter := frame.NewServiceAdapter(myPostgresService)
//	sup.Add(pgAdapter, frame.WithTier(frame.TierCritical))
type ServiceAdapter struct {
	svc  LegacyService
	stop chan struct{}
	done chan struct{}
}

// NewServiceAdapter wraps svc as a Component.
func NewServiceAdapter(svc LegacyService) *ServiceAdapter {
	return &ServiceAdapter{svc: svc}
}

func (a *ServiceAdapter) Name() string { return a.svc.Ident() }

// Start initialises the underlying service and then blocks until Stop is called
// or ctx is cancelled.
func (a *ServiceAdapter) Start(ctx context.Context) error {
	a.stop = make(chan struct{})
	a.done = make(chan struct{})

	if err := a.svc.Init(ctx); err != nil {
		return fmt.Errorf("%s: init: %w", a.svc.Ident(), err)
	}

	// Block until the supervisor asks us to stop or the context is cancelled.
	select {
	case <-a.stop:
	case <-ctx.Done():
	}

	close(a.done)
	return nil
}

// Stop signals Start to unblock and closes the underlying service.
func (a *ServiceAdapter) Stop(_ context.Context) error {
	if a.stop != nil {
		select {
		case <-a.stop:
			// already closed
		default:
			close(a.stop)
		}
	}
	// Wait for Start to return before calling Close so we don't race.
	if a.done != nil {
		select {
		case <-a.done:
		case <-time.After(5 * time.Second):
			// Best-effort; proceed to Close regardless.
		}
	}
	return a.svc.Close()
}

// Health delegates to the underlying service's Ping method.
// The Supervisor will call this on every health-check interval.
func (a *ServiceAdapter) Health(ctx context.Context) error {
	return a.svc.Ping(ctx)
}

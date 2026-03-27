package frame

import (
	"context"
	"fmt"
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
// Init is called inside Start; once Init returns without error, ready() is
// called so the supervisor can proceed to the next component.
//
// Example:
//
//	pgAdapter := frame.NewServiceAdapter(myPostgresService)
//	sup.Add(pgAdapter, frame.WithTier(frame.TierCritical))
type ServiceAdapter struct {
	svc    LegacyService
	stopCh chan struct{}
	doneCh chan struct{}
}

// NewServiceAdapter wraps svc as a Component.
func NewServiceAdapter(svc LegacyService) *ServiceAdapter {
	return &ServiceAdapter{svc: svc}
}

func (a *ServiceAdapter) Name() string { return a.svc.Ident() }

// Start initialises the underlying service, calls ready(), then blocks until
// Stop is called or ctx is cancelled. Channels are reinitialised at the top
// of each call so restarts are clean.
func (a *ServiceAdapter) Start(ctx context.Context, ready func()) error {
	a.stopCh = make(chan struct{})
	a.doneCh = make(chan struct{})

	if err := a.svc.Init(ctx); err != nil {
		return fmt.Errorf("%s: init: %w", a.svc.Ident(), err)
	}
	ready() // Init succeeded — supervisor proceeds

	select {
	case <-a.stopCh:
	case <-ctx.Done():
	}
	close(a.doneCh)
	return nil
}

// Stop signals Start to unblock and then closes the underlying service.
func (a *ServiceAdapter) Stop(ctx context.Context) error {
	select {
	case <-a.stopCh:
	default:
		close(a.stopCh)
	}
	select {
	case <-a.doneCh:
	case <-ctx.Done():
	}
	return a.svc.Close()
}

// Health delegates to the underlying service's Ping method.
func (a *ServiceAdapter) Health(ctx context.Context) error {
	return a.svc.Ping(ctx)
}

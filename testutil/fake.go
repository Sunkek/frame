// Package testutil provides configurable fakes for exercising samsara
// Supervisors and Applications in tests without hand-rolling component stubs.
//
// FakeComponent implements samsara.Component and samsara.HealthChecker with
// controllable ready/health/stop behaviour and goroutine-safe call counters. It
// has no dependencies beyond the standard library.
package testutil

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// FakeComponent is a configurable samsara.Component (and HealthChecker) for
// tests. Construct it with NewFakeComponent and tune behaviour via the options.
// All exported methods are safe for concurrent use.
type FakeComponent struct {
	name string

	startErr   error         // if non-nil, Start returns it immediately (never signals ready)
	readyDelay time.Duration // wait before calling ready()
	blockReady bool          // never call ready() (simulate a component that never becomes ready)
	stopErr    error         // returned by Stop

	healthErr atomic.Value // stores string; "" means healthy
	crashErr  atomic.Value // stores error box; set by Crash to make a running Start return

	starts  atomic.Int32
	stops   atomic.Int32
	healths atomic.Int32

	mu      sync.Mutex
	ready   chan struct{} // closed once ready() first fires
	crashCh chan struct{} // closed by Crash to unblock a running Start
}

// Option configures a FakeComponent at construction time.
type Option func(*FakeComponent)

// WithStartError makes Start return err immediately without signalling ready,
// simulating a component that fails to start.
func WithStartError(err error) Option {
	return func(f *FakeComponent) { f.startErr = err }
}

// WithReadyDelay delays the ready() call by d, simulating slow startup.
func WithReadyDelay(d time.Duration) Option {
	return func(f *FakeComponent) { f.readyDelay = d }
}

// WithBlockReady makes Start never call ready(), simulating a component that
// hangs during startup (exercises the supervisor's start timeout).
func WithBlockReady() Option {
	return func(f *FakeComponent) { f.blockReady = true }
}

// WithStopError makes Stop return err.
func WithStopError(err error) Option {
	return func(f *FakeComponent) { f.stopErr = err }
}

// WithInitialHealthError starts the component in an unhealthy state.
func WithInitialHealthError(err error) Option {
	return func(f *FakeComponent) {
		if err != nil {
			f.healthErr.Store(err.Error())
		}
	}
}

// NewFakeComponent constructs a FakeComponent with the given name and options.
func NewFakeComponent(name string, opts ...Option) *FakeComponent {
	f := &FakeComponent{
		name:    name,
		ready:   make(chan struct{}),
		crashCh: make(chan struct{}),
	}
	f.healthErr.Store("")
	for _, o := range opts {
		if o != nil {
			o(f)
		}
	}
	return f
}

// Name implements samsara.Component.
func (f *FakeComponent) Name() string { return f.name }

// Start implements samsara.Component. It honours the configured start/ready
// behaviour, then blocks until Stop is called, ctx is cancelled, or Crash is
// invoked (in which case it returns the crash error).
func (f *FakeComponent) Start(ctx context.Context, ready func()) error {
	f.starts.Add(1)

	if f.startErr != nil {
		return f.startErr
	}

	if f.readyDelay > 0 {
		select {
		case <-time.After(f.readyDelay):
		case <-ctx.Done():
			return nil
		}
	}

	if !f.blockReady {
		ready()
		f.mu.Lock()
		select {
		case <-f.ready:
		default:
			close(f.ready)
		}
		f.mu.Unlock()
	}

	select {
	case <-ctx.Done():
		return nil
	case <-f.crashCh:
		if v := f.crashErr.Load(); v != nil {
			if boxed, ok := v.(errBox); ok {
				return boxed.err
			}
		}
		return errors.New("testutil: fake component crashed")
	}
}

// Stop implements samsara.Component. It is idempotent and safe to call
// concurrently with Start.
func (f *FakeComponent) Stop(context.Context) error {
	f.stops.Add(1)
	return f.stopErr
}

// Health implements samsara.HealthChecker, returning the currently configured
// health error (nil when healthy).
func (f *FakeComponent) Health(context.Context) error {
	f.healths.Add(1)
	s, _ := f.healthErr.Load().(string)
	if s == "" {
		return nil
	}
	return errors.New(s)
}

// SetHealthError updates the error returned by subsequent Health calls. Pass nil
// to mark the component healthy again.
func (f *FakeComponent) SetHealthError(err error) {
	if err == nil {
		f.healthErr.Store("")
		return
	}
	f.healthErr.Store(err.Error())
}

// errBox lets a typed error be stored in an atomic.Value with a stable concrete
// type across calls.
type errBox struct{ err error }

// Crash makes a currently-running Start return err (simulating a post-ready
// crash). It is a no-op if the component is not running. Safe to call once.
func (f *FakeComponent) Crash(err error) {
	if err == nil {
		err = errors.New("testutil: fake component crashed")
	}
	f.crashErr.Store(errBox{err: err})
	f.mu.Lock()
	select {
	case <-f.crashCh:
	default:
		close(f.crashCh)
	}
	f.mu.Unlock()
}

// WaitReady blocks until Start has signalled ready() or timeout elapses,
// reporting whether ready fired in time.
func (f *FakeComponent) WaitReady(timeout time.Duration) bool {
	select {
	case <-f.ready:
		return true
	case <-time.After(timeout):
		return false
	}
}

// StartCount returns how many times Start has been invoked (increments on each
// restart).
func (f *FakeComponent) StartCount() int { return int(f.starts.Load()) }

// StopCount returns how many times Stop has been invoked.
func (f *FakeComponent) StopCount() int { return int(f.stops.Load()) }

// HealthCount returns how many times Health has been polled.
func (f *FakeComponent) HealthCount() int { return int(f.healths.Load()) }

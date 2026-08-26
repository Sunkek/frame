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

	starts  atomic.Int32
	stops   atomic.Int32
	healths atomic.Int32

	// Per-life state. A life begins when Start is called and ends when Start
	// returns, whether through Crash or context cancellation. Both channels are
	// replaced at the start of the next life so a fresh Start neither inherits
	// the previous life's crash nor its readiness.
	mu         sync.Mutex
	readyCh    chan struct{} // closed when the current life signals ready()
	readyFired bool          // whether readyCh is closed
	crashCh    chan struct{} // closed by Crash to unblock the running Start
	crashed    bool          // whether crashCh is closed
	crashErr   error         // error the crashed life's Start returns
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
		readyCh: make(chan struct{}),
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
// invoked (in which case it returns the crash error). Each call begins a fresh
// life: a crash or cancellation from a previous life does not carry over.
func (f *FakeComponent) Start(ctx context.Context, ready func()) error {
	f.starts.Add(1)

	if f.startErr != nil {
		return f.startErr
	}

	crashCh := f.beginLife()

	if f.readyDelay > 0 {
		select {
		case <-time.After(f.readyDelay):
		case <-ctx.Done():
			f.endLife()
			return nil
		}
	}

	if !f.blockReady {
		ready()
		f.signalReady()
	}

	select {
	case <-ctx.Done():
		f.endLife()
		return nil
	case <-crashCh:
		f.mu.Lock()
		err := f.crashErr
		f.mu.Unlock()
		if err == nil {
			err = errors.New("testutil: fake component crashed")
		}
		return err
	}
}

// beginLife starts a fresh life, replacing any channel the previous life
// already closed, and returns the crash channel this life must select on.
func (f *FakeComponent) beginLife() chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readyFired {
		f.readyCh = make(chan struct{})
		f.readyFired = false
	}
	if f.crashed {
		f.crashCh = make(chan struct{})
		f.crashed = false
		f.crashErr = nil
	}
	return f.crashCh
}

// endLife marks the current life as no longer ready, so a WaitReady that
// straddles a restart waits for the next life rather than the finished one.
func (f *FakeComponent) endLife() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clearReadyLocked()
}

// signalReady closes the current life's ready channel.
func (f *FakeComponent) signalReady() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.readyFired {
		f.readyFired = true
		close(f.readyCh)
	}
}

// clearReadyLocked resets readiness for the life that just ended. f.mu must be
// held.
func (f *FakeComponent) clearReadyLocked() {
	if f.readyFired {
		f.readyCh = make(chan struct{})
		f.readyFired = false
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

// Crash makes the currently-running Start return err, simulating a post-ready
// crash. It applies to the current life only: the component is left not ready,
// and a subsequent Start begins a fresh life that blocks as a healthy component
// would rather than returning err again. It is a no-op if the current life has
// already crashed.
func (f *FakeComponent) Crash(err error) {
	if err == nil {
		err = errors.New("testutil: fake component crashed")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clearReadyLocked()
	if f.crashed {
		return
	}
	f.crashErr = err
	f.crashed = true
	close(f.crashCh)
}

// WaitReady blocks until the current life signals ready() or timeout elapses,
// reporting whether ready fired in time. It reflects the current life only: a
// crashed or cancelled life is not ready, so after a crash WaitReady waits for
// the restarted component to signal ready again.
func (f *FakeComponent) WaitReady(timeout time.Duration) bool {
	f.mu.Lock()
	ch := f.readyCh
	f.mu.Unlock()
	select {
	case <-ch:
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

package frame

import (
	"math/rand/v2"
	"time"
)

// RestartPolicy decides whether a component should be restarted after a
// failure and, if so, how long to wait before the next attempt.
//
// attempt is zero-based: the first restart is attempt 0, the second is 1, etc.
// Returning false for restart means the component has failed permanently.
type RestartPolicy interface {
	ShouldRestart(err error, attempt int) (restart bool, delay time.Duration)
}

// NeverRestart returns a policy that never restarts a component.
// Use this for components whose failure should propagate immediately.
func NeverRestart() RestartPolicy { return neverRestart{} }

type neverRestart struct{}

func (neverRestart) ShouldRestart(_ error, _ int) (bool, time.Duration) { return false, 0 }

// AlwaysRestart returns a policy that restarts a component unconditionally
// with a fixed delay between attempts.
func AlwaysRestart(delay time.Duration) RestartPolicy {
	return alwaysRestart{delay: delay}
}

type alwaysRestart struct{ delay time.Duration }

func (p alwaysRestart) ShouldRestart(_ error, _ int) (bool, time.Duration) {
	return true, p.delay
}

// MaxRetries returns a policy that restarts a component up to maxRetries times
// with a fixed delay. After maxRetries attempts the component fails permanently.
func MaxRetries(maxRetries int, delay time.Duration) RestartPolicy {
	return maxRetriesPolicy{max: maxRetries, delay: delay}
}

type maxRetriesPolicy struct {
	max   int
	delay time.Duration
}

func (p maxRetriesPolicy) ShouldRestart(_ error, attempt int) (bool, time.Duration) {
	if attempt >= p.max {
		return false, 0
	}
	return true, p.delay
}

// ExponentialBackoff returns a policy that restarts a component up to
// maxRetries times. The delay doubles with each attempt starting from
// baseDelay, with ±25% jitter applied to spread restarts when many instances
// fail simultaneously:
//
//	attempt 0: baseDelay  × [0.75, 1.25)
//	attempt 1: 2×baseDelay × [0.75, 1.25)
//	attempt 2: 4×baseDelay × [0.75, 1.25)  …and so on
func ExponentialBackoff(maxRetries int, baseDelay time.Duration) RestartPolicy {
	return exponentialBackoff{max: maxRetries, base: baseDelay}
}

type exponentialBackoff struct {
	max  int
	base time.Duration
}

func (p exponentialBackoff) ShouldRestart(_ error, attempt int) (bool, time.Duration) {
	if attempt >= p.max {
		return false, 0
	}
	base := p.base * (1 << attempt)
	jitter := time.Duration(float64(base) * (0.75 + rand.Float64()*0.5))
	return true, jitter
}

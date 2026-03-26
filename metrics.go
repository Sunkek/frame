package frame

import "time"

// MetricsObserver receives structured telemetry events from the Supervisor.
// Implement this interface to bridge into Prometheus, OpenTelemetry, Datadog,
// or any other metrics backend without adding a hard dependency to this package.
//
// All methods are called synchronously from the supervisor goroutine that
// manages the component, so they must not block. Enqueue or use a non-blocking
// write if your backend requires I/O.
//
// All fields are optional at the implementation level — a partial observer
// that only cares about restarts is perfectly valid.
type MetricsObserver interface {
	// ComponentStarted is called each time a component's Start call returns
	// without error and the component is considered running.
	ComponentStarted(component string, attempt int)

	// ComponentStopped is called when a component's Stop call returns,
	// regardless of whether it returned an error.
	ComponentStopped(component string, err error)

	// ComponentRestarting is called when the supervisor decides to restart a
	// component after a failure. attempt is 1-based.
	ComponentRestarting(component string, err error, attempt int, delay time.Duration)

	// HealthCheckCompleted is called after every health check poll, whether
	// healthy or not. duration is how long the Health() call took.
	HealthCheckCompleted(component string, duration time.Duration, err error)
}

// nopMetrics discards all telemetry. Used as the default when no observer is
// registered.
type nopMetrics struct{}

func (nopMetrics) ComponentStarted(string, int)                          {}
func (nopMetrics) ComponentStopped(string, error)                        {}
func (nopMetrics) ComponentRestarting(string, error, int, time.Duration) {}
func (nopMetrics) HealthCheckCompleted(string, time.Duration, error)     {}

func newNopMetrics() MetricsObserver { return nopMetrics{} }

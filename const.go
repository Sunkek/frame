package frame

import "time"

const (
	// Application defaults.

	// defaultShutdownTimeout is how long Application.Run waits for the main
	// function and supervisor to exit cleanly after the root context is cancelled.
	defaultShutdownTimeout = 15 * time.Second

	// Supervisor / component defaults.

	// defaultHealthInterval is how often the supervisor polls HealthChecker
	// components after they have started successfully.
	defaultHealthInterval = 10 * time.Second

	// defaultStartTimeout caps the time a component's Start call may take.
	// If Start does not return within this window the supervisor treats it as
	// a failed start attempt.
	defaultStartTimeout = 15 * time.Second

	// defaultHealthTimeout caps each individual Health call.
	defaultHealthTimeout = 5 * time.Second

	// defaultStopTimeout caps each individual Stop call during shutdown.
	defaultStopTimeout = 10 * time.Second

	// defaultRestartResetWindow is how long a component must have been running
	// without fault before its restart-attempt counter is reset to zero.
	defaultRestartResetWindow = 5 * time.Minute

	// HealthServer defaults.

	// defaultHealthAddr is the default listen address for the HealthServer.
	defaultHealthAddr = ":8080"

	// defaultStartProbeWindow is how long the supervisor waits after calling
	// a component's Start before concluding the component is alive and blocking.
	// Components that fail immediately (e.g. can't bind a port) will return
	// within this window; components that succeed will still be running.
	defaultStartProbeWindow = 100 * time.Millisecond

	// defaultHealthReadTimeout caps how long the health HTTP server waits for
	// an incoming request to be fully read.
	defaultHealthReadTimeout = 5 * time.Second

	// defaultHealthWriteTimeout caps how long the health HTTP server waits to
	// write a response.
	defaultHealthWriteTimeout = 5 * time.Second
)

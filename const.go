package samsara

import "time"

const (
	// Application defaults.
	defaultShutdownTimeout = 15 * time.Second

	// shutdownGraceMarginDivisor sets the supervisor's default stop budget to
	// d - d/N of the application's shutdown timeout: 90% of the application
	// budget, leaving a margin so the supervisor finishes just inside it.
	shutdownGraceMarginDivisor = 10

	// Supervisor / component defaults.
	defaultHealthInterval     = 10 * time.Second
	defaultStartTimeout       = 15 * time.Second
	defaultHealthTimeout      = 5 * time.Second
	defaultStopTimeout        = 10 * time.Second
	defaultRestartResetWindow = 5 * time.Minute

	// HealthServer defaults.
	defaultHealthAddr         = ":9090"
	defaultHealthReadTimeout  = 5 * time.Second
	defaultHealthWriteTimeout = 5 * time.Second
)

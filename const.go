package samsara

import "time"

const (
	// Application defaults.
	defaultShutdownTimeout = 15 * time.Second

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

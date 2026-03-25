package frame

// appError is an immutable string-based error type for sentinel values.
// It is comparable with errors.Is without wrapping.
type appError string

func (e appError) Error() string { return string(e) }

const (
	// ErrMainOmitted is returned by Application.Run when no MainFunc was provided.
	ErrMainOmitted appError = "frame: main function is omitted"

	// ErrShutdownTimeout is returned when the application does not stop within
	// the configured ShutdownTimeout after the context is cancelled.
	ErrShutdownTimeout appError = "frame: shutdown timeout exceeded"

	// ErrComponentAlreadyRegistered is returned when a component with the same
	// name is added to the Supervisor more than once.
	ErrComponentAlreadyRegistered appError = "frame: component already registered"

	// ErrCircularDependency is returned when the Supervisor detects a cycle in
	// the component dependency graph.
	ErrCircularDependency appError = "frame: circular dependency detected"

	// ErrUnknownDependency is returned when a component declares a dependency on
	// a name that has not been registered with the Supervisor.
	ErrUnknownDependency appError = "frame: unknown dependency"

	// ErrSupervisorRunning is returned when Add is called after Run has started.
	ErrSupervisorRunning appError = "frame: cannot add component after supervisor has started"
)

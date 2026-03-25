package frame

// Logger is a minimal structured logging interface. It is intentionally narrow
// so that any slog, zap, zerolog, or logrus wrapper satisfies it with a thin
// adapter, keeping the framework free of logging dependencies.
//
// Key-value pairs are passed as alternating key, value arguments (slog style).
type Logger interface {
	Debug(msg string, kv ...any)
	Info(msg string, kv ...any)
	Error(msg string, kv ...any)
}

// nopLogger discards every log entry. It is used as the default when the caller
// does not supply a logger, so the framework never panics on a nil logger.
type nopLogger struct{}

func (nopLogger) Debug(string, ...any) {}
func (nopLogger) Info(string, ...any)  {}
func (nopLogger) Error(string, ...any) {}

func newNopLogger() Logger { return nopLogger{} }

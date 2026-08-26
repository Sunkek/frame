# Zero external dependencies

Samsara depends on nothing outside the Go standard library, and this is a hard
constraint rather than a current happy accident. A lifecycle supervisor sits at
the very bottom of a service's import graph, so any dependency it takes is
forced on every consumer and every consumer's version resolution.

The cost is that integration points must be expressed as interfaces the caller
implements — `Logger`, `MetricsObserver` — instead of concrete `slog`,
Prometheus, or OpenTelemetry types. Concrete adapters live in the separate
`samsara-components` module, which is free to take dependencies.

Reversing this would not just add a `go.mod` line; it would change the shape of
the public API, since the interfaces exist precisely to avoid the dependency.

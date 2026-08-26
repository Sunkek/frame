# The set of Components is fixed before the Supervisor runs

Components can only be added before `Run`; there is no dynamic add or remove at
runtime, and `Add` after start panics. The panic is what makes the property
enforceable rather than advisory: there is no error return to ignore.

This buys a specific concurrency property: the status map is built once, before
any goroutine exists, and is never structurally mutated afterwards. Per-component
management goroutines therefore read their own entry without taking the status
mutex, which keeps the hot path — health polling across many components — free
of a shared lock.

Dynamic membership was considered and deferred rather than rejected on
principle. Adding it means putting every status access behind the mutex first;
doing that later, in the presence of the existing goroutine choreography, is the
kind of change the race detector catches only sometimes. Anyone implementing it
should treat the lock migration as the first commit, not the last.

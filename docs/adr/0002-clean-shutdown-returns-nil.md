# A Component's Start returns nil on clean shutdown

When a Component's context is cancelled, its `Start` must return `nil`, not
`ctx.Err()`. The Supervisor cannot otherwise distinguish "you asked me to stop
and I did" from "I died on my own", and it needs that distinction to decide
whether to apply a restart policy.

This deviates from the common Go habit of propagating `ctx.Err()` upward, so
implementors reach for the wrong thing by default and reviewers should watch for
it. The alternative — inspecting the supervisor's own context instead of
trusting the return value — was rejected because a Component can fail for real
at the same moment a shutdown begins, and the return value is the only place
that difference is visible.

The contract is fixed for every existing implementation, in this repo and in
consumers, so it is expensive to change now.

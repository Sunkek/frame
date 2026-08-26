# Each option sets exactly one knob

Every `With…` option in the package takes a single value and sets a single knob.
Where a setting has more than one number, it gets more than one option:
`WithHealthFailThreshold(n)` and `WithHealthRecoverThreshold(n)`, not
`WithHealthThreshold(fail, recover int)`. The one exception is
`WithDependencies(names ...string)`, where the arguments are all the same kind of
thing and order carries no meaning.

The reason is that two same-typed parameters in one option make the call site
unreadable and the mistake silent. `samsara.WithHealthThreshold(3, 2)` did not
say which number was which, and swapping them compiled cleanly while inverting
the debounce: a component became slow to be declared unhealthy and quick to be
declared recovered, which is exactly backwards, and no test in the embedding
application would notice. Named single-knob options make the same error a
compile error, or at worst a visibly wrong line.

The cost is more exported names for the same surface, and options that must
compose rather than being set together — each threshold defaults independently
when its option is omitted, so partial configuration has to be a supported
state rather than an accident. That is the trade this package takes: the knob
count is small and fairly static, while call sites are written once and read
many times.

Two consequences for anyone extending the package. Adding a setting with several
components means several options, or a named struct passed as one value — never a
positional pair. And grouping knobs internally is unrelated to this: the health
knobs live together in the unexported `healthTuning` struct while still being
set by separate options, which is the intended shape.

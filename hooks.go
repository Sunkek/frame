package samsara

// EventHooks carries optional callbacks that the Supervisor fires on
// significant component lifecycle events. All fields are optional; a nil
// function is silently skipped.
//
// Hooks are called synchronously inside the supervisor goroutine that manages
// the component, so they must not block. Enqueue to a channel or spawn a
// goroutine if you need non-trivial work (e.g. sending a PagerDuty alert).
type EventHooks struct {
	// OnUnhealthy is called on a confirmed fault: either enough consecutive
	// failed Health probes to breach the configured threshold, or an
	// unexpected Start exit after ready(). It receives the component name and
	// the error that caused the fault.
	OnUnhealthy func(component string, err error)

	// OnRecovered is called when a component leaves the unhealthy state, after
	// enough consecutive successful Health probes to breach the recover
	// threshold. Either kind of fault enters that state, so a component that
	// crashed after ready(), restarted and is probing healthy again recovers
	// through this hook too. A component without a Health method has no probes
	// and so never recovers.
	OnRecovered func(component string)

	// OnFailed is called when a component fails permanently — either because
	// its restart policy decided not to retry, or because all retries were
	// exhausted. It receives the component name and the final error.
	OnFailed func(component string, err error)

	// OnRestart is called each time the supervisor schedules a restart attempt
	// for a component. It receives the component name, the triggering error,
	// and the attempt number (1-based).
	OnRestart func(component string, err error, attempt int)

	// OnReady is called each time a component signals ready() and the supervisor
	// considers it running — on first start and after every successful restart.
	// Use it to flip a load-balancer probe or emit lifecycle telemetry.
	OnReady func(component string)

	// BeforeStop is called immediately before the supervisor invokes a
	// component's Stop, whether for a restart or during shutdown. This is the
	// hook to begin draining in-flight work or deregister from a load balancer.
	BeforeStop func(component string)

	// OnStopped is called immediately after a component's Stop returns. It
	// receives the component name and the error Stop returned (nil on success).
	OnStopped func(component string, err error)
}

func (h *EventHooks) fireUnhealthy(component string, err error) {
	if h != nil && h.OnUnhealthy != nil {
		h.OnUnhealthy(component, err)
	}
}

func (h *EventHooks) fireRecovered(component string) {
	if h != nil && h.OnRecovered != nil {
		h.OnRecovered(component)
	}
}

func (h *EventHooks) fireFailed(component string, err error) {
	if h != nil && h.OnFailed != nil {
		h.OnFailed(component, err)
	}
}

func (h *EventHooks) fireRestart(component string, err error, attempt int) {
	if h != nil && h.OnRestart != nil {
		h.OnRestart(component, err, attempt)
	}
}

func (h *EventHooks) fireReady(component string) {
	if h != nil && h.OnReady != nil {
		h.OnReady(component)
	}
}

func (h *EventHooks) fireBeforeStop(component string) {
	if h != nil && h.BeforeStop != nil {
		h.BeforeStop(component)
	}
}

func (h *EventHooks) fireStopped(component string, err error) {
	if h != nil && h.OnStopped != nil {
		h.OnStopped(component, err)
	}
}

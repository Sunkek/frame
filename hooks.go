package frame

// EventHooks carries optional callbacks that the Supervisor fires on
// significant component lifecycle events. All fields are optional; a nil
// function is silently skipped.
//
// Hooks are called synchronously inside the supervisor goroutine that manages
// the component, so they must not block. Enqueue to a channel or spawn a
// goroutine if you need non-trivial work (e.g. sending a PagerDuty alert).
type EventHooks struct {
	// OnUnhealthy is called when a component's Health check returns a non-nil
	// error. It receives the component name and the health error.
	OnUnhealthy func(component string, err error)

	// OnRecovered is called when a component's Health check returns nil again
	// after a previous OnUnhealthy event.
	OnRecovered func(component string)

	// OnFailed is called when a component fails permanently — either because
	// its restart policy decided not to retry, or because all retries were
	// exhausted. It receives the component name and the final error.
	OnFailed func(component string, err error)

	// OnRestart is called each time the supervisor schedules a restart attempt
	// for a component. It receives the component name, the triggering error,
	// and the attempt number (1-based).
	OnRestart func(component string, err error, attempt int)
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

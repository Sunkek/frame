# The HealthServer is itself a supervised Component, registered first

The HTTP endpoints reporting liveness and readiness are served by a Component
registered with the Supervisor like any other, and by convention added before
all others.

The obvious alternative is to run the health endpoints outside supervision, as
part of the Application. That was rejected: registering it first means it starts
before anything else and stops last, so an orchestrator gets truthful readiness
during the whole startup and shutdown window — exactly the periods where a
misreported state does the most damage. Running it outside supervision would
either duplicate the ordering logic or leave those windows dark.

The consequence is a deliberate cycle in the object graph: the HealthServer
reads the Supervisor's liveness through an interface while being supervised by
it. That indirection is why `LivenessReporter` exists.

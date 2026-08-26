package samsara_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sunkek/samsara"
	"github.com/sunkek/samsara/testutil"
)

// captureLogger records every message it is handed, so a test can assert that
// a component logged at all and which logger it used.
type captureLogger struct {
	mu   sync.Mutex
	msgs []string
}

func (c *captureLogger) record(msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, msg)
}

func (c *captureLogger) Debug(msg string, _ ...any) { c.record(msg) }
func (c *captureLogger) Info(msg string, _ ...any)  { c.record(msg) }
func (c *captureLogger) Warn(msg string, _ ...any)  { c.record(msg) }
func (c *captureLogger) Error(msg string, _ ...any) { c.record(msg) }

// has reports whether any recorded message contains sub.
func (c *captureLogger) has(sub string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.msgs {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

func (c *captureLogger) empty() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.msgs) == 0
}

// runAppWith starts an application that owns a supervisor with the given
// health server and one fake component, then shuts it down once the component
// is ready.
func runAppWith(t *testing.T, sup *samsara.Supervisor, appOpts ...samsara.ApplicationOption) {
	t.Helper()
	fake := testutil.NewFakeComponent("fake")
	sup.Add(fake)

	app := samsara.NewApplication(append(appOpts, samsara.WithSupervisor(sup))...)
	done := make(chan error, 1)
	go func() { done <- app.Run() }()

	if !fake.WaitReady(2 * time.Second) {
		t.Fatal("component never became ready")
	}
	app.Shutdown(nil)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("app.Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("app.Run did not return")
	}
}

// The Application's logger reaches both the Supervisor and a HealthServer that
// was configured with no logger of its own.
func TestApplicationLoggerIsInherited(t *testing.T) {
	appLog := &captureLogger{}
	sup := samsara.NewSupervisor()
	hs := samsara.NewHealthServer(sup, samsara.WithHealthAddr("127.0.0.1:0"))
	sup.Add(hs)

	runAppWith(t, sup, samsara.WithLogger(appLog))

	if !appLog.has("application starting") {
		t.Error("application did not log through its own logger")
	}
	if !appLog.has("component starting") {
		t.Error("supervisor did not inherit the application logger")
	}
	if !appLog.has("health server starting") {
		t.Error("health server did not inherit the application logger")
	}
}

// An explicit supervisor or health-server logger outranks the inherited one.
func TestExplicitLoggersOutrankInherited(t *testing.T) {
	appLog, supLog, healthLog := &captureLogger{}, &captureLogger{}, &captureLogger{}

	sup := samsara.NewSupervisor(samsara.WithSupervisorLogger(supLog))
	hs := samsara.NewHealthServer(sup,
		samsara.WithHealthAddr("127.0.0.1:0"),
		samsara.WithHealthLogger(healthLog),
	)
	sup.Add(hs)

	runAppWith(t, sup, samsara.WithLogger(appLog))

	if !supLog.has("component starting") {
		t.Error("supervisor did not use its explicit logger")
	}
	if !healthLog.has("health server starting") {
		t.Error("health server did not use its explicit logger")
	}
	if appLog.has("component starting") || appLog.has("health server starting") {
		t.Error("application logger overrode an explicitly configured one")
	}
	if supLog.has("health server starting") {
		t.Error("health server used the supervisor logger despite its own")
	}
}

// A supervisor's own logger still reaches its components when there is no
// Application above it.
func TestSupervisorLoggerReachesComponents(t *testing.T) {
	supLog := &captureLogger{}
	sup := samsara.NewSupervisor(samsara.WithSupervisorLogger(supLog))
	hs := samsara.NewHealthServer(sup, samsara.WithHealthAddr("127.0.0.1:0"))
	sup.Add(hs)

	runAppWith(t, sup)

	if !supLog.has("health server starting") {
		t.Error("health server did not inherit the supervisor logger")
	}
}

// With no logger configured anywhere, nothing logs and nothing panics.
func TestNoLoggerAnywhere(t *testing.T) {
	unused := &captureLogger{}
	sup := samsara.NewSupervisor()
	hs := samsara.NewHealthServer(sup, samsara.WithHealthAddr("127.0.0.1:0"))
	sup.Add(hs)

	runAppWith(t, sup)

	if !unused.empty() {
		t.Error("a logger that was never configured received messages")
	}
}

// A nil logger is ignored rather than installed, so nothing panics later.
func TestNilLoggerIsIgnored(t *testing.T) {
	sup := samsara.NewSupervisor(samsara.WithSupervisorLogger(nil))
	hs := samsara.NewHealthServer(sup,
		samsara.WithHealthAddr("127.0.0.1:0"),
		samsara.WithHealthLogger(nil),
	)
	sup.Add(hs)

	runAppWith(t, sup, samsara.WithLogger(nil))
}

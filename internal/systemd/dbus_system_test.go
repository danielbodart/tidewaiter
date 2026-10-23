package systemd

import (
	"os"
	"regexp"
	"testing"
)

// systemBus is the host's real system bus, or a skip where there is none -
// a container, a sandbox, a machine without systemd - or where
// TIDEWAITER_SKIP_SYSTEM_BUS says not to use it.
func systemBus(t *testing.T) *DBus {
	t.Helper()
	if os.Getenv("TIDEWAITER_SKIP_SYSTEM_BUS") != "" {
		t.Skip("TIDEWAITER_SKIP_SYSTEM_BUS is set")
	}
	bus, err := SystemBus(map[string]string{"DBUS_SYSTEM_BUS_ADDRESS": os.Getenv("DBUS_SYSTEM_BUS_ADDRESS")})
	if err != nil {
		t.Skip(err)
	}
	if _, err := bus.connection(ctx); err != nil {
		t.Skip("no system bus here: ", err)
	}
	t.Cleanup(func() { bus.Close() })
	return bus
}

// Read-only: loading a unit, reading its properties and subscribing change
// nothing on the host, and need no privilege.
func TestSystemBusReadsSystemdJournaldWhichEverySystemdHostRuns(t *testing.T) {
	bus := systemBus(t)
	state, err := bus.Unit(ctx, "systemd-journald.service")
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, state.LoadState, "loaded")
	assertEqual(t, state.ActiveState, "active")
	assertEqual(t, state.ControlGroup, "/system.slice/systemd-journald.service")
	if state.MainPID <= 0 {
		t.Errorf("no main pid: %+v", state)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(state.InvocationID) {
		t.Errorf("invocation id %q", state.InvocationID)
	}
}

func TestSystemBusCallsAUnitWithNoFileNotFound(t *testing.T) {
	bus := systemBus(t)
	state, err := bus.Unit(ctx, "tidewaiter-no-such-unit.service")
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, state.LoadState, "not-found")
	assertEqual(t, IsStopped(state), true)
}

package systemd

import (
	"strings"
	"testing"

	"github.com/danielbodart/gokit/dbusx"
	"github.com/danielbodart/gokit/dbusx/dbustest"
	"github.com/danielbodart/tidewaiter/internal/fakes"
	"github.com/godbus/dbus/v5"
)

// on is the adapter, talking to this bus through DBUS_SYSTEM_BUS_ADDRESS, as
// it finds the real one.
func on(t *testing.T, fake *dbustest.Bus) *DBus {
	t.Helper()
	bus, err := SystemBus(map[string]string{"DBUS_SYSTEM_BUS_ADDRESS": fake.Address()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bus.Close() })
	return bus
}

func TestSaysWhereItWasGoingWhenThereIsNoBus(t *testing.T) {
	bus, err := SystemBus(map[string]string{"DBUS_SYSTEM_BUS_ADDRESS": "unix:path=/nonexistent/bus"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bus.Unit(ctx, "game.service"); err == nil || !strings.Contains(err.Error(), "/nonexistent/bus") {
		t.Fatalf("got %v", err)
	}
}

const job7 = dbus.ObjectPath("/org/freedesktop/systemd1/job/7")

// systemdAnswers is systemd's side of the conversation, for one running
// unit, with jobs numbered from 7.
func systemdAnswers(call *dbus.Message) dbustest.Reply {
	switch dbustest.Header[string](call, dbus.FieldMember) {
	case "AddMatch", "Subscribe", "Reload":
		return dbustest.Reply{}
	case "LoadUnit":
		return dbustest.Reply{Body: []any{dbusx.UnitPath(call.Body[0].(string))}}
	case "GetAll":
		if call.Body[0] == dbusx.SystemdUnit {
			return dbustest.Reply{Body: []any{map[string]dbus.Variant{
				"Id":           dbus.MakeVariant("game.service"),
				"LoadState":    dbus.MakeVariant("loaded"),
				"ActiveState":  dbus.MakeVariant("active"),
				"SubState":     dbus.MakeVariant("running"),
				"InvocationID": dbus.MakeVariant([]byte{0xde, 0xad, 0xbe, 0xef, 0, 1}),
			}}}
		}
		return dbustest.Reply{Body: []any{map[string]dbus.Variant{
			"Result":       dbus.MakeVariant("success"),
			"NRestarts":    dbus.MakeVariant(uint32(2)),
			"MainPID":      dbus.MakeVariant(uint32(4242)),
			"ControlGroup": dbus.MakeVariant("/system.slice/game.service"),
		}}}
	case "RestartUnit":
		return dbustest.Reply{Body: []any{job7}}
	}
	return dbustest.Reply{Err: dbus.NewError("org.freedesktop.DBus.Error.UnknownMethod", []any{"Unknown method"})}
}

func removeJob(fake *dbustest.Bus, job dbus.ObjectPath, result string) {
	fake.Signal(dbusx.SystemdPath, dbusx.SystemdManager, "JobRemoved", uint32(7), job, "game.service", result)
}

func TestReadsAUnitsStateFromItsUnitAndServiceProperties(t *testing.T) {
	fake := dbustest.New(t)
	fake.Answer(systemdAnswers)
	state, err := on(t, fake).Unit(ctx, "game.service")
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, state, UnitState{
		Name:         "game.service",
		LoadState:    "loaded",
		ActiveState:  "active",
		SubState:     "running",
		Result:       "success",
		NRestarts:    2,
		MainPID:      4242,
		ControlGroup: "/system.slice/game.service",
		InvocationID: "deadbeef0001",
	})

	// LoadUnit, so a unit nothing has started yet is still found.
	load := fake.Calls("LoadUnit")[0]
	assertEqual(t, dbustest.Header[dbus.ObjectPath](load, dbus.FieldPath), dbusx.SystemdPath)
	assertEqual(t, dbustest.Header[string](load, dbus.FieldInterface), dbusx.SystemdManager)
	assertEqual(t, load.Body, []any{"game.service"})
	all := fake.Calls("GetAll")
	assertEqual(t, dbustest.Header[dbus.ObjectPath](all[0], dbus.FieldPath), dbusx.UnitPath("game.service"))
	assertEqual(t, all[0].Body, []any{dbusx.SystemdUnit})
	assertEqual(t, all[1].Body, []any{serviceIface})
}

func TestAUnitThatIsNotAServiceHasNoServicePropertiesToAskFor(t *testing.T) {
	fake := dbustest.New(t)
	fake.Answer(systemdAnswers)
	state, err := on(t, fake).Unit(ctx, "game.scope")
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, len(fake.Calls("GetAll")), 1)
	assertEqual(t, state.MainPID, 0)
	assertEqual(t, state.ActiveState, "active")
}

func TestSubscribesToJobEndsBeforeItAsksForAnything(t *testing.T) {
	fake := dbustest.New(t)
	fake.Answer(systemdAnswers)
	if _, err := on(t, fake).Unit(ctx, "game.service"); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, fake.Members()[:3], []string{"Hello", "AddMatch", "Subscribe"})
	match := fake.Calls("AddMatch")[0].Body[0].(string)
	if !strings.Contains(match, "member='JobRemoved'") {
		t.Fatal(match)
	}
	assertEqual(t, dbustest.Header[string](fake.Calls("Subscribe")[0], dbus.FieldInterface), dbusx.SystemdManager)
}

func TestARestartWaitsForItsOwnJobToBeRemovedAndReturnsItsResult(t *testing.T) {
	fake := dbustest.New(t)
	fake.Answer(systemdAnswers)
	bus := on(t, fake)

	type ended struct {
		result JobResult
		err    error
	}
	done := make(chan ended, 1)
	go func() {
		result, err := bus.Restart(ctx, "game.service")
		done <- ended{result, err}
	}()
	waitingOn(t, bus, 1)
	// Someone else's job ending says nothing about ours.
	removeJob(fake, "/org/freedesktop/systemd1/job/6", "done")
	removeJob(fake, job7, "failed")
	got := <-done
	assertEqual(t, got.err, nil)
	assertEqual(t, got.result, JobFailed)
	assertEqual(t, fake.Calls("RestartUnit")[0].Body, []any{"game.service", "replace"})
}

func TestAJobThatEndsBeforeTheReplyToRestartUnitIsStillHeard(t *testing.T) {
	fake := dbustest.New(t)
	fake.Answer(func(call *dbus.Message) dbustest.Reply {
		reply := systemdAnswers(call)
		if dbustest.Header[string](call, dbus.FieldMember) == "RestartUnit" {
			reply.Before = func() { removeJob(fake, job7, "done") }
		}
		return reply
	})
	result, err := on(t, fake).Restart(ctx, "game.service")
	assertEqual(t, err, nil)
	assertEqual(t, result, JobDone)
}

func TestAResultItDoesNotKnowIsAFailure(t *testing.T) {
	fake := dbustest.New(t)
	fake.Answer(func(call *dbus.Message) dbustest.Reply {
		reply := systemdAnswers(call)
		if dbustest.Header[string](call, dbus.FieldMember) == "RestartUnit" {
			reply.Before = func() { removeJob(fake, job7, "frobnicated") }
		}
		return reply
	})
	result, _ := on(t, fake).Restart(ctx, "game.service")
	assertEqual(t, result, JobFailed)
}

func TestARestartFailsWhenTheBusDropsWhileWaitingRatherThanWaitingForever(t *testing.T) {
	fake := dbustest.New(t)
	fake.Answer(systemdAnswers)
	bus := on(t, fake)
	failed := make(chan error, 1)
	go func() {
		_, err := bus.Restart(ctx, "game.service")
		failed <- err
	}()
	waitingOn(t, bus, 1)
	fake.Disconnect()
	if err := <-failed; err == nil || !strings.Contains(err.Error(), "lost the bus") {
		t.Fatalf("got %v", err)
	}
	// And the next call opens a new connection.
	state, err := bus.Unit(ctx, "game.service")
	assertEqual(t, err, nil)
	assertEqual(t, state.ActiveState, "active")
	assertEqual(t, len(fake.Calls("Hello")), 2)
}

func TestARestartThatCannotBeQueuedSaysWhy(t *testing.T) {
	fake := dbustest.New(t)
	fake.Answer(func(call *dbus.Message) dbustest.Reply {
		if dbustest.Header[string](call, dbus.FieldMember) == "RestartUnit" {
			return dbustest.Reply{Err: dbus.NewError("org.freedesktop.systemd1.NoSuchUnit", []any{"Unit game.service not found."})}
		}
		return systemdAnswers(call)
	})
	_, err := on(t, fake).Restart(ctx, "game.service")
	if err == nil || !strings.Contains(err.Error(), "org.freedesktop.systemd1.NoSuchUnit: Unit game.service not found.") {
		t.Fatalf("got %v", err)
	}
}

func TestDaemonReloadIsTheManagersReload(t *testing.T) {
	fake := dbustest.New(t)
	fake.Answer(systemdAnswers)
	if err := on(t, fake).DaemonReload(ctx); err != nil {
		t.Fatal(err)
	}
	reload := fake.Calls("Reload")[0]
	assertEqual(t, dbustest.Header[string](reload, dbus.FieldInterface), dbusx.SystemdManager)
	assertEqual(t, dbustest.Header[dbus.ObjectPath](reload, dbus.FieldPath), dbusx.SystemdPath)
}

func TestAsksLogindToRebootNotInteractively(t *testing.T) {
	fake := dbustest.New(t)
	if err := on(t, fake).Reboot(ctx); err != nil {
		t.Fatal(err)
	}
	reboot := fake.Calls("Reboot")[0]
	assertEqual(t, dbustest.Header[string](reboot, dbus.FieldDestination), "org.freedesktop.login1")
	assertEqual(t, dbustest.Header[dbus.ObjectPath](reboot, dbus.FieldPath), dbus.ObjectPath("/org/freedesktop/login1"))
	assertEqual(t, dbustest.Header[string](reboot, dbus.FieldInterface), "org.freedesktop.login1.Manager")
	assertEqual(t, dbustest.Header[dbus.Signature](reboot, dbus.FieldSignature).String(), "b")
	assertEqual(t, reboot.Body, []any{false})
	// On a connection of its own, closed after: no subscription to systemd.
	assertEqual(t, fake.Calls("Subscribe"), nil)
	fakes.Await(t, "the reboot's connection to close", func() bool { return fake.Connected() == 0 })
}

func TestARebootRefusalIsAnErrorForTheDaemonToLog(t *testing.T) {
	fake := dbustest.New(t)
	fake.Answer(func(call *dbus.Message) dbustest.Reply {
		return dbustest.Reply{Err: dbus.NewError("org.freedesktop.login1.OperationInProgress", []any{"Operation denied due to active block inhibitor"})}
	})
	err := on(t, fake).Reboot(ctx)
	if err == nil || !strings.Contains(err.Error(), "block inhibitor") || !strings.Contains(err.Error(), "OperationInProgress") {
		t.Fatalf("got %v", err)
	}
}

func signal(member string, body ...any) *dbus.Signal {
	return &dbus.Signal{Path: dbusx.SystemdPath, Name: dbusx.SystemdManager + "." + member, Body: body}
}

func TestToJobRemovedReadsTheJobPathUnitAndResult(t *testing.T) {
	removed, ok := toJobRemoved(signal("JobRemoved", uint32(7), dbus.ObjectPath("/j/7"), "game.service", "timeout"))
	assertEqual(t, ok, true)
	assertEqual(t, removed, removal{job: "/j/7", unit: "game.service", result: JobTimeout})
}

func TestToJobRemovedIgnoresEveryOtherSignal(t *testing.T) {
	_, ok := toJobRemoved(signal("JobNew", uint32(7), dbus.ObjectPath("/j/7"), "game.service"))
	assertEqual(t, ok, false)
	_, ok = toJobRemoved(signal("JobRemoved"))
	assertEqual(t, ok, false)
}

func TestToUnitStateReadsAUnitThatIsNotRunningAsHavingNoPidCgroupOrResult(t *testing.T) {
	assertEqual(t, toUnitState("gone.service", map[string]dbus.Variant{
		"LoadState": dbus.MakeVariant("not-found"), "ActiveState": dbus.MakeVariant("inactive"), "SubState": dbus.MakeVariant("dead"),
	}, nil), UnitState{Name: "gone.service", LoadState: "not-found", ActiveState: "inactive", SubState: "dead"})
}

// waitingOn waits for n restarts to be waiting on their job, so a test acts
// on a restart that has had its reply.
func waitingOn(t *testing.T, bus *DBus, n int) {
	t.Helper()
	fakes.Await(t, "the restart to wait for its job", func() bool {
		bus.mu.Lock()
		c := bus.shared
		bus.mu.Unlock()
		if c == nil {
			return false
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.waiting) == n
	})
}

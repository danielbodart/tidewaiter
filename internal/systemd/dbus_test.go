package systemd

import (
	"strings"
	"testing"

	"github.com/danielbodart/tidewaiter/internal/fakes"
	"github.com/godbus/dbus/v5"
)

func address(t *testing.T, env map[string]string) string {
	t.Helper()
	bus, err := SystemBus(env)
	if err != nil {
		t.Fatal(err)
	}
	return bus.address
}

func TestSystemBusIsTheStandardSocketByDefault(t *testing.T) {
	assertEqual(t, address(t, map[string]string{}), "unix:path=/run/dbus/system_bus_socket")
	assertEqual(t, address(t, map[string]string{"DBUS_SYSTEM_BUS_ADDRESS": ""}), "unix:path=/run/dbus/system_bus_socket")
}

func TestSystemBusFollowsDBUS_SYSTEM_BUS_ADDRESS(t *testing.T) {
	assertEqual(t, address(t, map[string]string{"DBUS_SYSTEM_BUS_ADDRESS": "unix:path=/run/other/bus"}), "unix:path=/run/other/bus")
	assertEqual(t, address(t, map[string]string{"DBUS_SYSTEM_BUS_ADDRESS": "tcp:host=x;unix:guid=1,path=/run/b"}), "unix:guid=1,path=/run/b")
}

func TestSystemBusRefusesAnAddressItWillNotUse(t *testing.T) {
	_, err := SystemBus(map[string]string{"DBUS_SYSTEM_BUS_ADDRESS": "tcp:host=x,port=1"})
	if err == nil || !strings.Contains(err.Error(), "only unix: addresses") {
		t.Fatalf("got %v", err)
	}
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

func TestUnitPathsAreEscapedAsSystemdEscapesThem(t *testing.T) {
	assertEqual(t, unitPath("chandlery-bedrock.service"), dbus.ObjectPath("/org/freedesktop/systemd1/unit/chandlery_2dbedrock_2eservice"))
	assertEqual(t, unitPath("2fast.service"), dbus.ObjectPath("/org/freedesktop/systemd1/unit/_32fast_2eservice"))
}

const job7 = dbus.ObjectPath("/org/freedesktop/systemd1/job/7")

// systemdAnswers is systemd's side of the conversation, for one running
// unit, with jobs numbered from 7.
func systemdAnswers(call *dbus.Message) answer {
	switch header[string](call, dbus.FieldMember) {
	case "AddMatch", "Subscribe", "Reload":
		return answer{}
	case "LoadUnit":
		return answer{body: []any{unitPath(call.Body[0].(string))}}
	case "GetAll":
		if call.Body[0] == unitIface {
			return answer{body: []any{map[string]dbus.Variant{
				"Id":           dbus.MakeVariant("game.service"),
				"LoadState":    dbus.MakeVariant("loaded"),
				"ActiveState":  dbus.MakeVariant("active"),
				"SubState":     dbus.MakeVariant("running"),
				"InvocationID": dbus.MakeVariant([]byte{0xde, 0xad, 0xbe, 0xef, 0, 1}),
			}}}
		}
		return answer{body: []any{map[string]dbus.Variant{
			"Result":       dbus.MakeVariant("success"),
			"NRestarts":    dbus.MakeVariant(uint32(2)),
			"MainPID":      dbus.MakeVariant(uint32(4242)),
			"ControlGroup": dbus.MakeVariant("/system.slice/game.service"),
		}}}
	case "RestartUnit":
		return answer{body: []any{job7}}
	}
	return answer{err: dbus.NewError("org.freedesktop.DBus.Error.UnknownMethod", []any{"Unknown method"})}
}

func removeJob(fake *fakeBus, job dbus.ObjectPath, result string) {
	fake.Signal(managerPath, manager, "JobRemoved", uint32(7), job, "game.service", result)
}

func TestReadsAUnitsStateFromItsUnitAndServiceProperties(t *testing.T) {
	fake := newFakeBus(t)
	fake.Answer(systemdAnswers)
	state, err := fake.Bus(t).Unit(ctx, "game.service")
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
	assertEqual(t, header[dbus.ObjectPath](load, dbus.FieldPath), managerPath)
	assertEqual(t, header[string](load, dbus.FieldInterface), manager)
	assertEqual(t, load.Body, []any{"game.service"})
	all := fake.Calls("GetAll")
	assertEqual(t, header[dbus.ObjectPath](all[0], dbus.FieldPath), unitPath("game.service"))
	assertEqual(t, all[0].Body, []any{unitIface})
	assertEqual(t, all[1].Body, []any{serviceIface})
}

func TestAUnitThatIsNotAServiceHasNoServicePropertiesToAskFor(t *testing.T) {
	fake := newFakeBus(t)
	fake.Answer(systemdAnswers)
	state, err := fake.Bus(t).Unit(ctx, "game.scope")
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, len(fake.Calls("GetAll")), 1)
	assertEqual(t, state.MainPID, 0)
	assertEqual(t, state.ActiveState, "active")
}

func TestSubscribesToJobEndsBeforeItAsksForAnything(t *testing.T) {
	fake := newFakeBus(t)
	fake.Answer(systemdAnswers)
	if _, err := fake.Bus(t).Unit(ctx, "game.service"); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, fake.Members()[:3], []string{"Hello", "AddMatch", "Subscribe"})
	match := fake.Calls("AddMatch")[0].Body[0].(string)
	if !strings.Contains(match, "member='JobRemoved'") {
		t.Fatal(match)
	}
	assertEqual(t, header[string](fake.Calls("Subscribe")[0], dbus.FieldInterface), manager)
}

func TestARestartWaitsForItsOwnJobToBeRemovedAndReturnsItsResult(t *testing.T) {
	fake := newFakeBus(t)
	fake.Answer(systemdAnswers)
	bus := fake.Bus(t)

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
	fake := newFakeBus(t)
	fake.Answer(func(call *dbus.Message) answer {
		reply := systemdAnswers(call)
		if header[string](call, dbus.FieldMember) == "RestartUnit" {
			reply.before = func() { removeJob(fake, job7, "done") }
		}
		return reply
	})
	result, err := fake.Bus(t).Restart(ctx, "game.service")
	assertEqual(t, err, nil)
	assertEqual(t, result, JobDone)
}

func TestAResultItDoesNotKnowIsAFailure(t *testing.T) {
	fake := newFakeBus(t)
	fake.Answer(func(call *dbus.Message) answer {
		reply := systemdAnswers(call)
		if header[string](call, dbus.FieldMember) == "RestartUnit" {
			reply.before = func() { removeJob(fake, job7, "frobnicated") }
		}
		return reply
	})
	result, _ := fake.Bus(t).Restart(ctx, "game.service")
	assertEqual(t, result, JobFailed)
}

func TestARestartFailsWhenTheBusDropsWhileWaitingRatherThanWaitingForever(t *testing.T) {
	fake := newFakeBus(t)
	fake.Answer(systemdAnswers)
	bus := fake.Bus(t)
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
	fake := newFakeBus(t)
	fake.Answer(func(call *dbus.Message) answer {
		if header[string](call, dbus.FieldMember) == "RestartUnit" {
			return answer{err: dbus.NewError("org.freedesktop.systemd1.NoSuchUnit", []any{"Unit game.service not found."})}
		}
		return systemdAnswers(call)
	})
	_, err := fake.Bus(t).Restart(ctx, "game.service")
	if err == nil || !strings.Contains(err.Error(), "org.freedesktop.systemd1.NoSuchUnit: Unit game.service not found.") {
		t.Fatalf("got %v", err)
	}
}

func TestDaemonReloadIsTheManagersReload(t *testing.T) {
	fake := newFakeBus(t)
	fake.Answer(systemdAnswers)
	if err := fake.Bus(t).DaemonReload(ctx); err != nil {
		t.Fatal(err)
	}
	reload := fake.Calls("Reload")[0]
	assertEqual(t, header[string](reload, dbus.FieldInterface), manager)
	assertEqual(t, header[dbus.ObjectPath](reload, dbus.FieldPath), managerPath)
}

func TestAsksLogindToRebootNotInteractively(t *testing.T) {
	fake := newFakeBus(t)
	if err := fake.Bus(t).Reboot(ctx); err != nil {
		t.Fatal(err)
	}
	reboot := fake.Calls("Reboot")[0]
	assertEqual(t, header[string](reboot, dbus.FieldDestination), "org.freedesktop.login1")
	assertEqual(t, header[dbus.ObjectPath](reboot, dbus.FieldPath), dbus.ObjectPath("/org/freedesktop/login1"))
	assertEqual(t, header[string](reboot, dbus.FieldInterface), "org.freedesktop.login1.Manager")
	assertEqual(t, header[dbus.Signature](reboot, dbus.FieldSignature).String(), "b")
	assertEqual(t, reboot.Body, []any{false})
	// On a connection of its own, closed after: no subscription to systemd.
	assertEqual(t, fake.Calls("Subscribe"), nil)
	fakes.Await(t, "the reboot's connection to close", func() bool { return fake.Connected() == 0 })
}

func TestARebootRefusalIsAnErrorForTheDaemonToLog(t *testing.T) {
	fake := newFakeBus(t)
	fake.Answer(func(call *dbus.Message) answer {
		return answer{err: dbus.NewError("org.freedesktop.login1.OperationInProgress", []any{"Operation denied due to active block inhibitor"})}
	})
	err := fake.Bus(t).Reboot(ctx)
	if err == nil || !strings.Contains(err.Error(), "block inhibitor") || !strings.Contains(err.Error(), "OperationInProgress") {
		t.Fatalf("got %v", err)
	}
}

func signal(member string, body ...any) *dbus.Signal {
	return &dbus.Signal{Path: managerPath, Name: manager + "." + member, Body: body}
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

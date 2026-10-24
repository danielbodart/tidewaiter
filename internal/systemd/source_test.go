package systemd

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/danielbodart/gokit/logx/logtest"
	"github.com/danielbodart/tidewaiter/internal/decide"
	"github.com/danielbodart/tidewaiter/internal/detect"
	"github.com/danielbodart/tidewaiter/internal/fakes"
	"github.com/danielbodart/tidewaiter/internal/model"
	"github.com/danielbodart/tidewaiter/internal/source"
	"github.com/danielbodart/tidewaiter/internal/statestore"
)

const (
	unit    = "game.service"
	profile = "/nix/var/nix/profiles/tidewaiter/game"
	v1      = "/nix/store/aaaa-game-1"
	v2      = "/nix/store/bbbb-game-2"
	v3      = "/nix/store/cccc-game-3"
)

// player is a player's UDP flow to the game's port.
var player = detect.ConntrackEntry{Protocol: model.UDP, DestinationPort: 7777, Assured: true}

// harness is the systemd source over a unit, a profile, sockets and
// conntrack that exist only in memory: the real reconcile, decide and swap
// sequencing, with the far ends replaced. By default game.service is up on
// v1 with 7777/udp bound, and its units file asks for v2 after one idle
// sample.
type harness struct {
	t         *testing.T
	journal   *journal
	systemd   *fakeSystemd
	profiles  *fakeProfiles
	closure   *fakeClosure
	sockets   *fakeSockets
	conntrack *fakes.Conntrack
	units     *fakeUnits
	store     *memoryStore
	log       *logtest.Logger
	source    *Source
}

func defaults(fields map[string]any) map[string]any {
	all := map[string]any{"desired": v2, "profile": profile, "ports": []string{"7777/udp"}, "idleSamples": 1, "healthTimeout": 3}
	maps.Copy(all, fields)
	return all
}

func newHarness(t *testing.T, fields map[string]any, store *memoryStore) *harness {
	t.Helper()
	if store == nil {
		store = storeOf(nil)
	}
	journal := &journal{}
	h := &harness{
		t:         t,
		journal:   journal,
		systemd:   newFakeSystemd(journal).add(unitState(unit)),
		profiles:  newFakeProfiles(journal).seed(profile, v1),
		closure:   newFakeClosure(journal),
		sockets:   (&fakeSockets{}).bind(unit, 7777),
		conntrack: &fakes.Conntrack{},
		units:     newFakeUnits().declare(unit, defaults(fields)),
		store:     store,
		log:       &logtest.Logger{},
	}
	h.restartDaemon(false)
	return h
}

// restartDaemon is a new source over the same state store: Tidewaiter
// restarted.
func (h *harness) restartDaemon(dryRun bool) {
	h.source = New(Deps{
		Units:     h.units,
		Bus:       h.systemd,
		Profiles:  h.profiles,
		Closure:   h.closure,
		Sockets:   h.sockets,
		Conntrack: h.conntrack,
		Store:     h.store,
		Netns:     &fakes.Netns{},
		TCP:       &fakes.Prober{},
		Log:       h.log,
		Clock:     &ticking{now: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)},
	}, Options{DryRun: dryRun})
}

// passWith is one pass, with this pass's conntrack table (the shared dump);
// nil is a dump that failed.
func (h *harness) passWith(table []detect.ConntrackEntry) []source.Outcome {
	h.t.Helper()
	outcomes, err := h.source.Once(ctx, source.Pass{
		Conntrack: table,
		Hold:      func(workload string) { h.journal.add("hold %s", workload) },
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return outcomes
}

// pass is one pass over what conntrack holds now.
func (h *harness) pass() []source.Outcome {
	h.t.Helper()
	table, _ := h.conntrack.Dump(ctx)
	if table == nil {
		table = []detect.ConntrackEntry{}
	}
	return h.passWith(table)
}

func (h *harness) kinds() []string {
	h.t.Helper()
	var kinds []string
	for _, outcome := range h.pass() {
		kinds = append(kinds, outcome.Action.Kind.String())
	}
	return kinds
}

func (h *harness) current() string {
	current, _ := h.profiles.Current(ctx, profile)
	return current
}

func (h *harness) logged(text string) bool { return h.log.Contains(text) }

func (h *harness) count(text string) int {
	n := 0
	for _, line := range h.log.Lines() {
		if strings.Contains(line, text) {
			n++
		}
	}
	return n
}

func (h *harness) mustLog(text string) {
	h.t.Helper()
	if !h.logged(text) {
		h.t.Fatalf("never said %q:\n%s", text, h.log.Text())
	}
}

// neverBinds makes a restart onto whatever the profile now points at come
// up, but never bind its port if that is bad.
func neverBinds(h *harness, bad string) {
	h.systemd.onRestart = func(name string, fake *fakeSystemd) JobResult {
		fake.patch(name, func(state *UnitState) { state.ActiveState, state.InvocationID = "active", fake.nextInvocation() })
		h.sockets.clear()
		if h.current() != bad {
			h.sockets.bind(unit, 7777)
		}
		return JobDone
	}
}

func busy(outcome source.Outcome) *bool { return outcome.Busy }

// Deciding

func TestKeepsAUnitWhoseProfileIsAlreadyOnTheDesiredVersion(t *testing.T) {
	h := newHarness(t, map[string]any{"desired": v1}, nil)
	assertEqual(t, h.kinds(), []string{"keep"})
	assertEqual(t, h.journal.lines, nil)
}

func TestDefersWhileAPlayersFlowIsOnADeclaredPortThenSwapsOnceIdle(t *testing.T) {
	h := newHarness(t, map[string]any{"idleSamples": 2}, nil)
	h.conntrack.Flows(player)
	assertEqual(t, h.kinds(), []string{"defer"})
	assertEqual(t, h.kinds(), []string{"defer"})

	h.conntrack.Flows()
	assertEqual(t, h.kinds(), []string{"defer"}) // idle 1/2
	assertEqual(t, h.kinds(), []string{"update"})
	assertEqual(t, h.current(), v2)
}

func TestAFlowToSomeOtherPortIsNotAPlayer(t *testing.T) {
	h := newHarness(t, nil, nil)
	other := player
	other.DestinationPort = 8888
	h.conntrack.Flows(other)
	assertEqual(t, h.kinds(), []string{"update"})
}

func TestDefersWhenSomeoneArrivesBetweenTheDecisionAndTheSwap(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.conntrack.Flows(player) // what the re-sample right before the swap sees

	outcome := h.passWith([]detect.ConntrackEntry{})[0] // the pass's own dump was empty

	assertEqual(t, outcome.Action.Kind, decide.Defer)
	if !strings.Contains(outcome.Action.Reason, "arrived just before the swap") {
		t.Fatal(outcome.Action.Reason)
	}
	assertEqual(t, h.journal.lines, []string{"hold game.service"})
	assertEqual(t, h.current(), v1)
}

func TestAReSampleWhoseDumpFailsReadsAsSomeoneArriving(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.conntrack.FailWith(errors.New("operation not permitted"))
	outcome := h.passWith([]detect.ConntrackEntry{})[0]
	assertEqual(t, outcome.Action.Kind, decide.Defer)
	assertEqual(t, h.current(), v1)
}

func TestAUnitWithNoDeclaredPortsCannotBeReadSoItIsBusyAndSaysSoOnce(t *testing.T) {
	h := newHarness(t, map[string]any{"ports": []string{}}, nil)
	assertEqual(t, h.kinds(), []string{"defer"})
	assertEqual(t, h.kinds(), []string{"defer"})
	assertEqual(t, h.count("declares no ports"), 1)
}

func TestAConntrackDumpThatFailedReadsAsBusyNeverAsAnEmptyTable(t *testing.T) {
	h := newHarness(t, nil, nil)
	outcome := h.passWith(nil)[0]
	assertEqual(t, outcome.Action.Kind, decide.Defer)
	assertEqual(t, busy(outcome), source.Sampled(true))
}

func TestDetectorNoneSwapsWithoutLooking(t *testing.T) {
	h := newHarness(t, map[string]any{"detector": "none", "ports": []string{}}, nil)
	h.conntrack.Flows(player)
	assertEqual(t, h.kinds(), []string{"update"})
}

func TestDetectorTCPReadsTheUnitsOwnSockets(t *testing.T) {
	h := newHarness(t, map[string]any{"detector": "tcp", "ports": []string{"25565/tcp"}}, nil)
	netns := &fakes.Netns{}
	netns.Set(4242, detect.TCP4Table, fakes.ProcNetLine(25565, detect.StateEstablished))
	h.source.deps.Netns = netns
	assertEqual(t, h.kinds(), []string{"defer"})
	netns.Set(4242, detect.TCP4Table, fakes.ProcNetLine(25565, detect.StateListen))
	assertEqual(t, h.kinds(), []string{"update"})
}

func TestADetectorThatFailsIsBusyAndSaysWhy(t *testing.T) {
	h := newHarness(t, map[string]any{"detector": "tcp", "ports": []string{"25565/tcp"}}, nil)
	netns := &fakes.Netns{}
	netns.FailWith(errors.New("no such process"))
	h.source.deps.Netns = netns
	assertEqual(t, h.kinds(), []string{"defer"})
	h.mustLog("game.service: activity detector 'tcp' failed (no such process); assuming busy")
}

func TestAUnitStillActivatingIsLeftToSettle(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.systemd.patch(unit, func(state *UnitState) { state.ActiveState, state.SubState = "activating", "start" })
	outcome := h.pass()[0]
	assertEqual(t, outcome.Action.Kind, decide.Defer)
	if !strings.Contains(outcome.Action.Reason, "activating") {
		t.Fatal(outcome.Action.Reason)
	}
}

func TestDryRunDecidesButTouchesNothing(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.restartDaemon(true)
	assertEqual(t, h.kinds(), []string{"update"})
	assertEqual(t, h.journal.lines, nil)
	h.mustLog("dry run")
	// Not even the state file: the baseline it adopted on first sight is the
	// real daemon's to record.
	assertEqual(t, h.store.saves, 0)
}

func TestSpeaksOfVersionsAndUnitsNotImagesAndContainers(t *testing.T) {
	h := newHarness(t, map[string]any{"idleSamples": 3}, nil)
	outcome := h.pass()[0]
	if h.logged("container") {
		t.Fatal(h.log.Text())
	}
	if !strings.Contains(outcome.Action.Reason, "idle 1/3") {
		t.Fatal(outcome.Action.Reason)
	}
}

// Settled, for an automatic reboot

func TestSettledOnlyOnceIdleForTheWholeDebounceAndNotAgainTheMomentSomeoneIsOn(t *testing.T) {
	h := newHarness(t, map[string]any{"desired": v1, "idleSamples": 2}, nil)
	assertEqual(t, h.pass()[0].Settled, false) // idle 1/2
	assertEqual(t, h.pass()[0].Settled, true)
	h.conntrack.Flows(player)
	outcome := h.pass()[0]
	assertEqual(t, busy(outcome), source.Sampled(true))
	assertEqual(t, outcome.Settled, false)
	h.conntrack.Flows()
	assertEqual(t, h.pass()[0].Settled, false)
}

func TestNotSettledStraightAfterASwap(t *testing.T) {
	h := newHarness(t, nil, nil)
	outcome := h.pass()[0]
	assertEqual(t, outcome.Action.Kind, decide.Update)
	assertEqual(t, busy(outcome), source.Sampled(false))
	assertEqual(t, outcome.Settled, false)
}

func TestNotSettledWhileTheUnitIsOnItsWayUp(t *testing.T) {
	h := newHarness(t, map[string]any{"desired": v1}, nil)
	h.systemd.patch(unit, func(state *UnitState) { state.ActiveState, state.SubState = "activating", "start" })
	assertEqual(t, h.pass()[0].Settled, false)
}

func TestAStoppedUnitHasNobodyToWaitFor(t *testing.T) {
	h := newHarness(t, map[string]any{"desired": v1}, nil)
	h.systemd.patch(unit, stopped)
	outcome := h.pass()[0]
	assertEqual(t, busy(outcome), source.Sampled(false))
	assertEqual(t, outcome.Settled, true)
}

func TestAUnitThatCouldNotBeReadIsNotSampledOrSettled(t *testing.T) {
	h := newHarness(t, map[string]any{"desired": v1}, nil)
	h.systemd.fails = errBusGone
	outcome := h.pass()[0]
	assertEqual(t, busy(outcome), nil)
	assertEqual(t, outcome.Settled, false)
	assertEqual(t, outcome.Action.Reason, "systemd unreachable")
}

func stopped(state *UnitState) {
	state.ActiveState, state.SubState, state.MainPID, state.ControlGroup = "inactive", "dead", 0, ""
}

// Swapping

func TestHoldsTheSwapSetsTheProfileRestartsGatesAndPrunesInThatOrder(t *testing.T) {
	h := newHarness(t, nil, nil)
	outcome := h.pass()[0]

	assertEqual(t, outcome.Action.Kind, decide.Update)
	assertEqual(t, h.journal.lines, []string{"hold game.service", "set " + v2, "restart game.service", "prune +2"})
	assertEqual(t, h.current(), v2)
	h.mustLog("updated to " + v2 + " and it is healthy")
	assertEqual(t, h.store.memory(unit), statestore.UnitMemory{Good: ref(2)})
}

func TestKeepsKeepGenerationsPreviousGenerations(t *testing.T) {
	h := newHarness(t, map[string]any{"keepGenerations": 0, "desired": v3}, nil)
	h.profiles.seed(profile, v1, v2)
	h.pass()
	assertEqual(t, h.profiles.generationsOf(profile), []int{3})
}

func TestARestartJobThatFailsRollsTheProfileBackToTheGenerationItLeftAndPins(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.systemd.onRestart = func(name string, fake *fakeSystemd) JobResult {
		// The new version never says READY=1; the old one, back on
		// generation 1, does.
		if h.profiles.currentGeneration(profile) == 2 {
			fake.patch(name, func(state *UnitState) { state.ActiveState, state.Result = "failed", "timeout" })
			return JobFailed
		}
		fake.patch(name, func(state *UnitState) { state.ActiveState, state.InvocationID = "active", fake.nextInvocation() })
		return JobDone
	}

	h.pass()

	assertEqual(t, h.journal.lines, []string{
		"hold game.service", "set " + v2, "restart game.service", "switch 1", "delete 2", "restart game.service",
	})
	assertEqual(t, h.current(), v1)
	assertEqual(t, h.profiles.generationsOf(profile), []int{1})
	h.mustLog("the restart job ended 'failed'")
	h.mustLog("rolled back to " + v1 + " and pinned " + v2 + " out")
	assertEqual(t, h.store.memory(unit).Pinned, v2)

	// Pinned: no more restarts while v2 is what is desired.
	assertEqual(t, h.kinds(), []string{"pinned"})
	assertEqual(t, len(h.journal.starting("restart")), 2)
}

func TestAVersionThatComesUpButNeverBindsItsPortFailsUptimeAndRollsBack(t *testing.T) {
	h := newHarness(t, nil, nil)
	neverBinds(h, v2)

	h.pass()

	h.mustLog("no published port is bound")
	assertEqual(t, h.current(), v1)
	assertEqual(t, h.store.memory(unit).Pinned, v2)
}

func TestAPortBoundBySomeOtherUnitDoesNotCount(t *testing.T) {
	h := newHarness(t, nil, nil)
	neverBinds(h, v2)
	original := h.systemd.onRestart
	h.systemd.onRestart = func(name string, fake *fakeSystemd) JobResult {
		result := original(name, fake)
		h.sockets.bind("other.service", 7777)
		return result
	}

	h.pass()

	assertEqual(t, h.store.memory(unit).Pinned, v2)
}

func TestAUnitThatCrashesAndIsBroughtBackByRestartFailsTheGate(t *testing.T) {
	h := newHarness(t, nil, nil)
	restarts := 0
	h.systemd.beforeUnit = func(name string) {
		// Restart= brings v2 back on a new invocation after every read, as a
		// crash loop does.
		if restarts > 0 && h.current() == v2 {
			h.systemd.patch(name, func(state *UnitState) { state.NRestarts++ })
		}
	}
	h.systemd.onRestart = func(name string, fake *fakeSystemd) JobResult {
		restarts++
		fake.patch(name, func(state *UnitState) { state.ActiveState, state.InvocationID = "active", fake.nextInvocation() })
		return JobDone
	}

	h.pass()

	h.mustLog("the unit crashed and was restarted since it came up")
	assertEqual(t, h.current(), v1)
	assertEqual(t, h.store.memory(unit).Pinned, v2)
}

func TestThePinSurvivesARestartOfTidewaiter(t *testing.T) {
	h := newHarness(t, nil, nil)
	neverBinds(h, v2)
	h.pass()

	h.restartDaemon(false)
	assertEqual(t, h.kinds(), []string{"pinned"})
	assertEqual(t, len(h.journal.starting("set")), 1)
}

func TestANewDesiredVersionClearsThePin(t *testing.T) {
	h := newHarness(t, nil, nil)
	neverBinds(h, v2)
	h.pass()

	h.units.declare(unit, defaults(map[string]any{"desired": v3}))
	assertEqual(t, h.kinds(), []string{"update"})
	assertEqual(t, h.current(), v3)
	assertEqual(t, h.store.memory(unit).Pinned, "")
}

func TestANixEnvThatFailsIsUndoneAndRetriedAndPinnedAfterThreeInARow(t *testing.T) {
	h := newHarness(t, nil, nil)
	for range 3 {
		h.profiles.setFails = errors.New("cannot connect to the Nix daemon")
		assertEqual(t, h.kinds(), []string{"update"})
	}

	assertEqual(t, h.journal.starting("restart"), nil)
	assertEqual(t, h.current(), v1)
	memory := h.store.memory(unit)
	assertEqual(t, memory.Pinned, v2)
	assertEqual(t, memory.SwapFailures, 3)
	assertEqual(t, h.kinds(), []string{"pinned"})
}

func TestARestartThatCouldNotBeQueuedPutsTheProfileBackWithoutRestarting(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.systemd.restartFails = errors.New("Connection reset by peer")

	h.pass()

	assertEqual(t, h.journal.lines, []string{"hold game.service", "set " + v2, "restart game.service", "switch 1", "delete 2"})
	assertEqual(t, h.current(), v1)
	memory := h.store.memory(unit)
	assertEqual(t, memory.Failing, v2)
	assertEqual(t, memory.SwapFailures, 1)
}

func TestARestartWhoseEndWasLostButThatHappenedIsRestartedBackOntoTheOldVersion(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.systemd.onRestart = func(name string, fake *fakeSystemd) JobResult {
		fake.patch(name, func(state *UnitState) { state.InvocationID = fake.nextInvocation() })
		return JobDone
	}
	lost := true
	h.source.deps.Bus = lostEnd{h.systemd, &lost}

	h.pass()

	assertEqual(t, h.journal.lines, []string{
		"hold game.service", "set " + v2, "restart game.service", "switch 1", "delete 2", "restart game.service",
	})
	assertEqual(t, h.current(), v1)
}

// lostEnd restarts the unit but fails the first restart as a bus that
// dropped while waiting for the job does.
type lostEnd struct {
	*fakeSystemd
	lost *bool
}

func (l lostEnd) Restart(ctx context.Context, name string) (JobResult, error) {
	result, err := l.fakeSystemd.Restart(ctx, name)
	if *l.lost {
		*l.lost = false
		return "", errors.New("lost the bus while waiting for the restart job (D-Bus connection closed)")
	}
	return result, err
}

func TestWithBindClosureTheClosureIsBoundAndSystemdReloadedBeforeEachRestart(t *testing.T) {
	h := newHarness(t, map[string]any{"bindClosure": true}, nil)
	neverBinds(h, v2)

	h.pass()

	assertEqual(t, h.journal.lines, []string{
		"hold game.service",
		"bind " + v2, "daemon-reload", "set " + v2, "restart game.service",
		"switch 1", "delete 2", "bind " + v1, "daemon-reload", "restart game.service",
	})
}

func TestARollbackThatIsItselfUnhealthyAsksForAPersonRatherThanLooping(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.systemd.onRestart = func(name string, fake *fakeSystemd) JobResult {
		fake.patch(name, func(state *UnitState) { state.ActiveState = "failed" })
		return JobFailed
	}

	h.pass()
	h.pass()

	if !h.log.ErrorContains("manual attention needed") {
		t.Fatalf("never asked for a person, as an error:\n%s", h.log.Text())
	}
	assertEqual(t, len(h.journal.starting("restart")), 2)
}

// Stopped units

func TestAStoppedUnitHasItsProfileMovedForTheNextStartWithNoRestart(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.systemd.patch(unit, stopped)
	h.conntrack.Flows(player)

	outcome := h.pass()[0]

	assertEqual(t, outcome.Action.Kind, decide.Update)
	assertEqual(t, busy(outcome), source.Sampled(false))
	assertEqual(t, h.journal.lines, []string{"set " + v2})
	h.mustLog("updated the profile of a stopped unit")
}

func TestAStoppedUnitIsNotMovedOntoAPinnedVersion(t *testing.T) {
	h := newHarness(t, nil, storeOf(statestore.Memories{unit: {Pinned: v2}}))
	h.systemd.patch(unit, func(state *UnitState) { state.ActiveState, state.MainPID, state.ControlGroup = "failed", 0, "" })

	assertEqual(t, h.kinds(), []string{"pinned"})
	assertEqual(t, h.journal.lines, nil)
}

func TestAUnitSystemdDoesNotKnowIsLeftAloneSaidOnce(t *testing.T) {
	h := newHarness(t, nil, nil)
	clear(h.systemd.units)
	assertEqual(t, h.kinds(), []string{"keep"})
	assertEqual(t, h.kinds(), []string{"keep"})
	assertEqual(t, h.journal.lines, nil)
	assertEqual(t, h.count("does not know"), 1)
}

// Crash recovery

func TestAUnitFailedOnAGenerationNewerThanTheLastGoodOneIsRolledBackAndPinned(t *testing.T) {
	h := newHarness(t, nil, storeOf(statestore.Memories{unit: {Good: ref(1)}}))
	h.profiles.seed(profile, v1, v2) // Tidewaiter died after `set v2`
	h.systemd.patch(unit, func(state *UnitState) { state.ActiveState, state.Result = "failed", "exit-code" })

	outcome := h.pass()[0]

	assertEqual(t, outcome.Action.Kind, decide.Pinned)
	assertEqual(t, h.journal.lines, []string{"hold game.service", "switch 1", "delete 2", "restart game.service"})
	assertEqual(t, h.current(), v1)
	assertEqual(t, h.store.memory(unit).Pinned, v2)
}

func TestAUnitUpOnAGenerationWithNoRecordIsAdoptedAsTheBaseline(t *testing.T) {
	h := newHarness(t, map[string]any{"desired": v2}, storeOf(statestore.Memories{unit: {Good: ref(1)}}))
	h.profiles.seed(profile, v1, v2)

	assertEqual(t, h.kinds(), []string{"keep"})
	assertEqual(t, h.store.memory(unit), statestore.UnitMemory{Good: ref(2)})
	h.mustLog("adopting generation 2")
}

// A swap Tidewaiter died in the middle of

// interrupted is good=1 on v1, and a record of a swap to v2 started from it
// while invocation 0001 ran.
func interrupted(t *testing.T, fields map[string]any) *harness {
	return newHarness(t, fields, storeOf(statestore.Memories{unit: {
		Good:     ref(1),
		Swapping: &statestore.SwapRecord{From: ref(1), FromTarget: v1, To: v2, InvocationID: "0001"},
	}}))
}

func TestASwapIsRecordedDurablyBeforeAnythingChangesAndClearedOnceItCommits(t *testing.T) {
	h := newHarness(t, nil, nil)
	var seen []*statestore.SwapRecord
	h.profiles.beforeSet = func() { seen = append(seen, h.store.memory(unit).Swapping) }

	assertEqual(t, h.kinds(), []string{"update"})

	assertEqual(t, seen, []*statestore.SwapRecord{{From: ref(1), FromTarget: v1, To: v2, InvocationID: "0001"}})
	assertEqual(t, h.store.memory(unit), statestore.UnitMemory{Good: ref(2)})
}

func TestASwapIsNotStartedWhenItCannotBeRecorded(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.store.fails = errors.New("read-only file system")

	assertEqual(t, h.kinds(), []string{"defer"})
	assertEqual(t, h.journal.lines, []string{"hold game.service"})
	assertEqual(t, h.current(), v1)
}

func TestInterruptedAfterSetBeforeTheRestartTheProfileIsPutBackNotAdoptedAndTheSwapRedoneWhenIdle(t *testing.T) {
	h := interrupted(t, nil)
	h.profiles.seed(profile, v1, v2)

	assertEqual(t, h.kinds(), []string{"update"})

	assertEqual(t, h.journal.lines, []string{
		"switch 1", "delete 2", "hold game.service", "set " + v2, "restart game.service", "prune +2",
	})
	assertEqual(t, h.store.memory(unit), statestore.UnitMemory{Good: ref(2)})
	if h.logged("adopting") {
		t.Fatal(h.log.Text())
	}
}

func TestInterruptedAfterSetBeforeTheRestartWithAPlayerOnPutBackWithoutARestartAndWaits(t *testing.T) {
	h := interrupted(t, nil)
	h.profiles.seed(profile, v1, v2)
	h.conntrack.Flows(player)

	assertEqual(t, h.kinds(), []string{"defer"})

	assertEqual(t, h.journal.lines, []string{"switch 1", "delete 2"})
	assertEqual(t, h.current(), v1)
	assertEqual(t, h.store.memory(unit), statestore.UnitMemory{Good: ref(1)})
}

func TestInterruptedAfterTheClosureWasBoundBeforeSetTheDropInGoesBackToWhatTheProfileNames(t *testing.T) {
	h := interrupted(t, map[string]any{"bindClosure": true})
	h.conntrack.Flows(player)

	assertEqual(t, h.kinds(), []string{"defer"})

	assertEqual(t, h.journal.lines, []string{"bind " + v1, "daemon-reload"})
	assertEqual(t, h.store.memory(unit), statestore.UnitMemory{Good: ref(1)})
}

func TestInterruptedAfterTheRestartBeforeTheGateGatedNowAndCommittedWhenHealthy(t *testing.T) {
	h := interrupted(t, nil)
	h.profiles.seed(profile, v1, v2)
	h.systemd.patch(unit, func(state *UnitState) { state.InvocationID = "0002" })

	assertEqual(t, h.kinds(), []string{"keep"})

	assertEqual(t, h.journal.lines, []string{"hold game.service", "prune +2"})
	assertEqual(t, h.store.memory(unit), statestore.UnitMemory{Good: ref(2)})
}

func TestInterruptedAfterTheRestartBeforeTheGateGatedNowAndRolledBackAndPinnedWhenUnhealthy(t *testing.T) {
	h := interrupted(t, nil)
	h.profiles.seed(profile, v1, v2)
	h.systemd.patch(unit, func(state *UnitState) { state.InvocationID = "0002" })
	h.sockets.clear() // v2 came up but never bound its port
	neverBinds(h, v2)

	outcome := h.pass()[0]

	assertEqual(t, outcome.Action.Kind, decide.Pinned)
	assertEqual(t, h.journal.lines, []string{"hold game.service", "switch 1", "delete 2", "restart game.service"})
	assertEqual(t, h.current(), v1)
	assertEqual(t, h.store.memory(unit), statestore.UnitMemory{Good: ref(1), Pinned: v2})
}

func TestInterruptedAfterTheRestartAndFailedRolledBackAndPinned(t *testing.T) {
	h := interrupted(t, nil)
	h.profiles.seed(profile, v1, v2)
	h.systemd.patch(unit, func(state *UnitState) {
		state.InvocationID, state.ActiveState, state.Result = "0002", "failed", "exit-code"
	})

	assertEqual(t, h.kinds(), []string{"pinned"})

	assertEqual(t, h.current(), v1)
	assertEqual(t, h.store.memory(unit), statestore.UnitMemory{Good: ref(1), Pinned: v2})
}

func TestInterruptedInADryRunIsLeftAsItIs(t *testing.T) {
	h := interrupted(t, nil)
	h.profiles.seed(profile, v1, v2)
	h.restartDaemon(true)

	assertEqual(t, h.kinds(), []string{"keep"})
	assertEqual(t, h.journal.lines, nil)
	assertEqual(t, h.store.memory(unit).Swapping != nil, true)
}

func TestARollbacksPinIsSavedStraightAwayNotAtTheEndOfThePass(t *testing.T) {
	h := newHarness(t, nil, nil)
	neverBinds(h, v2)
	h.units.declare("other.service", map[string]any{"desired": v1, "profile": profile + "-other", "ports": []string{"7778/udp"}})
	h.systemd.add(unitState("other.service"))
	h.profiles.seed(profile+"-other", v1)
	// What is on disk when the pass moves on to the next unit: the moment a
	// crash would lose it.
	var onDisk *statestore.UnitMemory
	h.systemd.beforeUnit = func(name string) {
		if name == "other.service" && onDisk == nil {
			memory := h.store.memory(unit)
			onDisk = &memory
		}
	}

	h.pass()

	assertEqual(t, onDisk, &statestore.UnitMemory{Good: ref(1), Pinned: v2})
}

// Activity and list

func TestActivityReadsPlayersWithoutMovingTheIdleStreak(t *testing.T) {
	h := newHarness(t, map[string]any{"idleSamples": 2}, nil)
	activities, err := h.source.Activity(ctx, []detect.ConntrackEntry{player})
	assertEqual(t, err, nil)
	assertEqual(t, activities, []source.Activity{{Workload: unit, Busy: true}})
	activities, _ = h.source.Activity(ctx, []detect.ConntrackEntry{})
	assertEqual(t, activities, []source.Activity{{Workload: unit, Busy: false}})
	// Two idle ticks did not count as samples: the first pass is still 1/2.
	assertEqual(t, h.kinds(), []string{"defer"})
}

func TestActivityOfAStoppedUnitIsIdleAndOfOneSystemdCannotDescribeIsBusy(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.systemd.patch(unit, func(state *UnitState) { state.ActiveState = "inactive" })
	activities, _ := h.source.Activity(ctx, []detect.ConntrackEntry{player})
	assertEqual(t, activities, []source.Activity{{Workload: unit, Busy: false}})
	h.systemd.fails = errBusGone
	activities, _ = h.source.Activity(ctx, []detect.ConntrackEntry{})
	assertEqual(t, activities, []source.Activity{{Workload: unit, Busy: true}})
}

func TestActivityWithNoConntrackTableIsBusyAndSaysNothing(t *testing.T) {
	h := newHarness(t, map[string]any{"ports": []string{}}, nil)
	activities, _ := h.source.Activity(ctx, nil)
	assertEqual(t, activities, []source.Activity{{Workload: unit, Busy: true}})
	assertEqual(t, h.log.Lines(), nil)
}

func TestListShowsTheProfileTheStatusAndThePolicy(t *testing.T) {
	h := newHarness(t, nil, nil)
	lines, err := h.source.List(ctx)
	assertEqual(t, err, nil)
	assertEqual(t, lines, []string{
		unit + "  " + profile + " -> " + v1 + "  [update waiting]  desired=" + v2 + " " +
			"detector=conntrack health=systemd,port-connect,port-bound,uptime idle-samples=1",
	})
}

func TestListSaysWhenThereIsNoProfileOrTheVersionIsPinnedOut(t *testing.T) {
	h := newHarness(t, nil, storeOf(statestore.Memories{unit: {Pinned: v2}}))
	h.units.declare("other.service", map[string]any{"desired": v1, "profile": profile + "-other"})
	lines, _ := h.source.List(ctx)
	if !strings.Contains(lines[0], "-> "+v1+"  [pinned out]") || !strings.Contains(lines[1], "-> nothing  [no profile yet]") {
		t.Fatal(lines)
	}
}

func TestAUnitsFileThatCannotBeReadIsWarnedAboutOnceAndTheOthersCarryOn(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.units.files["broken.json"] = "{ not json"
	assertEqual(t, h.kinds(), []string{"update"})
	h.pass()
	n := 0
	for _, line := range h.log.Lines() {
		if strings.HasPrefix(line, "broken.json") {
			n++
		}
	}
	assertEqual(t, n, 1)
}

func TestASecondFileDeclaringTheSameUnitIsSkippedAndSaidOnce(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.units.files["zz-again.json"] = `{"unit": "game", "desired": "/nix/store/x", "profile": "/p/x"}`
	assertEqual(t, h.kinds(), []string{"update"})
	h.pass()
	assertEqual(t, h.count("zz-again.json: game.service is already declared by another file; skipping this one"), 1)
}

func TestAStateFileThatCannotBeReadFailsThePassRatherThanForgettingPins(t *testing.T) {
	h := newHarness(t, nil, &memoryStore{text: []byte("{ half")})
	if _, err := h.source.Once(ctx, source.Pass{Conntrack: []detect.ConntrackEntry{}}); err == nil {
		t.Fatal("a pass over an unreadable state file went ahead")
	}
	assertEqual(t, h.journal.lines, nil)
}

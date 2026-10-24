package daemon

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielbodart/gokit/await"
	"github.com/danielbodart/gokit/clock"
	"github.com/danielbodart/gokit/logx/logtest"
	"github.com/danielbodart/tidewaiter/internal/decide"
	"github.com/danielbodart/tidewaiter/internal/detect"
	"github.com/danielbodart/tidewaiter/internal/fakes"
	"github.com/danielbodart/tidewaiter/internal/model"
	"github.com/danielbodart/tidewaiter/internal/reboot"
	"github.com/danielbodart/tidewaiter/internal/source"
	"github.com/danielbodart/tidewaiter/internal/status"
)

var ctx = context.Background()

var (
	keep     = decide.Action{Kind: decide.Keep, Reason: "already on the latest image"}
	deferred = decide.Action{Kind: decide.Defer, Reason: "busy"}
	busy     = source.Sampled(true)
	idle     = source.Sampled(false)
)

// harness is the daemon over sources that report what they are told, with
// time stopped at 04:30, inside an automatic reboot's 03:00-05:00 window.
type harness struct {
	daemon    *Daemon
	sources   []*fakes.Source
	status    *fakes.Status
	conntrack *fakes.Conntrack
	log       *logtest.Logger
	check     *fakes.BootCheck
	rebooter  *fakes.Rebooter
	clock     *clock.Stopped
	systemd   *notifications
}

// notifications is what systemd is told, one sd_notify each, failing with
// err while it is set.
type notifications struct {
	mu   sync.Mutex
	sent []string
	err  error
}

func (n *notifications) notify(states ...string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.err != nil {
		return n.err
	}
	n.sent = append(n.sent, strings.Join(states, "\n"))
	return nil
}

func (n *notifications) failWith(err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.err = err
}

func (n *notifications) told() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.sent)
}

func (n *notifications) clear() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = nil
}

func newHarness(t *testing.T, names ...source.Name) *harness {
	t.Helper()
	h := &harness{
		status:    &fakes.Status{},
		conntrack: &fakes.Conntrack{},
		log:       &logtest.Logger{},
		check:     &fakes.BootCheck{},
		rebooter:  &fakes.Rebooter{},
		clock:     clock.NewStopped(time.Date(2026, 10, 23, 4, 30, 0, 0, time.UTC)),
		systemd:   &notifications{},
	}
	var sources []source.Source
	for _, name := range names {
		s := fakes.NewSource(name, h.status)
		h.sources = append(h.sources, s)
		sources = append(sources, s)
	}
	window, err := reboot.ParseWindow("03:00-05:00")
	if err != nil {
		t.Fatal(err)
	}
	h.daemon = New(Deps{
		Sources:    sources,
		Conntrack:  h.conntrack,
		Options:    Defaults,
		Log:        h.log,
		Status:     h.status,
		AutoReboot: &AutoReboot{Window: window, Label: "03:00-05:00", Check: h.check, Rebooter: h.rebooter},
		Notify:     h.systemd.notify,
		Clock:      h.clock,
	})
	return h
}

func both(t *testing.T) *harness { return newHarness(t, source.Docker, source.Systemd) }

func (h *harness) once(t *testing.T) []source.Outcome {
	t.Helper()
	outcomes, err := h.daemon.Once(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return outcomes
}

func (h *harness) last(t *testing.T) status.Status {
	t.Helper()
	last, ok := h.status.Last()
	if !ok {
		t.Fatal("no status written")
	}
	return last
}

func (h *harness) lastLine() string {
	lines := h.log.Lines()
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}

// run is Run on its own goroutine until stop is called, which waits for it to
// return.
func (h *harness) run(t *testing.T) (stop func()) {
	t.Helper()
	c, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		h.daemon.Run(c)
		close(done)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return")
		}
	}
}

func TestDumpsConntrackOnceAndHandsEverySourceTheSameTable(t *testing.T) {
	h := both(t)
	h.conntrack.Flows(detect.ConntrackEntry{Protocol: model.UDP, DestinationPort: 19132, Assured: true})

	h.once(t)

	if h.conntrack.Calls() != 1 {
		t.Errorf("dumped %d times", h.conntrack.Calls())
	}
	docker, systemd := h.sources[0].Passes()[0].Conntrack, h.sources[1].Passes()[0].Conntrack
	if len(docker) != 1 || !reflect.DeepEqual(docker, systemd) {
		t.Errorf("got %v and %v", docker, systemd)
	}
}

func TestAnEmptyTableReachesTheSourcesAsAnEmptyTableNotCouldNotTell(t *testing.T) {
	h := both(t)
	h.once(t)
	if table := h.sources[0].Passes()[0].Conntrack; table == nil || len(table) != 0 {
		t.Errorf("got %#v", table)
	}
}

func TestAFailedDumpReachesTheSourcesAsCouldNotTell(t *testing.T) {
	h := both(t)
	h.conntrack.FailWith(errors.New("operation not permitted"))

	h.once(t)
	h.once(t)

	if h.sources[1].Passes()[0].Conntrack != nil {
		t.Error("a failed dump read as a table")
	}
	warnings := 0
	for _, line := range h.log.Lines() {
		if strings.Contains(line, "could not read conntrack") {
			warnings++
		}
	}
	if warnings != 1 {
		t.Errorf("warned %d times", warnings)
	}
}

func TestConcatenatesTheOutcomesInSourceOrder(t *testing.T) {
	h := both(t)
	h.sources[0].Report(source.Outcome{Workload: "web", Action: keep, Busy: idle})
	h.sources[1].Report(source.Outcome{Workload: "game.service", Action: keep, Busy: idle})

	var names []string
	for _, outcome := range h.once(t) {
		names = append(names, outcome.Workload)
	}
	if !slices.Equal(names, []string{"web", "game.service"}) {
		t.Errorf("got %v", names)
	}
}

func TestASourceThatFailsDoesNotStopTheOtherAndTheErrorStillSurfaces(t *testing.T) {
	h := both(t)
	h.sources[0].FailWith(errors.New("docker socket gone"))
	h.sources[1].Report(source.Outcome{Workload: "game.service", Action: keep, Busy: idle})

	if _, err := h.daemon.Once(ctx); err == nil || err.Error() != "docker socket gone" {
		t.Errorf("got %v", err)
	}
	if len(h.sources[1].Passes()) != 1 {
		t.Error("the other source did not run")
	}
	if !h.log.ErrorContains("docker: Error: docker socket gone") {
		t.Errorf("logged %q", h.log.Lines())
	}
}

func TestSummarisesInNeutralWordsAcrossSources(t *testing.T) {
	h := both(t)
	h.sources[0].Report(source.Outcome{Workload: "web", Action: keep, Busy: idle})
	h.sources[1].Report(source.Outcome{Workload: "game.service", Action: keep, Busy: idle})
	h.once(t)
	if !slices.Contains(h.log.Lines(), "2 workloads up to date, nothing to do") {
		t.Errorf("logged %q", h.log.Lines())
	}
}

func TestSummarisesOnlyWhenItChanges(t *testing.T) {
	h := newHarness(t, source.Docker)
	h.sources[0].Report(source.Outcome{Workload: "web", Action: deferred, Busy: busy})
	h.once(t)
	h.once(t)
	if got := h.log.Lines(); !slices.Equal(got, []string{"1 deferred, 0 up to date"}) {
		t.Errorf("logged %q", got)
	}
}

func TestCallsUnitsUnitsWhenSystemdIsTheOnlySource(t *testing.T) {
	h := newHarness(t, source.Systemd)
	h.once(t)
	if !slices.Contains(h.log.Lines(), "No units to manage") {
		t.Errorf("logged %q", h.log.Lines())
	}
}

func TestSummarise(t *testing.T) {
	update := decide.Action{Kind: decide.Update}
	pinned := decide.Action{Kind: decide.Pinned}
	for _, c := range []struct {
		actions []decide.Action
		want    string
	}{
		{nil, "No containers to manage"},
		{[]decide.Action{keep}, "1 container up to date, nothing to do"},
		{[]decide.Action{keep, update, deferred, deferred, pinned}, "1 updated, 2 deferred, 1 pinned out, 1 up to date"},
	} {
		if got := summarise(c.actions, "container"); got != c.want {
			t.Errorf("got %q, want %q", got, c.want)
		}
	}
}

func TestTheStatusSaysExactlyWhichWorkloadsAreBusyAndThatTheyAreNotOnceIdle(t *testing.T) {
	h := both(t)
	h.sources[0].Report(source.Outcome{Workload: "web", Action: deferred, Busy: busy})
	h.sources[1].Report(
		source.Outcome{Workload: "bedrock.service", Action: deferred, Busy: busy},
		source.Outcome{Workload: "hytale.service", Action: keep, Busy: idle, Settled: true},
	)

	h.once(t)
	last := h.last(t)
	if !slices.Equal(last.Busy, []string{"bedrock.service", "web"}) {
		t.Errorf("busy %v", last.Busy)
	}
	want := []status.Workload{
		{Workload: "web", Source: source.Docker, Busy: true},
		{Workload: "bedrock.service", Source: source.Systemd, Busy: true},
		{Workload: "hytale.service", Source: source.Systemd, Settled: true},
	}
	if !reflect.DeepEqual(last.Workloads, want) {
		t.Errorf("workloads %+v", last.Workloads)
	}
	if last.StaleAfter != 60 || !last.Updated.Equal(h.clock.Now()) {
		t.Errorf("written %v, stale after %d", last.Updated, last.StaleAfter)
	}

	h.sources[0].Report(source.Outcome{Workload: "web", Action: keep, Busy: idle})
	h.sources[1].Report(source.Outcome{Workload: "bedrock.service", Action: keep, Busy: idle})
	h.once(t)
	if got := h.last(t).Busy; len(got) != 0 {
		t.Errorf("busy %v", got)
	}
}

func TestTheStatusSaysASwapIsUnderWayBeforeItStartsAndStopsAtTheEndOfThePass(t *testing.T) {
	h := newHarness(t, source.Systemd)
	h.sources[0].Hold("bedrock.service")
	h.sources[0].Report(source.Outcome{Workload: "bedrock.service", Action: keep, Busy: idle})

	h.once(t)

	if got := h.sources[0].SeenSwapping(); !reflect.DeepEqual(got, [][]string{{"bedrock.service"}}) {
		t.Errorf("saw %v", got)
	}
	if got := h.last(t).Swapping; len(got) != 0 {
		t.Errorf("still swapping %v", got)
	}
}

// A swap's pull, health gate and rollback can outlast staleAfter; `status`
// must still read "swapping", not a stale status that says no daemon.
func TestTheTickKeepsTheStatusFreshWhileALongPassHoldsASwap(t *testing.T) {
	h := newHarness(t, source.Systemd)
	h.sources[0].Hold("bedrock.service")
	release := make(chan struct{})
	h.sources[0].During(func(context.Context) { <-release })
	stop := h.run(t)
	defer stop()
	defer close(release)
	await.Until(t, "the pass to start", func() bool { return len(h.sources[0].Passes()) == 1 })

	// Only the tick loop waits on the clock; the pass loop is in the pass. A
	// tick queued behind the pass would never wait again, so bound the wait.
	waiting, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for range 30 { // 7.5 minutes, against a staleAfter of 60s
		if err := h.clock.BlockUntil(waiting, 1); err != nil {
			t.Fatal(err)
		}
		h.clock.Advance(Defaults.ActivityInterval)
	}
	// Back waiting, so the last tick's write is done.
	if err := h.clock.BlockUntil(waiting, 1); err != nil {
		t.Fatal(err)
	}

	last := h.last(t)
	if status.IsStale(last, h.clock.Now()) {
		t.Errorf("stale mid-swap: updated %s, now %s, stale after %ds", last.Updated, h.clock.Now(), last.StaleAfter)
	}
	if !reflect.DeepEqual(last.Swapping, []string{"bedrock.service"}) {
		t.Errorf("swapping %v", last.Swapping)
	}
	if got := len(h.sources[0].Ticks()); got != 0 {
		t.Errorf("read activity %d times under a pass", got)
	}
}

func TestKeepsAWorkloadsLastVerdictWhenAPassCouldNotSampleItAndCallsItUnsettled(t *testing.T) {
	h := newHarness(t, source.Docker)
	h.sources[0].Report(source.Outcome{Workload: "web", Action: deferred, Busy: busy})
	h.once(t)

	// The registry did not answer: no sample this pass.
	h.sources[0].Report(source.Outcome{Workload: "web", Action: decide.Action{Kind: decide.Keep, Reason: "registry unreachable"}})
	h.once(t)

	last := h.last(t)
	if !slices.Equal(last.Busy, []string{"web"}) ||
		!reflect.DeepEqual(last.Workloads, []status.Workload{{Workload: "web", Source: source.Docker, Busy: true}}) {
		t.Errorf("got %+v", last)
	}
}

func TestKeepsASourcesVerdictsWhenTheSourceFails(t *testing.T) {
	h := both(t)
	h.sources[1].Report(source.Outcome{Workload: "bedrock.service", Action: deferred, Busy: busy})
	h.once(t)

	h.sources[1].FailWith(errors.New("bus gone"))
	h.daemon.Once(ctx)

	if got := h.last(t).Busy; !slices.Equal(got, []string{"bedrock.service"}) {
		t.Errorf("busy %v", got)
	}
}

func TestDropsAWorkloadTheSourceNoLongerReports(t *testing.T) {
	h := newHarness(t, source.Docker)
	h.sources[0].Report(source.Outcome{Workload: "web", Action: deferred, Busy: busy})
	h.once(t)

	h.sources[0].Report()
	h.once(t)

	if last := h.last(t); len(last.Busy) != 0 || len(last.Workloads) != 0 {
		t.Errorf("got %+v", last)
	}
}

func TestTheActivityTickMovesItBetweenPasses(t *testing.T) {
	h := both(t)
	h.sources[1].Report(source.Outcome{Workload: "bedrock.service", Action: keep, Busy: idle})
	h.once(t)
	if got := h.last(t).Busy; len(got) != 0 {
		t.Fatalf("busy %v", got)
	}

	h.sources[1].Active(source.Activity{Workload: "bedrock.service", Busy: true})
	h.daemon.Tick(ctx)

	if got := h.last(t).Busy; !slices.Equal(got, []string{"bedrock.service"}) {
		t.Errorf("busy %v", got)
	}
	if h.conntrack.Calls() != 2 {
		t.Errorf("dumped %d times", h.conntrack.Calls())
	}
	// The tick asked for activity only; no pass ran.
	if len(h.sources[1].Passes()) != 1 {
		t.Error("a pass ran")
	}
	if got := h.last(t).Workloads; !reflect.DeepEqual(got, []status.Workload{{Workload: "bedrock.service", Source: source.Systemd, Busy: true}}) {
		t.Errorf("workloads %+v", got)
	}
}

func TestAStatusThatCannotBeWrittenIsWarnedAboutOnceAndThePassGoesOn(t *testing.T) {
	h := newHarness(t, source.Systemd)
	h.status.FailWith(errors.New("read-only file system"))
	h.sources[0].Report(source.Outcome{Workload: "bedrock.service", Action: deferred, Busy: busy})

	h.once(t)
	h.once(t)

	warnings := 0
	for _, line := range h.log.Lines() {
		if strings.Contains(line, "could not write the status") {
			warnings++
		}
	}
	if warnings != 1 {
		t.Errorf("warned %d times", warnings)
	}
}

func TestRunWritesTheStatusAndClearsItOnTheWayOut(t *testing.T) {
	h := newHarness(t, source.Systemd)
	h.sources[0].Report(source.Outcome{Workload: "bedrock.service", Action: deferred, Busy: busy})

	stop := h.run(t)
	await.Until(t, "the first pass", func() bool { return len(h.sources[0].Passes()) == 1 })
	await.Until(t, "its status", func() bool {
		last, ok := h.status.Last()
		return ok && slices.Equal(last.Busy, []string{"bedrock.service"})
	})
	stop()

	if !h.status.Cleared() {
		t.Error("not cleared")
	}
}

func TestRunPassesEveryIntervalAndTicksInBetween(t *testing.T) {
	h := newHarness(t, source.Systemd)
	stop := h.run(t)
	defer stop()
	await.Until(t, "the first pass", func() bool { return len(h.sources[0].Passes()) == 1 })

	// The pass loop and the tick loop each waiting on the clock.
	if err := h.clock.BlockUntil(ctx, 2); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(Defaults.ActivityInterval)
	await.Until(t, "a tick", func() bool { return len(h.sources[0].Ticks()) == 1 })
	if len(h.sources[0].Passes()) != 1 {
		t.Error("a tick ran a pass")
	}

	h.clock.Advance(Defaults.Interval - Defaults.ActivityInterval)
	await.Until(t, "the second pass", func() bool { return len(h.sources[0].Passes()) == 2 })
	if !h.log.Contains("Reconciling every 300s...") {
		t.Errorf("logged %q", h.log.Lines())
	}
}

func TestRunFinishesThePassInFlightBeforeReturning(t *testing.T) {
	h := newHarness(t, source.Systemd)
	release := make(chan struct{})
	finished := make(chan struct{})
	h.sources[0].During(func(c context.Context) {
		<-release
		// The pass's own context is not cancelled under it.
		if c.Err() == nil {
			close(finished)
		}
	})
	c, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		h.daemon.Run(c)
		close(done)
	}()
	await.Until(t, "the pass to start", func() bool { return len(h.sources[0].Passes()) == 1 })
	cancel()
	select {
	case <-done:
		t.Fatal("returned with a pass in flight")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	<-done
	select {
	case <-finished:
	default:
		t.Error("the pass was cancelled")
	}
}

// ready is a systemd source with one unit idle for its whole debounce, and a
// reboot needed.
func ready(t *testing.T) *harness {
	h := newHarness(t, source.Systemd)
	h.sources[0].Report(source.Outcome{Workload: "bedrock.service", Action: keep, Busy: idle, Settled: true})
	h.sources[0].Active(source.Activity{Workload: "bedrock.service", Busy: false})
	h.check.Need("kernel changed since boot")
	return h
}

func (h *harness) pass(t *testing.T) {
	t.Helper()
	h.once(t)
	h.daemon.ConsiderReboot(ctx)
}

func TestARebootGoesAheadWhenNeededInsideTheWindowWithEverythingSettled(t *testing.T) {
	h := ready(t)
	h.pass(t)
	if h.rebooter.Reboots() != 1 {
		t.Errorf("rebooted %d times", h.rebooter.Reboots())
	}
	if !slices.Contains(h.log.Lines(), "Automatic reboot (03:00-05:00): needed, kernel changed since boot; rebooting, as nothing is in use") {
		t.Errorf("logged %q", h.log.Lines())
	}
	if got := h.last(t).Reboot.Verdict; got != "rebooting, as nothing is in use" {
		t.Errorf("verdict %q", got)
	}
}

func TestARebootIsAskedForOnceNotAgainEveryPassAfter(t *testing.T) {
	h := ready(t)
	h.pass(t)
	h.pass(t)
	if h.rebooter.Reboots() != 1 {
		t.Errorf("rebooted %d times", h.rebooter.Reboots())
	}
}

func TestARebootIsNotNeededWhenTheBootedSystemIsWhatIsInstalled(t *testing.T) {
	h := ready(t)
	h.check.Need("")
	h.pass(t)
	if h.rebooter.Reboots() != 0 || !slices.Contains(h.log.Lines(), "Automatic reboot (03:00-05:00): not needed") {
		t.Errorf("rebooted %d, logged %q", h.rebooter.Reboots(), h.log.Lines())
	}
}

func TestARebootNeverHappensWhenItCannotTellWhetherOneIsNeeded(t *testing.T) {
	h := ready(t)
	h.check.FailWith(errors.New("cannot read /nix/var/nix/profiles/system"))
	h.pass(t)
	if h.rebooter.Reboots() != 0 || !h.log.Contains("could not tell whether a reboot is needed") {
		t.Errorf("rebooted %d, logged %q", h.rebooter.Reboots(), h.log.Lines())
	}
}

func TestARebootWaitsOutsideTheWindowHoweverIdle(t *testing.T) {
	h := ready(t)
	h.clock.Advance(30 * time.Minute) // 05:00
	h.pass(t)
	if h.rebooter.Reboots() != 0 {
		t.Error("rebooted outside the window")
	}
	if !slices.Contains(h.log.Lines(), "Automatic reboot (03:00-05:00): needed, kernel changed since boot; waiting for the window") {
		t.Errorf("logged %q", h.log.Lines())
	}
	want := status.Reboot{Window: "03:00-05:00", Needed: "kernel changed since boot", Verdict: "waiting for the window"}
	if got := h.last(t).Reboot; got == nil || *got != want {
		t.Errorf("status %+v", got)
	}

	h.clock.Advance(22 * time.Hour) // 03:00 the next day
	h.pass(t)
	if h.rebooter.Reboots() != 1 {
		t.Error("did not reboot in the next window")
	}
}

func TestARebootWaitsWhileAnythingIsInUseAndGoesOnceItIsNot(t *testing.T) {
	h := ready(t)
	h.sources[0].Report(source.Outcome{Workload: "bedrock.service", Action: deferred, Busy: busy})
	h.pass(t)
	if h.rebooter.Reboots() != 0 || !slices.Contains(h.log.Lines(), "Automatic reboot (03:00-05:00): needed, kernel changed since boot; waiting: bedrock.service in use") {
		t.Errorf("rebooted %d, logged %q", h.rebooter.Reboots(), h.log.Lines())
	}

	h.sources[0].Report(source.Outcome{Workload: "bedrock.service", Action: keep, Busy: idle, Settled: true})
	h.pass(t)
	if h.rebooter.Reboots() != 1 {
		t.Error("did not reboot once idle")
	}
}

func TestARebootWaitsForEverySource(t *testing.T) {
	h := both(t)
	h.check.Need("kernel changed since boot")
	h.sources[0].Report(source.Outcome{Workload: "web", Action: keep, Busy: busy})
	h.sources[1].Report(source.Outcome{Workload: "bedrock.service", Action: keep, Busy: idle, Settled: true})
	h.pass(t)
	if h.rebooter.Reboots() != 0 || !strings.Contains(h.lastLine(), "waiting: web in use") {
		t.Errorf("rebooted %d, logged %q", h.rebooter.Reboots(), h.log.Lines())
	}
}

func TestARebootWaitsUntilEverythingHasBeenIdleForItsWholeDebounce(t *testing.T) {
	h := ready(t)
	h.sources[0].Report(source.Outcome{Workload: "bedrock.service", Action: deferred, Busy: idle})
	h.pass(t)
	if h.rebooter.Reboots() != 0 || !strings.Contains(h.lastLine(), "waiting: bedrock.service not idle for long enough yet") {
		t.Errorf("rebooted %d, logged %q", h.rebooter.Reboots(), h.log.Lines())
	}
}

func TestARebootWaitsForAWorkloadThePassCouldNotSample(t *testing.T) {
	h := ready(t)
	h.sources[0].Report(source.Outcome{Workload: "bedrock.service", Action: decide.Action{Kind: decide.Keep, Reason: "systemd unreachable"}})
	h.pass(t)
	if h.rebooter.Reboots() != 0 {
		t.Error("rebooted")
	}
}

func TestARebootWaitsWhileASwapIsUnderWay(t *testing.T) {
	h := ready(t)
	// Asked from inside the pass, while the swap is held.
	h.sources[0].Hold("bedrock.service")
	h.sources[0].During(h.daemon.ConsiderReboot)
	h.once(t)
	if h.rebooter.Reboots() != 0 || !h.log.Contains("waiting: swapping bedrock.service") {
		t.Errorf("rebooted %d, logged %q", h.rebooter.Reboots(), h.log.Lines())
	}
}

func TestARebootLooksOnceMoreRightBeforeAndWaitsForSomeoneWhoHasJustJoined(t *testing.T) {
	h := ready(t)
	h.sources[0].Active(source.Activity{Workload: "bedrock.service", Busy: true})
	h.pass(t)
	if h.rebooter.Reboots() != 0 || !strings.Contains(h.lastLine(), "waiting: bedrock.service in use") {
		t.Errorf("rebooted %d, logged %q", h.rebooter.Reboots(), h.log.Lines())
	}
}

func TestARebootWaitsWhenTheLastLookCouldNotBeTaken(t *testing.T) {
	h := ready(t)
	h.sources[0].FailActivityWith(errors.New("bus gone"))
	h.pass(t)
	if h.rebooter.Reboots() != 0 || !strings.Contains(h.lastLine(), "waiting: activity could not be read") {
		t.Errorf("rebooted %d, logged %q", h.rebooter.Reboots(), h.log.Lines())
	}
}

func TestARebootRefusedByLogindIsAskedForAgainNextPass(t *testing.T) {
	h := ready(t)
	h.rebooter.FailWith(errors.New("Operation denied due to active block inhibitor"))
	h.pass(t)
	if h.rebooter.Reboots() != 0 || !strings.Contains(h.lastLine(), "the reboot was refused") {
		t.Errorf("rebooted %d, logged %q", h.rebooter.Reboots(), h.log.Lines())
	}

	h.rebooter.FailWith(nil)
	h.pass(t)
	if h.rebooter.Reboots() != 1 {
		t.Error("did not ask again")
	}
}

func TestARebootNeverHappensFromAPassThatFailed(t *testing.T) {
	h := ready(t)
	h.sources[0].FailWith(errors.New("bus gone"))
	stop := h.run(t)
	await.Until(t, "the pass", func() bool { return h.log.ErrorContains("Error: bus gone") })
	stop()
	if h.rebooter.Reboots() != 0 {
		t.Error("rebooted")
	}
}

func TestRunRebootsAtTheEndOfAPass(t *testing.T) {
	h := ready(t)
	stop := h.run(t)
	await.Until(t, "the reboot", func() bool { return h.rebooter.Reboots() == 1 })
	stop()
}

func TestWithNoStatusAndNoRebootRunDoesNotTick(t *testing.T) {
	s := fakes.NewSource(source.Systemd, nil)
	now := clock.NewStopped(time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC))
	d := New(Deps{Sources: []source.Source{s}, Conntrack: &fakes.Conntrack{}, Options: Defaults, Clock: now})
	c, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		d.Run(c)
		close(done)
	}()
	if err := now.BlockUntil(ctx, 1); err != nil {
		t.Fatal(err)
	}
	now.Advance(Defaults.ActivityInterval)
	cancel()
	<-done
	if len(s.Ticks()) != 0 {
		t.Error("ticked")
	}
}

// systemd

func TestAPassTellsSystemdWhatItIsSwappingThenWhatItConcluded(t *testing.T) {
	h := newHarness(t, source.Systemd)
	h.sources[0].Hold("bedrock.service")
	h.sources[0].Report(source.Outcome{Workload: "bedrock.service", Action: keep, Busy: idle, Settled: true})

	h.once(t)

	equal(t, h.systemd.told(), []string{
		"STATUS=Swapping bedrock.service",
		"STATUS=Idle: 1 unit up to date, nothing to do",
	})
}

func TestSystemdHearsWhatIsInUseAndAFailedPass(t *testing.T) {
	h := newHarness(t, source.Systemd)
	h.sources[0].Report(source.Outcome{Workload: "bedrock.service", Action: deferred, Busy: busy})
	h.once(t)
	h.sources[0].FailWith(errors.New("bus gone"))
	h.daemon.Once(ctx)

	equal(t, h.systemd.told(), []string{
		"STATUS=1 deferred, 0 up to date; in use: bedrock.service",
		"STATUS=The last pass failed: bus gone; in use: bedrock.service",
	})
}

func TestATickTellsSystemdOnlyWhatChanged(t *testing.T) {
	h := newHarness(t, source.Systemd)
	h.sources[0].Report(source.Outcome{Workload: "bedrock.service", Action: keep, Busy: idle, Settled: true})
	h.once(t)
	h.systemd.clear()

	h.daemon.Tick(ctx)
	equal(t, h.systemd.told(), []string(nil))

	h.sources[0].Active(source.Activity{Workload: "bedrock.service", Busy: true})
	h.daemon.Tick(ctx)
	equal(t, h.systemd.told(), []string{"STATUS=1 unit up to date, nothing to do; in use: bedrock.service"})
}

func TestSystemdHearsWhenARebootIsNeededAndWhatItWaitsFor(t *testing.T) {
	h := newHarness(t, source.Systemd)
	h.check.Need("kernel changed since boot")
	h.sources[0].Report(source.Outcome{Workload: "bedrock.service", Action: deferred, Busy: busy})
	h.once(t)
	h.daemon.ConsiderReboot(ctx)

	told := h.systemd.told()
	equal(t, told[len(told)-1], "STATUS=1 deferred, 0 up to date; in use: bedrock.service; reboot needed, kernel changed since boot: waiting: bedrock.service in use")
}

// Ready before the first pass: a pass can swap for minutes, and systemctl
// start - and a NixOS switch - must not wait on it.
func TestRunIsReadyBeforeItsFirstPassAndSaysWhenItIsStopping(t *testing.T) {
	h := newHarness(t, source.Systemd)
	release := make(chan struct{})
	h.sources[0].Hold("bedrock.service")
	h.sources[0].During(func(context.Context) { <-release })
	h.sources[0].Report(source.Outcome{Workload: "bedrock.service", Action: keep, Busy: idle, Settled: true})

	c, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		h.daemon.Run(c)
		close(done)
	}()
	await.Until(t, "the swap to start", func() bool { return len(h.systemd.told()) == 2 })
	equal(t, h.systemd.told(), []string{"READY=1\nSTATUS=Starting the first pass", "STATUS=Swapping bedrock.service"})

	cancel()
	await.Until(t, "systemd to hear it is stopping", func() bool { return len(h.systemd.told()) == 3 })
	close(release)
	<-done
	// The pass that finished after it says nothing more.
	equal(t, h.systemd.told()[2:], []string{"STOPPING=1\nSTATUS=Stopping once the swap of bedrock.service is done"})
}

func TestSystemdThatCannotBeToldIsWarnedAboutOnce(t *testing.T) {
	h := newHarness(t, source.Systemd)
	h.systemd.failWith(errors.New("sd_notify: connection refused"))
	h.sources[0].Report(source.Outcome{Workload: "bedrock.service", Action: deferred, Busy: busy})
	h.once(t)
	h.sources[0].Report(source.Outcome{Workload: "bedrock.service", Action: keep, Busy: idle})
	h.once(t)

	warnings := 0
	for _, line := range h.log.Lines() {
		if strings.Contains(line, "could not tell systemd") {
			warnings++
		}
	}
	equal(t, warnings, 1)
}

func equal[T any](t *testing.T, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

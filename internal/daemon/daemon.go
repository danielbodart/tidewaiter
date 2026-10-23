// Package daemon is the loop every source runs in, and everything they share.
//
// One pass dumps the host conntrack table once, runs each source in turn over
// it (so two swaps never overlap, whichever sources they belong to), logs one
// summary line when it changes, and writes what is busy, settled and being
// swapped to the status file. Between passes a faster activity tick keeps that
// current, so `tidewaiter status` sees a player who joined just after a pass
// within seconds rather than one interval later.
//
// With an automatic reboot, every pass of Run ends by asking whether the
// machine needs one and may have it now (see ConsiderReboot). Nothing here
// ever stands in a person's way: a reboot asked for by hand goes ahead.
//
// A source that fails does not stop the others; its workloads keep whatever
// busy verdict they last had, so a source that cannot be read never reads as
// idle.
package daemon

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danielbodart/gokit/clock"
	"github.com/danielbodart/tidewaiter/internal/decide"
	"github.com/danielbodart/tidewaiter/internal/detect"
	"github.com/danielbodart/tidewaiter/internal/reboot"
	"github.com/danielbodart/tidewaiter/internal/source"
	"github.com/danielbodart/tidewaiter/internal/status"
)

// Options shape the loop. What shapes a source - its label, dry run, grace,
// health polling - is that source's own.
type Options struct {
	// Between reconcile passes.
	Interval time.Duration
	// Between activity ticks, which keep the status and a reboot's gate
	// current.
	ActivityInterval time.Duration
}

// Defaults is how the daemon runs unless told otherwise.
var Defaults = Options{Interval: 300 * time.Second, ActivityInterval: 15 * time.Second}

// AutoReboot is an automatic reboot, when one is needed, inside a daily
// window, once nothing is in use. The window is in the clock's own location -
// the system clock's is local time.
type AutoReboot struct {
	Window reboot.Window
	// The window as given, "03:00-05:00", for the log and the status.
	Label    string
	Check    reboot.BootCheck
	Rebooter reboot.Rebooter
}

// Deps is what a Daemon is built from.
type Deps struct {
	// Run in this order, every pass.
	Sources   []source.Source
	Conntrack detect.ConntrackDumper
	Options   Options
	// Silent when nil.
	Log source.Log
	// status.None when nil: nothing written, and no activity tick unless
	// there is an automatic reboot to keep current.
	Status status.Sink
	// Nil for none.
	AutoReboot *AutoReboot
	// The system clock when nil.
	Clock clock.Clock
}

// Daemon runs sources, once or continuously.
type Daemon struct {
	sources    []source.Source
	conntrack  detect.ConntrackDumper
	options    Options
	log        source.Log
	status     status.Sink
	autoReboot *AutoReboot
	clock      clock.Clock

	// One source call at a time: a pass, a tick's activity read, or the last
	// look before a reboot. Sources need no locking of their own for it.
	turn sync.Mutex

	// Everything below, and every status write, under mu.
	mu sync.Mutex
	// Per source, what each workload was last seen as, in the order the
	// status lists them.
	seen map[source.Name]*verdicts
	// Workloads being swapped right now, held for the rest of the pass.
	held               map[string]bool
	lastSummary        string
	lastConntrackError string
	lastStatusError    string
	reboot             *status.Reboot
	lastRebootLine     string
	// Set once logind has accepted the reboot, so no later pass asks again.
	rebooting bool
}

// verdicts is one source's workloads as last seen.
type verdicts struct {
	order []string
	// The last busy verdict; absent for a workload never sampled.
	busy map[string]bool
	// Idle for its whole debounce at the last pass.
	settled map[string]bool
}

// New is a daemon over deps.
func New(deps Deps) *Daemon {
	if deps.Clock == nil {
		deps.Clock = clock.System{}
	}
	if deps.Status == nil {
		deps.Status = status.None
	}
	if deps.Log == nil {
		deps.Log = func(string) {}
	}
	// Passes and ticks both speak, on different goroutines, so their lines
	// are taken one at a time here rather than every Log having to be safe
	// for it.
	var speaking sync.Mutex
	log := func(line string) {
		speaking.Lock()
		defer speaking.Unlock()
		deps.Log(line)
	}
	return &Daemon{
		sources:    deps.Sources,
		conntrack:  deps.Conntrack,
		options:    deps.Options,
		log:        log,
		status:     deps.Status,
		autoReboot: deps.AutoReboot,
		clock:      deps.Clock,
		seen:       map[source.Name]*verdicts{},
		held:       map[string]bool{},
	}
}

// Once is one pass over every source.
//
// If a source failed, the others still ran; the first error is then
// returned, so the loop logs it and `check` exits non-zero, as a pass that
// could not see its containers always has.
func (d *Daemon) Once(ctx context.Context) ([]source.Outcome, error) {
	d.turn.Lock()
	defer d.turn.Unlock()

	pass := source.Pass{
		Conntrack: d.dumpConntrack(ctx),
		Hold: func(workload string) {
			d.mu.Lock()
			d.held[workload] = true
			d.mu.Unlock()
			d.publish()
		},
	}

	var outcomes []source.Outcome
	var failure error
	for _, s := range d.sources {
		own, err := s.Once(ctx, pass)
		if err != nil {
			if len(d.sources) > 1 {
				d.log(fmt.Sprintf("%s: Error: %s", s.Name(), err))
			}
			if failure == nil {
				failure = err
			}
			continue
		}
		d.record(s.Name(), own)
		outcomes = append(outcomes, own...)
	}

	d.mu.Lock()
	clear(d.held)
	d.mu.Unlock()
	d.publish()
	if failure != nil {
		return nil, failure
	}

	actions := make([]decide.Action, len(outcomes))
	for i, outcome := range outcomes {
		actions[i] = outcome.Action
	}
	summary := summarise(actions, d.noun())
	d.mu.Lock()
	changed := summary != d.lastSummary
	d.lastSummary = summary
	d.mu.Unlock()
	if changed {
		d.log(summary)
	}
	return outcomes, nil
}

// Tick re-reads activity only and writes the status: no digests, no
// decisions, no idle streaks, so it keeps the status current but never hurries
// a swap or a reboot.
//
// While a pass holds the sources - a swap's pull, health gate and rollback can
// run for minutes - the tick does not queue behind it: it writes the status as
// it stands, so Updated keeps moving and `status` reads "swapping", never a
// stale status that looks like a dead daemon. The busy verdicts keep their
// last values until the pass or the next free tick moves them.
func (d *Daemon) Tick(ctx context.Context) {
	if d.turn.TryLock() {
		d.readActivityHoldingTheTurn(ctx)
		d.turn.Unlock()
	}
	d.publish()
}

// InUse is the workloads in use, as last seen by a pass or a tick, sorted.
func (d *Daemon) InUse() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.inUse()
}

func (d *Daemon) inUse() []string {
	names := map[string]bool{}
	for _, seen := range d.seen {
		for workload, busy := range seen.busy {
			if busy {
				names[workload] = true
			}
		}
	}
	return sorted(names)
}

// Swapping is the workloads being swapped right now, sorted.
func (d *Daemon) Swapping() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return sorted(d.held)
}

// unsettled is the workloads not idle for their whole debounce at the last
// pass - never sampled included.
func (d *Daemon) unsettled() []string {
	names := map[string]bool{}
	for _, seen := range d.seen {
		for workload, settled := range seen.settled {
			if !settled {
				names[workload] = true
			}
		}
	}
	return sorted(names)
}

// Snapshot is what the status file says, as of now.
func (d *Daemon) Snapshot() status.Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.snapshot()
}

func (d *Daemon) snapshot() status.Status {
	workloads := []status.Workload{}
	for _, s := range d.sources {
		seen, ok := d.seen[s.Name()]
		if !ok {
			continue
		}
		for _, workload := range seen.order {
			workloads = append(workloads, status.Workload{
				Workload: workload,
				Source:   s.Name(),
				Busy:     seen.busy[workload],
				Settled:  seen.settled[workload],
			})
		}
	}
	snapshot := status.Status{
		Updated: d.clock.Now(),
		// Several ticks missed, and never less than a minute, so a slow
		// conntrack dump or a busy host doesn't read as a dead daemon.
		StaleAfter: max(60, int(4*d.options.ActivityInterval/time.Second)),
		Busy:       d.inUse(),
		Swapping:   sorted(d.held),
		Workloads:  workloads,
	}
	if d.reboot != nil {
		reboot := *d.reboot
		snapshot.Reboot = &reboot
	}
	return snapshot
}

// ConsiderReboot runs after a pass: reboot if the machine needs it, it is
// inside the window, and nothing is in use, unsettled or being swapped - the
// same idleness a swap waits for, across every source. Right before asking
// logind, activity is read once more, as a swap re-samples before it starts,
// so a player who joined since the pass is never cut off.
//
// Never outside the window. A window busy from start to end passes without a
// reboot, and tomorrow's is tried the same way. What it concluded is logged
// when it changes, and is in the status.
func (d *Daemon) ConsiderReboot(ctx context.Context) {
	d.mu.Lock()
	skip := d.autoReboot == nil || d.rebooting
	d.mu.Unlock()
	if skip {
		return
	}
	d.rebootIfReady(ctx, d.autoReboot)
	d.publish()
}

func (d *Daemon) rebootIfReady(ctx context.Context, auto *AutoReboot) {
	needed, err := auto.Check.Needed(ctx)
	if err != nil {
		d.rebootVerdict(auto, "", fmt.Sprintf("could not tell whether a reboot is needed (%s)", err))
		return
	}

	decision := func() reboot.Verdict {
		d.mu.Lock()
		defer d.mu.Unlock()
		return reboot.Decide(reboot.Inputs{
			Needed:    needed,
			InWindow:  auto.Window.Contains(d.clock.Now()),
			Busy:      d.inUse(),
			Unsettled: d.unsettled(),
			Swapping:  sorted(d.held),
		})
	}
	verdict := decision()
	if verdict.Kind == reboot.Reboot {
		if d.readActivity(ctx) {
			verdict = decision()
		} else {
			verdict = reboot.Verdict{Kind: reboot.Wait, Reason: "waiting: activity could not be read"}
		}
	}
	switch verdict.Kind {
	case reboot.NotNeeded:
		d.rebootVerdict(auto, "", "not needed")
		return
	case reboot.Wait:
		d.rebootVerdict(auto, needed, verdict.Reason)
		return
	}

	d.rebootVerdict(auto, needed, "rebooting, as nothing is in use")
	if err := auto.Rebooter.Reboot(ctx); err != nil {
		// Someone else's block inhibitor, or no logind at all: the next pass
		// in the window asks again.
		d.rebootVerdict(auto, needed, fmt.Sprintf("the reboot was refused (%s); trying again next pass", err))
		return
	}
	d.mu.Lock()
	d.rebooting = true
	d.mu.Unlock()
}

// Run reconciles on the interval until ctx is done, ticking activity in
// between when there is a status file or a reboot to keep current, and
// clearing the status on the way out.
//
// Returns once the pass in flight, if any, has finished: a pass is never
// abandoned halfway through a swap, so shutting down cannot leave a container
// stopped and not yet replaced. Passes run on a context that outlives ctx for
// that reason; a tick is only reading, so it is cut off.
func (d *Daemon) Run(ctx context.Context) {
	var wg sync.WaitGroup
	if d.status != status.None || d.autoReboot != nil {
		wg.Go(func() { d.tickEvery(ctx) })
	}
	d.runEvery(ctx, func(passes context.Context) {
		if _, err := d.Once(passes); err != nil {
			// Not fatal: Docker may be restarting, or the bus briefly gone.
			// The next pass tries again - and a pass that could not see
			// everything never leads to a reboot.
			d.log("Error: " + err.Error())
			return
		}
		d.ConsiderReboot(passes)
	})
	wg.Wait()
	_ = d.status.Clear()
}

// runEvery runs pass now, then once every interval until ctx is done, one at a
// time: the systemd source's swaps and the Docker source's share one queue,
// so a swap in one never overlaps a swap in the other. The interval is
// counted from the end of a pass, so a pass that overruns it is followed by
// the next after a full interval rather than straight away.
func (d *Daemon) runEvery(ctx context.Context, pass func(context.Context)) {
	passes := context.WithoutCancel(ctx)
	pass(passes)
	d.log(fmt.Sprintf("Reconciling every %ss...", seconds(d.options.Interval)))
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.clock.After(d.options.Interval):
			pass(passes)
		}
	}
}

// tickEvery is the activity tick, until ctx is done. One at a time, so a slow
// dump never stacks up behind itself.
func (d *Daemon) tickEvery(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.clock.After(d.options.ActivityInterval):
			d.Tick(ctx)
		}
	}
}

// readActivity reads every source's activity into the busy verdicts, and
// says whether every source answered; one that did not keeps its last
// verdicts, and the next pass will say what went wrong.
func (d *Daemon) readActivity(ctx context.Context) bool {
	d.turn.Lock()
	defer d.turn.Unlock()
	return d.readActivityHoldingTheTurn(ctx)
}

func (d *Daemon) readActivityHoldingTheTurn(ctx context.Context) bool {
	conntrack := d.dumpConntrack(ctx)
	answered := true
	for _, s := range d.sources {
		activity, err := s.Activity(ctx, conntrack)
		if err != nil {
			answered = false
			continue
		}
		d.mu.Lock()
		seen := d.verdictsOf(s.Name())
		seen.busy = map[string]bool{}
		order := make([]string, 0, len(activity))
		for _, a := range activity {
			if _, dup := seen.busy[a.Workload]; !dup {
				order = append(order, a.Workload)
			}
			seen.busy[a.Workload] = a.Busy
		}
		// A workload the tick did not report is still listed while the last
		// pass knew it, so a reboot keeps waiting on it.
		for _, workload := range seen.order {
			if _, reported := seen.busy[workload]; !reported && hasKey(seen.settled, workload) {
				order = append(order, workload)
			}
		}
		seen.order = order
		d.mu.Unlock()
	}
	return answered
}

// record remembers this pass's verdicts for a source.
//
// A workload the source no longer reports is gone and drops out; one it
// reports without a busy verdict (never sampled this pass) keeps its last one,
// and counts as unsettled.
func (d *Daemon) record(name source.Name, outcomes []source.Outcome) {
	d.mu.Lock()
	defer d.mu.Unlock()
	seen := d.verdictsOf(name)
	previous := seen.busy
	next := &verdicts{busy: map[string]bool{}, settled: map[string]bool{}}
	for _, outcome := range outcomes {
		if !hasKey(next.settled, outcome.Workload) {
			next.order = append(next.order, outcome.Workload)
		}
		if outcome.Busy != nil {
			next.busy[outcome.Workload] = *outcome.Busy
		} else if busy, known := previous[outcome.Workload]; known {
			next.busy[outcome.Workload] = busy
		}
		next.settled[outcome.Workload] = outcome.Settled
	}
	d.seen[name] = next
}

func (d *Daemon) verdictsOf(name source.Name) *verdicts {
	seen, ok := d.seen[name]
	if !ok {
		seen = &verdicts{busy: map[string]bool{}, settled: map[string]bool{}}
		d.seen[name] = seen
	}
	return seen
}

// rebootVerdict remembers, and logs when it changes, where an automatic
// reboot stands.
func (d *Daemon) rebootVerdict(auto *AutoReboot, needed, verdict string) {
	line := fmt.Sprintf("Automatic reboot (%s): %s", auto.Label, verdict)
	if needed != "" {
		line = fmt.Sprintf("Automatic reboot (%s): needed, %s; %s", auto.Label, needed, verdict)
	}
	d.mu.Lock()
	d.reboot = &status.Reboot{Window: auto.Label, Needed: needed, Verdict: verdict}
	changed := line != d.lastRebootLine
	d.lastRebootLine = line
	d.mu.Unlock()
	if changed {
		d.log(line)
	}
}

// publish writes the status, one write at a time so a tick and a pass never
// interleave. Never fatal: logged once per distinct problem, since the tick
// would otherwise repeat it every few seconds.
func (d *Daemon) publish() {
	d.mu.Lock()
	defer d.mu.Unlock()
	err := d.status.Write(d.snapshot())
	if err == nil {
		d.lastStatusError = ""
		return
	}
	if message := err.Error(); message != d.lastStatusError {
		d.lastStatusError = message
		d.log(fmt.Sprintf("Warning: could not write the status (%s)", message))
	}
}

// dumpConntrack is the host table, or nil - "could not tell" - when it cannot
// be read, warned about once per distinct problem.
func (d *Daemon) dumpConntrack(ctx context.Context) []detect.ConntrackEntry {
	entries, err := d.conntrack.Dump(ctx)
	d.mu.Lock()
	defer d.mu.Unlock()
	if err != nil {
		if message := err.Error(); message != d.lastConntrackError {
			d.lastConntrackError = message
			d.log(fmt.Sprintf("Warning: could not read conntrack (%s); assuming busy where it matters", message))
		}
		return nil
	}
	d.lastConntrackError = ""
	if entries == nil {
		// An empty table is a dump that found no flows, which must never be
		// mistaken for the nil of one that failed.
		entries = []detect.ConntrackEntry{}
	}
	return entries
}

// noun is what the summary calls the things it counts.
func (d *Daemon) noun() string {
	names := map[source.Name]bool{}
	for _, s := range d.sources {
		names[s.Name()] = true
	}
	switch {
	case len(names) != 1:
		return "workload"
	case names[source.Docker]:
		return "container"
	}
	return "unit"
}

// summarise is one line saying what the pass did, logged only when it
// changes.
//
// "Nothing to do" is the normal, healthy state and worth saying once - a
// daemon that reconciles every five minutes should be quiet, not chatty.
func summarise(actions []decide.Action, noun string) string {
	count := func(kind decide.Kind) int {
		n := 0
		for _, action := range actions {
			if action.Kind == kind {
				n++
			}
		}
		return n
	}
	var changes []string
	for _, part := range []struct {
		name string
		kind decide.Kind
	}{{"updated", decide.Update}, {"deferred", decide.Defer}, {"pinned out", decide.Pinned}} {
		if n := count(part.kind); n > 0 {
			changes = append(changes, fmt.Sprintf("%d %s", n, part.name))
		}
	}
	kept := count(decide.Keep)
	if len(changes) == 0 {
		switch kept {
		case 0:
			return fmt.Sprintf("No %ss to manage", noun)
		case 1:
			return fmt.Sprintf("1 %s up to date, nothing to do", noun)
		}
		return fmt.Sprintf("%d %ss up to date, nothing to do", kept, noun)
	}
	return strings.Join(append(changes, fmt.Sprintf("%d up to date", kept)), ", ")
}

func sorted(set map[string]bool) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func hasKey(set map[string]bool, key string) bool {
	_, ok := set[key]
	return ok
}

// seconds is a duration as the usage gives it: "300", "0.5".
func seconds(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', -1, 64)
}

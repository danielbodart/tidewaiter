package systemd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/danielbodart/gokit/clock"
	"github.com/danielbodart/gokit/sockdiag"
	"github.com/danielbodart/tidewaiter/internal/decide"
	"github.com/danielbodart/tidewaiter/internal/detect"
	"github.com/danielbodart/tidewaiter/internal/health"
	"github.com/danielbodart/tidewaiter/internal/model"
	"github.com/danielbodart/tidewaiter/internal/nix"
	"github.com/danielbodart/tidewaiter/internal/source"
	"github.com/danielbodart/tidewaiter/internal/statestore"
)

// Options are the source's own settings.
type Options struct {
	// Report what would change without changing it.
	DryRun bool
	// How often to re-check health while waiting.
	HealthPoll time.Duration
}

// Deps is everything the source talks to, each behind an interface so tests
// replace the far ends.
type Deps struct {
	Units    UnitsDirectory
	Bus      Bus
	Profiles nix.Profiles
	Closure  nix.ClosureBinder
	// The host's listening sockets, for port-bound and uptime by cgroup.
	Sockets sockdiag.Lister
	// For the re-sample right before a swap; the pass's own dump arrives in
	// source.Pass.
	Conntrack detect.ConntrackDumper
	Store     statestore.Store
	Netns     detect.Netns
	TCP       health.Prober
	Log       source.Log
	Clock     clock.Clock
}

// Source is systemd units whose program lives in a Nix profile, kept on the
// store path their units file asks for.
//
// Per unit, per pass: read the policy, the unit and the profile; a stopped
// unit just has its profile moved for whoever starts it next; a running one
// is sampled for activity, judged by the same decide.Decide as a container,
// and once idle for long enough swapped - mark it held, look once more for
// players, bind the new closure, `nix-env --set`, restart (the unit's own
// ExecStop saves and quits), health-gate, and on failure switch the profile
// back to the generation it left, restart again and pin the version out.
//
// Pins, the last good generation and a swap under way survive a restart of
// Tidewaiter (see statestore.UnitMemory), each saved the moment it changes;
// idle streaks do not.
//
// Not safe for concurrent use: the daemon calls one source method at a time.
type Source struct {
	deps    Deps
	options Options

	// Loaded on the first pass; nil until then.
	memories statestore.Memories
	// The memories as last saved, so a pass that changed nothing writes
	// nothing.
	saved   string
	streaks map[string]int
	// Units looked at since this daemon started, so crash recovery runs once
	// per unit.
	seen map[string]bool
	// The text last warned about per file, and the note last given per unit
	// and condition, so a pass every few minutes repeats nothing.
	files map[string]string
	notes map[string]string
}

var _ source.Source = (*Source)(nil)

// New is the systemd source.
func New(deps Deps, options Options) *Source {
	return &Source{
		deps:    deps,
		options: options,
		streaks: map[string]int{},
		seen:    map[string]bool{},
		files:   map[string]string{},
		notes:   map[string]string{},
	}
}

func (s *Source) Name() source.Name { return source.Systemd }

func (s *Source) log(format string, args ...any) { s.deps.Log(fmt.Sprintf(format, args...)) }

func (s *Source) Once(ctx context.Context, pass source.Pass) ([]source.Outcome, error) {
	if err := s.load(); err != nil {
		return nil, err
	}
	policies, err := s.policies()
	if err != nil {
		return nil, err
	}
	var outcomes []source.Outcome
	for _, policy := range policies {
		outcome, err := s.reconcile(ctx, policy, pass)
		if err != nil {
			s.log("%s: Error: %s", policy.Unit, err)
			outcome = source.Outcome{Workload: policy.Unit, Action: decide.Action{Kind: decide.Keep, Reason: "errored"}}
		}
		outcomes = append(outcomes, outcome)
	}
	s.save()
	return outcomes, nil
}

func (s *Source) Activity(ctx context.Context, conntrack []detect.ConntrackEntry) ([]source.Activity, error) {
	policies, err := s.policies()
	if err != nil {
		return nil, err
	}
	var activities []source.Activity
	for _, policy := range policies {
		state, err := s.deps.Bus.Unit(ctx, policy.Unit)
		switch {
		case err != nil:
			activities = append(activities, source.Activity{Workload: policy.Unit, Busy: true})
		case state.LoadState == "not-found" || IsStopped(state):
			// Nothing running, nobody playing.
			activities = append(activities, source.Activity{Workload: policy.Unit})
		default:
			busy := s.sample(ctx, policy, state, conntrack, true).InUse
			activities = append(activities, source.Activity{Workload: policy.Unit, Busy: busy})
		}
	}
	return activities, nil
}

func (s *Source) List(ctx context.Context) ([]string, error) {
	if err := s.load(); err != nil {
		return nil, err
	}
	policies, err := s.policies()
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, policy := range policies {
		current, err := s.deps.Profiles.Current(ctx, policy.Profile)
		if err != nil {
			current = ""
		}
		var status string
		switch {
		case s.memory(policy.Unit).Pinned == policy.Desired:
			status = "pinned out"
		case current == "":
			status = "no profile yet"
		case current == policy.Desired:
			status = "up to date"
		default:
			status = "update waiting"
		}
		shown := current
		if shown == "" {
			shown = "nothing"
		}
		lines = append(lines, fmt.Sprintf("%s  %s -> %s  [%s]  desired=%s detector=%s health=%s idle-samples=%d",
			policy.Unit, policy.Profile, shown, status, policy.Desired, policy.Detector, strings.Join(policy.Health, ","), policy.IdleSamples))
	}
	return lines, nil
}

// policies is every valid policy in the units directory, one per unit.
//
// Warnings are logged once per distinct file contents rather than every pass.
// A second file declaring a unit already declared is skipped: two profiles
// for one unit is a mistake to fix, not a choice to guess at.
func (s *Source) policies() ([]*Policy, error) {
	files, err := s.deps.Units.Read()
	if err != nil {
		return nil, err
	}
	var policies []*Policy
	declared := map[string]bool{}
	for _, file := range files {
		policy, warnings := ParsePolicy(file.Text, file.Name)
		if said, ok := s.files[file.Name]; !ok || said != string(file.Text) {
			for _, warning := range warnings {
				s.deps.Log(warning)
			}
			if policy != nil && declared[policy.Unit] {
				s.log("%s: %s is already declared by another file; skipping this one", file.Name, policy.Unit)
			}
			s.files[file.Name] = string(file.Text)
		}
		if policy != nil && !declared[policy.Unit] {
			declared[policy.Unit] = true
			policies = append(policies, policy)
		}
	}
	return policies, nil
}

func unsampled(unit string, action decide.Action) source.Outcome {
	return source.Outcome{Workload: unit, Action: action}
}

func sampled(unit string, action decide.Action, busy, settled bool) source.Outcome {
	return source.Outcome{Workload: unit, Action: action, Busy: source.Sampled(busy), Settled: settled}
}

func keep(reason string) decide.Action { return decide.Action{Kind: decide.Keep, Reason: reason} }

func (s *Source) reconcile(ctx context.Context, policy *Policy, pass source.Pass) (source.Outcome, error) {
	unit, desired := policy.Unit, policy.Desired

	state, err := s.deps.Bus.Unit(ctx, unit)
	if err != nil {
		s.log("%s: could not read the unit from systemd (%s); leaving it alone", unit, err)
		return unsampled(unit, keep("systemd unreachable")), nil
	}
	if state.LoadState == "not-found" {
		s.note(unit, "not-found", fmt.Sprintf("%s: systemd does not know this unit; leaving it alone", unit))
		// Settled: a unit with nothing running has nobody to wait for.
		return sampled(unit, keep("no such unit"), false, true), nil
	}

	current, generation, readable := s.readProfile(ctx, policy)
	if !readable {
		return unsampled(unit, keep("profile unreadable")), nil
	}

	// A pin, and a run of swap failures, only hold until desired moves on.
	memory := s.memory(unit)
	if memory.Pinned != "" && memory.Pinned != desired {
		memory.Pinned = ""
	}
	if memory.Failing != "" && memory.Failing != desired {
		memory.Failing, memory.SwapFailures = "", 0
	}
	s.remember(unit, memory)

	if !s.seen[unit] {
		s.seen[unit] = true
		recovered, err := s.firstSight(ctx, policy, state, current, generation, pass)
		if err != nil {
			return source.Outcome{}, err
		}
		// Something was just finished or undone: not settled until a pass
		// has looked again.
		if recovered != nil {
			return sampled(unit, *recovered, false, false), nil
		}
		// Recovery may have moved the profile back, or gated what it points at.
		if state, err = s.deps.Bus.Unit(ctx, unit); err != nil {
			return source.Outcome{}, err
		}
		if current, generation, readable = s.readProfile(ctx, policy); !readable {
			return unsampled(unit, keep("profile unreadable")), nil
		}
	}

	if IsStopped(state) {
		return sampled(unit, s.updateStopped(ctx, policy, current), false, true), nil
	}

	activity := s.sample(ctx, policy, state, pass.Conntrack, false)
	transitional := !IsUp(state)
	// A unit on its way up or down is neither idle nor a candidate: count
	// nothing until it settles.
	idleStreak := 0
	if !activity.InUse && !transitional {
		idleStreak = s.streaks[unit] + 1
	}
	s.streaks[unit] = idleStreak
	settled := !activity.InUse && idleStreak >= policy.IdleSamples

	if transitional && current != desired && s.memory(unit).Pinned != desired {
		action := decide.Action{Kind: decide.Defer, Reason: fmt.Sprintf("the unit is %s; waiting for it to settle", state.ActiveState)}
		s.log("%s: %s", unit, action.Reason)
		return sampled(unit, action, activity.InUse, false), nil
	}

	action := decide.Decide(decide.Inputs{
		IdleSamples: policy.IdleSamples,
		Current:     model.Digest(current),
		Desired:     model.Digest(desired),
		InUse:       activity.InUse,
		IdleStreak:  idleStreak,
	}, decide.State{Pinned: model.Digest(s.memory(unit).Pinned)}, decide.UnitWords)

	switch action.Kind {
	case decide.Defer:
		s.log("%s: %s", unit, action.Reason)
	case decide.Update:
		s.log("%s: updating - %s", unit, action.Reason)
		if s.options.DryRun {
			s.log("%s: dry run, not touching it", unit)
			break
		}
		if pass.Hold != nil {
			pass.Hold(unit)
		}
		swapped, err := s.swap(ctx, policy, state, current, generation)
		if err != nil {
			return source.Outcome{}, err
		}
		if swapped != nil {
			action = *swapped
		}
		// Just restarted, or someone arrived: settled again only once a pass
		// says so.
		return sampled(unit, action, false, false), nil
	}
	return sampled(unit, action, activity.InUse, settled), nil
}

// readProfile is the profile's target and generation, or false - having said
// why - when it cannot be read.
func (s *Source) readProfile(ctx context.Context, policy *Policy) (current string, generation int, ok bool) {
	current, err := s.deps.Profiles.Current(ctx, policy.Profile)
	if err == nil {
		generation, err = s.deps.Profiles.Generation(ctx, policy.Profile)
	}
	if err != nil {
		s.log("%s: could not read the profile %s (%s); leaving it alone", policy.Unit, policy.Profile, err)
		return "", 0, false
	}
	return current, generation, true
}

// updateStopped moves a stopped unit's profile. A stopped unit has nobody on
// it and is not Tidewaiter's to start: its profile is moved, so whoever starts
// it next starts the desired version, with no restart and so no gate. A
// pinned version is still not moved to.
func (s *Source) updateStopped(ctx context.Context, policy *Policy, current string) decide.Action {
	unit, desired, profile := policy.Unit, policy.Desired, policy.Profile
	if current == desired {
		return keep(decide.UnitWords.Keep)
	}
	if s.memory(unit).Pinned == desired {
		return decide.Action{Kind: decide.Pinned, Reason: decide.UnitWords.Pinned}
	}

	action := decide.Action{Kind: decide.Update, From: model.Digest(current), To: model.Digest(desired), Reason: "the unit is stopped"}
	if s.options.DryRun {
		s.log("%s: stopped; would move its profile to %s (dry run)", unit, desired)
		return action
	}
	err := func() error {
		if policy.BindClosure {
			if err := s.deps.Closure.Bind(ctx, unit, profile, desired); err != nil {
				return err
			}
		}
		if err := s.deps.Profiles.Set(ctx, profile, desired); err != nil {
			return err
		}
		if policy.BindClosure {
			return s.deps.Bus.DaemonReload(ctx)
		}
		return nil
	}()
	if err != nil {
		s.log("%s: could not move the stopped unit's profile (%s)", unit, err)
		return keep("profile update failed")
	}
	s.log("%s: updated the profile of a stopped unit to %s; it runs that when next started", unit, desired)
	return action
}

// swap is the swap. It returns the action that actually happened when it
// differs from the update decided on - a defer when someone arrived at the
// last moment.
func (s *Source) swap(ctx context.Context, policy *Policy, before UnitState, current string, previous int) (*decide.Action, error) {
	unit, desired, profile := policy.Unit, policy.Desired, policy.Profile

	// The idle verdict can be a whole pass old by now. Look once more, right
	// before anything changes, and back off if someone has just joined.
	if fresh := s.sample(ctx, policy, before, s.dumpConntrack(ctx), false); fresh.InUse {
		s.streaks[unit] = 0
		action := decide.Action{Kind: decide.Defer, Reason: "someone arrived just before the swap"}
		s.log("%s: %s; deferring", unit, action.Reason)
		return &action, nil
	}

	// Recorded, durably, before anything changes: a Tidewaiter that dies from
	// here until the gate's verdict must not mistake the profile's new
	// generation for one that is known good (see resumeSwap).
	memory := s.memory(unit)
	memory.Swapping = &statestore.SwapRecord{From: generationRef(previous), FromTarget: current, To: desired, InvocationID: before.InvocationID}
	s.remember(unit, memory)
	if !s.save() {
		memory.Swapping = nil
		s.remember(unit, memory)
		return &decide.Action{Kind: decide.Defer, Reason: "could not record the swap before starting it"}, nil
	}

	var bound, set bool
	var job JobResult
	err := func() (err error) {
		if policy.BindClosure {
			if err := s.deps.Closure.Bind(ctx, unit, profile, desired); err != nil {
				return err
			}
			bound = true
			if err := s.deps.Bus.DaemonReload(ctx); err != nil {
				return err
			}
		}
		if err := s.deps.Profiles.Set(ctx, profile, desired); err != nil {
			return err
		}
		set = true
		job, err = s.deps.Bus.Restart(ctx, unit)
		return err
	}()
	if err != nil {
		s.log("%s: FAILED during swap (%s); undoing what was done", unit, err)
		s.undo(ctx, policy, before, current, previous, bound, set)
		memory := s.memory(unit)
		memory.Swapping = nil
		s.remember(unit, memory)
		s.countFailure(unit, desired)
		s.save()
		return nil, nil
	}

	verdict, err := s.gate(ctx, policy, job)
	if err != nil {
		return nil, err
	}
	if verdict.Healthy {
		good, err := s.deps.Profiles.Generation(ctx, profile)
		if err != nil {
			good = 0
		}
		if err := s.deps.Profiles.Prune(ctx, profile, policy.KeepGenerations); err != nil {
			s.log("%s: updated, but pruning old generations failed (%s)", unit, err)
		}
		s.remember(unit, statestore.UnitMemory{Good: generationRef(good)})
		s.save()
		s.log("%s: updated to %s and it is healthy", unit, desired)
		return nil, nil
	}

	s.log("%s: the new version is unhealthy (%s); rolling back", unit, verdict.Reason)
	s.rollBack(ctx, policy, previous, current)
	memory = s.memory(unit)
	memory.Pinned, memory.Failing, memory.SwapFailures, memory.Swapping = desired, "", 0, nil
	s.remember(unit, memory)
	s.save()
	return nil, nil
}

// undo undoes a swap that failed before its gate, given how far it got: the
// profile back to the generation it left, the closure back to what that
// generation needs - and, only if the unit did restart regardless (the bus
// dropped after the job was queued), a restart back onto it.
func (s *Source) undo(ctx context.Context, policy *Policy, before UnitState, current string, previous int, bound, set bool) {
	unit, profile := policy.Unit, policy.Profile
	err := func() error {
		if set && previous != 0 {
			if err := s.switchBack(ctx, profile, previous); err != nil {
				return err
			}
		}
		if bound && current != "" {
			if err := s.deps.Closure.Bind(ctx, unit, profile, current); err != nil {
				return err
			}
			if err := s.deps.Bus.DaemonReload(ctx); err != nil {
				return err
			}
		}
		if set {
			after, err := s.deps.Bus.Unit(ctx, unit)
			if err != nil {
				return err
			}
			if after.InvocationID != before.InvocationID {
				_, err := s.deps.Bus.Restart(ctx, unit)
				return err
			}
		}
		return nil
	}()
	if err != nil {
		s.log("%s: undoing the swap itself failed (%s); manual attention needed", unit, err)
	}
}

// rollBack puts the unit back on the generation it ran before, restarts it
// and gates it again. Never retried: a rollback that fails is logged for a
// person, not looped on.
func (s *Source) rollBack(ctx context.Context, policy *Policy, previous int, previousTarget string) {
	unit, desired, profile := policy.Unit, policy.Desired, policy.Profile
	if previous == 0 || previousTarget == "" {
		s.log("%s: there is no earlier generation of %s to roll back to; manual attention needed", unit, profile)
		return
	}
	verdict, err := func() (health.Verdict, error) {
		if err := s.switchBack(ctx, profile, previous); err != nil {
			return health.Verdict{}, err
		}
		if policy.BindClosure {
			if err := s.deps.Closure.Bind(ctx, unit, profile, previousTarget); err != nil {
				return health.Verdict{}, err
			}
			if err := s.deps.Bus.DaemonReload(ctx); err != nil {
				return health.Verdict{}, err
			}
		}
		job, err := s.deps.Bus.Restart(ctx, unit)
		if err != nil {
			return health.Verdict{}, err
		}
		return s.gate(ctx, policy, job)
	}()
	switch {
	case err != nil:
		s.log("%s: rollback itself failed (%s); manual attention needed", unit, err)
	case verdict.Healthy:
		s.log("%s: rolled back to %s and pinned %s out until the desired version moves on", unit, previousTarget, desired)
	default:
		s.log("%s: rolled back to %s, but it is not healthy either (%s); manual attention needed", unit, previousTarget, verdict.Reason)
	}
}

// switchBack switches the profile to generation and deletes the one it
// leaves, so the newest generations stay the good ones for prune's `+N`, and
// the failed version is not offered by `nix-env --rollback` to someone later.
func (s *Source) switchBack(ctx context.Context, profile string, generation int) error {
	failed, err := s.deps.Profiles.Generation(ctx, profile)
	if err != nil {
		return err
	}
	if failed == generation {
		return nil
	}
	if err := s.deps.Profiles.SwitchGeneration(ctx, profile, generation); err != nil {
		return err
	}
	if failed != 0 {
		if err := s.deps.Profiles.DeleteGeneration(ctx, profile, failed); err != nil {
			s.log("%s: could not delete generation %d (%s)", profile, failed, err)
		}
	}
	return nil
}

// countFailure counts a swap-step failure against desired, pinning it out
// after source.MaxSwapFailures in a row, as the Docker source does.
func (s *Source) countFailure(unit, desired string) {
	memory := s.memory(unit)
	failures := 1
	if memory.Failing == desired {
		failures = memory.SwapFailures + 1
	}
	memory.Failing, memory.SwapFailures = desired, failures
	if failures >= source.MaxSwapFailures {
		s.log("%s: swap has failed %d times running for %s; pinning it out until the desired version moves on", unit, failures, desired)
		memory.Pinned = desired
	}
	s.remember(unit, memory)
}

// firstSight is the first look at a unit since this daemon started.
//
// A swap recorded as under way is finished or undone by resumeSwap. Without
// one, a unit that is up is adopted as it is, its generation recorded as good
// - someone moved the profile by hand, or this is the first sight of it. A
// unit that has failed on a generation newer than the last one recorded good
// says "something went wrong and nobody cleaned up", so that - and only that
// - is rolled back, and the version it failed on pinned out.
func (s *Source) firstSight(ctx context.Context, policy *Policy, state UnitState, current string, generation int, pass source.Pass) (*decide.Action, error) {
	unit, profile := policy.Unit, policy.Profile
	memory := s.memory(unit)
	if memory.Swapping != nil {
		return s.resumeSwap(ctx, policy, *memory.Swapping, state, generation, pass)
	}
	if generation == 0 {
		return nil, nil
	}
	good := generationOf(memory.Good)

	if IsUp(state) {
		if good != 0 && good != generation {
			s.log("%s: adopting generation %d of %s as the baseline (no record of it going in)", unit, generation, profile)
		}
		if good != generation {
			memory.Good = generationRef(generation)
			s.remember(unit, memory)
			s.save()
		}
		return nil, nil
	}

	if state.ActiveState != "failed" || good == 0 || generation <= good || s.options.DryRun {
		return nil, nil
	}

	s.log("%s: failed on generation %d of %s, newer than the last good one (%d) - a swap was interrupted; rolling back",
		unit, generation, profile, good)
	if pass.Hold != nil {
		pass.Hold(unit)
	}
	goodTarget, err := s.deps.Profiles.Target(ctx, profile, good)
	if err != nil {
		goodTarget = ""
	}
	s.rollBack(ctx, policy, good, goodTarget)
	if current != "" {
		memory := s.memory(unit)
		memory.Pinned = current
		s.remember(unit, memory)
		s.save()
	}
	return &decide.Action{Kind: decide.Pinned, Reason: "rolled back an interrupted swap"}, nil
}

// resumeSwap picks up a swap Tidewaiter died in the middle of, by whether
// the unit restarted since the swap began (its invocation id moved on):
//
//   - It did not. The unit still runs the version the swap left, whatever the
//     profile and the closure drop-in now say. Both are put back - without a
//     restart, players may be on - and the ordinary idle-gated path swaps
//     again.
//   - It did, and is failed. Rolled back to where the swap started, and the
//     version pinned out, as a failed gate would have.
//   - It did, and is up. The restart happened but its gate never ran: it is
//     gated now, committed if healthy, rolled back and pinned if not.
//   - It did, and is stopped. Someone stopped it since; nothing runs, so the
//     record is dropped and the stopped-unit path moves the profile.
//
// Only once the record is cleared is a generation ever adopted as good.
func (s *Source) resumeSwap(ctx context.Context, policy *Policy, record statestore.SwapRecord, state UnitState, generation int, pass source.Pass) (*decide.Action, error) {
	unit, profile := policy.Unit, policy.Profile
	from, to := generationOf(record.From), record.To
	finish := func(change func(*statestore.UnitMemory)) {
		memory := s.memory(unit)
		change(&memory)
		memory.Swapping = nil
		s.remember(unit, memory)
		s.save()
	}
	if s.options.DryRun {
		s.log("%s: a swap to %s was interrupted; leaving it as it is (dry run)", unit, to)
		action := keep("interrupted swap left alone in a dry run")
		return &action, nil
	}

	notRestarted := state.InvocationID == record.InvocationID
	if notRestarted || IsStopped(state) && state.ActiveState != "failed" {
		if notRestarted {
			s.log("%s: a swap to %s was interrupted before the restart; putting %s back", unit, to, profile)
		}
		err := func() error {
			if from != 0 && generation != 0 && generation != from {
				if err := s.switchBack(ctx, profile, from); err != nil {
					return err
				}
			}
			if policy.BindClosure && record.FromTarget != "" {
				if err := s.deps.Closure.Bind(ctx, unit, profile, record.FromTarget); err != nil {
					return err
				}
				return s.deps.Bus.DaemonReload(ctx)
			}
			return nil
		}()
		if err != nil {
			s.log("%s: could not put %s back (%s); trying again next pass", unit, profile, err)
			delete(s.seen, unit)
			action := keep("interrupted swap not yet undone")
			return &action, nil
		}
		finish(func(*statestore.UnitMemory) {})
		return nil, nil
	}

	if pass.Hold != nil {
		pass.Hold(unit)
	}
	if state.ActiveState != "failed" && (from == 0 || generation != from) {
		s.log("%s: a swap to %s was interrupted after the restart; gating it now", unit, to)
		verdict, err := s.gate(ctx, policy, JobDone)
		if err != nil {
			return nil, err
		}
		if verdict.Healthy {
			if err := s.deps.Profiles.Prune(ctx, profile, policy.KeepGenerations); err != nil {
				s.log("%s: updated, but pruning old generations failed (%s)", unit, err)
			}
			s.log("%s: updated to %s and it is healthy", unit, to)
			finish(func(memory *statestore.UnitMemory) {
				memory.Good, memory.Pinned, memory.Failing, memory.SwapFailures = generationRef(generation), "", "", 0
			})
			return nil, nil
		}
		s.log("%s: the new version is unhealthy (%s); rolling back", unit, verdict.Reason)
	} else {
		s.log("%s: a swap to %s was interrupted and the unit is %s; rolling back", unit, to, state.ActiveState)
	}
	s.rollBack(ctx, policy, from, record.FromTarget)
	finish(func(memory *statestore.UnitMemory) {
		memory.Pinned, memory.Failing, memory.SwapFailures = to, "", 0
	})
	return &decide.Action{Kind: decide.Pinned, Reason: "rolled back an interrupted swap"}, nil
}

// gate polls the policy's checks on the restart until they settle.
//
// A job that did not end done is unhealthy at once, whatever the checks - for
// a Type=notify unit it means the program never said it was up.
func (s *Source) gate(ctx context.Context, policy *Policy, job JobResult) (health.Verdict, error) {
	if job != JobDone {
		return health.Verdict{Reason: fmt.Sprintf("the restart job ended '%s'", job)}, nil
	}

	unit := policy.Unit
	baseline, err := s.deps.Bus.Unit(ctx, unit)
	if err != nil {
		return health.Verdict{}, err
	}
	latest := baseline
	grace := health.UptimeSeconds(policy.HealthTimeoutSeconds)
	var checks []health.Check
	for _, name := range policy.Health {
		switch name {
		case CheckSystemd:
			checks = append(checks, check{job: job, baseline: baseline, latest: &latest})
		case CheckUptime:
			checks = append(checks, health.Uptime(grace, s.deps.Clock))
		case CheckPortConnect:
			checks = append(checks, health.PortConnect(grace, s.deps.Clock))
		case CheckPortBound:
			checks = append(checks, health.PortBound)
		}
	}

	evaluate := func(ctx context.Context) ([]health.Result, error) {
		state, err := s.deps.Bus.Unit(ctx, unit)
		if err != nil {
			return nil, err
		}
		latest = state
		// One listing per evaluation, however many checks read it.
		var listing []sockdiag.Listener
		var listErr error
		listed := false
		list := func(ctx context.Context) ([]sockdiag.Listener, error) {
			if !listed {
				listing, listErr = s.deps.Sockets.Listening(ctx)
				listed = true
			}
			return listing, listErr
		}
		input := health.Input{
			Subject: health.Subject{
				// The crash signals feed Running too, so uptime fails a unit
				// that Restart= has quietly brought back, as it fails a
				// crash-looping container.
				Running:     IsUp(latest) && !RestartedSince(baseline, latest),
				Health:      model.NoHealth,
				NetworkMode: "host",
			},
			Ports: policy.Ports,
			Bound: health.CgroupBoundPorts(list, latest.ControlGroup),
			TCP:   s.deps.TCP,
		}
		results := make([]health.Result, 0, len(checks))
		for _, check := range checks {
			results = append(results, check.Evaluate(ctx, input))
		}
		return results, nil
	}

	timing := health.Timing{Timeout: time.Duration(policy.HealthTimeoutSeconds) * time.Second, Poll: s.options.HealthPoll}
	verdict, err := health.Gate(ctx, s.deps.Clock, timing, evaluate)
	if err != nil {
		return health.Verdict{Reason: fmt.Sprintf("the health gate was aborted (%s)", err)}, nil
	}
	return verdict, nil
}

// sample samples activity, fail-safe: anything that cannot tell reads as
// busy.
//
// quiet is for the activity tick, which runs every few seconds and must not
// repeat the pass's warnings.
func (s *Source) sample(ctx context.Context, policy *Policy, state UnitState, conntrack []detect.ConntrackEntry, quiet bool) detect.Result {
	unit := policy.Unit
	if policy.Detector == "none" {
		return detect.Result{}
	}
	if len(policy.Ports) == 0 {
		if !quiet {
			s.note(unit, "no-ports", fmt.Sprintf("%s: declares no ports, so its activity cannot be read; assuming busy", unit))
		}
		return detect.Busy
	}
	if policy.Detector == "conntrack" && conntrack == nil {
		return detect.Busy
	}
	if policy.Detector == "tcp" && state.MainPID == 0 {
		return detect.Busy
	}
	detector, known := detect.ByName(policy.Detector)
	if !known {
		return detect.Busy
	}
	result, err := detector.InUse(ctx, detect.Input{
		ID:        unit,
		PID:       state.MainPID,
		Published: policy.Ports,
		Conntrack: conntrack,
		Netns:     s.deps.Netns,
	})
	if err != nil {
		if !quiet {
			s.log("%s: activity detector '%s' failed (%s); assuming busy", unit, policy.Detector, err)
		}
		return detect.Busy
	}
	return detect.Combine(result)
}

// dumpConntrack is the table right now, or nil when it could not be read.
func (s *Source) dumpConntrack(ctx context.Context) []detect.ConntrackEntry {
	entries, err := s.deps.Conntrack.Dump(ctx)
	if err != nil {
		return nil
	}
	// A dump that worked is a table, however empty: nil is "could not tell".
	if entries == nil {
		entries = []detect.ConntrackEntry{}
	}
	return entries
}

// note logs a standing condition once, until it changes.
func (s *Source) note(unit, key, message string) {
	id := unit + "#" + key
	if s.notes[id] == message {
		return
	}
	s.notes[id] = message
	s.deps.Log(message)
}

func (s *Source) memory(unit string) statestore.UnitMemory {
	return s.memories[unit]
}

func (s *Source) remember(unit string, memory statestore.UnitMemory) {
	if memory == (statestore.UnitMemory{}) {
		delete(s.memories, unit)
		return
	}
	s.memories[unit] = memory
}

func (s *Source) load() error {
	if s.memories != nil {
		return nil
	}
	memories, err := s.deps.Store.Load()
	if err != nil {
		return fmt.Errorf("reading the systemd source's state: %w", err)
	}
	if memories == nil {
		memories = statestore.Memories{}
	}
	s.memories = memories
	s.saved = encode(memories)
	return nil
}

// save persists the memories, only when they changed since the last save.
// Called straight after every change that matters for safety, and once at
// the end of a pass. False when the write failed.
//
// A dry run keeps what it learns for the rest of its own passes but never
// writes it: the state file is the real daemon's, and a baseline adopted by a
// look-only run would be one the daemon never chose - or, as a user without
// write access to /var/lib/tidewaiter, a warning about a write nobody asked for.
func (s *Source) save() bool {
	text := encode(s.memories)
	if text == s.saved || s.options.DryRun {
		return true
	}
	if err := s.deps.Store.Save(s.copy()); err != nil {
		s.log("Warning: could not save the systemd source's state (%s)", err)
		return false
	}
	s.saved = text
	return true
}

// copy is the memories as a store may keep: nothing it holds is changed by
// a later pass.
func (s *Source) copy() statestore.Memories {
	memories := make(statestore.Memories, len(s.memories))
	for unit, memory := range s.memories {
		memories[unit] = memory
	}
	return memories
}

// encode is the memories as JSON, map keys sorted, to compare by value.
func encode(memories statestore.Memories) string {
	text, _ := json.Marshal(memories)
	return string(text)
}

// generationRef is a generation as the store keeps it: absent for none.
func generationRef(generation int) *int {
	if generation == 0 {
		return nil
	}
	return &generation
}

// generationOf is the store's generation, 0 for none.
func generationOf(generation *int) int {
	if generation == nil {
		return 0
	}
	return *generation
}

// Package systemd is the systemd source: units whose program lives in a Nix
// profile, kept on the store path their declaration in the units directory
// asks for, swapped only when nobody is on them, health-gated, and rolled
// back on failure. It also has what a unit swap needs of systemd - read a
// unit, restart it and hear its job end, reload - over the system bus, and
// logind's reboot, and `tidewaiter prepare`.
package systemd

import (
	"context"
	"fmt"

	"github.com/danielbodart/tidewaiter/internal/health"
)

// UnitState is what Tidewaiter reads about a unit, from
// org.freedesktop.systemd1.Unit and .Service.
type UnitState struct {
	Name string
	// "loaded", "not-found", "masked", ...
	LoadState string
	// "active", "activating", "deactivating", "inactive", "failed", "reloading".
	ActiveState string
	// "running", "start-pre", "dead", ...
	SubState string
	// Service.Result: "success", "exit-code", "signal", "timeout", ...
	Result string
	// Service.NRestarts: automatic restarts under Restart=, since the last
	// manual start.
	NRestarts int
	// Service.MainPID, 0 when there is none.
	MainPID int
	// Service.ControlGroup, "" when the unit is not running.
	ControlGroup string
	// Unit.InvocationID, hex; a new one on every start, automatic restarts
	// included.
	InvocationID string
}

// JobResult is how a systemd job ended, as the JobRemoved signal reports it.
type JobResult string

const (
	JobDone       JobResult = "done"
	JobCanceled   JobResult = "canceled"
	JobTimeout    JobResult = "timeout"
	JobFailed     JobResult = "failed"
	JobDependency JobResult = "dependency"
	JobSkipped    JobResult = "skipped"
)

var jobResults = []JobResult{JobDone, JobCanceled, JobTimeout, JobFailed, JobDependency, JobSkipped}

// Bus is the part of systemd's Manager Tidewaiter uses.
//
// An interface, so the source is tested against units in memory and the real
// one (DBus) talks to the system manager.
type Bus interface {
	// Unit is a unit's state. A name with no unit file is a unit whose
	// LoadState is "not-found", not an error.
	Unit(ctx context.Context, name string) (UnitState, error)
	// Restart is RestartUnit(name, "replace"), waiting for the job to finish.
	//
	// No timeout of its own: the unit's TimeoutStopSec and TimeoutStartSec
	// bound it, and they are the unit's to choose - a game server saving a
	// large world on the way down may rightly take minutes. An error only when
	// the job could not be queued, or its end could not be heard; a job that
	// ran and failed is its result.
	Restart(ctx context.Context, name string) (JobResult, error)
	// DaemonReload returns once the reload is done, so a drop-in just written
	// is in force.
	DaemonReload(ctx context.Context) error
}

// IsUp is whether a unit is up in a way that could be serving: active, or
// reloading while active.
func IsUp(state UnitState) bool {
	return state.ActiveState == "active" || state.ActiveState == "reloading"
}

// IsStopped is whether nothing of the unit is running: stopped, cleanly or not.
func IsStopped(state UnitState) bool {
	return state.ActiveState == "inactive" || state.ActiveState == "failed"
}

// RestartedSince is whether the unit has crashed and been brought back since
// baseline - the state read just after Tidewaiter's restart job finished.
//
// Restart= hides a crash from ActiveState: by the next poll the unit can be
// "active" again. But every start gets a new InvocationID and every automatic
// restart bumps NRestarts, so either moving means the version we started is
// not the process running now.
func RestartedSince(baseline, state UnitState) bool {
	return state.InvocationID != baseline.InvocationID || state.NRestarts > baseline.NRestarts
}

// Verdict is the `systemd` health check: what systemd itself says about the
// restart.
//
//	fail          - the restart job did not end done (for a Type=notify unit,
//	                the program never said READY=1 before TimeoutStartSec);
//	                the unit is failed, inactive or deactivating; or it crashed
//	                and Restart= brought it back since the job finished.
//	pass          - active (or reloading) on the invocation the job started.
//	inconclusive  - still activating.
//
// Pure, over the job's result, the state just after it and the state now, so
// every combination is a table test.
func Verdict(job JobResult, baseline, state UnitState) health.Result {
	if job != JobDone {
		return health.Failed(fmt.Sprintf("the restart job ended '%s'", job))
	}
	if RestartedSince(baseline, state) {
		return health.Failed("the unit crashed and was restarted since it came up")
	}
	switch state.ActiveState {
	case "active", "reloading":
		return health.Passed
	case "activating":
		return health.Waiting
	}
	reason := "the unit is " + state.ActiveState
	if state.Result != "" && state.Result != "success" {
		reason += " (" + state.Result + ")"
	}
	return health.Failed(reason)
}

// check is Verdict as a health.Check, over the state the gate last read.
type check struct {
	job      JobResult
	baseline UnitState
	latest   *UnitState
}

func (check) Name() string { return "systemd" }

func (c check) Evaluate(context.Context, health.Input) health.Result {
	return Verdict(c.job, c.baseline, *c.latest)
}

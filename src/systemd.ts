import type { CheckResult } from "./health.ts";

/** What Tidewaiter reads about a unit, from org.freedesktop.systemd1.Unit and .Service. */
export interface UnitState {
  readonly name: string;
  /** "loaded", "not-found", "masked", ... */
  readonly loadState: string;
  /** "active", "activating", "deactivating", "inactive", "failed", "reloading". */
  readonly activeState: string;
  /** "running", "start-pre", "dead", ... */
  readonly subState: string;
  /** Service.Result: "success", "exit-code", "signal", "timeout", ... */
  readonly result: string;
  /** Service.NRestarts: automatic restarts under Restart=, since the last manual start. */
  readonly nRestarts: number;
  /** Service.MainPID, 0 when there is none. */
  readonly mainPid: number;
  /** Service.ControlGroup, "" when the unit is not running. */
  readonly controlGroup: string;
  /** Unit.InvocationID, hex; a new one on every start, automatic restarts included. */
  readonly invocationId: string;
}

/** How a systemd job ended, as the JobRemoved signal reports it. */
export type JobResult = "done" | "canceled" | "timeout" | "failed" | "dependency" | "skipped";

/**
 * The part of systemd's Manager Tidewaiter uses.
 *
 * An interface, so the systemd source is tested against an in-memory unit and
 * the real one talks D-Bus to the system manager.
 */
export interface SystemdClient {
  unit(name: string): Promise<UnitState>;
  /**
   * RestartUnit(name, "replace"), and wait for the job to finish.
   *
   * No timeout of its own: the unit's TimeoutStopSec and TimeoutStartSec bound
   * it, and they are the unit's to choose - a game server saving a large world
   * on the way down may rightly take minutes. Throws only when the job could
   * not be queued; a job that ran and failed is its result.
   */
  restart(name: string): Promise<JobResult>;
  daemonReload(): Promise<void>;
}

/** Whether a unit is up in a way that could be serving: active, or reloading while active. */
export function isUp(state: UnitState): boolean {
  return state.activeState === "active" || state.activeState === "reloading";
}

/** Whether nothing of the unit is running: stopped, cleanly or not. */
export function isStopped(state: UnitState): boolean {
  return state.activeState === "inactive" || state.activeState === "failed";
}

/**
 * Whether the unit has crashed and been brought back since `baseline` - the
 * state read just after Tidewaiter's restart job finished.
 *
 * Restart= hides a crash from ActiveState: by the next poll the unit can be
 * "active" again. But every start gets a new InvocationID and every automatic
 * restart bumps NRestarts, so either moving means the version we started is
 * not the process running now.
 */
export function restartedSince(baseline: UnitState, state: UnitState): boolean {
  return state.invocationId !== baseline.invocationId || state.nRestarts > baseline.nRestarts;
}

/**
 * The `systemd` health check: what systemd itself says about the restart.
 *
 *   fail          - the restart job did not end `done` (for a Type=notify unit,
 *                   the program never said READY=1 before TimeoutStartSec);
 *                   the unit is failed, inactive or deactivating; or it crashed
 *                   and Restart= brought it back since the job finished.
 *   pass          - active (or reloading) on the invocation the job started.
 *   inconclusive  - still activating.
 *
 * Pure, over the job's result, the state just after it and the state now, so
 * every combination is a table test.
 */
export function systemdVerdict(job: JobResult, baseline: UnitState, state: UnitState): CheckResult {
  if (job !== "done") return { outcome: "fail", reason: `the restart job ended '${job}'` };
  if (restartedSince(baseline, state)) {
    return { outcome: "fail", reason: "the unit crashed and was restarted since it came up" };
  }
  switch (state.activeState) {
    case "active":
    case "reloading":
      return { outcome: "pass" };
    case "activating":
      return { outcome: "inconclusive" };
    default:
      return {
        outcome: "fail",
        reason: `the unit is ${state.activeState}${state.result && state.result !== "success" ? ` (${state.result})` : ""}`,
      };
  }
}

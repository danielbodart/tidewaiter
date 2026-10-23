import { DbusConnection, MessageType, SYSTEM_BUS, type Message } from "./dbus.ts";
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

const SYSTEMD = "org.freedesktop.systemd1";
const MANAGER_PATH = "/org/freedesktop/systemd1";
const MANAGER = "org.freedesktop.systemd1.Manager";
const UNIT = "org.freedesktop.systemd1.Unit";
const SERVICE = "org.freedesktop.systemd1.Service";
const PROPERTIES = "org.freedesktop.DBus.Properties";
const UNIT_PATHS = "/org/freedesktop/systemd1/unit";

/** Every JobRemoved systemd sends, which Subscribe turns on for this connection. */
const JOB_REMOVED = `type='signal',sender='${SYSTEMD}',interface='${MANAGER}',member='JobRemoved',path='${MANAGER_PATH}'`;

const JOB_RESULTS: readonly JobResult[] = ["done", "canceled", "timeout", "failed", "dependency", "skipped"];

interface Waiter {
  resolve(result: JobResult): void;
  reject(error: Error): void;
}

/**
 * systemd's Manager over the system bus.
 *
 * One connection, opened when first needed and again after it drops, that has
 * subscribed to JobRemoved: a restart is RestartUnit followed by waiting for
 * the job it returned to be removed, and the subscription is in place before
 * the first job is ever queued, so its end cannot be missed. Restarting
 * another unit needs root (or polkit's say-so), which the daemon has.
 */
export class DbusSystemdClient implements SystemdClient {
  private connection?: Promise<DbusConnection>;
  /** Restarts waiting on their job, by job path. */
  private readonly waiting = new Map<string, Waiter>();
  /**
   * Jobs that ended while a restart was in flight but before its job path was
   * known - the reply and the signal are separate messages, and nothing
   * promises the reply comes first. Emptied whenever no restart is in flight.
   */
  private readonly ended = new Map<string, JobResult>();
  private inFlight = 0;

  constructor(private readonly path: string = SYSTEM_BUS) {}

  private bus(): Promise<DbusConnection> {
    if (!this.connection) {
      const opening = this.open();
      this.connection = opening;
      opening.then(
        (connection) =>
          connection.onClose((error) => {
            if (this.connection === opening) this.connection = undefined;
            // A job's end can no longer be heard on this connection; the
            // restart's caller decides what that means (see SystemdSource.undo).
            for (const waiter of this.waiting.values()) {
              waiter.reject(new Error(`lost the bus while waiting for the restart job (${error.message})`));
            }
            this.waiting.clear();
          }),
        () => {
          if (this.connection === opening) this.connection = undefined;
        },
      );
    }
    return this.connection;
  }

  private async open(): Promise<DbusConnection> {
    const connection = await DbusConnection.open(this.path);
    try {
      connection.onSignal((message) => this.jobRemoved(message));
      await connection.call("org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus", "AddMatch", "s", [
        JOB_REMOVED,
      ]);
      // Without this systemd sends no JobRemoved at all. It lasts as long as
      // this connection does.
      await connection.call(SYSTEMD, MANAGER_PATH, MANAGER, "Subscribe");
      return connection;
    } catch (error) {
      connection.close();
      throw error;
    }
  }

  async unit(name: string): Promise<UnitState> {
    const bus = await this.bus();
    // LoadUnit, not GetUnit: a unit nothing has started yet is not loaded, and
    // GetUnit would call it missing. A name with no unit file comes back
    // loaded as "not-found", which is the answer.
    const [path] = (await bus.call(SYSTEMD, MANAGER_PATH, MANAGER, "LoadUnit", "s", [name])) as [string];
    const [unit] = (await bus.call(SYSTEMD, path, PROPERTIES, "GetAll", "s", [UNIT])) as [Record<string, unknown>];
    const service = name.endsWith(".service")
      ? ((await bus.call(SYSTEMD, path, PROPERTIES, "GetAll", "s", [SERVICE]))[0] as Record<string, unknown>)
      : {};
    return toUnitState(name, unit, service);
  }

  async restart(name: string): Promise<JobResult> {
    const bus = await this.bus();
    this.inFlight += 1;
    try {
      const [job] = (await bus.call(SYSTEMD, MANAGER_PATH, MANAGER, "RestartUnit", "ss", [name, "replace"])) as [string];
      const already = this.ended.get(job);
      if (already !== undefined) return already;
      return await new Promise<JobResult>((resolve, reject) => this.waiting.set(job, { resolve, reject }));
    } finally {
      this.inFlight -= 1;
      if (this.inFlight === 0) this.ended.clear();
    }
  }

  async daemonReload(): Promise<void> {
    // Answered once the reload is done, so the drop-in just written is in
    // force by the time this returns.
    await (await this.bus()).call(SYSTEMD, MANAGER_PATH, MANAGER, "Reload");
  }

  close(): void {
    void this.connection?.then((connection) => connection.close(), () => {});
    this.connection = undefined;
  }

  private jobRemoved(message: Message): void {
    const removed = toJobRemoved(message);
    if (!removed) return;
    const waiter = this.waiting.get(removed.job);
    if (waiter) {
      this.waiting.delete(removed.job);
      waiter.resolve(removed.result);
    } else if (this.inFlight > 0) {
      this.ended.set(removed.job, removed.result);
    }
  }
}

/** A JobRemoved signal's job path and result; undefined for any other message. */
export function toJobRemoved(message: Message): { job: string; unit: string; result: JobResult } | undefined {
  if (message.type !== MessageType.Signal || message.interface !== MANAGER || message.member !== "JobRemoved") {
    return undefined;
  }
  const [, job, unit, result] = (message.body ?? []) as [number, string, string, string];
  if (typeof job !== "string") return undefined;
  // A result this code does not know is not a success.
  const known = JOB_RESULTS.find((r) => r === result) ?? "failed";
  return { job, unit, result: known };
}

/**
 * The properties Tidewaiter reads, from GetAll on the Unit and Service
 * interfaces. InvocationID arrives as 16 bytes and is kept as hex, as
 * `systemctl show` prints it.
 */
export function toUnitState(
  name: string,
  unit: Readonly<Record<string, unknown>>,
  service: Readonly<Record<string, unknown>>,
): UnitState {
  const text = (value: unknown) => (typeof value === "string" ? value : "");
  const count = (value: unknown) => (typeof value === "number" ? value : typeof value === "bigint" ? Number(value) : 0);
  const invocation = Array.isArray(unit.InvocationID)
    ? (unit.InvocationID as number[]).map((byte) => byte.toString(16).padStart(2, "0")).join("")
    : "";
  return {
    name,
    loadState: text(unit.LoadState),
    activeState: text(unit.ActiveState),
    subState: text(unit.SubState),
    result: text(service.Result),
    nRestarts: count(service.NRestarts),
    mainPid: count(service.MainPID),
    controlGroup: text(service.ControlGroup),
    invocationId: invocation,
  };
}

/**
 * A unit's object path, escaped as systemd escapes it: every byte that is
 * not a letter or digit, and a leading digit, becomes `_` and two hex digits.
 * Only for tests and for reading paths: calls go through LoadUnit, which
 * returns the path itself.
 */
export function unitPath(name: string): string {
  let escaped = "";
  for (const [index, byte] of [...new TextEncoder().encode(name)].entries()) {
    const char = String.fromCharCode(byte);
    escaped += /[A-Za-z]/.test(char) || (index > 0 && /[0-9]/.test(char))
      ? char
      : `_${byte.toString(16).padStart(2, "0")}`;
  }
  return `${UNIT_PATHS}/${escaped}`;
}

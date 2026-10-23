import { type ConntrackEntry, type ConntrackSource, parseConntrack } from "./conntrack.ts";
import type { ClosureBinder } from "./closure.ts";
import type { Log } from "./daemon.ts";
import { decide, UNIT_WORDS, type Action } from "./decide.ts";
import { combine, detectorByName, type DetectorResult } from "./detectors.ts";
import { pollVerdict, uptimeSeconds } from "./gate.ts";
import {
  portBoundCheck,
  portConnectCheck,
  uptimeCheck,
  type CheckResult,
  type HealthCheck,
  type HealthInput,
  type HealthResult,
  type TcpProbe,
} from "./health.ts";
import { parseListeners, unitBoundPorts, type Listener, type ListenerSource } from "./listeners.ts";
import type { NetnsReader } from "./netns.ts";
import type { Profiles } from "./profile.ts";
import { MAX_SWAP_FAILURES, type Activity, type Outcome, type PassContext, type Source } from "./source.ts";
import type { Memories, StateStore, UnitMemory } from "./state-store.ts";
import { isStopped, isUp, restartedSince, systemdVerdict, type JobResult, type SystemdClient, type UnitState } from "./systemd.ts";
import { parseUnitPolicy, type UnitPolicy, type UnitsDirectory } from "./units.ts";

export interface SystemdOptions {
  /** Report what would change without changing it. */
  readonly dryRun: boolean;
  /** How often to re-check health while waiting, in milliseconds. */
  readonly healthPollMillis: number;
}

/** Everything the systemd source talks to, each behind an interface so tests replace the far ends. */
export interface SystemdDependencies {
  readonly units: UnitsDirectory;
  readonly systemd: SystemdClient;
  readonly profiles: Profiles;
  readonly listeners: ListenerSource;
  /** For the re-sample right before a swap; the pass's own dump arrives in the context. */
  readonly conntrack: ConntrackSource;
  readonly store: StateStore;
  readonly closure: ClosureBinder;
  readonly netns: NetnsReader;
  readonly tcp: TcpProbe;
  readonly log: Log;
  readonly now: () => number;
}

/** How far a swap got, so a failure undoes exactly what was done. */
interface Progress {
  bound: boolean;
  set: boolean;
}

/**
 * systemd units whose program lives in a Nix profile, kept on the store path
 * their units file asks for.
 *
 * Per unit, per pass: read the policy, the unit and the profile; a stopped unit
 * just has its profile moved for whoever starts it next; a running one is
 * sampled for activity, judged by the same decide() as a container, and once
 * idle for long enough swapped - hold the inhibitor, look once more for
 * players, bind the new closure, `nix-env --set`, restart (the unit's own
 * ExecStop saves and quits), health-gate, and on failure switch the profile
 * back to the generation it left, restart again and pin the version out.
 *
 * Pins and the last good generation survive a restart of Tidewaiter (see
 * UnitMemory); idle streaks do not.
 */
export class SystemdSource implements Source {
  readonly name = "systemd";

  constructor(
    private readonly deps: SystemdDependencies,
    private readonly options: SystemdOptions,
  ) {}

  private memories?: Map<string, UnitMemory>;
  private saved?: string;
  private readonly streaks = new Map<string, number>();
  /** Units looked at since this daemon started, so crash recovery runs once per unit. */
  private readonly seen = new Set<string>();
  /** The text last warned about per file (or note per unit), so a pass every minute repeats nothing. */
  private readonly warned = new Map<string, string>();

  private get log(): Log {
    return this.deps.log;
  }

  async once(context: PassContext): Promise<Outcome[]> {
    await this.load();
    const outcomes: Outcome[] = [];
    for (const policy of await this.policies()) {
      try {
        outcomes.push(await this.reconcile(policy, context));
      } catch (error) {
        this.log(`${policy.unit}: Error: ${(error as Error).message}`);
        outcomes.push({ workload: policy.unit, action: { kind: "keep", reason: "errored" } });
      }
    }
    await this.save();
    return outcomes;
  }

  async activity(conntrack: readonly ConntrackEntry[] | undefined): Promise<Activity[]> {
    const activities: Activity[] = [];
    for (const policy of await this.policies()) {
      let state: UnitState;
      try {
        state = await this.deps.systemd.unit(policy.unit);
      } catch {
        activities.push({ workload: policy.unit, busy: true });
        continue;
      }
      // Nothing running, nobody playing.
      if (state.loadState === "not-found" || isStopped(state)) {
        activities.push({ workload: policy.unit, busy: false });
        continue;
      }
      activities.push({ workload: policy.unit, busy: (await this.sample(policy, state, conntrack, true)).inUse });
    }
    return activities;
  }

  async list(): Promise<string[]> {
    await this.load();
    const lines: string[] = [];
    for (const policy of await this.policies()) {
      const current = await this.deps.profiles.current(policy.profile).catch(() => undefined);
      const memory = this.memory(policy.unit);
      const status = memory.pinned === policy.desired ? "pinned out"
        : current === undefined ? "no profile yet"
        : current === policy.desired ? "up to date"
        : "update waiting";
      lines.push(
        `${policy.unit}  ${policy.profile} -> ${current ?? "nothing"}  [${status}]  desired=${policy.desired} ` +
          `detector=${policy.detector} health=${policy.health.join(",")} idle-samples=${policy.idleSamples}`,
      );
    }
    return lines;
  }

  /**
   * Every valid policy in the units directory, one per unit.
   *
   * Warnings are logged once per distinct file contents rather than every
   * pass. A second file declaring a unit already declared is skipped: two
   * profiles for one unit is a mistake to fix, not a choice to guess at.
   */
  private async policies(): Promise<UnitPolicy[]> {
    const policies = new Map<string, UnitPolicy>();
    for (const file of await this.deps.units.read()) {
      const { policy, warnings } = parseUnitPolicy(file.text, file.name);
      if (this.warned.get(file.name) !== file.text) {
        for (const warning of warnings) this.log(warning);
        if (policy && policies.has(policy.unit)) {
          this.log(`${file.name}: ${policy.unit} is already declared by another file; skipping this one`);
        }
        this.warned.set(file.name, file.text);
      }
      if (policy && !policies.has(policy.unit)) policies.set(policy.unit, policy);
    }
    return [...policies.values()];
  }

  private async reconcile(policy: UnitPolicy, context: PassContext): Promise<Outcome> {
    const { unit, desired, profile } = policy;
    const outcome = (action: Action, busy?: boolean): Outcome =>
      busy === undefined ? { workload: unit, action } : { workload: unit, action, busy };

    let state: UnitState;
    try {
      state = await this.deps.systemd.unit(unit);
    } catch (error) {
      this.log(`${unit}: could not read the unit from systemd (${(error as Error).message}); leaving it alone`);
      return outcome({ kind: "keep", reason: "systemd unreachable" });
    }
    if (state.loadState === "not-found") {
      this.note(unit, "not-found", `${unit}: systemd does not know this unit; leaving it alone`);
      return outcome({ kind: "keep", reason: "no such unit" }, false);
    }

    let current: string | undefined;
    let generation: number | undefined;
    try {
      current = await this.deps.profiles.current(profile);
      generation = await this.deps.profiles.generation(profile);
    } catch (error) {
      this.log(`${unit}: could not read the profile ${profile} (${(error as Error).message}); leaving it alone`);
      return outcome({ kind: "keep", reason: "profile unreadable" });
    }

    // A pin, and a run of swap failures, only hold until desired moves on.
    let memory = this.memory(unit);
    if (memory.pinned !== undefined && memory.pinned !== desired) memory = { ...memory, pinned: undefined };
    if (memory.failing !== undefined && memory.failing !== desired) {
      memory = { ...memory, failing: undefined, swapFailures: undefined };
    }
    this.remember(unit, memory);

    if (!this.seen.has(unit)) {
      this.seen.add(unit);
      const recovered = await this.firstSight(policy, state, current, generation, context);
      if (recovered) return outcome(recovered, false);
    }

    if (isStopped(state)) return outcome(await this.updateStopped(policy, current), false);

    const activity = await this.sample(policy, state, context.conntrack);
    const transitional = !isUp(state);
    // A unit on its way up or down is neither idle nor a candidate: count
    // nothing until it settles.
    const idleStreak = activity.inUse || transitional ? 0 : (this.streaks.get(unit) ?? 0) + 1;
    this.streaks.set(unit, idleStreak);

    if (transitional && current !== desired && this.memory(unit).pinned !== desired) {
      const action: Action = { kind: "defer", reason: `the unit is ${state.activeState}; waiting for it to settle` };
      this.log(`${unit}: ${action.reason}`);
      return outcome(action, activity.inUse);
    }

    const action = decide(
      { policy, currentDigest: current, desired: { digest: desired }, inUse: activity.inUse, idleStreak },
      { pinnedDigest: this.memory(unit).pinned },
      UNIT_WORDS,
    );

    switch (action.kind) {
      case "keep":
      case "pinned":
        return outcome(action, activity.inUse);
      case "defer":
        this.log(`${unit}: ${action.reason}`);
        return outcome(action, activity.inUse);
      case "update":
        this.log(`${unit}: updating - ${action.reason}`);
        if (this.options.dryRun) {
          this.log(`${unit}: dry run, not touching it`);
          return outcome(action, activity.inUse);
        }
        await context.hold(unit);
        return outcome((await this.swap(policy, state, current, generation)) ?? action, false);
    }
  }

  /**
   * A stopped unit has nobody on it and is not Tidewaiter's to start: move its
   * profile, so whoever starts it next starts the desired version, with no
   * restart and so no gate. A pinned version is still not moved to.
   */
  private async updateStopped(policy: UnitPolicy, current: string | undefined): Promise<Action> {
    const { unit, desired, profile } = policy;
    if (current === desired) return { kind: "keep", reason: UNIT_WORDS.keep };
    if (this.memory(unit).pinned === desired) return { kind: "pinned", reason: UNIT_WORDS.pinned };

    const action: Action = { kind: "update", from: current, to: desired, reason: "the unit is stopped" };
    if (this.options.dryRun) {
      this.log(`${unit}: stopped; would move its profile to ${desired} (dry run)`);
      return action;
    }
    try {
      if (policy.bindClosure) await this.deps.closure.bind(unit, profile, desired);
      await this.deps.profiles.set(profile, desired);
      if (policy.bindClosure) await this.deps.systemd.daemonReload();
    } catch (error) {
      this.log(`${unit}: could not move the stopped unit's profile (${(error as Error).message})`);
      return { kind: "keep", reason: "profile update failed" };
    }
    this.log(`${unit}: updated the profile of a stopped unit to ${desired}; it runs that when next started`);
    return action;
  }

  /**
   * The swap. Returns the action that actually happened when it differs from
   * the update decided on - a defer when someone arrived at the last moment.
   */
  private async swap(
    policy: UnitPolicy,
    before: UnitState,
    current: string | undefined,
    previous: number | undefined,
  ): Promise<Action | undefined> {
    const { unit, desired, profile } = policy;

    // The idle verdict can be a whole pass old by now. Look once more, right
    // before anything changes, and back off if someone has just joined.
    const fresh = await this.sample(policy, before, await this.dumpConntrack());
    if (fresh.inUse) {
      this.streaks.set(unit, 0);
      const action: Action = { kind: "defer", reason: "someone arrived just before the swap" };
      this.log(`${unit}: ${action.reason}; deferring`);
      return action;
    }

    const progress: Progress = { bound: false, set: false };
    let job: JobResult;
    try {
      if (policy.bindClosure) {
        await this.deps.closure.bind(unit, profile, desired);
        progress.bound = true;
        await this.deps.systemd.daemonReload();
      }
      await this.deps.profiles.set(profile, desired);
      progress.set = true;
      job = await this.deps.systemd.restart(unit);
    } catch (error) {
      this.log(`${unit}: FAILED during swap (${(error as Error).message}); undoing what was done`);
      await this.undo(policy, before, current, previous, progress);
      this.countFailure(unit, desired);
      return undefined;
    }

    const health = await this.gate(policy, job);
    if (health.healthy) {
      const good = await this.deps.profiles.generation(profile).catch(() => undefined);
      await this.deps.profiles.prune(profile, policy.keepGenerations).catch((error) =>
        this.log(`${unit}: updated, but pruning old generations failed (${(error as Error).message})`),
      );
      this.remember(unit, { good });
      this.log(`${unit}: updated to ${desired} and it is healthy`);
      return undefined;
    }

    this.log(`${unit}: the new version is unhealthy (${health.reason}); rolling back`);
    await this.rollBack(policy, previous, current);
    this.remember(unit, { ...this.memory(unit), pinned: desired, failing: undefined, swapFailures: undefined });
    return undefined;
  }

  /**
   * Undo a swap that failed before its gate, given how far it got: the profile
   * back to the generation it left, the closure back to what that generation
   * needs - and, only if the unit did restart regardless (the bus dropped after
   * the job was queued), a restart back onto it.
   */
  private async undo(
    policy: UnitPolicy,
    before: UnitState,
    current: string | undefined,
    previous: number | undefined,
    progress: Progress,
  ): Promise<void> {
    const { unit, profile } = policy;
    try {
      if (progress.set && previous !== undefined) await this.switchBack(profile, previous);
      if (progress.bound && current !== undefined) {
        await this.deps.closure.bind(unit, profile, current);
        await this.deps.systemd.daemonReload();
      }
      if (progress.set) {
        const after = await this.deps.systemd.unit(unit);
        if (after.invocationId !== before.invocationId) await this.deps.systemd.restart(unit);
      }
    } catch (error) {
      this.log(`${unit}: undoing the swap itself failed (${(error as Error).message}); manual attention needed`);
    }
  }

  /**
   * Put the unit back on the generation it ran before, restart it and gate it
   * again. Never retried: a rollback that fails is logged for a person, not
   * looped on.
   */
  private async rollBack(policy: UnitPolicy, previous: number | undefined, previousTarget: string | undefined): Promise<void> {
    const { unit, desired, profile } = policy;
    if (previous === undefined || previousTarget === undefined) {
      this.log(`${unit}: there is no earlier generation of ${profile} to roll back to; manual attention needed`);
      return;
    }
    try {
      await this.switchBack(profile, previous);
      if (policy.bindClosure) {
        await this.deps.closure.bind(unit, profile, previousTarget);
        await this.deps.systemd.daemonReload();
      }
      const health = await this.gate(policy, await this.deps.systemd.restart(unit));
      if (health.healthy) {
        this.log(`${unit}: rolled back to ${previousTarget} and pinned ${desired} out until the desired version moves on`);
      } else {
        this.log(
          `${unit}: rolled back to ${previousTarget}, but it is not healthy either (${health.reason}); manual attention needed`,
        );
      }
    } catch (error) {
      this.log(`${unit}: rollback itself failed (${(error as Error).message}); manual attention needed`);
    }
  }

  /**
   * Switch the profile to `generation` and delete the one it leaves, so the
   * newest generations stay the good ones for prune's `+N`, and the failed
   * version is not offered by `nix-env --rollback` to someone later.
   */
  private async switchBack(profile: string, generation: number): Promise<void> {
    const failed = await this.deps.profiles.generation(profile);
    if (failed === generation) return;
    await this.deps.profiles.switchGeneration(profile, generation);
    if (failed !== undefined) {
      await this.deps.profiles.deleteGeneration(profile, failed).catch((error) =>
        this.log(`${profile}: could not delete generation ${failed} (${(error as Error).message})`),
      );
    }
  }

  /**
   * Count a swap-step failure against `desired`, pinning it out after
   * MAX_SWAP_FAILURES in a row, as the Docker source does.
   */
  private countFailure(unit: string, desired: string): void {
    const memory = this.memory(unit);
    const failures = memory.failing === desired ? (memory.swapFailures ?? 0) + 1 : 1;
    if (failures >= MAX_SWAP_FAILURES) {
      this.log(`${unit}: swap has failed ${failures} times running for ${desired}; pinning it out until the desired version moves on`);
      this.remember(unit, { ...memory, pinned: desired, failing: desired, swapFailures: failures });
      return;
    }
    this.remember(unit, { ...memory, failing: desired, swapFailures: failures });
  }

  /**
   * The first look at a unit since this daemon started.
   *
   * If Tidewaiter died mid-swap, the profile may point at a version whose gate
   * never ran. A unit that is up is adopted as it is, its generation recorded
   * as good. A unit that has failed on a generation newer than the last one
   * recorded good is the one fact that says "a swap went wrong and nobody
   * cleaned up", so that - and only that - is rolled back, and the version it
   * failed on pinned out.
   */
  private async firstSight(
    policy: UnitPolicy,
    state: UnitState,
    current: string | undefined,
    generation: number | undefined,
    context: PassContext,
  ): Promise<Action | undefined> {
    const { unit, profile } = policy;
    const { good } = this.memory(unit);
    if (generation === undefined) return undefined;

    if (isUp(state)) {
      if (good !== undefined && good !== generation) {
        this.log(`${unit}: adopting generation ${generation} of ${profile} as the baseline (no record of it going in)`);
      }
      if (good !== generation) this.remember(unit, { ...this.memory(unit), good: generation });
      return undefined;
    }

    if (state.activeState !== "failed" || good === undefined || generation <= good || this.options.dryRun) return undefined;

    this.log(
      `${unit}: failed on generation ${generation} of ${profile}, newer than the last good one (${good}) - ` +
        "a swap was interrupted; rolling back",
    );
    await context.hold(unit);
    const goodTarget = await this.deps.profiles.target(profile, good).catch(() => undefined);
    await this.rollBack(policy, good, goodTarget);
    if (current !== undefined) this.remember(unit, { ...this.memory(unit), pinned: current });
    return { kind: "pinned", reason: "rolled back an interrupted swap" };
  }

  /**
   * Gate the restart: poll the policy's checks until they settle.
   *
   * A job that did not end `done` is unhealthy at once, whatever the checks -
   * for a Type=notify unit it means the program never said it was up.
   */
  private async gate(policy: UnitPolicy, job: JobResult): Promise<HealthResult> {
    if (job !== "done") return { healthy: false, reason: `the restart job ended '${job}'` };

    const { unit } = policy;
    const baseline = await this.deps.systemd.unit(unit);
    let latest = baseline;
    const grace = uptimeSeconds(policy.healthTimeoutSeconds);
    const checks: HealthCheck[] = policy.health.map((name) => {
      switch (name) {
        case "systemd":
          return { name, evaluate: async () => systemdVerdict(job, baseline, latest) };
        case "uptime":
          return uptimeCheck(grace, this.deps.now);
        case "port-connect":
          return portConnectCheck(grace, this.deps.now);
        case "port-bound":
          return portBoundCheck;
      }
    });

    const evaluate = async (signal: AbortSignal): Promise<CheckResult[]> => {
      latest = await this.deps.systemd.unit(unit);
      let listeners: Promise<Listener[]> | undefined;
      const input: HealthInput = {
        // The crash signals feed `running` too, so uptime fails a unit that
        // Restart= has quietly brought back, as it fails a crash-looping container.
        container: {
          running: isUp(latest) && !restartedSince(baseline, latest),
          health: "none",
          spec: { networks: [], networkMode: "host" },
        },
        ports: policy.ports,
        bound: unitBoundPorts(() => (listeners ??= this.readListeners()), latest.controlGroup),
        tcp: this.deps.tcp,
        networkDriver: async () => undefined,
      };
      const results: CheckResult[] = [];
      for (const check of checks) results.push(await check.evaluate(input, signal));
      return results;
    };

    const verdict = await pollVerdict(
      evaluate,
      { timeoutSeconds: policy.healthTimeoutSeconds, pollMillis: this.options.healthPollMillis, now: this.deps.now },
      new AbortController().signal,
    );
    return verdict === "aborted" ? { healthy: false, reason: "the health gate was aborted" } : verdict;
  }

  private async readListeners(): Promise<Listener[]> {
    return parseListeners(await this.deps.listeners.dump());
  }

  /**
   * Sample activity, fail-safe: anything that cannot tell reads as busy.
   *
   * `quiet` is for the activity tick, which runs every few seconds and must
   * not repeat the pass's warnings.
   */
  private async sample(
    policy: UnitPolicy,
    state: UnitState,
    conntrack: readonly ConntrackEntry[] | undefined,
    quiet = false,
  ): Promise<DetectorResult> {
    const busy: DetectorResult = { inUse: true, confidence: "low" };
    const { unit } = policy;
    if (policy.detector === "none") return { inUse: false, confidence: "low" };
    if (policy.ports.length === 0) {
      if (!quiet) this.note(unit, "no-ports", `${unit}: declares no ports, so its activity cannot be read; assuming busy`);
      return busy;
    }
    if (policy.detector === "conntrack" && conntrack === undefined) return busy;
    if (policy.detector === "tcp" && state.mainPid === 0) return busy;

    try {
      const result = await detectorByName(policy.detector)!.inUse({
        id: unit,
        pid: state.mainPid,
        published: policy.ports,
        conntrack: conntrack ?? [],
        netns: this.deps.netns,
      });
      return combine([result]);
    } catch (error) {
      if (!quiet) this.log(`${unit}: activity detector '${policy.detector}' failed (${(error as Error).message}); assuming busy`);
      return busy;
    }
  }

  private async dumpConntrack(): Promise<readonly ConntrackEntry[] | undefined> {
    try {
      return parseConntrack(await this.deps.conntrack.dump());
    } catch {
      return undefined;
    }
  }

  /** Log a standing condition once, until it changes. */
  private note(unit: string, key: string, message: string): void {
    const id = `${unit}#${key}`;
    if (this.warned.get(id) === message) return;
    this.warned.set(id, message);
    this.log(message);
  }

  private memory(unit: string): UnitMemory {
    return this.memories?.get(unit) ?? {};
  }

  private remember(unit: string, memory: UnitMemory): void {
    const clean = Object.fromEntries(Object.entries(memory).filter(([, value]) => value !== undefined)) as UnitMemory;
    this.memories ??= new Map();
    if (Object.keys(clean).length === 0) this.memories.delete(unit);
    else this.memories.set(unit, clean);
  }

  private async load(): Promise<void> {
    if (this.memories !== undefined) return;
    const memories: Memories = await this.deps.store.load();
    this.memories = new Map(Object.entries(memories));
    this.saved = JSON.stringify(memories);
  }

  /** Persist the memories, only when this pass changed them. */
  private async save(): Promise<void> {
    const memories = Object.fromEntries([...(this.memories ?? new Map()).entries()].sort(([a], [b]) => a.localeCompare(b)));
    const json = JSON.stringify(memories);
    if (json === this.saved) return;
    try {
      await this.deps.store.save(memories);
      this.saved = json;
    } catch (error) {
      this.log(`Warning: could not save the systemd source's state (${(error as Error).message})`);
    }
  }
}

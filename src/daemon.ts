import { type ConntrackEntry, type ConntrackSource, parseConntrack } from "./conntrack.ts";
import type { Action } from "./decide.ts";
import { runEvery } from "./loop.ts";
import { decideReboot, inWindow, type BootCheck, type Rebooter, type Window } from "./reboot.ts";
import type { Outcome, PassContext, Source, SourceName } from "./source.ts";
import { NoStatus, type RebootStatus, type Status, type StatusSink, type WorkloadStatus } from "./status.ts";

export interface Options {
  /** The label that opts a container in. */
  readonly label: string;
  /** Seconds between reconcile passes. */
  readonly interval: number;
  /** Seconds between activity ticks, which keep the status and a reboot's gate current. */
  readonly activityInterval: number;
  /** Report what would change without changing it. */
  readonly dryRun: boolean;
  /** Default stop grace, when neither the label nor the container sets one. */
  readonly defaultGraceSeconds: number;
  /** How often to re-check health while waiting, in milliseconds. */
  readonly healthPollMillis: number;
}

export const DEFAULTS: Options = {
  label: "tidewaiter.autoupdate",
  interval: 300,
  activityInterval: 15,
  dryRun: false,
  defaultGraceSeconds: 10,
  healthPollMillis: 2000,
};

export type Log = (message: string) => void;

/** An automatic reboot, when one is needed, inside a daily window, once nothing is in use. */
export interface AutoReboot {
  readonly window: Window;
  /** The window as given, "03:00-05:00", for the log and the status. */
  readonly label: string;
  readonly check: BootCheck;
  readonly rebooter: Rebooter;
  /** The local time, for the window. */
  readonly now: () => Date;
}

/**
 * The loop every source runs in, and everything they share.
 *
 * One pass dumps the host conntrack table once, runs each source in turn over
 * it (so two swaps never overlap, whichever sources they belong to), logs one
 * summary line when it changes, and writes what is busy, settled and being
 * swapped to the status file. Between passes a faster activity tick keeps that
 * current, so `tidewaiter status` sees a player who joined just after a pass
 * within seconds rather than one `interval` later.
 *
 * With an automatic reboot, every pass of `run` ends by asking whether the
 * machine needs one and may have it now (see considerReboot). Nothing here
 * ever stands in a person's way: a reboot asked for by hand goes ahead.
 *
 * A source that throws does not stop the others; its workloads keep whatever
 * busy verdict they last had, so a source that cannot be read never reads as
 * idle.
 */
export class Daemon {
  constructor(
    private readonly sources: readonly Source[],
    private readonly conntrack: ConntrackSource,
    private readonly options: Pick<Options, "interval" | "activityInterval">,
    private readonly log: Log = console.log,
    private readonly status: StatusSink = NoStatus,
    private readonly autoReboot?: AutoReboot,
  ) {}

  /** The last busy verdict per workload, per source. */
  private readonly busy = new Map<SourceName, ReadonlyMap<string, boolean>>();
  /** Whether each workload was idle for its whole debounce at the last pass, per source. */
  private readonly settled = new Map<SourceName, ReadonlyMap<string, boolean>>();
  /** Workloads being swapped right now, held for the rest of the pass. */
  private readonly held = new Set<string>();
  private lastSummary?: string;
  private lastConntrackError?: string;
  private lastStatusError?: string;
  private writing: Promise<void> = Promise.resolve();
  private reboot?: RebootStatus;
  private lastRebootLine?: string;
  /** Set once logind has accepted the reboot, so no later pass asks again. */
  private rebooting = false;

  /**
   * One pass over every source.
   *
   * If a source threw, the others still ran; the first error is then rethrown,
   * so the loop logs it and `check` exits non-zero, as a pass that could not see
   * its containers always has.
   */
  async once(): Promise<Outcome[]> {
    const context: PassContext = {
      conntrack: await this.dumpConntrack(),
      hold: async (workload) => {
        this.held.add(workload);
        await this.publish();
      },
    };

    const outcomes: Outcome[] = [];
    let failure: Error | undefined;
    try {
      for (const source of this.sources) {
        try {
          const own = await source.once(context);
          this.record(source.name, own);
          outcomes.push(...own);
        } catch (error) {
          if (this.sources.length > 1) this.log(`${source.name}: Error: ${(error as Error).message}`);
          failure ??= error as Error;
        }
      }
    } finally {
      this.held.clear();
      await this.publish();
    }
    if (failure) throw failure;

    const summary = summarise(outcomes.map((outcome) => outcome.action), this.noun());
    if (summary !== this.lastSummary) {
      this.log(summary);
      this.lastSummary = summary;
    }
    return outcomes;
  }

  /**
   * Re-read activity only and write the status: no digests, no decisions, no
   * idle streaks, so the tick keeps the status current but never hurries a
   * swap or a reboot.
   */
  async tick(): Promise<void> {
    await this.readActivity();
    await this.publish();
  }

  /** The workloads in use, as last seen by a pass or a tick. */
  inUse(): string[] {
    const names = new Set<string>();
    for (const verdicts of this.busy.values()) {
      for (const [workload, busy] of verdicts) if (busy) names.add(workload);
    }
    return [...names].sort();
  }

  /** The workloads being swapped right now. */
  swapping(): string[] {
    return [...this.held].sort();
  }

  /** What the status file says, as of now. */
  snapshot(): Status {
    const workloads: WorkloadStatus[] = [];
    for (const source of this.sources) {
      const busy = this.busy.get(source.name);
      const settled = this.settled.get(source.name);
      // A workload never sampled has no busy verdict, but still holds a reboot off.
      const names = new Set([...(busy?.keys() ?? []), ...(settled?.keys() ?? [])]);
      for (const workload of names) {
        workloads.push({
          workload,
          source: source.name,
          busy: busy?.get(workload) ?? false,
          settled: settled?.get(workload) ?? false,
        });
      }
    }
    return {
      updated: new Date().toISOString(),
      // Several ticks missed, and never less than a minute, so a slow
      // conntrack dump or a busy host doesn't read as a dead daemon.
      staleAfter: Math.max(60, 4 * this.options.activityInterval),
      busy: this.inUse(),
      swapping: this.swapping(),
      workloads,
      ...(this.reboot ? { reboot: this.reboot } : {}),
    };
  }

  /**
   * After a pass: reboot if the machine needs it, it is inside the window, and
   * nothing is in use, unsettled or being swapped - the same idleness a swap
   * waits for, across every source. Right before asking logind, activity is
   * read once more, as a swap re-samples before it starts, so a player who
   * joined since the pass is never cut off.
   *
   * Never outside the window. A window busy from start to end passes without a
   * reboot, and tomorrow's is tried the same way. What it concluded is logged
   * when it changes, and is in the status.
   */
  async considerReboot(): Promise<void> {
    const auto = this.autoReboot;
    if (!auto || this.rebooting) return;
    await this.rebootIfReady(auto);
    await this.publish();
  }

  private async rebootIfReady(auto: AutoReboot): Promise<void> {

    let needed: string | undefined;
    try {
      needed = await auto.check.needed();
    } catch (error) {
      this.rebootVerdict(auto, undefined, `could not tell whether a reboot is needed (${(error as Error).message})`);
      return;
    }

    const inputs = () => ({
      needed,
      inWindow: inWindow(auto.window, auto.now()),
      busy: this.inUse(),
      unsettled: this.unsettled(),
      swapping: this.swapping(),
    });
    let verdict = decideReboot(inputs());
    if (verdict.kind === "reboot") {
      const answered = await this.readActivity();
      verdict = answered ? decideReboot(inputs()) : { kind: "wait", reason: "waiting: activity could not be read" };
    }
    if (verdict.kind === "not-needed") return this.rebootVerdict(auto, undefined, "not needed");
    if (verdict.kind === "wait") return this.rebootVerdict(auto, needed, verdict.reason);

    this.rebootVerdict(auto, needed, "rebooting, as nothing is in use");
    try {
      await auto.rebooter.reboot();
      this.rebooting = true;
    } catch (error) {
      // Someone else's block inhibitor, or no logind at all: the next pass in
      // the window asks again.
      this.rebootVerdict(auto, needed, `the reboot was refused (${(error as Error).message}); trying again next pass`);
    }
  }

  /**
   * Reconcile on an interval until the signal aborts, ticking activity in
   * between when there is a status file or a reboot to keep current, and
   * clearing the status on the way out.
   */
  async run(signal: AbortSignal): Promise<void> {
    const ticking = this.status === NoStatus && !this.autoReboot ? Promise.resolve() : this.tickEvery(signal);
    try {
      await runEvery(this.options.interval, async () => {
        await this.once();
        await this.considerReboot();
      }, signal, this.log);
      await ticking;
    } finally {
      await this.writing;
      await this.status.clear().catch(() => {});
    }
  }

  /** The activity tick: single-flight, so a slow dump never stacks up behind itself. */
  private async tickEvery(signal: AbortSignal): Promise<void> {
    let running: Promise<void> | undefined;
    const ticker = setInterval(() => {
      running ??= this.tick().catch(() => {}).finally(() => { running = undefined; });
    }, this.options.activityInterval * 1000);
    await new Promise<void>((resolve) => {
      if (signal.aborted) return resolve();
      signal.addEventListener("abort", () => resolve(), { once: true });
    });
    clearInterval(ticker);
    await running;
  }

  /**
   * Every source's activity, into the busy verdicts. True when every source
   * answered; one that did not keeps its last verdicts.
   */
  private async readActivity(): Promise<boolean> {
    const conntrack = await this.dumpConntrack();
    let answered = true;
    for (const source of this.sources) {
      try {
        const activity = await source.activity(conntrack);
        this.busy.set(source.name, new Map(activity.map((a) => [a.workload, a.busy])));
      } catch {
        // Keep the last verdicts; the next pass will say what went wrong.
        answered = false;
      }
    }
    return answered;
  }

  /**
   * Remember this pass's verdicts for a source.
   *
   * A workload the source no longer reports is gone and drops out; one it
   * reports without a busy verdict (never sampled this pass) keeps its last
   * one, and counts as unsettled.
   */
  private record(source: SourceName, outcomes: readonly Outcome[]): void {
    const previous = this.busy.get(source) ?? new Map<string, boolean>();
    const busy = new Map<string, boolean>();
    const settled = new Map<string, boolean>();
    for (const outcome of outcomes) {
      const verdict = outcome.busy ?? previous.get(outcome.workload);
      if (verdict !== undefined) busy.set(outcome.workload, verdict);
      settled.set(outcome.workload, outcome.settled ?? false);
    }
    this.busy.set(source, busy);
    this.settled.set(source, settled);
  }

  /** Workloads not idle for their whole debounce at the last pass - never sampled included. */
  private unsettled(): string[] {
    const names: string[] = [];
    for (const verdicts of this.settled.values()) {
      for (const [workload, settled] of verdicts) if (!settled) names.push(workload);
    }
    return names.sort();
  }

  /** Remember and log (when it changes) where an automatic reboot stands. */
  private rebootVerdict(auto: AutoReboot, needed: string | undefined, verdict: string): void {
    this.reboot = { window: auto.label, ...(needed === undefined ? {} : { needed }), verdict };
    const line = needed === undefined
      ? `Automatic reboot (${auto.label}): ${verdict}`
      : `Automatic reboot (${auto.label}): needed, ${needed}; ${verdict}`;
    if (line !== this.lastRebootLine) this.log(line);
    this.lastRebootLine = line;
  }

  /**
   * Write the status, one write at a time so a tick and a pass never
   * interleave. Never fatal: logged once per distinct problem, since the tick
   * would otherwise repeat it every few seconds.
   */
  private publish(): Promise<void> {
    const write = this.writing.then(async () => {
      try {
        await this.status.write(this.snapshot());
        this.lastStatusError = undefined;
      } catch (error) {
        const message = (error as Error).message;
        if (message !== this.lastStatusError) this.log(`Warning: could not write the status (${message})`);
        this.lastStatusError = message;
      }
    });
    this.writing = write;
    return write;
  }

  private async dumpConntrack(): Promise<readonly ConntrackEntry[] | undefined> {
    try {
      const entries = parseConntrack(await this.conntrack.dump());
      this.lastConntrackError = undefined;
      return entries;
    } catch (error) {
      const message = (error as Error).message;
      if (message !== this.lastConntrackError) {
        this.log(`Warning: could not read conntrack (${message}); assuming busy where it matters`);
      }
      this.lastConntrackError = message;
      return undefined;
    }
  }

  /** What the summary calls the things it counts. */
  private noun(): string {
    const names = new Set(this.sources.map((source) => source.name));
    if (names.size !== 1) return "workload";
    return names.has("docker") ? "container" : "unit";
  }
}

/**
 * One line saying what the pass did, logged only when it changes.
 *
 * "Nothing to do" is the normal, healthy state and worth saying once - a daemon
 * that reconciles every five minutes should be quiet, not chatty.
 */
function summarise(actions: readonly Action[], noun: string): string {
  const count = (kind: Action["kind"]) => actions.filter((action) => action.kind === kind).length;
  const parts = [
    ["updated", count("update")],
    ["deferred", count("defer")],
    ["pinned out", count("pinned")],
  ] as const;

  const changes = parts.filter(([, n]) => n > 0).map(([name, n]) => `${n} ${name}`);
  const kept = count("keep");

  if (changes.length === 0) {
    return kept === 0
      ? `No ${noun}s to manage`
      : `${kept} ${noun}${kept === 1 ? "" : "s"} up to date, nothing to do`;
  }
  return [...changes, `${kept} up to date`].join(", ");
}

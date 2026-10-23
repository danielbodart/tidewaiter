import { type ConntrackEntry, type ConntrackSource, parseConntrack } from "./conntrack.ts";
import type { Action } from "./decide.ts";
import { NoInhibitor, type Inhibitor } from "./inhibit.ts";
import { runEvery } from "./loop.ts";
import type { Outcome, PassContext, Source, SourceName } from "./source.ts";

export interface Options {
  /** The label that opts a container in. */
  readonly label: string;
  /** Seconds between reconcile passes. */
  readonly interval: number;
  /** Seconds between the inhibitor's activity ticks. */
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

/**
 * The loop every source runs in, and everything they share.
 *
 * One pass dumps the host conntrack table once, runs each source in turn over
 * it (so two swaps never overlap, whichever sources they belong to), logs one
 * summary line when it changes, and sets the shutdown inhibitor to exactly the
 * workloads that are busy. Between passes a faster activity tick keeps that
 * lock current, so a player who joins just after a pass holds off a reboot
 * within seconds rather than one `interval` later.
 *
 * A source that throws does not stop the others; its workloads keep whatever
 * busy verdict they last had, so a source that cannot be read never releases a
 * lock it was holding.
 */
export class Daemon {
  constructor(
    private readonly sources: readonly Source[],
    private readonly conntrack: ConntrackSource,
    private readonly inhibitor: Inhibitor,
    private readonly options: Pick<Options, "interval" | "activityInterval">,
    private readonly log: Log = console.log,
  ) {}

  /** The last busy verdict per workload, per source - the inhibitor's input. */
  private readonly busy = new Map<SourceName, ReadonlyMap<string, boolean>>();
  /** Workloads being swapped right now, held for the rest of the pass. */
  private readonly held = new Set<string>();
  private lastSummary?: string;
  private lastConntrackError?: string;
  private lastLockError?: string;

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
        await this.lock();
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
      await this.lock();
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
   * Re-read activity only and refresh the lock: no digests, no decisions, no
   * idle streaks, so the tick can hold a reboot off but never hurry a swap.
   */
  async tick(): Promise<void> {
    const conntrack = await this.dumpConntrack();
    for (const source of this.sources) {
      try {
        const activity = await source.activity(conntrack);
        this.busy.set(source.name, new Map(activity.map((a) => [a.workload, a.busy])));
      } catch {
        // Keep the last verdicts; the next pass will say what went wrong.
      }
    }
    await this.lock();
  }

  /** The workloads the lock is held for right now. */
  inUse(): string[] {
    const names = new Set(this.held);
    for (const verdicts of this.busy.values()) {
      for (const [workload, busy] of verdicts) if (busy) names.add(workload);
    }
    return [...names].sort();
  }

  /**
   * Reconcile on an interval until the signal aborts, ticking activity in
   * between when there is a lock to keep, and releasing the lock on the way out.
   */
  async run(signal: AbortSignal): Promise<void> {
    const ticking = this.inhibitor === NoInhibitor ? Promise.resolve() : this.tickEvery(signal);
    try {
      await runEvery(this.options.interval, () => this.once(), signal, this.log);
      await ticking;
    } finally {
      await this.inhibitor.set([]).catch(() => {});
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
   * Remember this pass's busy verdicts for a source.
   *
   * A workload the source no longer reports is gone and drops out; one it
   * reports without a verdict (never sampled this pass) keeps its last one.
   */
  private record(source: SourceName, outcomes: readonly Outcome[]): void {
    const previous = this.busy.get(source) ?? new Map<string, boolean>();
    const next = new Map<string, boolean>();
    for (const { workload, busy } of outcomes) {
      const verdict = busy ?? previous.get(workload);
      if (verdict !== undefined) next.set(workload, verdict);
    }
    this.busy.set(source, next);
  }

  private async lock(): Promise<void> {
    try {
      await this.inhibitor.set(this.inUse());
      this.lastLockError = undefined;
    } catch (error) {
      // Logged once per distinct problem: the tick would otherwise repeat it
      // every few seconds. Never fatal - the lock protects players, and the
      // swap gate does not depend on it.
      const message = (error as Error).message;
      if (message !== this.lastLockError) this.log(`Warning: ${message}`);
      this.lastLockError = message;
    }
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

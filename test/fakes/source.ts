import type { ConntrackEntry } from "../../src/conntrack.ts";
import type { Inhibitor } from "../../src/inhibit.ts";
import type { Activity, Outcome, PassContext, Source, SourceName } from "../../src/source.ts";

/**
 * A source that reports whatever a test tells it to.
 *
 * `outcomes` is what each pass returns, `activities` what each tick does;
 * `holds` names workloads to hold the lock for mid-pass, and `seenHeld` records
 * what the inhibitor held at that moment, so a test can prove the lock was on
 * before the "swap". `failWith` makes the next pass throw.
 */
export class FakeSource implements Source {
  outcomes: Outcome[] = [];
  activities: Activity[] = [];
  holds: string[] = [];
  failWith?: Error;
  readonly contexts: PassContext[] = [];
  readonly ticks: (readonly ConntrackEntry[] | undefined)[] = [];

  constructor(readonly name: SourceName, private readonly inhibitor?: FakeInhibitor) {}

  readonly seenHeld: string[][] = [];

  async once(context: PassContext): Promise<Outcome[]> {
    this.contexts.push(context);
    if (this.failWith) throw this.failWith;
    for (const workload of this.holds) {
      await context.hold(workload);
      this.seenHeld.push([...(this.inhibitor?.held ?? [])]);
    }
    return this.outcomes;
  }

  async activity(conntrack: readonly ConntrackEntry[] | undefined): Promise<Activity[]> {
    this.ticks.push(conntrack);
    if (this.failWith) throw this.failWith;
    return this.activities;
  }

  async list(): Promise<string[]> {
    return this.outcomes.map((outcome) => outcome.workload);
  }
}

/** Records every set() and what is held now; `failWith` makes set() throw. */
export class FakeInhibitor implements Inhibitor {
  held: readonly string[] = [];
  readonly sets: (readonly string[])[] = [];
  failWith?: Error;

  async set(workloads: readonly string[]): Promise<void> {
    this.sets.push(workloads);
    if (this.failWith) throw this.failWith;
    this.held = workloads;
  }
}

import type { ConntrackEntry } from "../../src/conntrack.ts";
import type { BootCheck, Rebooter } from "../../src/reboot.ts";
import type { Activity, Outcome, PassContext, Source, SourceName } from "../../src/source.ts";
import type { Status, StatusSink } from "../../src/status.ts";

/**
 * A source that reports whatever a test tells it to.
 *
 * `outcomes` is what each pass returns, `activities` what each tick does;
 * `holds` names workloads to hold mid-pass, and `seenSwapping` records what
 * the status said was swapping at that moment, so a test can prove it said so
 * before the "swap". `failWith` makes the next pass throw.
 */
export class FakeSource implements Source {
  outcomes: Outcome[] = [];
  activities: Activity[] = [];
  holds: string[] = [];
  failWith?: Error;
  readonly contexts: PassContext[] = [];
  readonly ticks: (readonly ConntrackEntry[] | undefined)[] = [];

  constructor(readonly name: SourceName, private readonly status?: FakeStatus) {}

  readonly seenSwapping: (readonly string[])[] = [];

  async once(context: PassContext): Promise<Outcome[]> {
    this.contexts.push(context);
    if (this.failWith) throw this.failWith;
    for (const workload of this.holds) {
      await context.hold(workload);
      this.seenSwapping.push(this.status?.last?.swapping ?? []);
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

/** Records every status written; `failWith` makes write() throw. */
export class FakeStatus implements StatusSink {
  readonly written: Status[] = [];
  cleared = false;
  failWith?: Error;

  get last(): Status | undefined {
    return this.written.at(-1);
  }

  async write(status: Status): Promise<void> {
    if (this.failWith) throw this.failWith;
    this.written.push(status);
    this.cleared = false;
  }

  async clear(): Promise<void> {
    this.cleared = true;
  }
}

/** Says a reboot is needed when `needed` is set; `failWith` makes it unable to tell. */
export class FakeBootCheck implements BootCheck {
  reason?: string;
  failWith?: Error;

  async needed(): Promise<string | undefined> {
    if (this.failWith) throw this.failWith;
    return this.reason;
  }
}

/** Counts reboots; `failWith` makes logind refuse. */
export class FakeRebooter implements Rebooter {
  reboots = 0;
  failWith?: Error;

  async reboot(): Promise<void> {
    if (this.failWith) throw this.failWith;
    this.reboots++;
  }
}

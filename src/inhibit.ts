import type { Subprocess } from "bun";

/**
 * Holds off shutdown and sleep while something Tidewaiter manages is in use.
 *
 * The daemon calls set() with exactly the workloads that are busy (or being
 * swapped) after every pass and every activity tick; an empty list releases
 * the lock. Idempotent, so calling it every tick with the same list costs
 * nothing.
 */
export interface Inhibitor {
  /** Hold the lock for exactly these workloads; empty releases it. */
  set(workloads: readonly string[]): Promise<void>;
}

/** No lock at all: the container default, where there is no logind to ask. */
export const NoInhibitor: Inhibitor = {
  async set() {},
};

/**
 * The words logind shows for the lock, in `systemd-inhibit --list` and to a
 * person whose `systemctl reboot` it refuses.
 */
export function inhibitReason(workloads: readonly string[]): string | undefined {
  if (workloads.length === 0) return undefined;
  return `in use: ${[...new Set(workloads)].sort().join(", ")}`;
}

/**
 * A logind block lock on shutdown and sleep, held by a `systemd-inhibit` child.
 *
 * logind hands an inhibitor lock out as a file descriptor, and the lock lasts
 * exactly as long as that descriptor is open. Neither Bun's sockets nor the
 * hand-written D-Bus client can receive a descriptor (SCM_RIGHTS), so the lock
 * is held by a child instead:
 *
 *   systemd-inhibit --what=shutdown:sleep --mode=block --who=tidewaiter --why=... cat
 *
 * with `cat` reading a pipe from Tidewaiter. systemd-inhibit takes the lock and
 * only then runs `cat`, so a newline written into the pipe and echoed back
 * proves the lock is held. Releasing is closing the pipe; and if Tidewaiter
 * dies for any reason the kernel closes it, `cat` and systemd-inhibit exit,
 * and the lock goes with them - no lock can outlive the daemon.
 *
 * `block`, not `delay`: a delay lock only holds a shutdown for
 * InhibitDelayMaxSec (5s by default), useless against a session that lasts
 * hours. Since systemd 257 a block lock also refuses root, which is what
 * system.autoUpgrade reboots as (see docs/systemd-source.md, section 6).
 *
 * A new reason takes the new lock before the old one is dropped, so there is
 * never a moment with no lock while something is busy. Calls are serialised,
 * so overlapping set()s from a pass and a tick apply in order.
 */
export class SystemdInhibitor implements Inhibitor {
  private held?: { readonly why: string; readonly child: Subprocess<"pipe", "pipe", "pipe"> };
  private queue: Promise<void> = Promise.resolve();

  /** `command` is the systemd-inhibit to run; tests point it at a stand-in. */
  constructor(private readonly command: readonly string[] = ["systemd-inhibit"]) {}

  set(workloads: readonly string[]): Promise<void> {
    const why = inhibitReason(workloads);
    const applied = this.queue.then(() => this.apply(why));
    this.queue = applied.catch(() => {});
    return applied;
  }

  /** The reason currently held, for tests and `list`. */
  get reason(): string | undefined {
    return this.held?.why;
  }

  private async apply(why: string | undefined): Promise<void> {
    // The same lock, still alive: nothing to do. A child that died under us
    // (someone killed it) is re-taken rather than trusted.
    if (why === this.held?.why && (why === undefined || running(this.held!.child))) return;

    const previous = this.held;
    this.held = why === undefined ? undefined : { why, child: await this.acquire(why) };
    if (previous) await release(previous.child);
  }

  private async acquire(why: string): Promise<Subprocess<"pipe", "pipe", "pipe">> {
    const child = Bun.spawn(
      [...this.command, "--what=shutdown:sleep", "--mode=block", "--who=tidewaiter", `--why=${why}`, "cat"],
      { stdin: "pipe", stdout: "pipe", stderr: "pipe" },
    );
    child.stdin.write("\n");
    await child.stdin.flush();

    // cat only runs once the lock is held, so the echo is the acknowledgement;
    // a systemd-inhibit that could not take it exits, closing stdout unechoed.
    const reader = child.stdout.getReader();
    const echoed = await reader.read().then((chunk) => !chunk.done && chunk.value.length > 0, () => false);
    reader.releaseLock();
    if (echoed) return child;

    child.kill();
    const stderr = await new Response(child.stderr).text().catch(() => "");
    await child.exited;
    throw new Error(
      `could not take the shutdown inhibitor (${this.command[0]} exited ${child.exitCode}: ${stderr.trim().slice(0, 300)})`,
    );
  }
}

/** Whether the lock's child is still there: neither exited nor killed by a signal. */
function running(child: Subprocess): boolean {
  return child.exitCode === null && child.signalCode === null;
}

/** Close the lock's pipe and wait for the child to go, which drops the lock. */
async function release(child: Subprocess<"pipe", "pipe", "pipe">): Promise<void> {
  try {
    await child.stdin.end();
  } catch {
    // Already gone - and the lock with it.
  }
  await child.exited;
}

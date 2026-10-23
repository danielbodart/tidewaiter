import type { Log } from "./daemon.ts";

/**
 * Run `pass` now and then every `interval` seconds until the signal aborts.
 *
 * Portical's single-flight closure, minus the event stream: a burst of ticks
 * still collapses into one queued pass, a pass that throws is logged and the
 * next tick tries again, and an abort waits for the pass in flight to finish
 * rather than cutting a swap off halfway.
 *
 * Lifted out of the Docker daemon so every source shares one loop: the systemd
 * source's passes and the Docker source's run in the same queue, one at a time,
 * so a swap in one never overlaps a swap in the other.
 */
export async function runEvery(
  interval: number,
  pass: () => Promise<unknown>,
  signal: AbortSignal,
  log: Log,
): Promise<void> {
  let pending: Promise<void> = Promise.resolve();
  let queued = false;

  const queue = () => {
    if (queued) return pending;
    queued = true;
    pending = pending.then(async () => {
      queued = false;
      try {
        await pass();
      } catch (error) {
        log(`Error: ${(error as Error).message}`);
      }
    });
    return pending;
  };

  await queue();

  const ticker = setInterval(queue, interval * 1000);
  signal.addEventListener("abort", () => clearInterval(ticker), { once: true });
  log(`Reconciling every ${interval}s...`);

  try {
    await new Promise<void>((resolve) => {
      if (signal.aborted) return resolve();
      signal.addEventListener("abort", () => resolve(), { once: true });
    });
  } finally {
    clearInterval(ticker);
    await pending;
  }
}

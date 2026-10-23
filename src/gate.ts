import { combine, type CheckResult, type HealthResult } from "./health.ts";

export interface GateTiming {
  /** How long to wait for a verdict before trusting the update anyway. */
  readonly timeoutSeconds: number;
  /** How often to re-evaluate the checks while waiting, in milliseconds. */
  readonly pollMillis: number;
  readonly now: () => number;
}

/**
 * Poll a set of health checks until they settle, the deadline passes, or the
 * signal aborts.
 *
 * `evaluate` runs every check once against fresh state - a re-inspected
 * container for Docker, re-read unit properties for systemd - and the verdict
 * each time is combine()'s: roll back on any active failure, commit once every
 * applicable check has passed, otherwise keep polling. At the deadline it
 * commits unless something actively failed, so a check that merely never
 * confirmed cannot veto.
 *
 * Shared by every source, so the gate's rules are written once. `aborted`
 * means another arm of a race (the Docker health_status event watch) settled
 * first and tore this one down; the caller decides what that means.
 */
export async function pollVerdict(
  evaluate: (signal: AbortSignal) => Promise<CheckResult[]>,
  timing: GateTiming,
  signal: AbortSignal,
): Promise<HealthResult | "aborted"> {
  const deadline = timing.now() + timing.timeoutSeconds * 1000;

  try {
    while (timing.now() < deadline && !signal.aborted) {
      const verdict = combine(await evaluate(signal), false);
      if (verdict !== undefined) return verdict;
      await sleep(timing.pollMillis, signal);
    }
  } catch (error) {
    if (signal.aborted) return "aborted";
    return { healthy: false, reason: `health check errored: ${(error as Error).message}` };
  }

  if (signal.aborted) return "aborted";
  // Deadline reached: commit unless something actively failed (combine with
  // timedOut=true), so a check that merely never confirmed cannot veto.
  const results = await evaluate(signal).catch(() => [] as CheckResult[]);
  return combine(results, true) ?? { healthy: true };
}

/** Sleep, waking early (and quietly) if the signal aborts. */
export function sleep(millis: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const timer = setTimeout(resolve, millis);
    signal.addEventListener("abort", () => { clearTimeout(timer); resolve(); }, { once: true });
  });
}

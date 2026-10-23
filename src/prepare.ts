import type { ClosureBinder } from "./closure.ts";
import type { Log } from "./daemon.ts";
import type { Profiles } from "./profile.ts";
import type { SystemdClient } from "./systemd.ts";
import { parseUnitPolicy, type UnitsDirectory } from "./units.ts";

export interface PrepareDependencies {
  readonly units: UnitsDirectory;
  readonly profiles: Profiles;
  readonly closure: ClosureBinder;
  /** Reloads systemd once the drop-ins are written; only daemonReload is used. */
  readonly systemd: Pick<SystemdClient, "daemonReload">;
  readonly log: Log;
}

export interface Prepared {
  /** Units whose profile did not exist and now points at their desired version. */
  readonly seeded: readonly string[];
  /** Units whose closure drop-in was written; one already there is left alone. */
  readonly bound: readonly string[];
  /** Units that could not be prepared; their errors are logged. */
  readonly failed: readonly string[];
}

/**
 * Make every declared unit startable, before anything starts it.
 *
 * Two things a unit running from a profile needs that nothing else provides:
 *
 * - **A profile.** A unit declared for the first time has none, and its
 *   ExecStart would fail. It is set to `desired` straight away: nothing is
 *   running on it, so there is nobody to wait for. An existing profile is never
 *   touched here - moving it is the daemon's job, when the unit is idle.
 * - **Its closure drop-in**, for a confined unit (`bindClosure`). The drop-in
 *   lives in /run, so it is gone after every reboot, and is written for
 *   whatever the profile points at now - which after a rollback is not what the
 *   unit's own definition names. Only written when missing, or bound for a
 *   different profile: once one exists the daemon is its only writer, since a
 *   prepare racing a swap could otherwise put the old closure back between the
 *   daemon's bind and its restart.
 *
 * Then one daemon-reload, so the drop-ins are in force before the units
 * start. Run by the NixOS module's tidewaiter-prepare.service, ordered before
 * every managed unit and re-run on every switch that changes a declaration.
 * One unit failing does not stop the rest.
 */
export async function prepare(deps: PrepareDependencies): Promise<Prepared> {
  const { log } = deps;
  const seeded: string[] = [];
  const bound: string[] = [];
  const failed: string[] = [];
  const done = new Set<string>();

  for (const file of await deps.units.read()) {
    const { policy, warnings } = parseUnitPolicy(file.text, file.name);
    for (const warning of warnings) log(warning);
    if (!policy || done.has(policy.unit)) continue;
    done.add(policy.unit);

    const { unit, profile, desired } = policy;
    try {
      let current = await deps.profiles.current(profile);
      if (current === undefined) {
        await deps.profiles.set(profile, desired);
        current = desired;
        seeded.push(unit);
        log(`${unit}: created ${profile} on ${desired}`);
      }
      if (policy.bindClosure && (await deps.closure.ensure(unit, profile, current))) bound.push(unit);
    } catch (error) {
      failed.push(unit);
      log(`${unit}: could not prepare it (${(error as Error).message})`);
    }
  }

  if (bound.length > 0) {
    try {
      await deps.systemd.daemonReload();
    } catch (error) {
      log(`Error: wrote the closure drop-ins but could not reload systemd (${(error as Error).message})`);
      return { seeded, bound, failed: [...failed, ...bound] };
    }
  }
  return { seeded, bound, failed };
}

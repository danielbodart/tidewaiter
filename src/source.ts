import type { ConntrackEntry } from "./conntrack.ts";
import type { Action } from "./decide.ts";

/**
 * What a source is handed for one pass.
 *
 * The daemon owns everything a pass shares across sources - one conntrack dump,
 * the shutdown inhibitor - and passes it in, so two sources never dump the
 * table twice and never fight over the lock.
 */
export interface PassContext {
  /**
   * The host conntrack table, dumped once per pass and shared by every source.
   *
   * Undefined when the dump failed - "could not tell", which a detector that
   * needs the table must read as busy, never as an empty table.
   */
  readonly conntrack: readonly ConntrackEntry[] | undefined;
  /**
   * Take the shutdown inhibitor for `workload` until the end of the pass.
   *
   * Called before any swap starts, so a reboot cannot land mid-swap. Never
   * throws: a lock that could not be taken is logged by the daemon, and the swap
   * goes ahead - the lock protects players, and an idle workload has none.
   */
  hold(workload: string): Promise<void>;
}

/** What one source concluded about one workload this pass. */
export interface Outcome {
  /** "chandlery-bedrock.service", or a container name. Unique within a source. */
  readonly workload: string;
  readonly action: Action;
  /**
   * The activity verdict this pass, fail-safe (could-not-tell reads as busy).
   *
   * Undefined when the pass never got as far as sampling - a registry that did
   * not answer, a unit systemd could not describe. The daemon then keeps what
   * it last knew about the workload rather than guessing either way.
   */
  readonly busy?: boolean;
}

/** One workload's activity, for the inhibitor's faster tick. */
export interface Activity {
  readonly workload: string;
  readonly busy: boolean;
}

/**
 * A kind of thing Tidewaiter keeps up to date: Docker containers, systemd units.
 *
 * Unlike Portical's sources, which only describe desired state for a shared
 * reconcile, a Tidewaiter source also acts - how a container is swapped has
 * nothing in common with how a unit is - so a source runs its own pass, built
 * from the shared pieces (decide, the detectors, the health checks and gate).
 */
export interface Source {
  readonly name: SourceName;
  /** Reconcile every workload once. One workload failing never fails the pass. */
  once(context: PassContext): Promise<Outcome[]>;
  /** Activity only - no digests, no swaps, no idle streaks - for the inhibitor's tick. */
  activity(conntrack: readonly ConntrackEntry[] | undefined): Promise<Activity[]>;
  /** One line per workload for `tidewaiter list`. */
  list(): Promise<string[]>;
}

export type SourceName = "docker" | "systemd";

/**
 * Consecutive swap-step failures on one target before it is pinned out.
 *
 * Shared by every source: a single failure may be a transient hiccup (a socket
 * blip, a busy Nix daemon) that says nothing about the version, but a
 * deterministic one would otherwise be retried every pass forever.
 */
export const MAX_SWAP_FAILURES = 3;

import type { Digest, DesiredImage, RunningContainer } from "./model.ts";
import type { ContainerPolicy } from "./labels.ts";

/**
 * The part of a snapshot decide() actually reads, whatever is being updated.
 *
 * Kept apart from Snapshot so the systemd source can judge a unit with the
 * same rules: there `currentDigest` and `desired.digest` are Nix store paths
 * (the profile's target and the module's desired path) rather than manifest
 * digests, but they are just as opaque - compared, never parsed - so nothing
 * about the decision changes.
 */
export interface Decidable {
  readonly policy: { readonly idleSamples: number };
  readonly currentDigest: Digest | undefined;
  readonly desired: { readonly digest: Digest };
  readonly inUse: boolean;
  readonly idleStreak: number;
}

/**
 * Everything decide() needs to know about one container, gathered up front.
 *
 * The daemon does all the I/O - list containers, resolve the registry digest,
 * sample activity, count idle streaks - and hands the result in as plain data.
 * decide() then makes the call with no Docker, no registry, no netns and,
 * crucially, no clock: "how long has this been idle" arrives as `idleStreak`, a
 * number the daemon already counted, exactly as portical's reconcile() takes a
 * lease as a number rather than reading the time itself.
 */
export interface Snapshot extends Decidable {
  readonly container: RunningContainer;
  readonly policy: ContainerPolicy;
  /**
   * The manifest digest the container's image is on right now.
   *
   * The local image's RepoDigest for its tag - the SAME kind of hash the
   * registry returns, so the two compare. Resolved by the daemon via
   * imageDigest(), not read off inspect (whose `.Image` is a config ID in a
   * different namespace that would never match). Undefined when the image is not
   * present locally at all, which reads as "something to pull".
   */
  readonly currentDigest: Digest | undefined;
  /** The registry's digest for this container's tag, resolved this pass. */
  readonly desired: DesiredImage;
  /**
   * Whether the activity gate says the container is busy right now.
   *
   * Already the conservative combination of the chosen detectors, and already
   * fail-safe: a detector that could not tell reads as `true` here. decide()
   * takes this at face value.
   */
  readonly inUse: boolean;
  /**
   * Consecutive passes the container has been seen idle, counted by the daemon.
   *
   * An update is only safe once this reaches the policy's `idleSamples`, so a
   * brief lull between packets does not read as an empty server.
   */
  readonly idleStreak: number;
}

/**
 * The per-container memory the daemon carries between passes.
 *
 * decide() only ever reads this; the daemon updates it as a consequence of what
 * apply() actually does. `pinnedDigest` is the one that changes a decision: a
 * container whose last update failed is pinned to the digest that failed, and
 * left alone until the registry moves to a different one - chasing a broken
 * image every pass would just fail every pass and keep restarting the service.
 */
export interface ContainerState {
  readonly pinnedDigest?: Digest;
}

/**
 * What should happen to one container this pass.
 *
 * Deliberately scalar, not a list: unlike portical, which reconciles a whole
 * set of forwards against a whole set of mappings and derives removals from
 * absence, Tidewaiter judges each container independently and never removes
 * one. The daemon maps decide() over the containers.
 *
 * Note what is NOT here: `rollback`. A rollback is a consequence of a health
 * check that has not happened when decide() runs - the daemon has to pull,
 * stop, recreate and wait before it knows. So the health-gate and its rollback
 * live entirely in the daemon's apply() for an `update`, the same way portical
 * keeps the delete-then-add ordering of a `replace` out of its pure core.
 */
export type Action =
  | { readonly kind: "keep"; readonly reason: string }
  | { readonly kind: "update"; readonly from: Digest | undefined; readonly to: Digest; readonly reason: string }
  | { readonly kind: "defer"; readonly reason: string }
  | { readonly kind: "pinned"; readonly reason: string };

/**
 * How decide() words its reasons, so a unit's log does not talk about images.
 * The rules are the same whatever is being updated; only the nouns change.
 */
export interface Words {
  readonly pinned: string;
  readonly keep: string;
  readonly busy: string;
  /** Completes "idle for N samples and ...". */
  readonly update: string;
}

export const IMAGE_WORDS: Words = {
  pinned: "a previous update to this image failed its health check; pinned out until the tag moves on",
  keep: "already on the latest image",
  busy: "the tide is in - the container has live flows",
  update: "a newer image is available",
};

export const UNIT_WORDS: Words = {
  pinned: "a previous update to this version failed its health check; pinned out until the desired version moves on",
  keep: "the profile is already on the desired version",
  busy: "the tide is in - the unit has live flows",
  update: "the desired version differs from the profile's",
};

/**
 * Decide what to do with one container, purely.
 *
 * The order of the checks is the policy:
 *
 *   1. Already on the desired digest -> keep. Nothing to chase.
 *   1. The desired digest is the one a previous update failed on -> pinned.
 *      Checked FIRST, before keep: a rollback pulls the (bad) image, so the
 *      local tag digest ends up equal to desired even though the container was
 *      put back on the old image. Without the pin winning here, that would read
 *      as "already on the latest" and the failure would be forgotten.
 *   2. Already on the desired digest -> keep. Nothing to chase.
 *   3. Busy, or not yet idle for long enough -> defer. Never disrupt a live
 *      session to chase a version; the version will still be there next pass.
 *   4. Otherwise -> update.
 */
export function decide(snapshot: Decidable, state: ContainerState, words: Words = IMAGE_WORDS): Action {
  const { currentDigest, desired, policy, inUse, idleStreak } = snapshot;
  const current = currentDigest;

  if (state.pinnedDigest !== undefined && state.pinnedDigest === desired.digest) {
    return {
      kind: "pinned",
      reason: words.pinned,
    };
  }

  if (desired.digest === current) {
    return { kind: "keep", reason: words.keep };
  }

  if (inUse) {
    return { kind: "defer", reason: words.busy };
  }

  if (idleStreak < policy.idleSamples) {
    return {
      kind: "defer",
      reason: `idle ${idleStreak}/${policy.idleSamples} samples - waiting for the debounce window`,
    };
  }

  return {
    kind: "update",
    from: current,
    to: desired.digest,
    reason: `idle for ${idleStreak} samples and ${words.update}`,
  };
}

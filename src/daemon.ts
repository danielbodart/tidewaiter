import { type ConntrackEntry, type ConntrackSource, parseConntrack } from "./conntrack.ts";
import { decide, type Action, type ContainerState, type Snapshot } from "./decide.ts";
import { combine, detectorByName, netioDetector, type Detector, type DetectorResult } from "./detectors.ts";
import type { DockerClient } from "./docker.ts";
import {
  healthProberByName,
  uptimeHealthProber,
  verdictForDockerHealth,
  type HealthProber,
  type HealthResult,
} from "./health.ts";
import { parseLabels, type ContainerPolicy } from "./labels.ts";
import type { Digest, PublishedPort, RunningContainer } from "./model.ts";
import type { NetnsReader } from "./netns.ts";
import { planSwap, recreatable, rollbackName, type SwapStep } from "./recreate.ts";
import { parseRef, type AuthSource, type RegistryClient } from "./registry.ts";

/**
 * How far a swap got, so a failure can be undone precisely.
 *
 * `parked` is the load-bearing one: it is true only once the old container has
 * been renamed out of the way, which is what tells the rollback whether the
 * original still wears its own name (do not touch it) or has been safely parked
 * (restore it).
 */
type SwapProgress =
  | { readonly ok: true; readonly stopped: boolean; readonly parked: boolean; readonly created: boolean; readonly started: boolean }
  | { readonly ok: false; readonly error: Error; readonly stopped: boolean; readonly parked: boolean; readonly created: boolean; readonly started: boolean };

export interface Options {
  /** The label that opts a container in. */
  readonly label: string;
  /** Seconds between reconcile passes. */
  readonly interval: number;
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
  dryRun: false,
  defaultGraceSeconds: 10,
  healthPollMillis: 2000,
};

export type Log = (message: string) => void;

/** Per-container memory carried between passes. Extends decide()'s ContainerState. */
interface State extends ContainerState {
  readonly idleStreak: number;
  /** The image the container ran before the last update, for rollback and prune. */
  readonly previousImage?: { readonly ref: string; readonly digest: Digest };
  /** Digests retained beyond the current one, oldest first, for keep-images. */
  readonly keptImages: readonly Digest[];
  /** True once we have logged the "adopting as baseline" note for this container. */
  readonly adopted?: boolean;
}

const INITIAL: State = { idleStreak: 0, keptImages: [] };

/**
 * The reconcile loop, reshaped from portical for updates instead of forwards.
 *
 * The spine is portical's: an interval-driven, single-flight, summary-on-change
 * loop with per-item failure isolation. What changed: the trigger is the
 * interval alone (a newer registry digest is not a Docker event, so there is
 * nothing to watch for), containers are processed one at a time (a swap can
 * take minutes at the health gate, and must not starve the others), and the
 * per-container decision is pure decide() with all its inputs gathered first.
 */
export class Tidewaiter {
  constructor(
    private readonly docker: DockerClient,
    private readonly registry: RegistryClient,
    private readonly conntrack: ConntrackSource,
    private readonly netns: NetnsReader,
    private readonly options: Options,
    private readonly log: Log = console.log,
    /** Injected for the uptime prober's clock; tests supply a controllable one. */
    private readonly now: () => number = Date.now,
    /**
     * Resolves an X-Registry-Auth value for an image ref, for private pulls.
     *
     * The same credential source the registry client uses for digest lookups,
     * threaded through so `docker pull` is authenticated too - without it a
     * private image resolves a digest fine and then fails to pull. Undefined
     * means "anonymous everywhere", which is fine for public images.
     */
    private readonly auth?: AuthSource,
  ) {}

  private readonly state = new Map<string, State>();
  private lastSummary?: string;

  /**
   * The netio detector, built once on first use and kept, because it is
   * stateful - it remembers each container's previous byte sample to compute a
   * rate across passes. Lazy rather than a field initializer so it does not race
   * the constructor's parameter-property assignment of `docker`/`now`. The
   * stateless detectors come from detectorByName() per pass.
   */
  private netioSingleton?: Detector;

  private netioFor(): Detector {
    return (this.netioSingleton ??= netioDetector({
      netBytes: (id) => this.docker.netBytes(id),
      now: this.now,
    }));
  }

  /**
   * One pass: bring every opted-in container into line with its registry, once.
   *
   * Container listing failures propagate rather than being swallowed - a pass
   * that cannot see the containers must not proceed as if there were none.
   */
  async once(): Promise<Action[]> {
    const containers = await this.docker.containers(this.options.label);

    // The host conntrack table, dumped once and shared by every container's
    // detector this pass - NAT happens host-wide, so one dump serves all.
    // A failed dump is not fatal: each detector that needs it will read an
    // empty table, and (being unable to tell) the daemon's fail-safe applies.
    const conntrack = await this.dumpConntrack();

    const actions: Action[] = [];
    for (const container of containers) {
      actions.push(await this.reconcile(container, conntrack));
    }

    const summary = summarise(actions);
    if (summary !== this.lastSummary) {
      this.log(summary);
      this.lastSummary = summary;
    }
    return actions;
  }

  private async dumpConntrack(): Promise<readonly ConntrackEntry[]> {
    try {
      return parseConntrack(await this.conntrack.dump());
    } catch (error) {
      this.log(`Warning: could not read conntrack (${(error as Error).message}); assuming busy where it matters`);
      return [];
    }
  }

  /** Decide and apply for one container, isolated so its failure spares the rest. */
  private async reconcile(container: RunningContainer, conntrack: readonly ConntrackEntry[]): Promise<Action> {
    const name = container.spec.name;
    const { policy, warnings } = parseLabels(container.spec.labels, container.spec.published);
    for (const warning of warnings) this.log(`${name}: ${warning}`);

    // Not opted in (or an unrecognised autoupdate value - the warning above said
    // so): leave it completely alone. The Docker query already filters on the
    // label key, so reaching here with no policy means a bad value, not a
    // missing label.
    if (policy === undefined) {
      return { kind: "keep", reason: "not opted in" };
    }

    let previous = this.state.get(name) ?? INITIAL;

    // What the tag should resolve to now, per the container's autoupdate policy:
    //   registry - ask the registry for the tag's current manifest digest.
    //   local    - read the locally-stored image for the tag; no registry
    //              contact, so it updates when a build/pull changed it on the
    //              host. Undefined (image gone locally) means nothing to do.
    // Either source failing leaves the container alone this pass - never updated
    // on a guess.
    let desiredDigest: Digest | undefined;
    try {
      desiredDigest = policy.autoupdate === "local"
        ? await this.docker.imageDigest(container.spec.image)
        : await this.registry.digest(container.spec.image);
    } catch (error) {
      const source = policy.autoupdate === "local" ? "local image" : "registry";
      this.log(`${name}: could not resolve ${source} digest (${(error as Error).message}); leaving it alone`);
      return { kind: "keep", reason: `${source} unreachable` };
    }
    if (desiredDigest === undefined) {
      // local policy and the tag is not present locally - nothing to move to.
      return { kind: "keep", reason: "no local image for the tag to update to" };
    }

    // The manifest digest the container is actually running: the RepoDigest of
    // the *specific image the container was created from*, looked up by that
    // image's config ID (container.imageId), NOT by the tag. Two hazards this
    // avoids:
    //   - inspect's `.Image` is a config ID, a different hash from a manifest
    //     digest; comparing it to the registry answer would never match, so
    //     every container would look outdated and update every pass.
    //   - looking up by tag would drift the moment a pull moves the tag: after a
    //     failed swap or a rollback, the tag points at the new (bad) image while
    //     the container still runs the old one. Keying on the container's own
    //     image id pins "current" to what is truly running.
    // Undefined means that image has no registry digest locally (e.g. built, not
    // pulled), which reads as "something to pull".
    let currentDigest: Digest | undefined;
    try {
      currentDigest = await this.docker.imageDigest(container.imageId);
    } catch (error) {
      this.log(`${name}: could not read the running image digest (${(error as Error).message}); leaving it alone`);
      return { kind: "keep", reason: "running image digest unreadable" };
    }

    // A pin only holds until the tag moves on; clear it the moment it does.
    if (previous.pinnedDigest !== undefined && previous.pinnedDigest !== desiredDigest) {
      previous = { ...previous, pinnedDigest: undefined };
    }

    // First time we have seen this container and it is on neither the desired
    // digest nor a previous one we recorded: adopt it as the baseline rather
    // than assume anything about an in-flight swap we have no memory of.
    if (
      !previous.adopted &&
      currentDigest !== desiredDigest &&
      previous.previousImage?.digest !== currentDigest
    ) {
      this.log(
        `${name}: adopting the running image as the current baseline (no rollback record from a prior run)`,
      );
      previous = { ...previous, adopted: true };
    }

    const ports = policy.ports ?? container.spec.published;
    const activity = await this.sample(container, policy, ports, conntrack);
    const idleStreak = activity.inUse ? 0 : previous.idleStreak + 1;

    const snapshot: Snapshot = {
      container,
      policy,
      currentDigest,
      desired: { container: name, ref: container.spec.image, digest: desiredDigest },
      inUse: activity.inUse,
      idleStreak,
    };

    const action = decide(snapshot, previous);
    const next = await this.apply(action, container, policy, previous, idleStreak, desiredDigest, currentDigest);
    this.state.set(name, next);
    return action;
  }

  /**
   * Sample activity, fail-safe.
   *
   * A detector that throws - conntrack missing, /proc gone because the
   * container just died - must never abort the pass or read as idle. It is
   * caught here and folded in as "in use, low confidence", so uncertainty
   * always defers rather than disrupts. A misconfigured `ports` override
   * (empty after validation) is the same: cannot tell, so assume busy.
   */
  private async sample(
    container: RunningContainer,
    policy: ContainerPolicy,
    ports: readonly PublishedPort[],
    conntrack: readonly ConntrackEntry[],
  ): Promise<DetectorResult> {
    if (policy.ports !== undefined && policy.ports.length === 0) {
      return { inUse: true, confidence: "low" };
    }

    // netio is stateful and needs Docker + a clock, so it is built via
    // netioFor() rather than resolved by name, mirroring the uptime prober.
    const detector = policy.detector === "netio" ? this.netioFor() : detectorByName(policy.detector);
    if (!detector) {
      return { inUse: true, confidence: "low" };
    }

    try {
      const result = await detector.inUse({
        id: container.id,
        pid: container.pid,
        published: ports,
        conntrack,
        netns: this.netns,
      });
      return combine([result]);
    } catch (error) {
      this.log(
        `${container.spec.name}: activity detector '${policy.detector}' failed (${(error as Error).message}); assuming busy`,
      );
      return { inUse: true, confidence: "low" };
    }
  }

  private async apply(
    action: Action,
    container: RunningContainer,
    policy: ContainerPolicy,
    previous: State,
    idleStreak: number,
    desiredDigest: Digest,
    currentDigest: Digest | undefined,
  ): Promise<State> {
    const name = container.spec.name;
    const base: State = { ...previous, idleStreak };

    switch (action.kind) {
      case "keep":
        return base;

      case "defer":
        this.log(`${name}: ${action.reason}`);
        return base;

      case "pinned":
        // pinnedDigest is already set from a prior failed update; keep it.
        return { ...base, pinnedDigest: desiredDigest };

      case "update": {
        this.log(`${name}: updating - ${action.reason}`);
        if (this.options.dryRun) {
          this.log(`${name}: dry run, not touching it`);
          return base;
        }
        return this.performUpdate(container, policy, base, desiredDigest, currentDigest);
      }
    }
  }

  /**
   * The swap: pull, stop, rename-not-delete recreate, health-gate, commit or
   * roll back. Wrapped so any failure logs and leaves the container running
   * whatever it was - one bad update must not take the daemon down.
   *
   * Two things a naive version gets wrong, both fixed here:
   *
   *   - A swap step (not just the health check) can fail, and the rollback that
   *     is safe *after a complete swap* is dangerous partway through one. So the
   *     swap is executed with a progress checkpoint, and rollback is derived
   *     from how far it actually got - never force-removing the original before
   *     it has been safely parked.
   *
   *   - A transient Docker error during the swap must not pin the tag. Pinning
   *     means "this image is bad, stop trying it", and a bounced API call says
   *     nothing about the image. Only a genuine health-check failure of a
   *     successfully-stood-up new container pins.
   */
  private async performUpdate(
    container: RunningContainer,
    policy: ContainerPolicy,
    base: State,
    desiredDigest: Digest,
    currentDigest: Digest | undefined,
  ): Promise<State> {
    const name = container.spec.name;
    const spec = container.spec;

    const recreatability = recreatable(spec);
    if (!recreatability.ok) {
      this.log(`${name}: ${recreatability.reason}`);
      return base;
    }

    // A container sharing another's network namespace is recreated verbatim, but
    // the reference is fragile: if that other container was itself recreated, the
    // `container:<id>` mode names an id that no longer exists. Warn - once, at the
    // moment of an actual swap rather than every idle pass - rather than fail,
    // since it works fine as long as the referenced container is stable.
    if (spec.networkMode?.startsWith("container:")) {
      this.log(
        `${name}: network mode is '${spec.networkMode}', which shares another container's netns; ` +
          "recreating it verbatim - if that container was replaced the reference may be stale",
      );
    }

    const grace = policy.graceSeconds ?? spec.stopTimeout ?? this.options.defaultGraceSeconds;
    // The digest we are leaving, for rollback bookkeeping and keep-images. When
    // the image was somehow not present locally, there is nothing to roll back
    // to, so previousImage stays undefined and a health failure cannot pretend
    // to restore something that never existed.
    const previousImage =
      currentDigest === undefined ? undefined : { ref: spec.image, digest: currentDigest };
    // The tag is re-pulled unpinned: the ref does not change, only what it
    // resolves to. planSwap takes both specs as data; they are the same object
    // here because chasing the tag *is* the update.
    const newSpec = { ...spec, image: spec.image };
    const plan = planSwap(spec, newSpec, grace);

    // Only the registry policy pulls: `local` deliberately never touches the
    // network - the newer image is already on the host (that is the whole point
    // of the policy), so pulling would defeat it and could even fail on an
    // air-gapped or build-on-host box.
    if (policy.autoupdate === "registry") {
      try {
        await this.docker.pull(spec.image, this.authFor(spec.image));
      } catch (error) {
        this.log(`${name}: FAILED to pull ${spec.image} (${(error as Error).message}); leaving it running`);
        return base;
      }
    }

    // Stand up the new container, tracking progress so a failure rolls back
    // exactly what was done and no more.
    const progress = await this.runSwap(plan.swap, name);
    if (!progress.ok) {
      this.log(`${name}: FAILED during swap (${progress.error.message}); undoing what was done`);
      await this.undoSwap(plan, progress, name);
      // A swap-step failure is infrastructure, not a bad image: do not pin.
      return { ...base, previousImage };
    }

    const health = await this.waitHealthy(name, policy);
    if (health.healthy) {
      await this.runSteps(plan.commit).catch((error) =>
        this.log(`${name}: updated, but cleaning up the old container failed (${(error as Error).message})`),
      );
      const kept = previousImage
        ? await this.prune(previousImage.digest, base.keptImages, policy.keepImages, name)
        : base.keptImages;
      this.log(`${name}: updated to ${desiredDigest} and it is healthy`);
      return {
        idleStreak: base.idleStreak,
        keptImages: kept,
        previousImage,
        adopted: true,
      };
    }

    // A genuinely unhealthy new image, after a complete swap. This is the one
    // case that both rolls fully back and pins the tag out.
    this.log(`${name}: new image is unhealthy (${health.reason}); rolling back`);
    await this.undoSwap(plan, progress, name);
    this.log(`${name}: rolled back to the previous image and pinned it out until the tag moves on`);
    return { ...base, pinnedDigest: desiredDigest, previousImage };
  }

  /**
   * Execute the swap steps, remembering how far it got.
   *
   * The checkpoint is what makes rollback safe: `parked` becomes true only once
   * the old container has been renamed away, so undoSwap knows whether the
   * original still wears its own name (leave it be) or has been parked (restore
   * it). `created` gates removing the new container - there is nothing to remove
   * if create never ran.
   */
  private async runSwap(steps: readonly SwapStep[], name: string): Promise<SwapProgress> {
    const progress = { stopped: false, parked: false, created: false, started: false };
    try {
      for (const step of steps) {
        await this.runStep(step);
        switch (step.op) {
          case "stop": progress.stopped = true; break;
          case "rename": progress.parked = true; break;
          case "create": progress.created = true; break;
          case "start": progress.started = true; break;
        }
      }
      return { ok: true, ...progress };
    } catch (error) {
      return { ok: false, error: error as Error, ...progress };
    }
  }

  /**
   * Undo a swap given how far it got, safely and best-effort.
   *
   * Never force-removes the original before it has been parked - the bug a blind
   * rollback caused. The order is: drop the new container if one was created,
   * then either restore the parked original or, if it was never parked, just
   * make sure the still-named original is running again.
   */
  private async undoSwap(plan: ReturnType<typeof planSwap>, progress: SwapProgress, name: string): Promise<void> {
    const parked = rollbackName(name);
    try {
      // The new container, if any, wears the original name (it was created after
      // the old one was renamed away). Remove it so the name is free.
      if (progress.created) {
        await this.docker.remove(name, { force: true });
      }
      if (progress.parked) {
        // Old container safely parked: put its name back and start it.
        await this.docker.rename(parked, name);
        await this.docker.start(name);
      } else if (progress.stopped) {
        // Old container was only stopped, never renamed - it still has its own
        // name. Nothing to remove or rename; just start it back up.
        await this.docker.start(name);
      }
    } catch (error) {
      this.log(`${name}: rollback itself failed (${(error as Error).message}); manual attention needed`);
    }
    // plan is unused beyond documenting intent; the safe rollback is derived
    // from progress, not walked blindly from plan.rollback.
    void plan;
  }

  /** Walk an ordered list of swap steps against Docker (used for commit). */
  private async runSteps(steps: readonly SwapStep[]): Promise<void> {
    for (const step of steps) await this.runStep(step);
  }

  private async runStep(step: SwapStep): Promise<void> {
    switch (step.op) {
      case "stop":
        await this.docker.stop(step.container, step.graceSeconds);
        break;
      case "rename":
        await this.docker.rename(step.container, step.to);
        break;
      case "create":
        await this.docker.create(step.spec);
        break;
      case "connect":
        await this.docker.connectNetwork(step.container, step.network);
        break;
      case "start":
        await this.docker.start(step.container);
        break;
      case "remove":
        await this.docker.remove(step.container, { force: step.force });
        break;
    }
  }

  /** The X-Registry-Auth for an image's registry, if any credential is known. */
  private authFor(ref: string): string | undefined {
    // AuthSource is keyed by registry host, so the ref has to be resolved to its
    // host first - `ghcr.io/me/app:1` -> `ghcr.io`, bare `nginx` -> Docker Hub.
    return this.auth?.(parseRef(ref).registry);
  }

  /**
   * Wait for the new container to prove healthy, up to the policy timeout.
   *
   * For health=docker, this races two sources: a watch on the Docker event
   * stream for the container's `health_status` events, and the inspect-poll
   * below. The event watch settles the instant Docker's probe flips, without
   * waiting out a poll interval; the poll is kept as a belt-and-braces fallback
   * in case the stream drops or an event is missed. For every other prober the
   * event stream carries nothing useful, so only the poll runs.
   *
   * One AbortController scopes both to the timeout and tears them down on any
   * outcome, so no event listener or poll timer leaks. Times out to a definite
   * "unhealthy" so a container that never settles rolls back rather than
   * hanging the pass forever.
   */
  private async waitHealthy(name: string, policy: ContainerPolicy): Promise<HealthResult> {
    const controller = new AbortController();
    const timeout = { healthy: false as const, reason: `did not become healthy within ${policy.healthTimeoutSeconds}s` };

    const races: Promise<HealthResult>[] = [this.pollHealthy(name, policy, controller.signal, timeout)];
    if (policy.health === "docker") {
      races.push(this.watchHealthEvents(name, controller.signal));
    }

    try {
      return await Promise.race(races);
    } finally {
      // Settle the watch/poll and free the event listener and any timer.
      controller.abort();
    }
  }

  /**
   * Poll the chosen prober until it settles or the timeout elapses.
   *
   * The always-present arm of waitHealthy. Aborts cleanly when the controller
   * fires (the event watch won the race, or the whole wait is being torn down).
   */
  private async pollHealthy(
    name: string,
    policy: ContainerPolicy,
    signal: AbortSignal,
    timeout: HealthResult,
  ): Promise<HealthResult> {
    const prober = this.proberFor(policy);
    const deadline = this.now() + policy.healthTimeoutSeconds * 1000;

    try {
      while (this.now() < deadline && !signal.aborted) {
        const fresh = await this.docker.inspect(name);
        const result =
          prober.name === "docker"
            ? verdictForDockerHealth(fresh.health)
            : await prober.check({ container: fresh, ports: policy.ports ?? fresh.spec.published, netns: this.netns }, signal);

        if (result !== undefined) return result;
        await this.sleep(this.options.healthPollMillis, signal);
      }
    } catch (error) {
      if (signal.aborted) return neverResolves<HealthResult>(signal);
      return { healthy: false, reason: `health check errored: ${(error as Error).message}` };
    }

    // Aborted because the event watch already answered: yield the race rather
    // than reporting a spurious timeout.
    if (signal.aborted) return neverResolves<HealthResult>(signal);
    return timeout;
  }

  /**
   * Resolve as soon as a `health_status` event for the container arrives.
   *
   * The fast path for health=docker: Docker emits `health_status: healthy` /
   * `unhealthy` the moment its probe flips, so this beats the poll by up to a
   * whole interval. Filtered to this container. If the stream errors or ends,
   * it hands the decision back to the poll by never resolving (the poll's own
   * timeout still bounds the wait).
   */
  private async watchHealthEvents(name: string, signal: AbortSignal): Promise<HealthResult> {
    try {
      for await (const event of this.docker.events(this.options.label, signal)) {
        if (signal.aborted) break;
        if (event.container !== name || !event.action.startsWith("health_status")) continue;
        if (event.status === "healthy") return { healthy: true };
        if (event.status === "unhealthy") {
          return { healthy: false, reason: "Docker reports the container unhealthy" };
        }
      }
    } catch {
      // Stream dropped - let the poll carry the wait.
    }
    return neverResolves<HealthResult>(signal);
  }

  private proberFor(policy: ContainerPolicy): HealthProber {
    if (policy.health === "uptime") {
      return uptimeHealthProber(policy.healthTimeoutSeconds > 30 ? 30 : policy.healthTimeoutSeconds, this.now);
    }
    return healthProberByName(policy.health) ?? healthProberByName("port")!;
  }

  /**
   * Prune images beyond keep-images, retaining {current, previous}.
   *
   * The invariant is that the just-superseded previous image and everything
   * newer stays; anything older than the N most recent previous images is
   * removed. With the default keepImages=1 this keeps exactly the one previous
   * image (for an immediate rollback) and prunes the rest.
   */
  private async prune(
    supersededDigest: Digest,
    kept: readonly Digest[],
    keepImages: number,
    name: string,
  ): Promise<readonly Digest[]> {
    const history = [supersededDigest, ...kept];
    const retain = history.slice(0, Math.max(0, keepImages));
    const remove = history.slice(Math.max(0, keepImages));

    for (const digest of remove) {
      try {
        await this.docker.removeImage(digest);
      } catch (error) {
        this.log(`${name}: could not prune image ${digest} (${(error as Error).message})`);
      }
    }
    return retain;
  }

  private sleep(millis: number, signal: AbortSignal): Promise<void> {
    return new Promise((resolve) => {
      const timer = setTimeout(resolve, millis);
      signal.addEventListener("abort", () => { clearTimeout(timer); resolve(); }, { once: true });
    });
  }

  /**
   * Reconcile on an interval until the signal aborts.
   *
   * Portical's single-flight closure, minus the event stream: there is no
   * event that says "a newer image exists", so the interval is the whole
   * trigger. A burst of ticks still collapses into one queued pass.
   */
  async run(signal: AbortSignal): Promise<void> {
    let pending: Promise<void> = Promise.resolve();
    let queued = false;

    const pass = () => {
      if (queued) return pending;
      queued = true;
      pending = pending.then(async () => {
        queued = false;
        try {
          await this.once();
        } catch (error) {
          this.log(`Error: ${(error as Error).message}`);
        }
      });
      return pending;
    };

    await pass();

    const ticker = setInterval(pass, this.options.interval * 1000);
    signal.addEventListener("abort", () => clearInterval(ticker), { once: true });
    this.log(`Reconciling every ${this.options.interval}s...`);

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
}

/**
 * A promise that never settles, for the losing arm of a health-check race.
 *
 * When one arm (event watch or poll) produces the verdict, the other must not
 * also resolve the race with a stale value - so it returns this instead. The
 * `signal` parameter documents that the arm is already being torn down by the
 * caller's AbortController; nothing here needs to act on it, and the abandoned
 * promise is collected once the race settles.
 */
function neverResolves<T>(_signal: AbortSignal): Promise<T> {
  return new Promise<T>(() => {});
}

/**
 * One line saying what the pass did, logged only when it changes.
 *
 * "Nothing to do" is the normal, healthy state and worth saying once - a daemon
 * that reconciles every five minutes should be quiet, not chatty.
 */
function summarise(actions: readonly Action[]): string {
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
      ? "No containers to manage"
      : `${kept} container${kept === 1 ? "" : "s"} up to date, nothing to do`;
  }
  return [...changes, `${kept} up to date`].join(", ");
}

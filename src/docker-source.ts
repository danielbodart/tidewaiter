import type { ConntrackEntry } from "./conntrack.ts";
import type { Log, Options } from "./daemon.ts";
import { decide, type Action, type ContainerState, type Snapshot } from "./decide.ts";
import { combine, detectorByName, netioDetector, type Detector, type DetectorResult } from "./detectors.ts";
import type { DockerClient } from "./docker.ts";
import {
  BunTcpProbe,
  checkByName,
  combine as combineHealth,
  dockerVerdict,
  netnsBoundPorts,
  portConnectCheck,
  uptimeCheck,
  type CheckResult,
  type HealthCheck,
  type HealthResult,
  type TcpProbe,
} from "./health.ts";
import { pollVerdict, uptimeSeconds } from "./gate.ts";
import { parseLabels, type ContainerPolicy } from "./labels.ts";
import type { Digest, PublishedPort, RunningContainer } from "./model.ts";
import type { NetnsReader } from "./netns.ts";
import { planSwap, recreatable, rollbackName, type SwapStep } from "./recreate.ts";
import { parseRef, type AuthSource, type RegistryClient } from "./registry.ts";
import { MAX_SWAP_FAILURES, type Activity, type Outcome, type PassContext, type Source } from "./source.ts";

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


/** Per-container memory carried between passes. Extends decide()'s ContainerState. */
interface State extends ContainerState {
  readonly idleStreak: number;
  /** The image the container ran before the last update, for rollback and prune. */
  readonly previousImage?: { readonly ref: string; readonly digest: Digest };
  /** Digests retained beyond the current one, oldest first, for keep-images. */
  readonly keptImages: readonly Digest[];
  /** True once we have logged the "adopting as baseline" note for this container. */
  readonly adopted?: boolean;
  /**
   * Consecutive swap-step failures against `failingDigest`, for back-off.
   *
   * A single swap-step failure is not pinned (it may be a transient Docker
   * hiccup - a socket blip, momentary disk pressure - that says nothing about
   * the image). But a *deterministic* failure (a genuinely broken image) would
   * otherwise retry every pass forever. After MAX_SWAP_FAILURES in a row on the
   * same target digest, the container is pinned out just as a health failure
   * pins it, and a new digest clears the count.
   */
  readonly swapFailures?: number;
  readonly failingDigest?: Digest;
}

const INITIAL: State = { idleStreak: 0, keptImages: [] };

/**
 * Labelled Docker containers, kept on the digest their tag resolves to.
 *
 * The reconcile, reshaped from portical for updates instead of forwards: per-
 * item failure isolation, containers processed one at a time (a swap can take
 * minutes at the health gate, and must not starve the others), and the
 * per-container decision pure decide() with all its inputs gathered first. The
 * loop, the shared conntrack dump and the summary belong to the Daemon, which
 * runs this alongside any other source.
 */
export class DockerSource implements Source {
  readonly name = "docker";

  constructor(
    private readonly docker: DockerClient,
    private readonly registry: RegistryClient,
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
    /**
     * Opens a TCP connection to a host port, for the `port` health probe.
     *
     * Injected like netns so tests answer without a real socket. Defaults to a
     * real connect() against the host, which is where a host-networked
     * Tidewaiter reaches a container's published ports.
     */
    private readonly tcp: TcpProbe = new BunTcpProbe(),
  ) {}

  private readonly state = new Map<string, State>();

  /**
   * The target digest of the in-flight (or completed) background pull per
   * container, keyed by name.
   *
   * Deliberately NOT part of the per-container State that decide() reads: a
   * warmed cache changes nothing about the *decision*, only how fast the
   * eventual swap runs, so it lives here as daemon plumbing alongside
   * netioSingleton rather than in decide()'s memory. The recorded digest is the
   * dedupe key - one pull per target digest, relaunched only when the tag moves
   * to a different one, or when a prior pull failed and cleared its entry.
   */
  private readonly prefetches = new Map<string, Digest>();

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
   *
   * The host conntrack table arrives in the context, dumped once by the daemon
   * and shared by every container's detector (and every other source) - NAT
   * happens host-wide, so one dump serves all. A dump that failed arrives as
   * undefined and is read here as an empty table, as it always has been.
   */
  async once(context: PassContext): Promise<Outcome[]> {
    const containers = await this.docker.containers(this.options.label);
    const conntrack = context.conntrack ?? [];

    const outcomes: Outcome[] = [];
    for (const container of containers) {
      outcomes.push(await this.reconcile(container, conntrack, context));
    }
    return outcomes;
  }

  /**
   * Whether each opted-in container is busy right now, for the inhibitor.
   *
   * The same detectors and fail-safe as a pass, but nothing is resolved,
   * decided or remembered - in particular no idle streak moves, so the faster
   * tick can never hurry an update along.
   */
  async activity(conntrack: readonly ConntrackEntry[] | undefined): Promise<Activity[]> {
    const containers = await this.docker.containers(this.options.label);
    const activities: Activity[] = [];
    for (const container of containers) {
      const { policy } = parseLabels(container.spec.labels, container.spec.published);
      if (policy === undefined) continue;
      const ports = policy.ports ?? container.spec.published;
      const result = await this.sample(container, policy, ports, conntrack ?? []);
      activities.push({ workload: container.spec.name, busy: result.inUse });
    }
    return activities;
  }

  /** One line per opted-in container: its image, whether an update waits, and its policy. */
  async list(): Promise<string[]> {
    const containers = await this.docker.containers(this.options.label);
    const lines: string[] = [];
    for (const container of containers) {
      const { policy } = parseLabels(container.spec.labels, container.spec.published);
      if (policy === undefined) {
        // Has the label key but an unrecognised value - shown so a typo is visible.
        lines.push(`${container.spec.name}  ${container.spec.image}  [not a known autoupdate policy]`);
        continue;
      }
      // Source the desired digest the same way a pass would for this policy.
      let desired: string;
      try {
        desired = policy.autoupdate === "local"
          ? (await this.docker.imageDigest(container.spec.image)) ?? "no local image"
          : await this.registry.digest(container.spec.image);
      } catch {
        desired = policy.autoupdate === "local" ? "no local image" : "registry unreachable";
      }
      // Compare like with like: manifest digest vs the running image's RepoDigest
      // (by its config id), not inspect's config id directly.
      const current = await this.docker.imageDigest(container.imageId).catch(() => undefined);
      const state = desired.startsWith("no ") || desired.endsWith("unreachable") ? desired
        : desired === current ? "up to date"
        : "update available";
      lines.push(
        `${container.spec.name}  ${container.spec.image}  [${state}]  ` +
          `autoupdate=${policy.autoupdate} detector=${policy.detector} health=${policy.health.join(",")} idle-samples=${policy.idleSamples}`,
      );
    }
    return lines;
  }

  /** Decide and apply for one container, isolated so its failure spares the rest. */
  private async reconcile(
    container: RunningContainer,
    conntrack: readonly ConntrackEntry[],
    context: PassContext,
  ): Promise<Outcome> {
    const name = container.spec.name;
    const outcome = (action: Action, busy?: boolean): Outcome =>
      busy === undefined ? { workload: name, action } : { workload: name, action, busy };
    const { policy, warnings } = parseLabels(container.spec.labels, container.spec.published);
    for (const warning of warnings) this.log(`${name}: ${warning}`);

    // Not opted in (or an unrecognised autoupdate value - the warning above said
    // so): leave it completely alone. The Docker query already filters on the
    // label key, so reaching here with no policy means a bad value, not a
    // missing label.
    if (policy === undefined) {
      return outcome({ kind: "keep", reason: "not opted in" }, false);
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
      return outcome({ kind: "keep", reason: `${source} unreachable` });
    }
    if (desiredDigest === undefined) {
      // local policy and the tag is not present locally - nothing to move to.
      return outcome({ kind: "keep", reason: "no local image for the tag to update to" });
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
      return outcome({ kind: "keep", reason: "running image digest unreadable" });
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

    // Warm the cache ahead of the swap: the moment a newer digest appears, start
    // pulling it in the background - even while the container is still busy - so
    // that when the tide finally goes out the swap is a fast local recreate, not
    // a multi-minute download inside the very idle window we were waiting for.
    // Fire-and-forget: a pull can take minutes and must never stall the
    // single-flight pass or starve the other containers.
    this.prefetch(container, policy, desiredDigest, currentDigest, previous.pinnedDigest);

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
    // The lock goes on before the swap starts, so a reboot cannot land mid-way.
    if (action.kind === "update" && !this.options.dryRun) await context.hold(name);
    const next = await this.apply(action, container, policy, previous, idleStreak, desiredDigest, currentDigest);
    this.state.set(name, next);
    return outcome(action, activity.inUse);
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
   * Start (or skip) a background pull of a newer image, so it is already on the
   * host by the time the container goes idle enough to swap.
   *
   * Detached on purpose - the promise is not awaited, so the reconcile pass
   * moves straight on. The gates mirror performUpdate's own pull:
   *   - `registry` only: `local` deliberately never touches the network.
   *   - nothing to fetch when already on the desired digest.
   *   - a pinned-out (health-failed) digest is a bad image; do not warm it.
   *   - dry run touches nothing.
   * Dedupe: the recorded digest means one pull per target, launched once and
   * left in flight across passes; a different desired digest supersedes it. A
   * failed pull is purely speculative - it never pins and never disrupts; it
   * just clears its record so the next pass retries, and the real swap does its
   * own pull with full error handling regardless.
   */
  private prefetch(
    container: RunningContainer,
    policy: ContainerPolicy,
    desiredDigest: Digest,
    currentDigest: Digest | undefined,
    pinnedDigest: Digest | undefined,
  ): void {
    if (this.options.dryRun) return;
    if (policy.autoupdate !== "registry") return;
    if (desiredDigest === currentDigest) return;
    if (pinnedDigest === desiredDigest) return;

    const name = container.spec.name;
    if (this.prefetches.get(name) === desiredDigest) return;

    const ref = container.spec.image;
    this.prefetches.set(name, desiredDigest);
    this.log(`${name}: prefetching ${ref} in the background so the swap is ready when the tide goes out`);
    this.docker.pull(ref, this.authFor(ref)).catch((error) => {
      this.prefetches.delete(name);
      this.log(`${name}: prefetch of ${ref} failed (${(error as Error).message}); will retry`);
    });
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

      // Count consecutive failures on THIS digest. One (or a few) may be a
      // transient Docker hiccup, so we retry - but a deterministic failure (a
      // genuinely broken image) would retry every pass forever, so after
      // MAX_SWAP_FAILURES in a row we pin it out, exactly as a health failure
      // does. A different target digest resets the count.
      const failures = base.failingDigest === desiredDigest ? (base.swapFailures ?? 0) + 1 : 1;
      if (failures >= MAX_SWAP_FAILURES) {
        this.log(
          `${name}: swap has failed ${failures} times running for ${desiredDigest}; ` +
            "pinning it out until the tag moves on",
        );
        return { ...base, previousImage, pinnedDigest: desiredDigest, swapFailures: failures, failingDigest: desiredDigest };
      }
      return { ...base, previousImage, swapFailures: failures, failingDigest: desiredDigest };
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
   * Builds the container's set of health checks (from `tidewaiter.health`) ONCE,
   * so the stateful `uptime` check keeps its start time across every evaluation.
   * Then races two arms under one AbortController:
   *   - the inspect-poll, the always-present arm, which re-evaluates every check
   *     each interval and combines them;
   *   - a Docker `health_status` event watch, present only when `docker` is one
   *     of the checks, which re-runs the same combination the instant Docker's
   *     own probe flips (overriding just the docker check with the event's
   *     verdict) - so a healthy/unhealthy event settles the gate without waiting
   *     out a poll interval.
   *
   * The controller tears both down on any outcome, so no listener or timer
   * leaks. The gate's verdict follows combine()'s rule: roll back only on an
   * active failure; at the timeout, commit unless something failed.
   */
  private async waitHealthy(name: string, policy: ContainerPolicy): Promise<HealthResult> {
    const controller = new AbortController();
    const checks = this.checksFor(policy);
    // A resolver scoped to THIS swap: memoised for the gate's life (so the
    // port-connect probe does not re-query /networks every poll) and rebuilt next
    // swap (so nothing goes stale, and no restart is ever needed to see a changed
    // network). Failure-tolerant - see networkDriverResolver.
    const networkDriver = networkDriverResolver(this.docker);

    const races: Promise<HealthResult>[] = [this.pollHealthy(name, policy, checks, networkDriver, controller.signal)];
    if (policy.health.includes("docker")) {
      races.push(this.watchHealthEvents(name, policy, checks, networkDriver, controller.signal));
    }

    try {
      return await Promise.race(races);
    } finally {
      controller.abort();
    }
  }

  /** Evaluate every check against a freshly-inspected container, in order. */
  private async evaluateChecks(
    name: string,
    policy: ContainerPolicy,
    checks: readonly HealthCheck[],
    networkDriver: (network: string) => Promise<string | undefined>,
    signal: AbortSignal,
    dockerOverride?: CheckResult,
  ): Promise<CheckResult[]> {
    const fresh = await this.docker.inspect(name);
    const input = {
      container: fresh,
      ports: policy.ports ?? fresh.spec.published,
      bound: netnsBoundPorts(this.netns, fresh.pid),
      tcp: this.tcp,
      networkDriver,
    };
    const results: CheckResult[] = [];
    for (const check of checks) {
      // The event watch already has Docker's verdict from the event; use it
      // rather than re-reading a possibly-stale inspect for the docker check.
      results.push(check.name === "docker" && dockerOverride ? dockerOverride : await check.evaluate(input, signal));
    }
    return results;
  }

  /**
   * The always-present arm: poll the combined gate until it settles or times out.
   */
  private async pollHealthy(
    name: string,
    policy: ContainerPolicy,
    checks: readonly HealthCheck[],
    networkDriver: (network: string) => Promise<string | undefined>,
    signal: AbortSignal,
  ): Promise<HealthResult> {
    const verdict = await pollVerdict(
      (signal) => this.evaluateChecks(name, policy, checks, networkDriver, signal),
      { timeoutSeconds: policy.healthTimeoutSeconds, pollMillis: this.options.healthPollMillis, now: this.now },
      signal,
    );
    return verdict === "aborted" ? neverResolves<HealthResult>(signal) : verdict;
  }

  /**
   * The fast path when `docker` is one of the checks: settle on a health_status
   * event by re-running the full combination with docker's result taken from the
   * event. A healthy event commits (if the other checks pass/skip); an unhealthy
   * event fails the docker check and rolls back at once. If the stream drops it
   * never resolves and the poll carries the wait.
   */
  private async watchHealthEvents(
    name: string,
    policy: ContainerPolicy,
    checks: readonly HealthCheck[],
    networkDriver: (network: string) => Promise<string | undefined>,
    signal: AbortSignal,
  ): Promise<HealthResult> {
    try {
      for await (const event of this.docker.events(this.options.label, signal)) {
        if (signal.aborted) break;
        if (event.container !== name || !event.action.startsWith("health_status")) continue;
        if (event.status !== "healthy" && event.status !== "unhealthy") continue;

        const results = await this.evaluateChecks(name, policy, checks, networkDriver, signal, dockerVerdict(event.status));
        const verdict = combineHealth(results, false);
        if (verdict !== undefined) return verdict;
      }
    } catch {
      // Stream dropped - let the poll carry the wait.
    }
    return neverResolves<HealthResult>(signal);
  }

  /** The set of health checks for a container, from its `tidewaiter.health`. */
  private checksFor(policy: ContainerPolicy): HealthCheck[] {
    return policy.health.map((name) => {
      // uptime and port-connect are stateful and share the same grace window
      // (uptimeSeconds), so both are built per swap with the clock rather than
      // resolved as singletons.
      if (name === "uptime") return uptimeCheck(uptimeSeconds(policy.healthTimeoutSeconds), this.now);
      if (name === "port-connect") return portConnectCheck(uptimeSeconds(policy.healthTimeoutSeconds), this.now);
      return checkByName(name)!;
    });
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
}

/**
 * A network-driver resolver scoped to a single health gate.
 *
 * Two properties, both deliberate:
 *   - MEMOISED, so the port-connect probe (evaluated every poll, on two racing
 *     arms) hits /networks once per network per swap, not once per poll. Built
 *     fresh each swap, so it is never stale and no restart is ever needed to pick
 *     up a network that changed between swaps.
 *   - FAILURE-TOLERANT: any lookup error becomes `undefined`, which the probe
 *     reads as "not a driver we can route to" and simply does not probe. A
 *     transient /networks hiccup must never surface as a health-check error and
 *     roll back an otherwise-healthy update.
 */
function networkDriverResolver(docker: DockerClient): (network: string) => Promise<string | undefined> {
  const memo = new Map<string, Promise<string | undefined>>();
  return (network) => {
    let hit = memo.get(network);
    if (hit === undefined) {
      hit = docker.networkDriver(network).catch(() => undefined);
      memo.set(network, hit);
    }
    return hit;
  };
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

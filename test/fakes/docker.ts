import type { DockerClient, DockerEvent } from "../../src/docker.ts";
import type {
  ContainerSpec,
  Digest,
  HealthStatus,
  NetworkAttachment,
  PublishedPort,
  RunningContainer,
} from "../../src/model.ts";

/**
 * Docker as an object, so a whole update flow runs without a daemon or socket.
 *
 * Public mutable fields and recorded-call arrays, the same idiom as portical's
 * FakeDocker - tests set `running`/`images` directly and assert on `pulls`,
 * `creates`, and the rest. Container lifecycle methods mutate `running` so a
 * later inspect() sees the swap that happened.
 */
export class FakeDocker implements DockerClient {
  running: RunningContainer[] = [];
  /**
   * The manifest digest each LOCAL image resolves to, keyed by image id AND by
   * tag ref (both accepted by Docker's /images/{name}/json, so both accepted by
   * imageDigest()). A tag entry moves when a pull lands a new image; an image-id
   * entry is stable, which is why the daemon keys "current" on the container's
   * image id rather than its tag.
   */
  images: Record<string, Digest> = {};
  /**
   * What a pull of a ref lands: the registry's current digest for the tag.
   *
   * A pull mints a new local image (a fresh config id) whose RepoDigest is this
   * value, and repoints the tag at it - modelling Docker fetching a newer
   * manifest while the still-running old container keeps its old image id.
   */
  pullLands: Record<string, Digest> = {};
  /** Set to make listing/inspect fail, as a Docker restart would. */
  failWith?: Error;
  /** Set to make a pull throw, e.g. a registry hiccup during a prefetch. */
  failPull?: Error;
  /**
   * Make one lifecycle operation throw, to exercise a mid-swap failure.
   *
   * Keyed by op name ("stop"/"rename"/"create"/"start"/"remove"). Lets a test
   * fail exactly the step it wants - e.g. `stop` failing before anything was
   * renamed, which is the case a blind rollback used to mishandle.
   */
  failOp: Partial<Record<"stop" | "rename" | "create" | "start" | "remove", Error>> = {};

  readonly pulls: { ref: string; auth?: string }[] = [];
  readonly creates: ContainerSpec[] = [];
  readonly starts: string[] = [];
  readonly stops: { container: string; graceSeconds: number }[] = [];
  readonly removes: { container: string; force: boolean }[] = [];
  readonly renames: { container: string; to: string }[] = [];
  readonly removedImages: string[] = [];
  readonly connects: { container: string; network: NetworkAttachment }[] = [];

  /**
   * Per-container health as inspect() should report it, keyed by name.
   *
   * Lets a test say "after the swap this container comes up healthy" or
   * "unhealthy", which is what drives the commit-vs-rollback branch. Defaults to
   * the container's own `health` field when not overridden.
   */
  healthByName: Record<string, HealthStatus> = {};

  /**
   * Cumulative network bytes per container id, for the netio detector.
   *
   * Tests bump this between passes to model traffic; `netBytesFailWith` makes
   * the read throw, to exercise the daemon's fail-safe on a stats error.
   */
  netBytesById: Record<string, number> = {};
  netBytesFailWith?: Error;

  private waiting: { resolve: (event?: DockerEvent) => void }[] = [];
  private queue: DockerEvent[] = [];

  async containers(label: string): Promise<RunningContainer[]> {
    if (this.failWith) throw this.failWith;
    return this.running.filter((container) => container.spec.labels[label] !== undefined);
  }

  /**
   * Per-container run state as inspect() should report it, keyed by name.
   *
   * Lets a test say "after the swap this container exited/crash-looped", which
   * drives the uptime check's active-failure path. Defaults to the container's
   * own `running` field (true) when not overridden.
   */
  runningByName: Record<string, boolean> = {};

  async inspect(container: string): Promise<RunningContainer> {
    if (this.failWith) throw this.failWith;
    const found = this.find(container);
    if (!found) throw new Error(`no such container: ${container}`);
    const health = this.healthByName[found.spec.name] ?? found.health;
    const running = this.runningByName[found.spec.name] ?? found.running;
    return { ...found, health, running };
  }

  /** Accepts an image id or a tag ref, as Docker's /images/{name}/json does. */
  async imageDigest(idOrRef: string): Promise<Digest | undefined> {
    return this.images[idOrRef];
  }

  async pull(ref: string, auth?: string): Promise<void> {
    this.pulls.push({ ref, auth });
    if (this.failPull) throw this.failPull;
    // A pull fetches the registry's current manifest for the tag into a fresh
    // local image (a new config id), and points the tag at it. The digest is
    // reachable by both the tag and the new id afterwards.
    const landed = this.pullLands[ref];
    if (landed !== undefined) {
      this.images[ref] = landed;
      this.images[this.imageIdFor(ref)] = landed;
    }
  }

  async create(spec: ContainerSpec): Promise<string> {
    if (this.failOp.create) throw this.failOp.create;
    this.creates.push(spec);
    const id = `id-${spec.name}-${this.creates.length}`;
    // The new container is created from whatever image the tag currently points
    // at locally - its image id, so a later imageDigest(imageId) is stable even
    // if the tag moves again.
    this.running.push({
      id,
      pid: 1000 + this.creates.length,
      spec,
      imageId: this.imageIdFor(spec.image),
      health: "none",
      running: true,
    });
    return id;
  }

  /** The stable local image id a tag currently resolves to. */
  private imageIdFor(ref: string): string {
    const digest = this.images[ref];
    return digest === undefined ? `config-${ref}` : `config-${digest}`;
  }

  async start(container: string): Promise<void> {
    if (this.failOp.start) throw this.failOp.start;
    this.starts.push(container);
  }

  async stop(container: string, graceSeconds: number): Promise<void> {
    if (this.failOp.stop) throw this.failOp.stop;
    this.stops.push({ container, graceSeconds });
  }

  async remove(container: string, options: { force?: boolean } = {}): Promise<void> {
    if (this.failOp.remove) throw this.failOp.remove;
    // Real Docker 404s on a container that is not there; the daemon's rollback
    // is written to tolerate that, so model it as a no-op rather than an error.
    this.removes.push({ container, force: options.force ?? false });
    this.running = this.running.filter(
      (c) => c.spec.name !== container && c.id !== container,
    );
  }

  async rename(container: string, name: string): Promise<void> {
    if (this.failOp.rename) throw this.failOp.rename;
    this.renames.push({ container, to: name });
    const found = this.find(container);
    if (found) {
      this.running = this.running.map((c) =>
        c === found ? { ...c, spec: { ...c.spec, name } } : c,
      );
    }
  }

  async removeImage(ref: string): Promise<void> {
    this.removedImages.push(ref);
  }

  async connectNetwork(container: string, network: NetworkAttachment): Promise<void> {
    this.connects.push({ container, network });
  }

  async netBytes(container: string): Promise<number> {
    if (this.netBytesFailWith) throw this.netBytesFailWith;
    return this.netBytesById[container] ?? 0;
  }

  async *events(_label: string, signal: AbortSignal): AsyncIterable<DockerEvent> {
    while (!signal.aborted) {
      const queued = this.queue.shift();
      if (queued) { yield queued; continue; }
      const next = await new Promise<DockerEvent | undefined>((resolve) => {
        this.waiting.push({ resolve });
        signal.addEventListener("abort", () => resolve(undefined), { once: true });
      });
      if (!next) return;
      yield next;
    }
  }

  emit(event: DockerEvent): void {
    const waiter = this.waiting.shift();
    if (waiter) waiter.resolve(event);
    else this.queue.push(event);
  }

  private find(container: string): RunningContainer | undefined {
    return this.running.find((c) => c.spec.name === container || c.id === container);
  }
}

/** A ContainerSpec with sensible defaults, overridable field by field. */
export function spec(name: string, overrides: Partial<ContainerSpec> = {}): ContainerSpec {
  return {
    name,
    image: "app:latest",
    env: [],
    labels: { "tidewaiter.autoupdate": "registry" },
    mounts: [],
    published: [],
    exposed: [],
    networks: [],
    capAdd: [],
    capDrop: [],
    privileged: false,
    devices: [],
    extraHosts: [],
    sysctls: {},
    ...overrides,
  };
}

/** A RunningContainer with sensible defaults. */
export function runningContainer(
  name: string,
  overrides: Omit<Partial<RunningContainer>, "spec"> & { spec?: Partial<ContainerSpec> } = {},
): RunningContainer {
  const { spec: specOverrides, ...rest } = overrides;
  return {
    id: `id-${name}`,
    pid: 4242,
    imageId: "config-current",
    health: "none",
    running: true,
    spec: spec(name, specOverrides),
    ...rest,
  };
}

export function port(hostPort: number, containerPort = hostPort, protocol: "tcp" | "udp" = "tcp"): PublishedPort {
  return { hostPort, containerPort, protocol };
}

import { ok, type Handler } from "./http.ts";
import type {
  ContainerSpec,
  Device,
  Digest,
  Healthcheck,
  HealthStatus,
  Mount,
  NetworkAttachment,
  Protocol,
  PublishedPort,
  RestartPolicy,
  RunningContainer,
} from "./model.ts";
import { endpointOf, toCreatePayload } from "./recreate.ts";

/**
 * A Docker lifecycle event, narrowed to what Tidewaiter watches.
 *
 * `status` carries the health verdict for a `health_status` event
 * (`health_status: healthy` / `health_status: unhealthy`), which is the one
 * event Tidewaiter actually reacts to - during a swap's health gate, when the
 * container defines a HEALTHCHECK. Everything else is informational.
 */
export interface DockerEvent {
  readonly action: string;
  readonly container: string;
  readonly status?: string;
}

/**
 * The Docker operations Tidewaiter needs.
 *
 * An interface, not a concrete client, so the daemon runs against a fake in
 * tests - the same seam portical uses. Extended well beyond portical's
 * read-mostly set: Tidewaiter pulls, creates, stops, renames and removes, so
 * the whole update flow is expressible against this interface and testable with
 * no Docker at all.
 */
export interface DockerClient {
  /** Every container carrying the given label, fully inspected. */
  containers(label: string): Promise<RunningContainer[]>;
  /** One container, fully inspected - name, id or short id. */
  inspect(container: string): Promise<RunningContainer>;
  /** The digest of a local image, as Docker records it in RepoDigests. */
  imageDigest(ref: string): Promise<Digest | undefined>;
  /** Pull an image. `auth` is a ready X-Registry-Auth header value, if any. */
  pull(ref: string, auth?: string): Promise<void>;
  /** Create a container from a spec, returning its new id. */
  create(spec: ContainerSpec): Promise<string>;
  start(container: string): Promise<void>;
  /** Stop a container, giving it `graceSeconds` to drain before SIGKILL. */
  stop(container: string, graceSeconds: number): Promise<void>;
  remove(container: string, options?: { readonly force?: boolean }): Promise<void>;
  rename(container: string, name: string): Promise<void>;
  removeImage(ref: string): Promise<void>;
  /** Attach an already-created container to an additional network. */
  connectNetwork(container: string, network: NetworkAttachment): Promise<void>;
  /**
   * Cumulative network bytes (rx + tx, summed over interfaces) for a container.
   *
   * A single point-in-time counter, deliberately: a *rate* needs two of these an
   * interval apart, which the netio detector computes across passes. Lives on
   * the Docker client because it already owns the socket and the body-draining
   * discipline the /stats call needs.
   */
  netBytes(container: string): Promise<number>;
  events(label: string, signal: AbortSignal): AsyncIterable<DockerEvent>;
}

/** Engine API version. 1.41 ships with Docker 20.10, old enough to be safe. */
const API = "v1.41";

/**
 * Talks to the Docker Engine API over a Handler.
 *
 * Takes a Handler rather than a socket path, so tests hand it an in-memory
 * Docker and it cannot tell the difference - lifted straight from portical's
 * HttpDockerClient and extended with the write operations the update flow
 * needs. Every one of those, like portical's `post`, drains its response body:
 * Bun keeps the unix socket alive between requests, and an unread body leaves
 * the *next* request reading a stale reply.
 */
export class HttpDockerClient implements DockerClient {
  constructor(private readonly handler: Handler) {}

  private async get(path: string, signal?: AbortSignal): Promise<Response> {
    try {
      return await ok(await this.handler(new Request(`http://docker${path}`, { signal })));
    } catch (cause) {
      throw new Error(`Docker API ${path} failed: ${(cause as Error).message}`, { cause });
    }
  }

  async containers(label: string): Promise<RunningContainer[]> {
    const listed = (await this.get(
      `/${API}/containers/json?filters=${filters({ label: [label] })}`,
    ).then((response) => response.json())) as readonly { Id: string }[];

    // Inspected one at a time rather than mapped off the list: the list
    // endpoint carries neither the PID nor the full config a faithful recreate
    // needs, so an inspect per opted-in container is unavoidable. There are
    // only ever a handful of them.
    return Promise.all(listed.map((entry) => this.inspect(entry.Id)));
  }

  async inspect(container: string): Promise<RunningContainer> {
    const raw = (await this.get(`/${API}/containers/${container}/json`).then((response) =>
      response.json(),
    )) as RawContainerInspect;
    return toRunning(raw);
  }

  async imageDigest(ref: string): Promise<Digest | undefined> {
    // A missing image is a normal answer (nothing pulled yet), not an error, so
    // 404 comes back as undefined rather than throwing.
    const response = await this.handler(new Request(`http://docker/${API}/images/${encodeURIComponent(ref)}/json`));
    if (response.status === 404) {
      await response.text();
      return undefined;
    }
    const raw = (await ok(response).then((r) => r.json())) as RawImageInspect;
    return firstDigest(raw.RepoDigests);
  }

  async pull(ref: string, auth?: string): Promise<void> {
    const headers: Record<string, string> = {};
    if (auth !== undefined) headers["X-Registry-Auth"] = auth;

    // POST /images/create streams NDJSON progress until the pull finishes. The
    // body must be drained to the end for two reasons: it is what makes the
    // call block until the image is actually here, and (as with every call on
    // this kept-alive socket) an unread body desyncs the next request.
    try {
      const response = await ok(
        await this.handler(
          new Request(`http://docker/${API}/images/create?fromImage=${encodeURIComponent(ref)}`, {
            method: "POST",
            headers,
          }),
        ),
      );
      await drain(response);
    } catch (cause) {
      throw new Error(`Docker API pull ${ref} failed: ${(cause as Error).message}`, { cause });
    }
  }

  async create(spec: ContainerSpec): Promise<string> {
    const created = JSON.parse(
      await this.post(
        `/${API}/containers/create?name=${encodeURIComponent(spec.name)}`,
        toCreatePayload(spec),
      ),
    ) as { Id: string };
    return created.Id;
  }

  async start(container: string): Promise<void> {
    await this.post(`/${API}/containers/${container}/start`);
  }

  async stop(container: string, graceSeconds: number): Promise<void> {
    await this.post(`/${API}/containers/${container}/stop?t=${Math.max(0, Math.floor(graceSeconds))}`);
  }

  async remove(container: string, options: { readonly force?: boolean } = {}): Promise<void> {
    const force = options.force ? "&force=1" : "";
    await this.delete(`/${API}/containers/${container}?v=1${force}`);
  }

  async rename(container: string, name: string): Promise<void> {
    await this.post(`/${API}/containers/${container}/rename?name=${encodeURIComponent(name)}`);
  }

  async removeImage(ref: string): Promise<void> {
    await this.delete(`/${API}/images/${encodeURIComponent(ref)}`);
  }

  async connectNetwork(container: string, network: NetworkAttachment): Promise<void> {
    await this.post(`/${API}/networks/${encodeURIComponent(network.name)}/connect`, {
      Container: container,
      EndpointConfig: endpointOf(network),
    });
  }

  async netBytes(container: string): Promise<number> {
    // stream=false gives one stats snapshot and closes, rather than the default
    // streaming feed that never ends - one read, one rate sample.
    const raw = (await this.get(`/${API}/containers/${container}/stats?stream=false`).then((response) =>
      response.json(),
    )) as RawStats;
    return netBytesOf(raw);
  }

  async *events(label: string, signal: AbortSignal): AsyncIterable<DockerEvent> {
    const query = filters({
      type: ["container"],
      event: ["start", "die", "stop", "kill", "health_status"],
      label: [label],
    });
    const response = await this.get(`/${API}/events?filters=${query}`, signal);
    if (!response.body) return;

    for await (const line of lines(response.body)) {
      const raw = JSON.parse(line) as RawEvent;
      const action = raw.Action ?? "";
      yield {
        // A health_status event's Action is "health_status: healthy"; the
        // verdict is split out into `status` so a consumer can match on it
        // without string-surgery.
        action,
        container: raw.Actor?.Attributes?.name ?? raw.id ?? "",
        status: action.startsWith("health_status") ? action.split(":")[1]?.trim() : undefined,
      };
    }
  }

  /**
   * POST returning the drained body text, so it is always read.
   *
   * Same discipline as portical: an unread reply on the kept-alive unix socket
   * desyncs the next request. Every write above routes through here or through
   * `delete`/`drain`.
   */
  private async post(path: string, body?: unknown): Promise<string> {
    try {
      const response = await ok(
        await this.handler(
          new Request(`http://docker${path}`, {
            method: "POST",
            headers: body === undefined ? {} : { "Content-Type": "application/json" },
            body: body === undefined ? undefined : JSON.stringify(body),
          }),
        ),
      );
      return await response.text();
    } catch (cause) {
      throw new Error(`Docker API ${path} failed: ${(cause as Error).message}`, { cause });
    }
  }

  private async delete(path: string): Promise<void> {
    try {
      const response = await ok(
        await this.handler(new Request(`http://docker${path}`, { method: "DELETE" })),
      );
      await response.text();
    } catch (cause) {
      throw new Error(`Docker API ${path} failed: ${(cause as Error).message}`, { cause });
    }
  }
}

/** Fully consume a response body, discarding it, without buffering line by line. */
async function drain(response: Response): Promise<void> {
  if (!response.body) return;
  const reader = response.body.getReader();
  // eslint-disable-next-line no-constant-condition
  while (true) {
    const { done } = await reader.read();
    if (done) break;
  }
}

/** Docker wants its filters as a URL-encoded JSON map of name to values. */
function filters(spec: Record<string, string[]>): string {
  return encodeURIComponent(JSON.stringify(spec));
}

/** Split a chunked NDJSON body into lines, holding partial lines back. */
async function* lines(body: ReadableStream<Uint8Array>): AsyncIterable<string> {
  const decoder = new TextDecoder();
  let buffer = "";

  for await (const chunk of body) {
    buffer += decoder.decode(chunk, { stream: true });
    let newline: number;
    while ((newline = buffer.indexOf("\n")) !== -1) {
      const line = buffer.slice(0, newline).trim();
      buffer = buffer.slice(newline + 1);
      if (line !== "") yield line;
    }
  }
}

/** The shape of `docker inspect <container>` we read. */
export interface RawContainerInspect {
  Id: string;
  Name?: string;
  Image?: string;
  State?: { Pid?: number; Health?: { Status?: string }; Running?: boolean; Restarting?: boolean; Status?: string };
  Config?: {
    Image?: string;
    Env?: string[];
    Cmd?: string[] | null;
    Entrypoint?: string[] | null;
    Labels?: Record<string, string> | null;
    ExposedPorts?: Record<string, unknown> | null;
    User?: string;
    WorkingDir?: string;
    StopSignal?: string;
    StopTimeout?: number | null;
    Healthcheck?: {
      Test?: string[];
      Interval?: number;
      Timeout?: number;
      Retries?: number;
      StartPeriod?: number;
    } | null;
  };
  HostConfig?: {
    Binds?: string[] | null;
    Mounts?: RawMount[] | null;
    PortBindings?: Record<string, { HostIp?: string; HostPort?: string }[] | null> | null;
    RestartPolicy?: { Name?: string; MaximumRetryCount?: number } | null;
    CapAdd?: string[] | null;
    CapDrop?: string[] | null;
    Privileged?: boolean;
    NetworkMode?: string;
    Devices?: { PathOnHost?: string; PathInContainer?: string; CgroupPermissions?: string }[] | null;
    ExtraHosts?: string[] | null;
    Sysctls?: Record<string, string> | null;
  };
  Mounts?: RawMount[] | null;
  NetworkSettings?: {
    Networks?: Record<
      string,
      { Aliases?: string[] | null; IPAMConfig?: { IPv4Address?: string; IPv6Address?: string } | null }
    > | null;
  };
}

interface RawMount {
  Type?: string;
  Name?: string;
  Source?: string;
  Destination?: string;
  Mode?: string;
  RW?: boolean;
}

interface RawImageInspect {
  RepoDigests?: string[] | null;
}

/** The slice of `docker stats` we read: per-interface rx/tx byte counters. */
interface RawStats {
  networks?: Record<string, { rx_bytes?: number; tx_bytes?: number }> | null;
}

/**
 * Sum rx + tx bytes across every interface in a stats snapshot.
 *
 * Exported and pure, tested against captured /stats JSON. A container with no
 * `networks` block (host networking, or stats taken before any traffic) totals
 * zero, which a rate calculation reads as "no bytes moved" - correct.
 */
export function netBytesOf(raw: RawStats): number {
  let total = 0;
  for (const iface of Object.values(raw.networks ?? {})) {
    total += (iface.rx_bytes ?? 0) + (iface.tx_bytes ?? 0);
  }
  return total;
}

interface RawEvent {
  Action?: string;
  id?: string;
  Actor?: { Attributes?: { name?: string } };
}

/**
 * Turn a `docker inspect` payload into our RunningContainer.
 *
 * Exported and pure, so it is tested against captured inspect JSON - this
 * mapping is the fiddliest read Tidewaiter does, and getting a field wrong here
 * silently breaks a recreate later.
 */
export function toRunning(raw: RawContainerInspect): RunningContainer {
  return {
    id: raw.Id,
    pid: raw.State?.Pid ?? 0,
    // The config ID under `.Image`, kept for diagnostics only. The manifest
    // digest an update decision actually needs is resolved separately, via
    // imageDigest(ref), because inspect does not carry it.
    imageId: raw.Image ?? "",
    health: healthOf(raw.State?.Health?.Status),
    // Genuinely up: Running true and NOT mid-restart. An exited container has
    // Running=false; a crash-looping one has Running=true but Restarting=true -
    // both are "not staying up", which the uptime check treats as a failure.
    running: raw.State?.Running === true && raw.State?.Restarting !== true,
    spec: toSpec(raw),
  };
}

function healthOf(status: string | undefined): HealthStatus {
  switch (status) {
    case "starting":
    case "healthy":
    case "unhealthy":
      return status;
    default:
      return "none";
  }
}

/**
 * Extract the faithful-recreate spec from an inspect payload.
 *
 * Only the fields recreate.ts knows how to re-apply are read; runtime-only
 * fields (Id, State, the live IP under NetworkSettings) are deliberately left
 * behind so they can never leak back into a create call.
 */
export function toSpec(raw: RawContainerInspect): ContainerSpec {
  const config = raw.Config ?? {};
  const hostConfig = raw.HostConfig ?? {};

  return {
    name: (raw.Name ?? raw.Id).replace(/^\//, ""),
    image: config.Image ?? "",
    env: config.Env ?? [],
    cmd: config.Cmd ?? undefined,
    entrypoint: config.Entrypoint ?? undefined,
    labels: config.Labels ?? {},
    mounts: mountsOf(raw.Mounts ?? hostConfig.Mounts ?? undefined),
    published: publishedPorts(hostConfig.PortBindings ?? undefined),
    exposed: Object.keys(config.ExposedPorts ?? {}),
    networks: networksOf(raw.NetworkSettings?.Networks ?? undefined),
    networkMode: hostConfig.NetworkMode,
    restartPolicy: restartPolicyOf(hostConfig.RestartPolicy ?? undefined),
    capAdd: hostConfig.CapAdd ?? [],
    capDrop: hostConfig.CapDrop ?? [],
    privileged: hostConfig.Privileged ?? false,
    devices: devicesOf(hostConfig.Devices ?? undefined),
    extraHosts: hostConfig.ExtraHosts ?? [],
    sysctls: hostConfig.Sysctls ?? {},
    user: config.User || undefined,
    workingDir: config.WorkingDir || undefined,
    stopSignal: config.StopSignal || undefined,
    stopTimeout: config.StopTimeout ?? undefined,
    healthcheck: healthcheckOf(config.Healthcheck ?? undefined),
  };
}

function mountsOf(raw: readonly RawMount[] | undefined): Mount[] {
  if (!raw) return [];
  const mounts: Mount[] = [];
  for (const mount of raw) {
    const type = mount.Type;
    if (type !== "bind" && type !== "volume" && type !== "tmpfs") continue;
    mounts.push({
      type,
      // A named volume must be reattached by its Name, not by the host _data
      // path Docker also reports - recreating from the path would make a fresh
      // bind, not reuse the volume. A bind has no Name, so it falls back to
      // Source, which for a bind is the host path we do want.
      source: type === "volume" ? mount.Name ?? mount.Source ?? "" : mount.Source ?? mount.Name ?? "",
      target: mount.Destination ?? "",
      readonly: mount.RW === false,
      // A named volume has a Name; an anonymous one is a 64-hex Docker id used
      // as both Name and Source. recreate.ts refuses those rather than minting
      // a fresh empty volume and silently losing the data.
      anonymous: type === "volume" && isAnonymousVolume(mount.Name),
    });
  }
  return mounts;
}

function isAnonymousVolume(name: string | undefined): boolean {
  return name !== undefined && /^[0-9a-f]{64}$/.test(name);
}

function publishedPorts(
  bindings: Record<string, { HostPort?: string }[] | null> | undefined,
): PublishedPort[] {
  if (!bindings) return [];
  const found = new Map<string, PublishedPort>();

  for (const [portProto, hosts] of Object.entries(bindings)) {
    if (!hosts) continue;
    const [portText, proto] = portProto.split("/");
    const protocol = proto as Protocol;
    if (protocol !== "tcp" && protocol !== "udp") continue;
    const containerPort = Number(portText);

    for (const host of hosts) {
      if (host.HostPort === undefined || host.HostPort === "") continue;
      const hostPort = Number(host.HostPort);
      const key = `${protocol}/${hostPort}`;
      if (!found.has(key)) found.set(key, { hostPort, containerPort, protocol });
    }
  }

  return [...found.values()];
}

function networksOf(
  networks:
    | Record<
        string,
        { Aliases?: string[] | null; IPAMConfig?: { IPv4Address?: string; IPv6Address?: string } | null }
      >
    | undefined,
): NetworkAttachment[] {
  if (!networks) return [];
  return Object.entries(networks).map(([name, network]) => ({
    name,
    aliases: network.Aliases ?? [],
    ipv4Address: network.IPAMConfig?.IPv4Address || undefined,
    ipv6Address: network.IPAMConfig?.IPv6Address || undefined,
  }));
}

function restartPolicyOf(
  policy: { Name?: string; MaximumRetryCount?: number } | undefined,
): RestartPolicy | undefined {
  if (!policy || !policy.Name || policy.Name === "no") return undefined;
  return { name: policy.Name, maximumRetryCount: policy.MaximumRetryCount ?? 0 };
}

function devicesOf(
  devices: { PathOnHost?: string; PathInContainer?: string; CgroupPermissions?: string }[] | undefined,
): Device[] {
  if (!devices) return [];
  return devices.map((device) => ({
    pathOnHost: device.PathOnHost ?? "",
    pathInContainer: device.PathInContainer ?? "",
    cgroupPermissions: device.CgroupPermissions ?? "",
  }));
}

function healthcheckOf(
  check: { Test?: string[]; Interval?: number; Timeout?: number; Retries?: number; StartPeriod?: number } | undefined,
): Healthcheck | undefined {
  if (!check || !check.Test || check.Test.length === 0) return undefined;
  return {
    test: check.Test,
    intervalNanos: check.Interval,
    timeoutNanos: check.Timeout,
    retries: check.Retries,
    startPeriodNanos: check.StartPeriod,
  };
}

/**
 * The first digest Docker records for a local image.
 *
 * RepoDigests entries look like `nginx@sha256:...`; we want the `sha256:...`
 * part, which is what the registry client also returns, so the two compare.
 */
function firstDigest(repoDigests: readonly string[] | null | undefined): Digest | undefined {
  const first = repoDigests?.[0];
  if (first === undefined) return undefined;
  const at = first.lastIndexOf("@");
  return at === -1 ? first : first.slice(at + 1);
}

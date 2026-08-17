/**
 * The vocabulary the rest of Tidewaiter is written in.
 *
 * The important distinction, mirroring portical's Rule-vs-Mapping split, is
 * "what is running now" (RunningContainer, read off Docker) versus "what we
 * want it pinned to" (the registry's digest for its tag). Every update decision
 * is a comparison of the two, so keeping them apart keeps the decision honest.
 */

export type Protocol = "tcp" | "udp";

export const PROTOCOLS: readonly Protocol[] = ["tcp", "udp"];

/**
 * An image digest.
 *
 * Opaque outside equality - it is compared, never parsed. `sha256:abc...` for a
 * manifest or config digest. Two containers on the same digest are the same
 * image however they were tagged, which is the whole basis of "is there
 * anything new to pull?".
 */
export type Digest = string;

/** The Docker health states, plus "none" for a container with no HEALTHCHECK. */
export type HealthStatus = "starting" | "healthy" | "unhealthy" | "none";

/** A port the container publishes on the Docker host. */
export interface PublishedPort {
  readonly hostPort: number;
  readonly containerPort: number;
  readonly protocol: Protocol;
}

/**
 * Everything about a container that a faithful recreate must reproduce.
 *
 * Read off `docker inspect` and re-applied verbatim on recreate, minus the
 * image. Deliberately a flat, hand-picked set of fields rather than the whole
 * inspect blob: the blob also carries runtime-only state (the container's id,
 * its current IP, its PID) that must never be sent back to /containers/create.
 * recreate.ts owns the mapping in both directions; this is the shape in the
 * middle.
 */
export interface ContainerSpec {
  readonly name: string;
  /** The image ref as configured, e.g. "nginx:1.27" - the tag we keep chasing. */
  readonly image: string;
  readonly env: readonly string[];
  readonly cmd?: readonly string[];
  readonly entrypoint?: readonly string[];
  readonly labels: Readonly<Record<string, string>>;
  readonly mounts: readonly Mount[];
  readonly published: readonly PublishedPort[];
  readonly exposed: readonly string[];
  readonly networks: readonly NetworkAttachment[];
  readonly networkMode?: string;
  readonly restartPolicy?: RestartPolicy;
  readonly capAdd: readonly string[];
  readonly capDrop: readonly string[];
  readonly privileged: boolean;
  readonly devices: readonly Device[];
  readonly extraHosts: readonly string[];
  readonly sysctls: Readonly<Record<string, string>>;
  readonly user?: string;
  readonly workingDir?: string;
  readonly stopSignal?: string;
  /** The container's own stop timeout, in seconds, if it set one. */
  readonly stopTimeout?: number;
  readonly healthcheck?: Healthcheck;
}

/** A mount, faithful to Docker's long-form so bind/volume/tmpfs survive recreate. */
export interface Mount {
  readonly type: "bind" | "volume" | "tmpfs";
  readonly source: string;
  readonly target: string;
  readonly readonly: boolean;
  /** True for a volume with no name - recreate cannot reproduce it faithfully. */
  readonly anonymous: boolean;
}

/** A network the container is attached to, with anything needed to reattach it. */
export interface NetworkAttachment {
  readonly name: string;
  readonly aliases: readonly string[];
  /** A static address, when one was assigned rather than handed out by IPAM. */
  readonly ipv4Address?: string;
  readonly ipv6Address?: string;
  /**
   * The address the container is ACTUALLY reachable at right now, as Docker
   * reports it under `NetworkSettings.Networks[name].IPAddress` - the runtime
   * IP IPAM handed out, distinct from the static `ipv4Address` a user pinned.
   *
   * Runtime-only, so recreate.ts must NOT send it back to /containers/create
   * (like the PID, it is live state). It exists for one reader: the port-connect
   * health probe, which connects to `ipAddress:containerPort` to test the exact
   * path a client takes on a bridge/user network - bypassing docker-proxy, which
   * would falsely accept, and the netns bind check, which is blind to *which*
   * address a socket listens on.
   */
  readonly ipAddress?: string;
}

export interface RestartPolicy {
  readonly name: string;
  readonly maximumRetryCount: number;
}

export interface Device {
  readonly pathOnHost: string;
  readonly pathInContainer: string;
  readonly cgroupPermissions: string;
}

export interface Healthcheck {
  readonly test: readonly string[];
  readonly intervalNanos?: number;
  readonly timeoutNanos?: number;
  readonly retries?: number;
  readonly startPeriodNanos?: number;
}

/**
 * A container as it is running now, with the facts an update decision needs.
 *
 * `pid` is the container's main process as seen from the host - Tidewaiter runs
 * in the host PID namespace, so this is directly usable to read
 * /proc/<pid>/net/* for the netns-scoped detectors and health probes.
 */
export interface RunningContainer {
  readonly id: string;
  readonly pid: number;
  readonly spec: ContainerSpec;
  /**
   * The image config ID this container was created from, as inspect reports it
   * under `.Image` (a local content-address of the image *config*).
   *
   * NOT the registry manifest digest, and deliberately NOT used to decide
   * whether an update is due - those two are different hashes and would never
   * compare equal. The update decision compares the registry's manifest digest
   * for the tag against the *local image's* RepoDigest, both resolved by the
   * daemon via imageDigest(); this field is kept only for logging/diagnostics.
   */
  readonly imageId: string;
  readonly health: HealthStatus;
  /**
   * Whether the container is genuinely up right now: `State.Running` true AND
   * `State.Restarting` false. A container that has exited (`Status=exited`) or
   * is crash-looping under a restart policy (`Status=restarting`) is NOT
   * running by this measure - both mean the new image failed to stay up, which
   * the uptime health check reads as an active failure.
   */
  readonly running: boolean;
}

/** What the registry says this container's tag should resolve to, this pass. */
export interface DesiredImage {
  readonly container: string;
  readonly ref: string;
  readonly digest: Digest;
}

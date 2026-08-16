import { PROTOCOLS, type Protocol, type PublishedPort } from "./model.ts";

/**
 * The labels Tidewaiter reads off a container, mirroring portical's
 * `portical.upnp.*` convention so the two read as a family.
 *
 * `AUTOUPDATE` is the opt-in, Podman-style: its *value* is the update policy,
 * and a container is managed only if it carries the label with a value we
 * recognise. There is no separate boolean - presence of a valid policy is the
 * opt-in, absence (or an unrecognised value) means "leave it alone entirely".
 */
export const AUTOUPDATE = "tidewaiter.autoupdate";
export const DETECTOR = "tidewaiter.detector";
export const PORTS = "tidewaiter.ports";
export const IDLE_SAMPLES = "tidewaiter.idle-samples";
export const HEALTH = "tidewaiter.health";
export const HEALTH_TIMEOUT = "tidewaiter.health-timeout";
export const GRACE = "tidewaiter.grace";
export const KEEP_IMAGES = "tidewaiter.keep-images";

/**
 * The update policies, mirroring Podman's `io.containers.autoupdate`:
 *   registry - compare the tag against the registry, pull if the digest moved.
 *   local    - compare the tag against the locally-stored image, no registry
 *              contact; recreate if a build/pull changed it under us.
 */
export type PolicyName = "registry" | "local";
export type DetectorName = "conntrack" | "tcp" | "netio" | "none";
export type HealthName = "docker" | "port-connect" | "port-bound" | "uptime";

const POLICY_NAMES: readonly PolicyName[] = ["registry", "local"];
const DETECTOR_NAMES: readonly DetectorName[] = ["conntrack", "tcp", "netio", "none"];
const HEALTH_NAMES: readonly HealthName[] = ["docker", "port-connect", "port-bound", "uptime"];

/** The default health gate: every check, combined (any active failure rolls back). */
export const DEFAULT_HEALTH: readonly HealthName[] = ["docker", "port-connect", "port-bound", "uptime"];

/**
 * A container's resolved Tidewaiter policy.
 *
 * Only ever built for a container that is validly opted in, so `autoupdate` is
 * always a real policy, never absent. The rest have defaults, so an opted-in
 * container with only `tidewaiter.autoupdate=registry` gets a complete, sensible
 * policy. `ports` undefined means "use whatever the container publishes".
 */
export interface ContainerPolicy {
  readonly autoupdate: PolicyName;
  readonly detector: DetectorName;
  /**
   * Which published ports the activity gate and health probe should consider.
   *
   * Undefined means "every port the container publishes", which is the right
   * default. The override exists for a specific, real case: a container that
   * publishes a service port *and* a port that gets constant automated traffic -
   * a Prometheus metrics endpoint scraped every few seconds, an admin/health
   * port a load balancer polls. Those keep live flows open the whole time, so
   * the gate would read the container "busy" forever and it would never update.
   * Naming just the service port (e.g. `tidewaiter.ports=25565`) scopes both the
   * idle check and the health probe to the port that actually matters. (Note
   * this is unrelated to the TIME_WAIT filtering in the conntrack detector -
   * that drops *closed* connections; this drops a whole *port* from
   * consideration, including its live traffic.)
   */
  readonly ports?: readonly PublishedPort[];
  readonly idleSamples: number;
  /**
   * The set of health checks that gate the swap, combined.
   *
   * A comma-separated `tidewaiter.health` list, default all four
   * (docker, port-connect, port-bound, uptime). The gate rolls back only if a
   * check ACTIVELY fails; a check that merely never confirms does not veto (see
   * health.ts combine()), because false-unhealthy is the worse error. Each check
   * covers a different blind spot - docker tests app internals, port-connect
   * tests reachability, port-bound bypasses docker-proxy and is the only UDP
   * signal, uptime catches a container that exits or crash-loops.
   */
  readonly health: readonly HealthName[];
  readonly healthTimeoutSeconds: number;
  /** Undefined means "use the container's own stop timeout". */
  readonly graceSeconds?: number;
  readonly keepImages: number;
}

/** The defaults every field but `autoupdate` falls back to. */
export const POLICY_DEFAULTS = {
  detector: "conntrack",
  idleSamples: 3,
  health: DEFAULT_HEALTH,
  healthTimeoutSeconds: 120,
  keepImages: 1,
} as const satisfies Omit<ContainerPolicy, "autoupdate" | "ports" | "graceSeconds">;

export interface ParsedPolicy {
  /**
   * The resolved policy, or undefined when the container is not opted in.
   *
   * Undefined is the important state: no `tidewaiter.autoupdate` label, or one
   * whose value is not a known policy. The daemon leaves such a container
   * completely alone - it is never inspected for activity, never updated.
   */
  readonly policy?: ContainerPolicy;
  readonly warnings: readonly string[];
}

/**
 * Read a container's labels into a policy, collecting problems as data.
 *
 * Opt-in is decided first and decisively: without a `tidewaiter.autoupdate`
 * label whose value is a known policy, this returns `{ policy: undefined }` and
 * the container is not managed at all. An unrecognised value is a warning (a
 * typo like `autoupdate=registy` should say so, not silently do nothing).
 *
 * Once opted in, the rest of the labels are parsed with defaults, warnings
 * returned rather than thrown, following portical's `parseLabel`: one
 * misconfigured secondary label must not stop the container being managed with
 * the default for that setting.
 *
 * `published` is the container's actual published ports, needed to validate a
 * `tidewaiter.ports` override against reality - an override naming a port the
 * container does not publish is a warning and dropped.
 */
export function parseLabels(
  labels: Readonly<Record<string, string>>,
  published: readonly PublishedPort[],
): ParsedPolicy {
  const warnings: string[] = [];

  const autoupdateValue = labels[AUTOUPDATE];
  if (autoupdateValue === undefined) {
    // No opt-in label at all: not ours to manage. Silent - this is the common
    // case for the vast majority of containers on a host.
    return { policy: undefined, warnings };
  }
  const autoupdate = POLICY_NAMES.find((name) => name === autoupdateValue.trim().toLowerCase());
  if (autoupdate === undefined) {
    // Opted in by the key, but the value is not a policy we know. Warn and leave
    // it alone rather than guess - never update on an ambiguous instruction.
    return {
      policy: undefined,
      warnings: [`${AUTOUPDATE}: '${autoupdateValue}' is not a known policy (${POLICY_NAMES.join(", ")}); ignoring this container`],
    };
  }

  const detector = oneOf(labels[DETECTOR], DETECTOR_NAMES, POLICY_DEFAULTS.detector, DETECTOR, warnings);
  const health = parseHealth(labels[HEALTH], warnings);

  const idleSamples = positiveInt(labels[IDLE_SAMPLES], POLICY_DEFAULTS.idleSamples, IDLE_SAMPLES, warnings);
  const healthTimeoutSeconds = positiveInt(
    labels[HEALTH_TIMEOUT], POLICY_DEFAULTS.healthTimeoutSeconds, HEALTH_TIMEOUT, warnings,
  );
  const keepImages = nonNegativeInt(labels[KEEP_IMAGES], POLICY_DEFAULTS.keepImages, KEEP_IMAGES, warnings);

  const graceSeconds = labels[GRACE] === undefined
    ? undefined
    : nonNegativeInt(labels[GRACE], 0, GRACE, warnings);

  const ports = labels[PORTS] === undefined
    ? undefined
    : selectPorts(labels[PORTS], published, warnings);

  return {
    policy: {
      autoupdate, detector, ports, idleSamples, health, healthTimeoutSeconds, graceSeconds, keepImages,
    },
    warnings,
  };
}

/**
 * Parse a comma-separated `tidewaiter.health` list into the checks to run.
 *
 * Absent -> the default set (all four). Unknown names are warned about and
 * dropped, keeping the recognised ones. `port` (the old single value) is no
 * longer a check name; it is mapped to `port-connect,port-bound` with a note so
 * old labels keep meaning what they did. An empty or all-unknown list falls back
 * to the default rather than leaving a container with no gate.
 */
function parseHealth(value: string | undefined, warnings: string[]): readonly HealthName[] {
  if (value === undefined) return DEFAULT_HEALTH;

  const chosen: HealthName[] = [];
  for (const raw of value.split(",")) {
    const term = raw.trim().toLowerCase();
    if (term === "") continue;

    if (term === "port") {
      // The former all-in-one value, split into its two mechanisms.
      for (const name of ["port-connect", "port-bound"] as const) {
        if (!chosen.includes(name)) chosen.push(name);
      }
      continue;
    }

    const name = HEALTH_NAMES.find((n) => n === term);
    if (name === undefined) {
      warnings.push(`${HEALTH}: '${raw.trim()}' is not a known check (${HEALTH_NAMES.join(", ")}); ignoring it`);
      continue;
    }
    if (!chosen.includes(name)) chosen.push(name);
  }

  return chosen.length > 0 ? chosen : DEFAULT_HEALTH;
}

const MAX_PORT = 65535;
const PORT = /^(\d{1,5})(?:\/(tcp|udp))?$/i;

/**
 * Resolve a `tidewaiter.ports` override against the container's real ports.
 *
 * Each term is `port` or `port/proto`; a term with no protocol matches either.
 * A term that names a port the container does not actually publish is reported
 * and dropped - it is almost always a typo, and silently watching a port
 * nobody serves would read as "always idle" and update a live container.
 */
function selectPorts(
  value: string,
  published: readonly PublishedPort[],
  warnings: string[],
): readonly PublishedPort[] {
  const selected: PublishedPort[] = [];

  for (const raw of value.split(",")) {
    const term = raw.trim();
    if (term === "") continue;

    const match = PORT.exec(term);
    if (!match) {
      warnings.push(`${PORTS}: ignoring unrecognised port '${term}'`);
      continue;
    }

    const hostPort = Number(match[1]);
    if (hostPort < 1 || hostPort > MAX_PORT) {
      warnings.push(`${PORTS}: ignoring '${term}', ports must be between 1 and ${MAX_PORT}`);
      continue;
    }

    const protocol = match[2]?.toLowerCase() as Protocol | undefined;
    const matching = published.filter(
      (p) => p.hostPort === hostPort && (protocol === undefined || p.protocol === protocol),
    );

    if (matching.length === 0) {
      warnings.push(
        `${PORTS}: '${term}' is not a port this container publishes; ignoring it`,
      );
      continue;
    }

    selected.push(...matching);
  }

  return dedupePorts(selected);
}

function dedupePorts(ports: readonly PublishedPort[]): PublishedPort[] {
  const byKey = new Map<string, PublishedPort>();
  for (const port of ports) {
    const key = `${port.protocol}/${port.hostPort}`;
    if (!byKey.has(key)) byKey.set(key, port);
  }
  return [...byKey.values()];
}

function oneOf<T extends string>(
  value: string | undefined, allowed: readonly T[], fallback: T, name: string, warnings: string[],
): T {
  if (value === undefined) return fallback;
  const lower = value.trim().toLowerCase() as T;
  if (allowed.includes(lower)) return lower;
  warnings.push(`${name}: '${value}' is not one of ${allowed.join(", ")}; using ${fallback}`);
  return fallback;
}

function positiveInt(value: string | undefined, fallback: number, name: string, warnings: string[]): number {
  const parsed = integer(value, name, warnings);
  if (parsed === undefined) return fallback;
  if (parsed < 1) {
    warnings.push(`${name}: '${value}' must be at least 1; using ${fallback}`);
    return fallback;
  }
  return parsed;
}

function nonNegativeInt(value: string | undefined, fallback: number, name: string, warnings: string[]): number {
  const parsed = integer(value, name, warnings);
  if (parsed === undefined) return fallback;
  if (parsed < 0) {
    warnings.push(`${name}: '${value}' must not be negative; using ${fallback}`);
    return fallback;
  }
  return parsed;
}

function integer(value: string | undefined, name: string, warnings: string[]): number | undefined {
  if (value === undefined) return undefined;
  const parsed = Number(value);
  if (!Number.isInteger(parsed)) {
    warnings.push(`${name}: '${value}' is not a whole number; ignoring it`);
    return undefined;
  }
  return parsed;
}

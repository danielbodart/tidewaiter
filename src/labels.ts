import { PROTOCOLS, type Protocol, type PublishedPort } from "./model.ts";

/**
 * The labels Tidewaiter reads off a container, mirroring portical's
 * `portical.upnp.*` convention so the two read as a family.
 */
export const ENABLE = "tidewaiter.enable";
export const POLICY = "tidewaiter.policy";
export const DETECTOR = "tidewaiter.detector";
export const PORTS = "tidewaiter.ports";
export const IDLE_SAMPLES = "tidewaiter.idle-samples";
export const HEALTH = "tidewaiter.health";
export const HEALTH_TIMEOUT = "tidewaiter.health-timeout";
export const GRACE = "tidewaiter.grace";
export const KEEP_IMAGES = "tidewaiter.keep-images";

export type DetectorName = "conntrack" | "tcp" | "netio" | "none";
export type HealthName = "docker" | "port" | "uptime";
export type PolicyName = "registry";

const DETECTOR_NAMES: readonly DetectorName[] = ["conntrack", "tcp", "netio", "none"];
const HEALTH_NAMES: readonly HealthName[] = ["docker", "port", "uptime"];

/**
 * A container's resolved Tidewaiter policy.
 *
 * Every field has a default, so an opted-in container with only
 * `tidewaiter.enable=true` gets a complete, sensible policy. `ports` is left
 * undefined to mean "use whatever the container publishes"; a bad override is
 * reported as a warning and dropped rather than silently narrowing the set.
 */
export interface ContainerPolicy {
  readonly enable: boolean;
  readonly policy: PolicyName;
  readonly detector: DetectorName;
  /** Undefined means "every port the container publishes". */
  readonly ports?: readonly PublishedPort[];
  readonly idleSamples: number;
  readonly health: HealthName;
  readonly healthTimeoutSeconds: number;
  /** Undefined means "use the container's own stop timeout". */
  readonly graceSeconds?: number;
  readonly keepImages: number;
}

export const DEFAULT_POLICY: ContainerPolicy = {
  enable: false,
  policy: "registry",
  detector: "conntrack",
  idleSamples: 3,
  health: "port",
  healthTimeoutSeconds: 120,
  keepImages: 1,
};

export interface ParsedPolicy {
  readonly policy: ContainerPolicy;
  readonly warnings: readonly string[];
}

/**
 * Read a container's labels into a policy, collecting problems as data.
 *
 * Warnings are returned rather than thrown, following portical's `parseLabel`:
 * one misconfigured label must not stop the container being managed with the
 * defaults, nor take the daemon down. The caller logs them.
 *
 * `published` is the container's actual published ports, needed to validate a
 * `tidewaiter.ports` override against reality - an override naming a port the
 * container does not publish is a warning, and (see the detector) is treated as
 * "cannot tell", never as "idle".
 */
export function parseLabels(
  labels: Readonly<Record<string, string>>,
  published: readonly PublishedPort[],
): ParsedPolicy {
  const warnings: string[] = [];

  const enable = boolean(labels[ENABLE], DEFAULT_POLICY.enable, ENABLE, warnings);

  const policy = oneOf(labels[POLICY], ["registry"], DEFAULT_POLICY.policy, POLICY, warnings);
  const detector = oneOf(labels[DETECTOR], DETECTOR_NAMES, DEFAULT_POLICY.detector, DETECTOR, warnings);
  const health = oneOf(labels[HEALTH], HEALTH_NAMES, DEFAULT_POLICY.health, HEALTH, warnings);

  const idleSamples = positiveInt(labels[IDLE_SAMPLES], DEFAULT_POLICY.idleSamples, IDLE_SAMPLES, warnings);
  const healthTimeoutSeconds = positiveInt(
    labels[HEALTH_TIMEOUT], DEFAULT_POLICY.healthTimeoutSeconds, HEALTH_TIMEOUT, warnings,
  );
  const keepImages = nonNegativeInt(labels[KEEP_IMAGES], DEFAULT_POLICY.keepImages, KEEP_IMAGES, warnings);

  const graceSeconds = labels[GRACE] === undefined
    ? undefined
    : nonNegativeInt(labels[GRACE], 0, GRACE, warnings);

  const ports = labels[PORTS] === undefined
    ? undefined
    : selectPorts(labels[PORTS], published, warnings);

  return {
    policy: {
      enable, policy, detector, ports, idleSamples, health, healthTimeoutSeconds, graceSeconds, keepImages,
    },
    warnings,
  };
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

function boolean(value: string | undefined, fallback: boolean, name: string, warnings: string[]): boolean {
  if (value === undefined) return fallback;
  const lower = value.trim().toLowerCase();
  if (lower === "true" || lower === "1" || lower === "yes") return true;
  if (lower === "false" || lower === "0" || lower === "no") return false;
  warnings.push(`${name}: '${value}' is not a boolean; using ${fallback}`);
  return fallback;
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

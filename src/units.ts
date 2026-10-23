import { readdir, readFile } from "node:fs/promises";
import { join } from "node:path";
import { dedupePorts, nonNegativeInt, oneOf, parsePortTerm, positiveInt, POLICY_DEFAULTS } from "./labels.ts";
import { PROTOCOLS, type PublishedPort } from "./model.ts";

/**
 * The checks a unit's swap can be gated on.
 *
 * Docker's set minus `docker` (a unit has no HEALTHCHECK), plus `systemd`:
 * the restart job's result and the unit's own ActiveState, NRestarts and
 * InvocationID, which for a Type=notify unit is the program itself saying it
 * is up.
 */
export type UnitHealthName = "systemd" | "port-connect" | "port-bound" | "uptime";
/** `netio` reads Docker's /stats, which a unit has no equivalent of. */
export type UnitDetectorName = "conntrack" | "tcp" | "none";

const UNIT_HEALTH_NAMES: readonly UnitHealthName[] = ["systemd", "port-connect", "port-bound", "uptime"];
const UNIT_DETECTOR_NAMES: readonly UnitDetectorName[] = ["conntrack", "tcp", "none"];

export const DEFAULT_UNIT_HEALTH: readonly UnitHealthName[] = UNIT_HEALTH_NAMES;

/**
 * A unit's resolved Tidewaiter policy, from its file in the units directory.
 *
 * The container policy's shape where the two overlap, so the shared pieces
 * (decide, the detectors, the checks) read it the same way. What a unit adds is
 * where its program lives: `profile` is the Nix profile its ExecStart runs
 * from, and `desired` the store path that profile should point at.
 */
export interface UnitPolicy {
  /** The unit's full name, always with a suffix: "chandlery-bedrock.service". */
  readonly unit: string;
  /** The store path the profile should point at. */
  readonly desired: string;
  /** The Nix profile the unit's ExecStart runs from. */
  readonly profile: string;
  readonly detector: UnitDetectorName;
  /**
   * The unit's ports, which ARE its published ports: a host unit has no
   * mapping, so `hostPort === containerPort`. Empty means the activity gate
   * cannot tell, and fails safe to busy unless the detector is `none`.
   */
  readonly ports: readonly PublishedPort[];
  readonly idleSamples: number;
  readonly health: readonly UnitHealthName[];
  readonly healthTimeoutSeconds: number;
  /** Previous profile generations kept, for a manual rollback. */
  readonly keepGenerations: number;
  /** Maintain a runtime drop-in binding the profile's closure into a confined unit. */
  readonly bindClosure: boolean;
}

export interface ParsedUnitPolicy {
  /** Undefined when the file cannot be used at all; the warnings say why. */
  readonly policy?: UnitPolicy;
  readonly warnings: readonly string[];
}

/**
 * Read one units-directory file into a policy, collecting problems as data.
 *
 * Only `unit`, `desired` and `profile` are required; a file without them, or
 * that is not a JSON object, is skipped with a warning - never guessed at,
 * because a guessed profile is a profile we would overwrite. Every other field
 * falls back to its default with a warning, as a bad label does: one wrong
 * setting must not stop the unit being managed.
 *
 * `fileName` is only for the warnings; `unit` is the identity. A `unit`
 * written without a suffix is taken to be a service.
 */
export function parseUnitPolicy(text: string, fileName: string): ParsedUnitPolicy {
  const warnings: string[] = [];
  const skip = (reason: string): ParsedUnitPolicy => ({ warnings: [`${fileName}: ${reason}; skipping it`] });

  let json: unknown;
  try {
    json = JSON.parse(text);
  } catch (error) {
    return skip(`not valid JSON (${(error as Error).message})`);
  }
  if (typeof json !== "object" || json === null || Array.isArray(json)) return skip("not a JSON object");
  const fields = json as Record<string, unknown>;

  const unitName = requiredString(fields, "unit");
  const desired = requiredString(fields, "desired");
  const profile = requiredString(fields, "profile");
  if (unitName === undefined) return skip("'unit' must be a non-empty string");
  if (desired === undefined) return skip("'desired' must be a non-empty string");
  if (profile === undefined) return skip("'profile' must be a non-empty string");
  if (!desired.startsWith("/")) return skip(`'desired' must be an absolute path, got '${desired}'`);
  if (!profile.startsWith("/")) return skip(`'profile' must be an absolute path, got '${profile}'`);

  const unit = /\.[a-z]+$/.test(unitName) ? unitName : `${unitName}.service`;
  const warn = (field: string) => `${fileName}: ${field}`;

  const detector = oneOf(
    stringField(fields, "detector", warnings, fileName), UNIT_DETECTOR_NAMES, "conntrack", warn("detector"), warnings,
  );
  const ports = parsePorts(fields.ports, fileName, warnings);
  const health = parseHealth(fields.health, fileName, warnings);
  const idleSamples = positiveInt(
    numberField(fields, "idleSamples"), POLICY_DEFAULTS.idleSamples, warn("idleSamples"), warnings,
  );
  const healthTimeoutSeconds = positiveInt(
    numberField(fields, "healthTimeout"), POLICY_DEFAULTS.healthTimeoutSeconds, warn("healthTimeout"), warnings,
  );
  const keepGenerations = nonNegativeInt(numberField(fields, "keepGenerations"), 1, warn("keepGenerations"), warnings);

  let bindClosure = false;
  if (fields.bindClosure !== undefined) {
    if (typeof fields.bindClosure === "boolean") bindClosure = fields.bindClosure;
    else warnings.push(`${fileName}: bindClosure: '${String(fields.bindClosure)}' is not true or false; using false`);
  }

  return {
    policy: {
      unit, desired, profile, detector, ports, idleSamples, health, healthTimeoutSeconds, keepGenerations, bindClosure,
    },
    warnings,
  };
}

function requiredString(fields: Record<string, unknown>, name: string): string | undefined {
  const value = fields[name];
  return typeof value === "string" && value.trim() !== "" ? value.trim() : undefined;
}

function stringField(
  fields: Record<string, unknown>, name: string, warnings: string[], fileName: string,
): string | undefined {
  const value = fields[name];
  if (value === undefined || typeof value === "string") return value;
  warnings.push(`${fileName}: ${name}: '${String(value)}' is not a string; ignoring it`);
  return undefined;
}

/** A number field as the label helpers read it: text, so one validator serves both. */
function numberField(fields: Record<string, unknown>, name: string): string | undefined {
  const value = fields[name];
  return value === undefined ? undefined : String(value);
}

/**
 * The unit's ports, `tidewaiter.ports` grammar, as a JSON array.
 *
 * Not checked against "what is published", as a container's override is: a
 * host unit publishes nothing, so the declared ports are all there is. A bare
 * port means both protocols.
 */
function parsePorts(value: unknown, fileName: string, warnings: string[]): readonly PublishedPort[] {
  if (value === undefined) return [];
  if (!Array.isArray(value)) {
    warnings.push(`${fileName}: ports: must be a list like ["19132/udp", "25565"]; ignoring it`);
    return [];
  }

  const ports: PublishedPort[] = [];
  for (const raw of value) {
    const parsed = parsePortTerm(String(raw).trim());
    if (typeof parsed === "string") {
      warnings.push(`${fileName}: ports: ${parsed}`);
      continue;
    }
    for (const protocol of parsed.protocol === undefined ? PROTOCOLS : [parsed.protocol]) {
      ports.push({ hostPort: parsed.port, containerPort: parsed.port, protocol });
    }
  }
  return dedupePorts(ports);
}

/**
 * The checks to gate on, as a JSON array. Absent, empty or all-unknown is the
 * default set, so a unit is never left with no gate; `docker` is named
 * specifically, because it is the one a container's policy copied over would
 * bring.
 */
function parseHealth(value: unknown, fileName: string, warnings: string[]): readonly UnitHealthName[] {
  if (value === undefined) return DEFAULT_UNIT_HEALTH;
  if (!Array.isArray(value)) {
    warnings.push(`${fileName}: health: must be a list like ["systemd", "uptime"]; using the default`);
    return DEFAULT_UNIT_HEALTH;
  }

  const chosen: UnitHealthName[] = [];
  for (const raw of value) {
    const term = String(raw).trim().toLowerCase();
    if (term === "docker") {
      warnings.push(`${fileName}: health: 'docker' needs a container's HEALTHCHECK, which a unit does not have; ignoring it`);
      continue;
    }
    const name = UNIT_HEALTH_NAMES.find((n) => n === term);
    if (name === undefined) {
      warnings.push(`${fileName}: health: '${String(raw)}' is not a known check (${UNIT_HEALTH_NAMES.join(", ")}); ignoring it`);
      continue;
    }
    if (!chosen.includes(name)) chosen.push(name);
  }
  return chosen.length > 0 ? chosen : DEFAULT_UNIT_HEALTH;
}

/** One file from the units directory, unparsed. */
export interface UnitFile {
  readonly name: string;
  readonly text: string;
}

/**
 * Where managed units are declared: one JSON file per unit.
 *
 * An interface so tests hand files in from memory; the real one reads a
 * directory each pass, so a unit added or dropped by a switch is picked up
 * without restarting Tidewaiter.
 */
export interface UnitsDirectory {
  read(): Promise<UnitFile[]>;
}

export class DirUnitsDirectory implements UnitsDirectory {
  constructor(readonly path: string) {}

  /** Every `*.json` in the directory, sorted; a directory that is not there declares nothing. */
  async read(): Promise<UnitFile[]> {
    let names: string[];
    try {
      names = await readdir(this.path);
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === "ENOENT") return [];
      throw error;
    }
    const files: UnitFile[] = [];
    for (const name of names.filter((n) => n.endsWith(".json")).sort()) {
      files.push({ name, text: await readFile(join(this.path, name), "utf8") });
    }
    return files;
  }
}

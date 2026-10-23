import { join } from "node:path";
import { NixClosureBinder } from "./closure.ts";
import { CliConntrackSource } from "./conntrack.ts";
import { Daemon, DEFAULTS, type Options } from "./daemon.ts";
import { systemBusPath } from "./dbus.ts";
import { HttpDockerClient } from "./docker.ts";
import { DockerSource } from "./docker-source.ts";
import { BunTcpProbe } from "./health.ts";
import { http, overUnixSocket, withTimeout } from "./http.ts";
import { NoInhibitor, SystemdInhibitor } from "./inhibit.ts";
import { SsListenerSource } from "./listeners.ts";
import { ProcNetnsReader, resolveProcRoot } from "./netns.ts";
import { prepare } from "./prepare.ts";
import { NixProfiles } from "./profile.ts";
import { dockerConfigAuth, HttpRegistryClient } from "./registry.ts";
import type { Source, SourceName } from "./source.ts";
import { FileStateStore } from "./state-store.ts";
import { DbusSystemdClient } from "./systemd.ts";
import { SystemdSource } from "./systemd-source.ts";
import { DirUnitsDirectory } from "./units.ts";
import { version } from "./version.ts";

const DEFAULT_UNITS_DIR = "/etc/tidewaiter/units";
const DEFAULT_STATE_DIR = "/var/lib/tidewaiter";
const SOURCE_NAMES: readonly SourceName[] = ["docker", "systemd"];

const USAGE = `tidewaiter - update Docker containers and systemd units only when they are idle

Usage: tidewaiter [options] [command]

Commands:
  run       Reconcile continuously on an interval (default)
  check     Reconcile once and exit
  list      Show managed containers and units, and whether an update is waiting
  prepare   Create missing unit profiles and write closure drop-ins, then exit

Options:
  -s, --source NAME       docker or systemd; repeat or comma-separate for both
                          (default docker)
  -d, --interval SECONDS  Seconds between reconcile passes (default ${DEFAULTS.interval})
  -n, --dry-run           Report what would change without changing it
      --inhibit           Hold a logind shutdown/sleep lock while anything is busy
      --no-inhibit        Do not (the default)
      --activity-interval SECONDS
                          Seconds between the lock's activity checks (default ${DEFAULTS.activityInterval})
      --version           Show the version and exit
  -h, --help              Show this message

Docker source:
  -l, --label LABEL       Opt-in label to read (default ${DEFAULTS.label})
      --docker-socket P   Docker socket (default /var/run/docker.sock)

systemd source:
      --units-dir DIR     Where managed units are declared (default ${DEFAULT_UNITS_DIR})
      --state-dir DIR     Where pins are kept (default ${DEFAULT_STATE_DIR})

Environment:
  TIDEWAITER_SOURCES      Same as --source
  TIDEWAITER_INTERVAL     Same as --interval
  TIDEWAITER_INHIBIT      true or false, as --inhibit / --no-inhibit
  DOCKER_SOCKET           Same as --docker-socket
  TIDEWAITER_UNITS_DIR    Same as --units-dir
  STATE_DIRECTORY         Same as --state-dir, as systemd's StateDirectory= sets it

Opt a container in with 'tidewaiter.autoupdate=registry' (or =local). Tidewaiter
compares the image the container runs against what its tag should resolve to -
the registry's digest, or the locally-stored image for 'local' - and when they
differ it waits until the container is idle (no live network flows), pulls (for
registry), recreates, health-checks, and rolls back if the new image is
unhealthy. A container with no autoupdate label is never touched.

Opt a systemd unit in with a JSON file in the units directory naming the unit,
the Nix profile its ExecStart runs from, and the store path that profile should
point at. When they differ Tidewaiter waits until the unit is idle, sets the
profile, restarts the unit, health-checks it, and on failure switches the
profile back and restarts it again. The NixOS module writes these files.
`;

interface Parsed {
  readonly command: string;
  readonly options: Options;
  readonly socket: string;
  /** In the order given, each once. */
  readonly sources: readonly SourceName[];
  readonly unitsDir: string;
  readonly stateDir: string;
  readonly inhibit: boolean;
  readonly help: boolean;
  readonly showVersion: boolean;
}

export function parseArguments(argv: readonly string[], env: Record<string, string | undefined> = {}): Parsed {
  let command = "run";
  let socket = env.DOCKER_SOCKET ?? "/var/run/docker.sock";
  let unitsDir = env.TIDEWAITER_UNITS_DIR || DEFAULT_UNITS_DIR;
  // systemd joins several StateDirectory= entries with colons; the first is ours.
  let stateDir = env.STATE_DIRECTORY?.split(":")[0] || DEFAULT_STATE_DIR;
  let inhibit = env.TIDEWAITER_INHIBIT ? flag(env.TIDEWAITER_INHIBIT, "TIDEWAITER_INHIBIT") : false;
  let sources: SourceName[] | undefined;
  let help = false;
  let showVersion = false;
  const options: Record<string, unknown> = {
    ...DEFAULTS,
    interval: env.TIDEWAITER_INTERVAL ? number(env.TIDEWAITER_INTERVAL, "TIDEWAITER_INTERVAL") : DEFAULTS.interval,
  };

  const rest = [...argv];
  while (rest.length > 0) {
    const argument = rest.shift()!;
    const value = () => {
      const next = rest.shift();
      if (next === undefined) throw new Error(`${argument} needs a value`);
      return next;
    };

    switch (argument) {
      case "-s": case "--source": sources = [...(sources ?? []), ...sourceNames(value(), argument)]; break;
      case "-d": case "--interval": options.interval = number(value(), argument); break;
      case "--activity-interval": options.activityInterval = positive(value(), argument); break;
      case "-l": case "--label": options.label = value(); break;
      case "--docker-socket": socket = value(); break;
      case "--units-dir": unitsDir = value(); break;
      case "--state-dir": stateDir = value(); break;
      case "--inhibit": inhibit = true; break;
      case "--no-inhibit": inhibit = false; break;
      case "-n": case "--dry-run": options.dryRun = true; break;
      case "--version": showVersion = true; break;
      case "-h": case "--help": help = true; break;
      default:
        if (argument.startsWith("-")) throw new Error(`unknown option '${argument}'`);
        // A leading path to the binary, as an overridden entrypoint might pass,
        // is skipped rather than mistaken for a command.
        if (LEGACY_ENTRYPOINT.test(argument)) break;
        command = argument;
    }
  }

  // Docker by default, so the image and every existing compose file behave
  // exactly as they did before there was a choice.
  sources ??= env.TIDEWAITER_SOURCES ? sourceNames(env.TIDEWAITER_SOURCES, "TIDEWAITER_SOURCES") : ["docker"];

  return {
    command,
    options: options as unknown as Options,
    socket,
    sources: [...new Set(sources)],
    unitsDir,
    stateDir,
    inhibit,
    help,
    showVersion,
  };
}

function sourceNames(value: string, name: string): SourceName[] {
  const names = value.split(",").map((part) => part.trim()).filter((part) => part !== "");
  if (names.length === 0) throw new Error(`${name} needs a source name (${SOURCE_NAMES.join(", ")})`);
  return names.map((source) => {
    const known = SOURCE_NAMES.find((n) => n === source);
    if (!known) throw new Error(`${name}: '${source}' is not a source (${SOURCE_NAMES.join(", ")})`);
    return known;
  });
}

function flag(value: string, name: string): boolean {
  if (/^(1|true|yes|on)$/i.test(value)) return true;
  if (/^(0|false|no|off)$/i.test(value)) return false;
  throw new Error(`${name} must be true or false, got '${value}'`);
}

function positive(value: string, name: string): number {
  const parsed = number(value, name);
  if (parsed === 0) throw new Error(`${name} must be more than 0`);
  return parsed;
}

const LEGACY_ENTRYPOINT = /\/tidewaiter$/;

function number(value: string, name: string): number {
  const parsed = Number(value);
  if (!Number.isFinite(parsed) || parsed < 0) throw new Error(`${name} must be a positive number, got '${value}'`);
  return parsed;
}

export async function main(argv: readonly string[]): Promise<number> {
  let parsed: Parsed;
  try {
    parsed = parseArguments(argv, Bun.env);
  } catch (error) {
    console.error(`Error: ${(error as Error).message}\n`);
    console.error(USAGE);
    return 2;
  }

  if (parsed.showVersion) {
    console.log(version);
    return 0;
  }
  if (parsed.help) {
    console.log(USAGE);
    return 0;
  }

  console.log(`Tidewaiter ${version}`);

  const conntrack = new CliConntrackSource();
  // The host's /proc, bind-mounted read-only at /host/proc in the container, is
  // what lets us read another container's /proc/<pid>/net/* (host networking
  // shares the net ns, not the pid ns). Falls back to /proc when running native.
  const procRoot = await resolveProcRoot();
  const netns = new ProcNetnsReader(procRoot);
  // Opened only when a source or command needs it: the container has no bus.
  let systemd: DbusSystemdClient | undefined;
  const systemdClient = () => (systemd ??= new DbusSystemdClient(systemBusPath(Bun.env)));

  if (parsed.command === "prepare") {
    const result = await prepare({
      units: new DirUnitsDirectory(parsed.unitsDir),
      profiles: new NixProfiles(),
      closure: new NixClosureBinder(),
      systemd: systemdClient(),
      log: console.log,
    });
    console.log(
      `Prepared ${parsed.unitsDir}: ${result.seeded.length} profile(s) created, ` +
        `${result.bound.length} closure drop-in(s) written, ${result.failed.length} failed`,
    );
    return result.failed.length === 0 ? 0 : 1;
  }

  const sources: Source[] = [];
  for (const name of parsed.sources) {
    if (name === "docker") {
      const dockerClient = new HttpDockerClient(overUnixSocket(parsed.socket));
      // One credential source, shared by the registry client (for digest lookups)
      // and the daemon (for authenticated `docker pull`), so a private image both
      // resolves and pulls with the same creds from ~/.docker/config.json.
      const auth = await dockerConfigAuth();
      const registry = new HttpRegistryClient(withTimeout(http, 30_000), auth);
      sources.push(new DockerSource(dockerClient, registry, netns, parsed.options, console.log, Date.now, auth));
    } else {
      sources.push(
        new SystemdSource(
          {
            units: new DirUnitsDirectory(parsed.unitsDir),
            systemd: systemdClient(),
            profiles: new NixProfiles(),
            listeners: new SsListenerSource(),
            conntrack,
            store: new FileStateStore(join(parsed.stateDir, "systemd.json")),
            closure: new NixClosureBinder(),
            netns,
            tcp: new BunTcpProbe(),
            log: console.log,
            now: Date.now,
          },
          parsed.options,
        ),
      );
    }
  }

  if (parsed.command === "list") {
    const lines: string[] = [];
    for (const source of sources) lines.push(...(await source.list()));
    const nothing = parsed.sources.length > 1 ? "Nothing is opted in."
      : parsed.sources[0] === "docker" ? "No opted-in containers."
      : "No managed units.";
    console.log(lines.length === 0 ? nothing : lines.join("\n"));
    return 0;
  }

  const inhibitor = parsed.inhibit ? new SystemdInhibitor() : NoInhibitor;
  const tidewaiter = new Daemon(sources, conntrack, inhibitor, parsed.options, console.log);
  console.log(`Sources: ${parsed.sources.join(", ")}${parsed.inhibit ? "; holding off shutdown while anything is busy" : ""}`);
  if (parsed.options.dryRun) console.log("Dry run - nothing will be changed");

  switch (parsed.command) {
    case "check":
      await tidewaiter.once();
      return 0;

    case "run": {
      const controller = new AbortController();
      for (const signal of ["SIGINT", "SIGTERM"] as const) {
        process.on(signal, () => {
          console.log(`\nReceived ${signal}, shutting down...`);
          controller.abort();
        });
      }
      await tidewaiter.run(controller.signal);
      return 0;
    }

    default:
      console.error(`Error: '${parsed.command}' is not a command\n`);
      console.error(USAGE);
      return 2;
  }
}

if (import.meta.main) {
  main(Bun.argv.slice(2))
    .then((code) => process.exit(code))
    .catch((error: Error) => {
      console.error(`Error: ${error.message}`);
      process.exit(1);
    });
}

import { join } from "node:path";
import { NixClosureBinder } from "./closure.ts";
import { CliConntrackSource } from "./conntrack.ts";
import { type AutoReboot, Daemon, DEFAULTS, type Options } from "./daemon.ts";
import { systemBusPath } from "./dbus.ts";
import { HttpDockerClient } from "./docker.ts";
import { DockerSource } from "./docker-source.ts";
import { BunTcpProbe } from "./health.ts";
import { http, overUnixSocket, withTimeout } from "./http.ts";
import { SsListenerSource } from "./listeners.ts";
import { ProcNetnsReader, resolveProcRoot } from "./netns.ts";
import { prepare } from "./prepare.ts";
import { NixProfiles } from "./profile.ts";
import { LogindRebooter, NixosBootCheck, parseWindow } from "./reboot.ts";
import { dockerConfigAuth, HttpRegistryClient } from "./registry.ts";
import type { Source, SourceName } from "./source.ts";
import { FileStateStore } from "./state-store.ts";
import { DEFAULT_STATUS_FILE, describeStatus, FileStatus, isBusy, NoStatus, readStatus } from "./status.ts";
import { DbusSystemdClient } from "./systemd.ts";
import { SystemdSource } from "./systemd-source.ts";
import { DirUnitsDirectory } from "./units.ts";
import { version } from "./version.ts";

const DEFAULT_UNITS_DIR = "/etc/tidewaiter/units";
const DEFAULT_STATE_DIR = "/var/lib/tidewaiter";
const DEFAULT_BOOTED_SYSTEM = "/run/booted-system";
const DEFAULT_CURRENT_SYSTEM = "/nix/var/nix/profiles/system";
const SOURCE_NAMES: readonly SourceName[] = ["docker", "systemd"];

const USAGE = `tidewaiter - update Docker containers and systemd units only when they are idle

Usage: tidewaiter [options] [command]

Commands:
  run       Reconcile continuously on an interval (default)
  check     Reconcile once and exit
  list      Show managed containers and units, and whether an update is waiting
  status    Show what the running daemon sees in use or swapping; exit 0 when
            nothing is, 1 when something is, 2 when no daemon is writing one
  prepare   Create missing unit profiles and missing closure drop-ins, then exit

Options:
  -s, --source NAME       docker or systemd; repeat or comma-separate for both
                          (default docker)
  -d, --interval SECONDS  Seconds between reconcile passes (default ${DEFAULTS.interval})
  -n, --dry-run           Report what would change without changing it
      --status-file PATH  Where run keeps what is in use and swapping, and where
                          status reads it (status's default ${DEFAULT_STATUS_FILE};
                          run writes none unless given one)
      --activity-interval SECONDS
                          Seconds between activity checks between passes, with a
                          status file or an automatic reboot (default ${DEFAULTS.activityInterval})
      --auto-reboot-window HH:MM-HH:MM
                          Reboot when the installed NixOS needs it (its kernel,
                          initrd or modules changed since boot), only inside this
                          daily window of local time, and only once everything is
                          idle and nothing is being swapped
      --booted-system P   The system that booted (default ${DEFAULT_BOOTED_SYSTEM})
      --current-system P  The system installed (default ${DEFAULT_CURRENT_SYSTEM})
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
  TIDEWAITER_STATUS_FILE  Same as --status-file
  TIDEWAITER_AUTO_REBOOT_WINDOW
                          Same as --auto-reboot-window
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
  /** Undefined unless given: run writes no status, and status reads the default. */
  readonly statusFile?: string;
  readonly autoRebootWindow?: string;
  readonly bootedSystem: string;
  readonly currentSystem: string;
  readonly help: boolean;
  readonly showVersion: boolean;
}

export function parseArguments(argv: readonly string[], env: Record<string, string | undefined> = {}): Parsed {
  let command = "run";
  let socket = env.DOCKER_SOCKET ?? "/var/run/docker.sock";
  let unitsDir = env.TIDEWAITER_UNITS_DIR || DEFAULT_UNITS_DIR;
  // systemd joins several StateDirectory= entries with colons; the first is ours.
  let stateDir = env.STATE_DIRECTORY?.split(":")[0] || DEFAULT_STATE_DIR;
  let statusFile = env.TIDEWAITER_STATUS_FILE || undefined;
  let autoRebootWindow = env.TIDEWAITER_AUTO_REBOOT_WINDOW || undefined;
  let bootedSystem = DEFAULT_BOOTED_SYSTEM;
  let currentSystem = DEFAULT_CURRENT_SYSTEM;
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
      case "--status-file": statusFile = value(); break;
      case "--auto-reboot-window": autoRebootWindow = value(); break;
      case "--booted-system": bootedSystem = value(); break;
      case "--current-system": currentSystem = value(); break;
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
  // Checked now, so a typo stops the daemon at start rather than at 3am.
  if (autoRebootWindow !== undefined) parseWindow(autoRebootWindow);

  return {
    command,
    options: options as unknown as Options,
    socket,
    sources: [...new Set(sources)],
    unitsDir,
    stateDir,
    ...(statusFile === undefined ? {} : { statusFile }),
    ...(autoRebootWindow === undefined ? {} : { autoRebootWindow }),
    bootedSystem,
    currentSystem,
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

  // Before the banner: its output is for scripts as much as for people.
  if (parsed.command === "status") return status(parsed.statusFile ?? DEFAULT_STATUS_FILE);

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

  // Only `run` writes a status, or reboots: a `check` beside a running daemon
  // must not overwrite what the daemon says.
  const running = parsed.command === "run";
  const autoReboot: AutoReboot | undefined = running && parsed.autoRebootWindow !== undefined
    ? {
        window: parseWindow(parsed.autoRebootWindow),
        label: parsed.autoRebootWindow,
        check: new NixosBootCheck(parsed.bootedSystem, parsed.currentSystem),
        rebooter: new LogindRebooter(systemBusPath(Bun.env)),
        now: () => new Date(),
      }
    : undefined;
  const statusSink = running && parsed.statusFile !== undefined ? new FileStatus(parsed.statusFile) : NoStatus;
  const tidewaiter = new Daemon(sources, conntrack, parsed.options, console.log, statusSink, autoReboot);
  console.log(`Sources: ${parsed.sources.join(", ")}`);
  if (autoReboot) console.log(`Rebooting automatically when needed, between ${autoReboot.label}, once nothing is in use`);
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

/** `tidewaiter status`: what the running daemon last wrote, and 0 idle, 1 busy, 2 cannot tell. */
async function status(path: string): Promise<number> {
  const current = await readStatus(path);
  if (current === undefined) {
    console.log(`No status at ${path}: is Tidewaiter running, with --status-file?`);
    return 2;
  }
  console.log(describeStatus(current, Date.now()).join("\n"));
  return isBusy(current) ? 1 : 0;
}

if (import.meta.main) {
  main(Bun.argv.slice(2))
    .then((code) => process.exit(code))
    .catch((error: Error) => {
      console.error(`Error: ${error.message}`);
      process.exit(1);
    });
}

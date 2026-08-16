import { CliConntrackSource } from "./conntrack.ts";
import { DEFAULTS, Tidewaiter, type Options } from "./daemon.ts";
import { HttpDockerClient } from "./docker.ts";
import { http, overUnixSocket, withTimeout } from "./http.ts";
import { parseLabels } from "./labels.ts";
import { ProcNetnsReader } from "./netns.ts";
import { dockerConfigAuth, HttpRegistryClient } from "./registry.ts";
import { version } from "./version.ts";

const USAGE = `tidewaiter - update Docker containers only when they are idle, driven by a label

Usage: tidewaiter [options] [command]

Commands:
  run       Reconcile continuously on an interval (default)
  check     Reconcile once and exit
  list      Show opted-in containers and whether an update is waiting

Options:
  -d, --interval SECONDS  Seconds between reconcile passes (default ${DEFAULTS.interval})
  -l, --label LABEL       Opt-in label to read (default ${DEFAULTS.label})
      --docker-socket P   Docker socket (default /var/run/docker.sock)
  -n, --dry-run           Report what would change without changing it
      --version           Show the version and exit
  -h, --help              Show this message

Environment:
  DOCKER_SOCKET           Same as --docker-socket
  TIDEWAITER_INTERVAL     Same as --interval

Opt a container in with 'tidewaiter.enable=true'. Tidewaiter compares the
registry digest for its tag against the running image, and when a newer one
exists it waits until the container is idle (no live network flows), pulls,
recreates, health-checks, and rolls back if the new image is unhealthy.
`;

interface Parsed {
  readonly command: string;
  readonly options: Options;
  readonly socket: string;
  readonly help: boolean;
  readonly showVersion: boolean;
}

export function parseArguments(argv: readonly string[], env: Record<string, string | undefined> = {}): Parsed {
  let command = "run";
  let socket = env.DOCKER_SOCKET ?? "/var/run/docker.sock";
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
      case "-d": case "--interval": options.interval = number(value(), argument); break;
      case "-l": case "--label": options.label = value(); break;
      case "--docker-socket": socket = value(); break;
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

  return { command, options: options as unknown as Options, socket, help, showVersion };
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

  const docker = new HttpDockerClient(overUnixSocket(parsed.socket));
  // One credential source, shared by the registry client (for digest lookups)
  // and the daemon (for authenticated `docker pull`), so a private image both
  // resolves and pulls with the same creds from ~/.docker/config.json.
  const auth = await dockerConfigAuth();
  const registry = new HttpRegistryClient(withTimeout(http, 30_000), auth);
  const conntrack = new CliConntrackSource();
  const netns = new ProcNetnsReader();

  if (parsed.command === "list") {
    const containers = await docker.containers(parsed.options.label);
    if (containers.length === 0) {
      console.log("No opted-in containers.");
      return 0;
    }
    for (const container of containers) {
      const { policy } = parseLabels(container.spec.labels, container.spec.published);
      let desired = "unknown";
      try {
        desired = await registry.digest(container.spec.image);
      } catch {
        desired = "unreachable";
      }
      // Compare like with like: the registry's manifest digest against the local
      // image's RepoDigest (imageDigest), not inspect's config ID.
      const current = await docker.imageDigest(container.spec.image).catch(() => undefined);
      const state = desired === "unreachable" ? "registry unreachable"
        : desired === current ? "up to date"
        : "update available";
      console.log(
        `${container.spec.name}  ${container.spec.image}  [${state}]  ` +
          `detector=${policy.detector} health=${policy.health} idle-samples=${policy.idleSamples}`,
      );
    }
    return 0;
  }

  const tidewaiter = new Tidewaiter(docker, registry, conntrack, netns, parsed.options, console.log, Date.now, auth);
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

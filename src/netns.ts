/**
 * Reaching a container's network namespace, host-side, through /proc.
 *
 * Tidewaiter runs in the host PID namespace (network_mode: host), so a
 * container's main-process PID - handed out by `docker inspect` as State.Pid -
 * is directly usable here. Linux exposes every process's netns-scoped socket
 * tables at /proc/<pid>/net/*, so reading a container's TCP/UDP sockets needs
 * no setns(), no nsenter and no CAP_SYS_ADMIN: just read access to that path.
 *
 * This is the whole reason the TCP-backup detector and the `port` health probe
 * are so much simpler than portical's relay-container trick - portical needed a
 * different *source address* to talk to the gateway, which /proc cannot give;
 * we only need to *read* socket state, which it hands over directly.
 */

/** The four socket tables a detector or probe might read. */
export type NetTable = "tcp" | "tcp6" | "udp" | "udp6";

/**
 * The path to one of a container process's netns-scoped socket tables.
 *
 * `procRoot` is normally `/proc`, but in the deployed container it is
 * `/host/proc` - the host's /proc bind-mounted read-only. That mount is what
 * lets the daemon reach *another* container's /proc/<pid>/net/* at all: sharing
 * the host network namespace (network_mode: host) does NOT share the host PID
 * namespace, so without either `pid: host` or this read-only /proc mount the
 * daemon cannot see other containers' PIDs. The read-only mount is the smaller
 * grant, so it is what the compose uses.
 */
export function procNetPath(pid: number, table: NetTable, procRoot = "/proc"): string {
  return `${procRoot}/${pid}/net/${table}`;
}

/**
 * The one filesystem dependency the netns-scoped detectors and probes have.
 *
 * An interface so tests hand in canned /proc/net/{tcp,udp} tables keyed by pid
 * rather than needing a real container - the same "inject the I/O boundary"
 * move as the Handler seam.
 */
export interface NetnsReader {
  read(pid: number, table: NetTable): Promise<string>;
}

/**
 * Reads the real /proc, for production.
 *
 * `procRoot` defaults to `/host/proc` when that path exists (the deployed
 * container, where the host's /proc is bind-mounted read-only) and `/proc`
 * otherwise (running natively on a host). main.ts resolves it once at startup.
 */
export class ProcNetnsReader implements NetnsReader {
  constructor(private readonly procRoot: string = "/proc") {}

  async read(pid: number, table: NetTable): Promise<string> {
    return Bun.file(procNetPath(pid, table, this.procRoot)).text();
  }
}

/**
 * Where to read PIDs' /proc from: the host's /proc bind-mounted at /host/proc if
 * present, else the local /proc. Resolved once at startup and handed to the
 * reader, so a missing mount degrades to /proc rather than being a hard error.
 */
export async function resolveProcRoot(): Promise<string> {
  try {
    if (await Bun.file("/host/proc/self/net/tcp").exists()) return "/host/proc";
  } catch {
    // fall through
  }
  return "/proc";
}

export interface ProcNetEntry {
  readonly localPort: number;
  readonly state: string;
}

/**
 * Parse /proc/net/tcp (or udp) into local port + state.
 *
 * Pure and exported, tested against captured /proc output. The format is
 * fixed-column hex: the local address column is `HEXIP:HEXPORT`, the `st` column
 * is the connection state. Only the local port and state matter here.
 */
export function parseProcNet(text: string): ProcNetEntry[] {
  const entries: ProcNetEntry[] = [];

  for (const raw of text.split("\n")) {
    const line = raw.trim();
    if (line === "" || line.startsWith("sl")) continue;

    const columns = line.split(/\s+/);
    // Columns: sl local_address rem_address st ...
    const local = columns[1];
    const state = columns[3];
    if (local === undefined || state === undefined) continue;

    const colon = local.lastIndexOf(":");
    if (colon === -1) continue;
    const port = parseInt(local.slice(colon + 1), 16);
    if (!Number.isFinite(port)) continue;

    // State is a two-hex-digit code (01 ESTABLISHED, 0A LISTEN, ...);
    // upper-case it so a comparison need not worry about letter case.
    entries.push({ localPort: port, state: state.toUpperCase() });
  }

  return entries;
}

/**
 * The local ports in a given socket state, across a family's v4 and v6 tables.
 *
 * The one read pattern both the TCP detector and the `port` health probe share:
 * read /proc/<pid>/net/{tcp,tcp6} (or udp/udp6), parse, keep the entries in the
 * wanted `st` state, return their local ports as a set. Kept here, in the module
 * that owns the /proc concern, so neither caller re-implements the read.
 */
export async function portsInState(
  reader: NetnsReader,
  pid: number,
  family: "tcp" | "udp",
  state: string,
): Promise<Set<number>> {
  const v6: NetTable = family === "tcp" ? "tcp6" : "udp6";
  const text = `${await reader.read(pid, family)}\n${await reader.read(pid, v6)}`;
  return new Set(parseProcNet(text).filter((entry) => entry.state === state).map((entry) => entry.localPort));
}

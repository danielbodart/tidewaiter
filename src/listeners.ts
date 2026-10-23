import type { BoundPorts } from "./health.ts";
import type { Protocol } from "./model.ts";

/** A listening TCP or bound UDP socket on the host, and the cgroup that made it. */
export interface Listener {
  readonly protocol: Protocol;
  readonly port: number;
  /** "/system.slice/foo.service", or "" when ss could not say. */
  readonly cgroup: string;
}

/**
 * The host's listening sockets, as raw `ss -Hlntu -e` text.
 *
 * An interface, like ConntrackSource, so tests feed captured output with no
 * subprocess and no sock_diag.
 */
export interface ListenerSource {
  dump(): Promise<string>;
}

/**
 * Lists sockets with `ss -Hlntu -e`: no header, listening only, numeric, TCP
 * and UDP, extended - which is what adds `cgroup:` (sock_diag, Linux 5.7 and
 * iproute2 5.9 or later). Unprivileged: every socket's cgroup is visible to
 * any user.
 */
export class SsListenerSource implements ListenerSource {
  async dump(): Promise<string> {
    const process = Bun.spawn(["ss", "-Hlntu", "-e"], { stdout: "pipe", stderr: "pipe" });
    const [stdout, stderr, exitCode] = await Promise.all([
      new Response(process.stdout).text(),
      new Response(process.stderr).text(),
      process.exited,
    ]);
    if (exitCode !== 0) throw new Error(`ss exited with status ${exitCode}: ${stderr.trim().slice(0, 300)}`);
    return stdout;
  }
}

// A line looks like:
//   udp UNCONN 0 0   127.0.0.53%lo:53  0.0.0.0:*  uid:153 ino:16991 sk:3 cgroup:/system.slice/systemd-resolved.service <->
//   tcp LISTEN 0 4096        [::]:5355     [::]:*  uid:153 ino:16986 sk:1007 cgroup:/system.slice/systemd-resolved.service v6only:1 <->
// The local address is the fifth field and its port follows the last colon,
// whatever the address looks like (v4, [v6], `*`, with or without %interface).
const LOCAL_PORT = /:(\d{1,5})$/;
const CGROUP = /\bcgroup:(\S+)/;

/**
 * Parse `ss -Hlntu -e` into listeners.
 *
 * Pure and exported, tested against captured output. A line that is not TCP
 * or UDP, or whose local port cannot be read, is skipped rather than guessed.
 */
export function parseListeners(text: string): Listener[] {
  const listeners: Listener[] = [];
  for (const raw of text.split("\n")) {
    const fields = raw.trim().split(/\s+/);
    const protocol = fields[0];
    if (protocol !== "tcp" && protocol !== "udp") continue;
    const port = Number(LOCAL_PORT.exec(fields[4] ?? "")?.[1]);
    if (!Number.isInteger(port) || port < 1 || port > 65535) continue;
    listeners.push({ protocol, port, cgroup: CGROUP.exec(raw)?.[1] ?? "" });
  }
  return listeners;
}

/**
 * BoundPorts for a unit on the host's network: the ports of the sockets its
 * cgroup holds.
 *
 * The host netns is shared by every service, so a port number alone would
 * also see whatever else is on it - the old version still shutting down, a
 * stray copy, another service entirely. A socket belongs to the unit when the
 * cgroup that created it is the unit's ControlGroup or beneath it (a
 * Delegate=yes unit's sub-cgroups); `foo.service2` is not beneath
 * `foo.service`.
 *
 * Throws - which the checks read as "cannot tell yet", never as "nothing
 * bound" - when the unit has no cgroup (not running) or ss gave no cgroups at
 * all (a kernel or iproute2 too old to say), so a host that cannot attribute
 * sockets never fails a healthy update for it.
 */
export function unitBoundPorts(listeners: () => Promise<readonly Listener[]>, controlGroup: string): BoundPorts {
  return async (family) => {
    if (controlGroup === "") throw new Error("the unit has no cgroup, so its sockets cannot be told apart");
    const all = await listeners();
    if (all.length > 0 && all.every((listener) => listener.cgroup === "")) {
      throw new Error("ss reports no socket cgroups on this host (needs Linux 5.7+ and iproute2 5.9+)");
    }
    const ports = all
      .filter((l) => l.protocol === family && (l.cgroup === controlGroup || l.cgroup.startsWith(`${controlGroup}/`)))
      .map((l) => l.port);
    return new Set(ports);
  };
}

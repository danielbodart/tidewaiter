import type { Protocol } from "./model.ts";

/**
 * One flow from the host's conntrack table, reduced to what a detector needs.
 *
 * `destinationPort` is the original-direction dport - i.e. the host port a
 * client connected to, which is what we match against a container's published
 * host ports. `assured` marks a flow conntrack has seen traffic in both
 * directions on, which separates a real session from a lone probe or a SYN that
 * went nowhere.
 *
 * `state` is the TCP connection state (ESTABLISHED, TIME_WAIT, ...), undefined
 * for UDP (which conntrack tracks with no such state). It is load-bearing: a
 * TIME_WAIT / CLOSE_WAIT flow is a connection that has *finished*, and it
 * lingers in the table for up to ~2 minutes. Counting those as "in use" would
 * make a container read busy for two minutes after its last request ended, so
 * the detector uses this to count only genuinely-active flows.
 */
export interface ConntrackEntry {
  readonly protocol: Protocol;
  readonly destinationPort: number;
  readonly assured: boolean;
  readonly state?: string;
}

/**
 * The host's conntrack table, as raw `conntrack -L` text.
 *
 * An interface, so the daemon can dump once per pass and hand the same text to
 * every container's detector, and so tests feed captured output with no
 * netlink, no CAP_NET_ADMIN and no subprocess.
 */
export interface ConntrackSource {
  dump(signal?: AbortSignal): Promise<string>;
}

/**
 * Reads the host conntrack table by shelling out to `conntrack -L`.
 *
 * A shellout rather than raw netlink FFI: PLAN.md confirmed `conntrack -L`
 * works over netlink on the target Flatcar host (where /proc/net/nf_conntrack
 * is compiled out), and conntrack-tools is in the image anyway. The whole
 * table comes back host-wide - NAT and routing happen at the host regardless of
 * which netns the destination process lives in - so one dump per pass serves
 * every container, filtered in memory by published host port.
 *
 * Needs CAP_NET_ADMIN, granted at runtime via the compose `cap_add`.
 */
export class CliConntrackSource implements ConntrackSource {
  async dump(signal?: AbortSignal): Promise<string> {
    // -o extended asks for a stable, parseable line format; a non-zero exit or
    // empty output is surfaced to the caller, which treats "cannot tell" as
    // in-use (fail-safe) rather than as "no flows, go ahead".
    const process = Bun.spawn(["conntrack", "-L"], {
      stdout: "pipe",
      stderr: "pipe",
      signal,
    });

    const [stdout, stderr, exitCode] = await Promise.all([
      new Response(process.stdout).text(),
      new Response(process.stderr).text(),
      process.exited,
    ]);

    if (exitCode !== 0) {
      throw new Error(`conntrack -L exited with status ${exitCode}: ${stderr.trim().slice(0, 300)}`);
    }
    return stdout;
  }
}

// A conntrack -L line looks like:
//   tcp      6 431999 ESTABLISHED src=10.0.0.5 dst=10.0.0.1 sport=51000 dport=25565 \
//     src=10.0.0.1 dst=10.0.0.5 sport=25565 dport=51000 [ASSURED] mark=0 use=1
//   udp      17 29 src=10.0.0.5 dst=10.0.0.1 sport=40000 dport=19132 [UNREPLIED] ...
//
// The destination port that matters is the *original-direction* dport - the
// host port the client aimed at. It is the first `dport=` on the line; the
// second belongs to the reply tuple (the client's own source port).
const LINE = /^(tcp|udp)\s/;
const DPORT = /\bdport=(\d{1,5})\b/;
// The TCP connection state is the 4th whitespace field on a tcp line
// (`tcp  6  431999  ESTABLISHED  src=...`), all caps. UDP lines have no such
// field, so this simply does not match for them.
const TCP_STATE = /^tcp\s+\d+\s+\d+\s+([A-Z_]+)/;

/**
 * Parse `conntrack -L` output into flows, keeping only TCP and UDP.
 *
 * Pure and exported, so it is tested against captured real output - the parsing
 * is where the interesting mistakes live, exactly as portical tests toContainer
 * against captured Engine API JSON rather than mocking the socket.
 *
 * Other protocols (ICMP, etc.) are dropped: PLAN.md scopes the gate to TCP and
 * UDP only. A line we cannot read a destination port off is skipped rather than
 * guessed at.
 */
export function parseConntrack(text: string): ConntrackEntry[] {
  const entries: ConntrackEntry[] = [];

  for (const raw of text.split("\n")) {
    const line = raw.trim();
    const protocolMatch = LINE.exec(line);
    if (!protocolMatch) continue;

    const protocol = protocolMatch[1] as Protocol;

    const dportMatch = DPORT.exec(line);
    if (!dportMatch) continue;
    const destinationPort = Number(dportMatch[1]);
    if (destinationPort < 1 || destinationPort > 65535) continue;

    entries.push({
      protocol,
      destinationPort,
      assured: line.includes("[ASSURED]"),
      state: TCP_STATE.exec(line)?.[1],
    });
  }

  return entries;
}

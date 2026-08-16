import type { ConntrackEntry } from "./conntrack.ts";
import { parseProcNet, type NetnsReader } from "./netns.ts";
import type { PublishedPort } from "./model.ts";

// Re-exported from its home in netns.ts so existing importers keep working.
export { parseProcNet as parseProcNetTcp, type ProcNetEntry } from "./netns.ts";

/**
 * What a detector reports about one container.
 *
 * `sessions` is undefined when a detector can sense presence but not count it -
 * a UDP server shares one socket across all clients, so conntrack can say "busy"
 * without saying "how many". `confidence` lets the daemon distinguish a real
 * "idle" from a "could not tell", which under the fail-safe rule become very
 * different outcomes.
 */
export interface DetectorResult {
  readonly inUse: boolean;
  readonly sessions?: number;
  readonly confidence: "low" | "high";
}

export interface DetectorInput {
  /** The container id or name, for detectors that ask Docker about it directly. */
  readonly id: string;
  readonly pid: number;
  readonly published: readonly PublishedPort[];
  /** The host conntrack table for this pass, dumped once and shared. */
  readonly conntrack: readonly ConntrackEntry[];
  readonly netns: NetnsReader;
}

/**
 * A way of telling whether a container is in use, per PLAN.md's Detector idea.
 *
 * Kept pluggable, and combined conservatively (in use if any says so), so a
 * future protocol-aware detector can be added without touching the daemon.
 */
export interface Detector {
  readonly name: string;
  inUse(input: DetectorInput): Promise<DetectorResult>;
}

/**
 * The primary detector: live flows from the host conntrack table.
 *
 * The only thing that sees UDP peers at all, since a UDP server has no
 * per-client socket to count. Counts flows whose destination is one of the
 * container's published host ports, preferring ASSURED ones - a flow conntrack
 * has seen both directions on is a real session, where a lone SYN or a probe
 * packet is not. Presence is high-confidence; ASSURED flows are counted as
 * sessions, unassured ones still mark "in use" but are not counted.
 */
export const conntrackDetector: Detector = {
  name: "conntrack",
  async inUse(input) {
    const ports = new Set(input.published.map((port) => `${port.protocol}/${port.hostPort}`));
    const flows = input.conntrack.filter((entry) =>
      ports.has(`${entry.protocol}/${entry.destinationPort}`),
    );
    const assured = flows.filter((flow) => flow.assured);

    if (assured.length > 0) {
      return { inUse: true, sessions: assured.length, confidence: "high" };
    }
    if (flows.length > 0) {
      // Flows exist but none is ASSURED yet - treat as in use (conservative)
      // but do not claim a session count we cannot stand behind.
      return { inUse: true, confidence: "high" };
    }
    return { inUse: false, confidence: "high" };
  },
};

/**
 * The backup detector: ESTABLISHED TCP sockets in the container's netns.
 *
 * TCP only - UDP has no socket-state fallback, which is exactly why conntrack is
 * the primary. Reads /proc/<pid>/net/tcp[6] host-side and counts ESTABLISHED
 * connections whose local port is one the container publishes. High confidence
 * for what it can see, but it is blind to UDP, so it is a supplement, never a
 * replacement for conntrack on a UDP service.
 */
export const tcpDetector: Detector = {
  name: "tcp",
  async inUse(input) {
    const containerPorts = new Set(
      input.published.filter((p) => p.protocol === "tcp").map((p) => p.containerPort),
    );
    if (containerPorts.size === 0) {
      return { inUse: false, confidence: "low" };
    }

    // Count connections, not ports: three clients on port 80 is three sessions.
    // The `port` health probe only needs set membership (is anything listening),
    // so it uses netns.portsInState; the detector needs the count, so it parses.
    const text = `${await input.netns.read(input.pid, "tcp")}\n${await input.netns.read(input.pid, "tcp6")}`;
    const established = parseProcNet(text).filter(
      (conn) => conn.state === TCP_ESTABLISHED && containerPorts.has(conn.localPort),
    );

    return established.length > 0
      ? { inUse: true, sessions: established.length, confidence: "high" }
      : { inUse: false, confidence: "high" };
  },
};

/** Below this many network bytes per second, a container reads as idle. */
export const DEFAULT_NETIO_THRESHOLD_BYTES = 1024;

export interface NetioDeps {
  /** Cumulative rx+tx bytes for a container, from Docker /stats. */
  readonly netBytes: (id: string) => Promise<number>;
  /** The clock, injected so the rate is computed deterministically in tests. */
  readonly now: () => number;
  /** Bytes-per-second at or below which the container counts as idle. */
  readonly thresholdBytesPerSec?: number;
}

/**
 * A zero-privilege signal: Docker's /stats network-I/O *rate*.
 *
 * A rate needs two samples an interval apart, so this is a factory holding the
 * previous {bytes, at} per container between passes - the same shape as
 * uptimeHealthProber, which is why (like uptime) the daemon constructs it rather
 * than resolving a singleton. The first sample for a container has nothing to
 * compare against, so it reports "cannot tell" (low confidence), which the
 * fail-safe reads as in use - it never green-lights an update on a single
 * reading. Later samples compute (Δbytes / Δseconds) and give a high-confidence
 * busy/idle verdict against the threshold.
 *
 * TCP-and-UDP blind spots do not apply here - it counts bytes, not sockets - but
 * it cannot distinguish a health-check ping from a real client, so it is a
 * coarse supplement, offered as an explicit opt-in via tidewaiter.detector.
 */
export function netioDetector(deps: NetioDeps): Detector {
  const threshold = deps.thresholdBytesPerSec ?? DEFAULT_NETIO_THRESHOLD_BYTES;
  const previous = new Map<string, { bytes: number; at: number }>();

  return {
    name: "netio",
    async inUse(input) {
      const bytes = await deps.netBytes(input.id);
      const at = deps.now();
      const prior = previous.get(input.id);
      previous.set(input.id, { bytes, at });

      if (prior === undefined) {
        // No baseline yet - cannot compute a rate. Fail toward caution.
        return { inUse: true, confidence: "low" };
      }

      const seconds = (at - prior.at) / 1000;
      // Two samples at the same instant (or clock skew) give no usable rate;
      // treat as "cannot tell" rather than dividing by zero.
      if (seconds <= 0) return { inUse: true, confidence: "low" };

      const rate = (bytes - prior.bytes) / seconds;
      return rate > threshold
        ? { inUse: true, confidence: "high" }
        : { inUse: false, confidence: "high" };
    },
  };
}

/** Opt out of activity detection entirely - always reads as idle. */
export const noneDetector: Detector = {
  name: "none",
  async inUse() {
    return { inUse: false, confidence: "low" };
  },
};

/**
 * The stateless detectors, resolvable by name.
 *
 * netio is absent on purpose: it is stateful (it remembers the previous byte
 * sample) and needs Docker + a clock, so the daemon builds it per its
 * dependencies via netioDetector(), exactly as it builds uptimeHealthProber().
 */
const DETECTORS: Record<string, Detector> = {
  conntrack: conntrackDetector,
  tcp: tcpDetector,
  none: noneDetector,
};

export function detectorByName(name: string): Detector | undefined {
  return DETECTORS[name];
}

/**
 * Combine several detector results conservatively: in use if ANY says so.
 *
 * Pure, taking results rather than detectors, so the composition rule is
 * testable on its own. Confidence is "high" only when a high-confidence
 * detector actually contributed to the verdict - so a lone low-confidence
 * "cannot tell" stays low, and the daemon's fail-safe still applies.
 */
export function combine(results: readonly DetectorResult[]): DetectorResult {
  if (results.length === 0) return { inUse: false, confidence: "low" };

  const inUse = results.some((result) => result.inUse);
  const relevant = results.filter((result) => result.inUse === inUse);
  const confidence = relevant.some((result) => result.confidence === "high") ? "high" : "low";
  const sessions = results
    .map((result) => result.sessions)
    .filter((count): count is number => count !== undefined)
    .reduce<number | undefined>((total, count) => (total ?? 0) + count, undefined);

  return sessions === undefined ? { inUse, confidence } : { inUse, sessions, confidence };
}

/** The `st` value Linux reports for an ESTABLISHED TCP connection. */
const TCP_ESTABLISHED = "01";

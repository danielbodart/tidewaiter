import { portsInState, type NetnsReader } from "./netns.ts";
import type { HealthStatus, PublishedPort, RunningContainer } from "./model.ts";

/**
 * The verdict of one health probe.
 *
 * A tri-state hides behind this: `{healthy:true}` and `{healthy:false}` are
 * final answers, but a prober may also be *inconclusive* mid-wait (docker
 * health still "starting", uptime not yet elapsed), which it reports as
 * `undefined` from check() below - distinct from a definite "unhealthy".
 */
export type HealthResult = { readonly healthy: true } | { readonly healthy: false; readonly reason: string };

/**
 * Can a TCP connection be opened to a host port right now?
 *
 * The seam for the TCP health probe, so tests decide the answer without a real
 * socket - the same "inject the I/O boundary" move as NetnsReader. Tidewaiter
 * runs host-networked, so the real implementation just connect()s to the port on
 * the host: the very port a client hits, which is what "healthy" actually means.
 * Proven on a live host to distinguish open from closed cleanly, where a UDP
 * send cannot (a published-but-unbound UDP port is silent, indistinguishable
 * from a bound one - hence UDP still uses the bound-socket check below).
 */
export interface TcpProbe {
  connectable(hostPort: number, signal: AbortSignal): Promise<boolean>;
}

export interface HealthInput {
  readonly container: RunningContainer;
  readonly ports: readonly PublishedPort[];
  readonly netns: NetnsReader;
  readonly tcp: TcpProbe;
}

/**
 * A way of deciding whether a freshly-swapped container is actually up.
 *
 * check() returns a verdict, or undefined for "still inconclusive, ask again" -
 * the daemon polls it until it settles or the health-timeout runs out. Probing
 * from outside (the `port` default) needs no tooling baked into the image,
 * which is why PLAN.md argues it is a better default than trusting an image's
 * own HEALTHCHECK.
 */
export interface HealthProber {
  readonly name: string;
  check(input: HealthInput, signal: AbortSignal): Promise<HealthResult | undefined>;
}

/**
 * Trust Docker's own HEALTHCHECK verdict.
 *
 * Authoritative when the image defines one. "starting" is inconclusive (the
 * probe has not settled), "healthy"/"unhealthy" are final, and "none" means the
 * container has no HEALTHCHECK at all - which is a misconfiguration for this
 * prober, reported as unhealthy so it does not silently pass.
 */
export const dockerHealthProber: HealthProber = {
  name: "docker",
  async check(input) {
    return verdictForDockerHealth(input.container.health);
  },
};

export function verdictForDockerHealth(health: HealthStatus): HealthResult | undefined {
  switch (health) {
    case "healthy":
      return { healthy: true };
    case "unhealthy":
      return { healthy: false, reason: "Docker reports the container unhealthy" };
    case "starting":
      return undefined;
    case "none":
      return {
        healthy: false,
        reason: "health=docker but the container defines no HEALTHCHECK; nothing to read",
      };
  }
}

/** The `st` code Linux reports for a bound (listening) UDP socket. */
const UDP_LISTEN = "07";

/**
 * Probe from outside: can the published ports actually be reached?
 *
 * The default, because it needs nothing inside the container. Two mechanisms,
 * each the robust one for its protocol - a distinction proven empirically on a
 * live host:
 *
 *   TCP - a real connect() to the published HOST port, the exact port a client
 *         hits. Success is unambiguous ("open") and refusal is unambiguous
 *         ("closed"). No PID, no /proc, no netns - so none of the startup-timing
 *         fragility that reading a socket table by pid suffers. A connect() that
 *         is refused mid-startup just reads as "not up yet, keep waiting".
 *
 *   UDP - has no connect handshake, and a UDP send cannot tell an open port from
 *         a published-but-unbound one (Docker's DNAT forwards the datagram either
 *         way and nothing answers - both go silent, confirmed on a live host).
 *         So the ONLY generic UDP signal is a bound socket, read from
 *         /proc/<pid>/net/udp in the container's netns. "Bound" is not "fully
 *         functional", but it is the ceiling of what is generically knowable;
 *         a protocol-aware probe (A2S/RakNet) is the deferred game-server path.
 */
export const portHealthProber: HealthProber = {
  name: "port",
  async check(input, signal) {
    if (input.ports.length === 0) {
      return { healthy: false, reason: "health=port but the container publishes no ports to probe" };
    }

    // TCP: connect() to each published host port. Any one not yet accepting means
    // "not up yet" - keep waiting; only the overall health-timeout fails it.
    for (const p of input.ports) {
      if (p.protocol !== "tcp") continue;
      if (!(await input.tcp.connectable(p.hostPort, signal))) return undefined;
    }

    // UDP: bound-socket check in the container's netns. A read failure means the
    // netns/proc is not ready yet (freshly started, or the pid briefly moved) -
    // inconclusive, poll again, never an immediate unhealthy.
    const udpPorts = new Set(input.ports.filter((p) => p.protocol === "udp").map((p) => p.containerPort));
    if (udpPorts.size > 0) {
      try {
        const bound = await portsInState(input.netns, input.container.pid, "udp", UDP_LISTEN);
        const missing = [...udpPorts].find((port) => !bound.has(port));
        if (missing !== undefined) return undefined;
      } catch {
        return undefined;
      }
    }

    return { healthy: true };
  },
};

/**
 * The real TCP probe: open a connection to a host port, briefly.
 *
 * Tidewaiter is host-networked, so a published port is reachable at 127.0.0.1 on
 * the host - the same path a client takes. Connects, then closes at once; a
 * refusal (nothing accepting yet) is a clean `false`, any other error is treated
 * as not-yet-up too, so a startup race never reads as a hard failure.
 */
export class BunTcpProbe implements TcpProbe {
  constructor(private readonly host: string = "127.0.0.1", private readonly timeoutMillis: number = 2000) {}

  async connectable(hostPort: number, signal: AbortSignal): Promise<boolean> {
    let socket: { end(): void } | undefined;
    try {
      socket = await Promise.race([
        Bun.connect({ hostname: this.host, port: hostPort, socket: { data() {}, open(s) { s.end(); } } }),
        new Promise<never>((_, reject) => {
          const timer = setTimeout(() => reject(new Error("timed out")), this.timeoutMillis);
          signal.addEventListener("abort", () => { clearTimeout(timer); reject(new Error("aborted")); }, { once: true });
        }),
      ]);
      return true;
    } catch {
      return false;
    } finally {
      socket?.end();
    }
  }
}

/**
 * The weakest gate: the container simply stays running for a while.
 *
 * Last resort, for an image with no HEALTHCHECK and no useful port to probe.
 * Needs a clock, so `now` is injected - the daemon supplies Date.now, tests
 * supply a controllable one, keeping the whole thing deterministic. Records the
 * moment it first sees the container and passes once `seconds` have elapsed; a
 * container that has already exited is not "still running", so a missing
 * container reads as unhealthy.
 */
export function uptimeHealthProber(seconds: number, now: () => number): HealthProber {
  let firstSeen: number | undefined;
  return {
    name: "uptime",
    async check() {
      const at = now();
      if (firstSeen === undefined) firstSeen = at;
      return at - firstSeen >= seconds * 1000 ? { healthy: true } : undefined;
    },
  };
}

const PROBERS: Record<string, HealthProber> = {
  docker: dockerHealthProber,
  port: portHealthProber,
};

/**
 * Resolve a prober by name.
 *
 * `uptime` is not here because it is stateful (it must remember when it first
 * saw the container) and needs a clock, so the daemon constructs it per swap
 * via uptimeHealthProber(); docker and port are stateless singletons.
 */
export function healthProberByName(name: string): HealthProber | undefined {
  return PROBERS[name];
}

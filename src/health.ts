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

export interface HealthInput {
  readonly container: RunningContainer;
  readonly ports: readonly PublishedPort[];
  readonly netns: NetnsReader;
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
/** The `st` code for a listening TCP socket. */
const TCP_LISTEN = "0A";

/**
 * Probe from outside: is something listening on the published ports?
 *
 * The default, because it needs nothing inside the container. For TCP it checks
 * for a LISTEN socket in the container's netns (a real connect() would also
 * work, but reading the socket table avoids leaving half-open connections and
 * reuses the detector's netns machinery). For UDP - which has no connect
 * handshake - a bound socket is the only generic signal there is. "Bound" is
 * not "fully functional", but it matches what most real healthchecks test.
 */
export const portHealthProber: HealthProber = {
  name: "port",
  async check(input) {
    if (input.ports.length === 0) {
      return { healthy: false, reason: "health=port but the container publishes no ports to probe" };
    }

    const tcpPorts = new Set(input.ports.filter((p) => p.protocol === "tcp").map((p) => p.containerPort));
    const udpPorts = new Set(input.ports.filter((p) => p.protocol === "udp").map((p) => p.containerPort));

    if (tcpPorts.size > 0) {
      const listening = await portsInState(input.netns, input.container.pid, "tcp", TCP_LISTEN);
      const missing = [...tcpPorts].find((port) => !listening.has(port));
      if (missing !== undefined) return undefined; // not up yet - keep waiting
    }

    if (udpPorts.size > 0) {
      const bound = await portsInState(input.netns, input.container.pid, "udp", UDP_LISTEN);
      const missing = [...udpPorts].find((port) => !bound.has(port));
      if (missing !== undefined) return undefined;
    }

    return { healthy: true };
  },
};

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

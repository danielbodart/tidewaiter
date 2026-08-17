import { portsInState, type NetnsReader } from "./netns.ts";
import type { HealthStatus, PublishedPort, RunningContainer } from "./model.ts";

/**
 * What a single connect() attempt to a client-facing endpoint concluded.
 *
 *   accepted    - the handshake completed: something is listening and serving.
 *   refused     - the path reached a host that answered with RST (ECONNREFUSED):
 *                 the route is wired, nothing is accepting there *yet*. On a
 *                 truthful endpoint (a container IP, no proxy in front) this is
 *                 the signal that lets port-connect actively fail once the grace
 *                 has elapsed - the route works but the app is not serving.
 *   unreachable - no host answered at all (EHOSTUNREACH/ENETUNREACH, or a
 *                 timeout). We cannot see the endpoint from here - the classic
 *                 macvlan/ipvlan case where the host has no route to the child
 *                 interface. Never a failure: "we could not look" must not veto.
 */
export type ConnectOutcome = "accepted" | "refused" | "unreachable";

/**
 * The verdict of the combined health gate.
 *
 * `{healthy:true}` commit, `{healthy:false}` roll back. The gate is a set of
 * checks and the final verdict comes from combine() below - a single check
 * never decides on its own.
 */
export type HealthResult = { readonly healthy: true } | { readonly healthy: false; readonly reason: string };

/**
 * What one check concluded this poll.
 *
 *   pass         - affirmatively confirmed (docker=healthy, a port accepted).
 *   fail         - actively broken (docker=unhealthy, the container exited).
 *   inconclusive - not confirmed yet; keep polling (still "starting", nothing
 *                  bound yet, uptime not elapsed). Crucially NOT a failure - a
 *                  slow or flaky probe must never veto a good update.
 *   skip         - the check cannot apply to this container (docker with no
 *                  HEALTHCHECK, port-connect with no TCP ports). Contributes
 *                  nothing to the verdict.
 *
 * The whole design leans on the pass/inconclusive/skip distinction: only a
 * `fail` rolls an update back, because false-unhealthy (our probe wrongly
 * vetoing a good update) is the worse error - a genuinely-broken image that
 * merely goes quiet is caught next cycle by the activity gate or a crash loop.
 */
export type CheckOutcome = "pass" | "fail" | "inconclusive" | "skip";

export interface CheckResult {
  readonly outcome: CheckOutcome;
  /** Set on `fail`, to explain the rollback. */
  readonly reason?: string;
}

const PASS: CheckResult = { outcome: "pass" };
const INCONCLUSIVE: CheckResult = { outcome: "inconclusive" };
const SKIP: CheckResult = { outcome: "skip" };
const fail = (reason: string): CheckResult => ({ outcome: "fail", reason });

/**
 * Open a TCP connection to a `host:port` and report how it went.
 *
 * The seam for the TCP connect check, so tests decide the answer without a real
 * socket - the same "inject the I/O boundary" move as NetnsReader. Tidewaiter
 * runs host-networked, so the real implementation connect()s from the host to
 * the exact address a client would use: a container's routable IP on a bridge
 * network, or 127.0.0.1 for a published host port. The three-way outcome (not a
 * bool) is what lets the check tell "route works, nothing accepting" (a real
 * fault) apart from "no route from here" (our own blind spot, never a fault).
 */
export interface TcpProbe {
  connect(host: string, port: number, signal: AbortSignal): Promise<ConnectOutcome>;
}

/**
 * A client-facing endpoint to probe, and whether reaching it is authoritative.
 *
 *   truthful - a direct path to the container with no docker-proxy in front (a
 *              container IP on a bridge/user network, or 127.0.0.1 in host
 *              network mode). A `refused` here is a real "not serving" signal.
 *   !truthful - a published host port fronted by docker-proxy, which answers a
 *              connect() even with no backend. An `accepted` here is worth a
 *              PASS, but a `refused`/`unreachable` can never fail - the proxy
 *              would mask a dead backend anyway, so it carries no veto.
 */
export interface ClientEndpoint {
  readonly host: string;
  readonly port: number;
  readonly truthful: boolean;
}

export interface HealthInput {
  readonly container: RunningContainer;
  readonly ports: readonly PublishedPort[];
  readonly netns: NetnsReader;
  readonly tcp: TcpProbe;
  /**
   * Resolve a network's driver (bridge, macvlan, overlay, host, ...) by name.
   *
   * The seam port-connect uses to decide which container IPs a host-networked
   * daemon can actually route to - only bridge/host driven networks. Injected
   * like netns/tcp so tests answer without Docker; the daemon backs it with a
   * memoised /networks lookup so it costs one call per network per daemon, not
   * one per poll. `undefined` (network gone, or unresolvable) reads as
   * not-reachable, so an IP we cannot classify is never probed-to-fail.
   */
  readonly networkDriver: (network: string) => Promise<string | undefined>;
}

/**
 * The network drivers whose container IPs a host-networked Tidewaiter can reach.
 *
 * `bridge` (default and user-defined) puts an interface on the host, so the host
 * routes straight to the container's IP. `host` shares the host stack outright.
 * Everything else - macvlan/ipvlan (the host has no route to the child
 * interface by default), overlay (a multi-host VXLAN the host is not on), none -
 * is NOT reachable from here, so port-connect must not treat a connect result to
 * such an IP as authoritative: a coincidental foreign RST on that subnet would
 * otherwise read as the container refusing, and fail a healthy update.
 */
const HOST_REACHABLE_DRIVERS: ReadonlySet<string> = new Set(["bridge", "host"]);

/** Whether a host-networked daemon can route to a container IP on this driver. */
export function reachableDriver(driver: string | undefined): boolean {
  return driver !== undefined && HOST_REACHABLE_DRIVERS.has(driver);
}

/**
 * One health check: name plus a pure-ish evaluation to a CheckResult.
 *
 * Evaluated fresh each poll against a freshly-inspected container. Stateless
 * except `uptime`, which is built per swap via a factory so it can remember when
 * it first saw the container.
 */
export interface HealthCheck {
  readonly name: string;
  evaluate(input: HealthInput, signal: AbortSignal): Promise<CheckResult>;
}

/**
 * Trust Docker's own HEALTHCHECK verdict, when the image defines one.
 *
 * Authoritative about the app's internal self-check. "healthy" passes,
 * "unhealthy" fails, "starting" is inconclusive, and "none" (no HEALTHCHECK)
 * SKIPS - it is not a failure to lack one, it just means this check has nothing
 * to say. (The old single-prober version failed on "none"; under the combined
 * gate that would wrongly veto every healthcheck-less container.)
 */
export const dockerCheck: HealthCheck = {
  name: "docker",
  async evaluate(input) {
    switch (input.container.health) {
      case "healthy":
        return PASS;
      case "unhealthy":
        return fail("Docker reports the container unhealthy");
      case "starting":
        return INCONCLUSIVE;
      case "none":
        return SKIP;
    }
  },
};

/** For the daemon's health_status event fast-path: turn an event into a check result. */
export function dockerVerdict(health: HealthStatus): CheckResult {
  switch (health) {
    case "healthy":
      return PASS;
    case "unhealthy":
      return fail("Docker reports the container unhealthy");
    case "starting":
      return INCONCLUSIVE;
    case "none":
      return SKIP;
  }
}

/**
 * The TCP endpoints a client actually uses to reach this container's ports.
 *
 * Exported and driver-gated (async only because resolving a driver is a lookup),
 * so the address logic is tested on its own. For each TCP port in scope it
 * emits, in preference order:
 *
 *   1. TRUTHFUL container-IP endpoints - one per network the container has a
 *      runtime IP on AND whose driver a host-networked daemon can route to
 *      (reachableDriver: bridge/host), at the *container* port. On such a network
 *      the host reaches this IP with no docker-proxy between, so a connect() here
 *      tests the exact path a client takes AND catches a service bound to the
 *      wrong address (e.g. the container's own loopback) - which the netns bind
 *      check, blind to listen address, waves through. An IP on a driver we CANNOT
 *      route to (macvlan, overlay, ...) is dropped, never probed: a stray RST
 *      from that subnet must never read as the container refusing.
 *
 *   2. A PROXIED fallback - 127.0.0.1 at the *host* port, for every published
 *      port. docker-proxy answers this even with no backend, so it can only ever
 *      PASS (weak), never fail. Kept so a container behind a firewalled bridge,
 *      where the truthful endpoint is momentarily unreachable, can still confirm.
 *
 * A container in host network mode publishes nothing and has no per-network IP,
 * so its service is reachable at 127.0.0.1 on the container port itself - that
 * is emitted as a truthful loopback endpoint.
 */
export async function clientEndpoints(
  container: RunningContainer,
  ports: readonly PublishedPort[],
  networkDriver: (network: string) => Promise<string | undefined>,
): Promise<ClientEndpoint[]> {
  const tcp = ports.filter((p) => p.protocol === "tcp");
  if (tcp.length === 0) return [];

  // Resolve, once, the container IPs on a driver we can actually route to.
  const reachableIps: string[] = [];
  for (const net of container.spec.networks) {
    if (!net.ipAddress) continue;
    if (reachableDriver(await networkDriver(net.name))) reachableIps.push(net.ipAddress);
  }

  const endpoints: ClientEndpoint[] = [];
  const seen = new Set<string>();
  const add = (host: string, port: number, truthful: boolean) => {
    const key = `${host}:${port}`;
    if (seen.has(key)) return;
    seen.add(key);
    endpoints.push({ host, port, truthful });
  };

  const hostMode = (container.spec.networkMode ?? "").startsWith("host");

  for (const p of tcp) {
    // Host networking shares the host stack: the container port is live on
    // loopback directly, no proxy - a truthful endpoint.
    if (hostMode) add("127.0.0.1", p.containerPort, true);
    // Every routable container IP, at the container port: the real client path.
    for (const ip of reachableIps) add(ip, p.containerPort, true);
    // The proxied fallback, at the host port.
    add("127.0.0.1", p.hostPort, false);
  }

  return endpoints;
}

/**
 * TCP only: a real connect() to the address a client actually uses.
 *
 * Stateful, so - like uptimeCheck - it is built per swap via this factory and
 * remembers when it first saw the container, and it needs a clock. Each poll it
 * probes every client endpoint (clientEndpoints):
 *
 *   - ANY endpoint accepts                 -> pass (something is serving, now).
 *   - before `seconds` have elapsed        -> inconclusive (a startup race must
 *                                             never read as a broken image).
 *   - at/after `seconds`, still not passing:
 *       a TRUTHFUL endpoint was refused    -> fail. The route to the container
 *         is wired but nothing accepts there: bound to the wrong address, or not
 *         listening at all. This is the one thing port-bound (blind to listen
 *         address) and docker-proxy (which fakes an accept) both miss.
 *       otherwise (only unreachable, or    -> inconclusive. We could not get an
 *         only the proxied fallback left)     authoritative "no" - never veto.
 *
 * The grace reuses the same `seconds` as uptime, so a slow-but-real app that
 * starts serving within the window still passes; no new deadline is introduced.
 * Skips when no TCP endpoint applies (no TCP ports, no reachable address).
 */
export function portConnectCheck(seconds: number, now: () => number): HealthCheck {
  let firstSeen: number | undefined;
  return {
    name: "port-connect",
    async evaluate(input, signal) {
      const endpoints = await clientEndpoints(input.container, input.ports, input.networkDriver);
      if (endpoints.length === 0) return SKIP;

      let truthfulRefused = false;
      for (const e of endpoints) {
        const outcome = await input.tcp.connect(e.host, e.port, signal);
        if (outcome === "accepted") return PASS;
        if (outcome === "refused" && e.truthful) truthfulRefused = true;
      }

      const at = now();
      if (firstSeen === undefined) firstSeen = at;
      if (at - firstSeen < seconds * 1000) return INCONCLUSIVE;

      return truthfulRefused
        ? fail(`up for ${seconds}s but refuses connections at its own address - the container is not serving clients`)
        : INCONCLUSIVE; // could not get an authoritative "no"; do not veto
    },
  };
}

/** The `st` code Linux reports for a bound (listening) UDP socket. */
const UDP_LISTEN = "07";
/** The `st` code for a listening TCP socket. */
const TCP_LISTEN = "0A";

/**
 * TCP and UDP: a bound/listening socket in the container's netns.
 *
 * Reads /proc/<pid>/net/{tcp,udp}[6] host-side. Passes as soon as ANY published
 * port has a LISTEN (TCP) or bound (UDP) socket - again "at least one", so
 * unused ports do not hold it back. This is the ONLY generic UDP signal (a UDP
 * send cannot tell open from unbound), and for TCP it usefully bypasses
 * docker-proxy (which answers a connect() even with no backend). Never actively
 * fails: nothing bound yet, or a netns read that throws because /proc is not
 * ready, is inconclusive. Skips when no ports are published.
 */
export const portBoundCheck: HealthCheck = {
  name: "port-bound",
  async evaluate(input) {
    if (input.ports.length === 0) return SKIP;
    const bound = await anyPortBound(input);
    // undefined = the netns could not be read yet (proc not ready); poll again.
    return bound === true ? PASS : INCONCLUSIVE;
  },
};

/**
 * Whether any published port has a bound/listening socket in the container's
 * netns. `undefined` means the netns could not be read yet (proc not ready, or
 * the pid briefly moved) - distinct from a definite "nothing is bound".
 *
 * Shared between port-bound (which turns "yes" into a pass) and the uptime check
 * (which, at its deadline, turns "no" into a fail - the "bound by T seconds"
 * rule that catches a container the network path accepts but nothing serves).
 */
async function anyPortBound(input: HealthInput): Promise<boolean | undefined> {
  const tcpPorts = new Set(input.ports.filter((p) => p.protocol === "tcp").map((p) => p.containerPort));
  const udpPorts = new Set(input.ports.filter((p) => p.protocol === "udp").map((p) => p.containerPort));

  try {
    if (tcpPorts.size > 0) {
      const listening = await portsInState(input.netns, input.container.pid, "tcp", TCP_LISTEN);
      if ([...tcpPorts].some((port) => listening.has(port))) return true;
    }
    if (udpPorts.size > 0) {
      const bound = await portsInState(input.netns, input.container.pid, "udp", UDP_LISTEN);
      if ([...udpPorts].some((port) => bound.has(port))) return true;
    }
  } catch {
    return undefined; // proc not readable yet
  }
  return false; // read fine, nothing bound
}

/**
 * The real TCP probe: open a connection to `host:port`, briefly, and classify it.
 *
 * Connects, then closes at once. The point of the three-way outcome is the error
 * split: a RST (ECONNREFUSED) means the route reached a host that said "no
 * backend" - `refused`, an authoritative no. No route at all (EHOSTUNREACH,
 * ENETUNREACH) or a timeout means we simply cannot see the endpoint from here -
 * `unreachable`, which never fails a check. Anything else unexpected is treated
 * as `unreachable` too (fail-open), so a probe quirk never rolls back a good
 * update on its own.
 */
export class BunTcpProbe implements TcpProbe {
  constructor(private readonly timeoutMillis: number = 2000) {}

  async connect(host: string, port: number, signal: AbortSignal): Promise<ConnectOutcome> {
    let socket: { end(): void } | undefined;
    try {
      socket = await Promise.race([
        Bun.connect({ hostname: host, port, socket: { data() {}, open(s) { s.end(); } } }),
        new Promise<never>((_, reject) => {
          const timer = setTimeout(() => reject(new Error("timed out")), this.timeoutMillis);
          signal.addEventListener("abort", () => { clearTimeout(timer); reject(new Error("aborted")); }, { once: true });
        }),
      ]);
      return "accepted";
    } catch (error) {
      return classifyConnectError(error);
    } finally {
      socket?.end();
    }
  }
}

/**
 * Map a connect() failure to an outcome.
 *
 * Only a clean connection-refused (the route works, the peer sent RST) is an
 * authoritative `refused`. Everything else - no route, timeout, abort, or an
 * error shape we do not recognise - is `unreachable`, the outcome that can never
 * fail a check, keeping the probe fail-open on anything ambiguous.
 */
export function classifyConnectError(error: unknown): ConnectOutcome {
  const code = (error as { code?: string })?.code ?? "";
  const message = (error as { message?: string })?.message ?? "";
  if (code === "ECONNREFUSED" || /econnrefused|connection refused/i.test(message)) return "refused";
  return "unreachable";
}

/**
 * The always-applicable check: the container stays up, and by the time it has
 * been up long enough it has bound a port (if it publishes any).
 *
 * Stateful, so it is built per swap via this factory - it records when it first
 * saw the container. Needs a clock, injected (daemon supplies Date.now, tests a
 * controllable one). It is the one check that couples "stayed up" with "is
 * serving", and the only generic one that can catch a container the network path
 * accepts but which never actually serves:
 *
 *   - Container exited / crash-looping (`running` false) -> fail.
 *   - Before `seconds` have elapsed -> inconclusive (give it time).
 *   - At/after `seconds`, still running:
 *       publishes ports and at least one is bound  -> pass.
 *       publishes ports and NONE is bound          -> fail ("up but not serving")
 *         - a real service binds within seconds; a no-backend container (where
 *           docker-proxy answers a connect() but nothing listens inside) is
 *           caught here rather than committed. The bind deadline is the uptime
 *           grace we already grant, so a slow-but-real app that binds within
 *           `seconds` still passes; we add no new deadline.
 *       publishes NO ports                          -> pass (nothing to bind;
 *           staying up is all we can ask of a portless worker).
 *       netns not readable yet                      -> inconclusive (poll again).
 */
export function uptimeCheck(seconds: number, now: () => number): HealthCheck {
  let firstSeen: number | undefined;
  return {
    name: "uptime",
    async evaluate(input) {
      if (!input.container.running) return fail("the container exited or is restarting");
      const at = now();
      if (firstSeen === undefined) firstSeen = at;
      if (at - firstSeen < seconds * 1000) return INCONCLUSIVE;

      // Up long enough. If it publishes ports, require at least one bound by now.
      if (input.ports.length === 0) return PASS;
      const bound = await anyPortBound(input);
      if (bound === undefined) return INCONCLUSIVE; // netns not readable yet
      return bound
        ? PASS
        : fail(`up for ${seconds}s but no published port is bound - the container is not serving`);
    },
  };
}

const CHECKS: Record<string, HealthCheck> = {
  docker: dockerCheck,
  "port-bound": portBoundCheck,
};

/**
 * Resolve a check by name.
 *
 * `uptime` and `port-connect` are not here: both are stateful (they remember
 * when they first saw the container) and need a clock, so the daemon constructs
 * them per swap via uptimeCheck()/portConnectCheck(). Only docker and port-bound
 * are stateless singletons resolvable by name.
 */
export function checkByName(name: string): HealthCheck | undefined {
  return CHECKS[name];
}

/**
 * Combine the checks' results into a verdict, or undefined for "keep polling".
 *
 * The rule, tuned so a flaky probe cannot veto a good update:
 *   - ANY check failed          -> roll back (unhealthy), immediately.
 *   - not timed out yet:
 *       every applicable check passed (>=1 pass, none inconclusive) -> commit;
 *       otherwise                                                    -> keep polling.
 *   - timed out (deadline reached): commit, UNLESS something failed. A check
 *     still inconclusive at the deadline does not veto - "we could not fully
 *     confirm in time" reads as "trust the update", because false-unhealthy is
 *     the worse error and a genuinely-broken image is caught next cycle.
 *
 * A gate where every check skipped (e.g. no HEALTHCHECK, no ports) settles to
 * commit - but in practice `uptime` is always applicable and passes before the
 * timeout, so that is an edge case only.
 */
export function combine(results: readonly CheckResult[], timedOut: boolean): HealthResult | undefined {
  const failed = results.find((r) => r.outcome === "fail");
  if (failed) return { healthy: false, reason: failed.reason ?? "a health check failed" };

  if (timedOut) {
    // Nothing failed by the deadline: trust the update.
    return { healthy: true };
  }

  const anyInconclusive = results.some((r) => r.outcome === "inconclusive");
  const anyPass = results.some((r) => r.outcome === "pass");
  if (!anyInconclusive && anyPass) return { healthy: true };

  // Every check skipped and none is inconclusive: nothing left to wait for.
  if (!anyInconclusive && !anyPass) return { healthy: true };

  return undefined; // keep polling
}

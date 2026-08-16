import { portsInState, type NetnsReader } from "./netns.ts";
import type { HealthStatus, PublishedPort, RunningContainer } from "./model.ts";

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
 * Can a TCP connection be opened to a host port right now?
 *
 * The seam for the TCP connect check, so tests decide the answer without a real
 * socket - the same "inject the I/O boundary" move as NetnsReader. Tidewaiter
 * runs host-networked, so the real implementation just connect()s to the port on
 * the host: the very port a client hits.
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
 * TCP only: a real connect() to a published host port.
 *
 * Tidewaiter is host-networked, so it connects to the exact port a client hits.
 * Passes as soon as ANY published TCP port accepts a connection - a container
 * with several published ports (a service port plus unused admin/metrics ones)
 * must not be held back because one of them never comes up. Never actively
 * fails: a port not yet accepting is inconclusive (keep waiting), because a
 * startup race must not read as a broken image. Skips when no TCP ports are
 * published.
 */
export const portConnectCheck: HealthCheck = {
  name: "port-connect",
  async evaluate(input, signal) {
    const tcpPorts = input.ports.filter((p) => p.protocol === "tcp");
    if (tcpPorts.length === 0) return SKIP;

    for (const p of tcpPorts) {
      if (await input.tcp.connectable(p.hostPort, signal)) return PASS;
    }
    return INCONCLUSIVE; // none accepting yet
  },
};

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
  "port-connect": portConnectCheck,
  "port-bound": portBoundCheck,
};

/**
 * Resolve a check by name.
 *
 * `uptime` is not here because it is stateful and needs a clock, so the daemon
 * constructs it per swap via uptimeCheck(); the other three are stateless
 * singletons.
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

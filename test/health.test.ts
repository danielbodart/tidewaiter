import { describe, expect, test } from "bun:test";
import {
  checkByName,
  combine,
  dockerCheck,
  dockerVerdict,
  portBoundCheck,
  portConnectCheck,
  uptimeCheck,
  type CheckResult,
  type HealthInput,
} from "../src/health.ts";
import { runningContainer, port } from "./fakes/docker.ts";
import { FakeNetnsReader } from "./fakes/netns.ts";
import { FakeTcpProbe } from "./fakes/tcp.ts";

const noSignal = new AbortController().signal;

/** A HealthInput with all seams defaulted; override per test. */
function input(over: Partial<HealthInput> = {}): HealthInput {
  return {
    container: runningContainer("app"),
    ports: [],
    netns: new FakeNetnsReader(),
    tcp: new FakeTcpProbe(),
    networkDriver: async () => "bridge", // default: reachable, override for macvlan/overlay
    ...over,
  };
}

const r = (outcome: CheckResult["outcome"]): CheckResult => ({ outcome });

describe("dockerCheck", () => {
  test("healthy=pass, unhealthy=fail, starting=inconclusive", async () => {
    const at = (health: "healthy" | "unhealthy" | "starting" | "none") =>
      dockerCheck.evaluate(input({ container: runningContainer("app", { health }) }), noSignal);
    expect((await at("healthy")).outcome).toBe("pass");
    expect((await at("unhealthy")).outcome).toBe("fail");
    expect((await at("starting")).outcome).toBe("inconclusive");
  });

  test("no HEALTHCHECK (none) SKIPS - it is not a failure to lack one", async () => {
    const result = await dockerCheck.evaluate(input({ container: runningContainer("app", { health: "none" }) }), noSignal);
    expect(result.outcome).toBe("skip");
  });

  test("dockerVerdict maps an event status the same way", () => {
    expect(dockerVerdict("healthy").outcome).toBe("pass");
    expect(dockerVerdict("unhealthy").outcome).toBe("fail");
  });
});

describe("portConnectCheck (real connect to the client-facing address)", () => {
  /** A container attached to a bridge network with a routable runtime IP. */
  const onBridge = (ip: string) =>
    runningContainer("app", { spec: { networks: [{ name: "bridge", aliases: [], ipAddress: ip }] } });

  test("passes as soon as ANY endpoint accepts (proxied host port, no container IP)", async () => {
    const tcp = new FakeTcpProbe().listening(18081); // only the second host port is up
    const check = portConnectCheck(10, () => 0);
    const result = await check.evaluate(
      input({ ports: [port(18080, 80, "tcp"), port(18081, 81, "tcp")], tcp }),
      noSignal,
    );
    expect(result.outcome).toBe("pass");
  });

  test("with no container IP, falls back to connecting to the HOST port", async () => {
    const tcp = new FakeTcpProbe().listening(18080);
    const check = portConnectCheck(10, () => 0);
    await check.evaluate(input({ ports: [port(18080, 80, "tcp")], tcp }), noSignal);
    expect(tcp.probed).toContain(18080); // the proxied fallback endpoint
  });

  test("prefers the container IP at the CONTAINER port when the container is on a bridge", async () => {
    const tcp = new FakeTcpProbe().acceptingAt("172.17.0.2", 80);
    const check = portConnectCheck(10, () => 0);
    const result = await check.evaluate(
      input({ container: onBridge("172.17.0.2"), ports: [port(18080, 80, "tcp")], tcp }),
      noSignal,
    );
    expect(result.outcome).toBe("pass");
    expect(tcp.probedTargets).toContainEqual({ host: "172.17.0.2", port: 80 });
  });

  test("FAILS past the grace when the container IP is reachable but REFUSES (wrong-address bind)", async () => {
    // The unique catch: a socket bound to the container's own loopback passes
    // port-bound (blind to listen address) but is unreachable to clients here.
    let now = 1_000_000;
    const tcp = new FakeTcpProbe().refusingAt("172.17.0.2", 80);
    const check = portConnectCheck(10, () => now);
    const inp = input({ container: onBridge("172.17.0.2"), ports: [port(18080, 80, "tcp")], tcp });
    expect((await check.evaluate(inp, noSignal)).outcome).toBe("inconclusive"); // first sight, within grace
    now += 10_000;
    expect((await check.evaluate(inp, noSignal)).outcome).toBe("fail"); // route works, nothing serving
  });

  test("a macvlan container IP is NEVER probed - a stray RST there must not fail a healthy update", async () => {
    // The bug driver-gating fixes: a macvlan IP whose subnet is coincidentally
    // routable to some OTHER host would answer with a foreign RST (refused). If
    // we probed it, that would fail a perfectly healthy update. So we skip it.
    let now = 1_000_000;
    const tcp = new FakeTcpProbe().refusingAt("10.0.0.5", 80); // a foreign RST at that IP
    const macvlan = runningContainer("app", {
      spec: { networks: [{ name: "pub", aliases: [], ipAddress: "10.0.0.5" }] },
    });
    const check = portConnectCheck(10, () => now);
    // published on a host port too, so there is still a proxied endpoint.
    const inp = input({
      container: macvlan,
      ports: [port(18080, 80, "tcp")],
      tcp,
      networkDriver: async () => "macvlan",
    });
    await check.evaluate(inp, noSignal);
    now += 10_000;
    const result = await check.evaluate(inp, noSignal);
    expect(result.outcome).toBe("inconclusive"); // NOT fail
    expect(tcp.probedTargets).not.toContainEqual({ host: "10.0.0.5", port: 80 }); // never touched
  });

  test("an overlay container IP is likewise skipped (host is not on the VXLAN)", async () => {
    const tcp = new FakeTcpProbe().refusingAt("10.1.2.3", 80);
    const overlay = runningContainer("app", {
      spec: { networks: [{ name: "mesh", aliases: [], ipAddress: "10.1.2.3" }] },
    });
    const check = portConnectCheck(10, () => 0);
    const inp = input({ container: overlay, ports: [port(80, 80, "tcp")], tcp, networkDriver: async () => "overlay" });
    await check.evaluate(inp, noSignal);
    expect(tcp.probedTargets).not.toContainEqual({ host: "10.1.2.3", port: 80 });
  });

  test("an unresolvable driver (network gone) is treated as not-reachable, never probed", async () => {
    const tcp = new FakeTcpProbe().refusingAt("172.17.0.2", 80);
    const check = portConnectCheck(10, () => 0);
    const inp = input({
      container: onBridge("172.17.0.2"),
      ports: [port(80, 80, "tcp")],
      tcp,
      networkDriver: async () => undefined,
    });
    await check.evaluate(inp, noSignal);
    expect(tcp.probedTargets).not.toContainEqual({ host: "172.17.0.2", port: 80 });
  });

  test("firewalled bridge: truthful endpoint unreachable but the proxied fallback accepts => pass", async () => {
    const tcp = new FakeTcpProbe().unreachableAt("172.17.0.2", 80).listening(18080);
    const check = portConnectCheck(10, () => 0);
    const result = await check.evaluate(
      input({ container: onBridge("172.17.0.2"), ports: [port(18080, 80, "tcp")], tcp }),
      noSignal,
    );
    expect(result.outcome).toBe("pass");
  });

  test("host network mode: probes 127.0.0.1 at the container port, truthfully", async () => {
    let now = 1_000_000;
    const tcp = new FakeTcpProbe().refusingAt("127.0.0.1", 8080);
    const check = portConnectCheck(10, () => now);
    const host = runningContainer("app", { spec: { networkMode: "host" } });
    const inp = input({ container: host, ports: [port(8080, 8080, "tcp")], tcp });
    await check.evaluate(inp, noSignal);
    now += 10_000;
    expect((await check.evaluate(inp, noSignal)).outcome).toBe("fail"); // reachable on loopback, refuses
  });

  test("no TCP ports published => skip", async () => {
    const check = portConnectCheck(10, () => 0);
    const result = await check.evaluate(input({ ports: [port(19132, 19132, "udp")] }), noSignal);
    expect(result.outcome).toBe("skip");
  });
});

describe("portBoundCheck (TCP+UDP bound-socket in netns)", () => {
  test("passes when a bound UDP socket (state 07) is present", async () => {
    // 4ABC = 19132, state 07 = bound udp
    const bound = "  sl  local_address\n   0: 00000000:4ABC 00000000:0000 07 0 0 0";
    const netns = new FakeNetnsReader().set(4242, "udp", bound).set(4242, "udp6", "");
    const result = await portBoundCheck.evaluate(input({ ports: [port(19132, 19132, "udp")], netns }), noSignal);
    expect(result.outcome).toBe("pass");
  });

  test("passes when ANY of several ports is bound (unused ones do not hold it back)", async () => {
    // 1F91 = 8081 LISTEN; 8080 not bound.
    const listening = "  sl\n   0: 00000000:1F91 00000000:0000 0A 0 0 0";
    const netns = new FakeNetnsReader().set(4242, "tcp", listening).set(4242, "tcp6", "");
    const result = await portBoundCheck.evaluate(
      input({ ports: [port(28080, 8080, "tcp"), port(28081, 8081, "tcp")], netns }),
      noSignal,
    );
    expect(result.outcome).toBe("pass");
  });

  test("nothing bound yet is inconclusive", async () => {
    const netns = new FakeNetnsReader().set(4242, "udp", "  sl\n").set(4242, "udp6", "");
    const result = await portBoundCheck.evaluate(input({ ports: [port(19132, 19132, "udp")], netns }), noSignal);
    expect(result.outcome).toBe("inconclusive");
  });

  test("a netns read that throws (proc not ready) is inconclusive, never a failure", async () => {
    const netns = new FakeNetnsReader();
    netns.failWith = new Error("ENOENT: no such file or directory, open '/proc/999/net/udp'");
    const result = await portBoundCheck.evaluate(input({ ports: [port(19132, 19132, "udp")], netns }), noSignal);
    expect(result.outcome).toBe("inconclusive");
  });

  test("no ports published => skip", async () => {
    const result = await portBoundCheck.evaluate(input({ ports: [] }), noSignal);
    expect(result.outcome).toBe("skip");
  });
});

describe("uptimeCheck", () => {
  test("inconclusive until the seconds elapse, then passes", async () => {
    let now = 1_000_000;
    const check = uptimeCheck(10, () => now);
    const inp = input();
    expect((await check.evaluate(inp, noSignal)).outcome).toBe("inconclusive"); // first sight
    now += 5_000;
    expect((await check.evaluate(inp, noSignal)).outcome).toBe("inconclusive"); // 5s < 10s
    now += 5_000;
    expect((await check.evaluate(inp, noSignal)).outcome).toBe("pass"); // 10s
  });

  test("actively FAILS if the container has exited or is restarting", async () => {
    let now = 1_000_000;
    const check = uptimeCheck(10, () => now);
    const exited = input({ container: runningContainer("app", { running: false }) });
    const result = await check.evaluate(exited, noSignal);
    expect(result.outcome).toBe("fail");
  });

  describe("bound-by-uptime: at the deadline a container that publishes ports must have one bound", () => {
    const bound = "  sl  local_address\n   0: 00000000:1F90 00000000:0000 0A 0 0 0"; // 8080 LISTEN

    test("passes when a published port is bound by the deadline", async () => {
      let now = 1_000_000;
      const check = uptimeCheck(10, () => now);
      const netns = new FakeNetnsReader().set(4242, "tcp", bound).set(4242, "tcp6", "");
      const inp = input({ ports: [port(8080, 8080, "tcp")], netns });
      expect((await check.evaluate(inp, noSignal)).outcome).toBe("inconclusive"); // first sight
      now += 10_000;
      expect((await check.evaluate(inp, noSignal)).outcome).toBe("pass"); // up 10s AND bound
    });

    test("FAILS when the deadline passes and NO published port is bound (no-backend / not serving)", async () => {
      let now = 1_000_000;
      const check = uptimeCheck(10, () => now);
      const netns = new FakeNetnsReader().set(4242, "tcp", "  sl\n").set(4242, "tcp6", ""); // nothing bound
      const inp = input({ ports: [port(8080, 8080, "tcp")], netns });
      await check.evaluate(inp, noSignal); // first sight (inconclusive)
      now += 10_000;
      const result = await check.evaluate(inp, noSignal);
      expect(result.outcome).toBe("fail"); // up but not serving
    });

    test("before the deadline, an unbound container is inconclusive (given time to bind)", async () => {
      let now = 1_000_000;
      const check = uptimeCheck(10, () => now);
      const netns = new FakeNetnsReader().set(4242, "tcp", "  sl\n").set(4242, "tcp6", "");
      const inp = input({ ports: [port(8080, 8080, "tcp")], netns });
      await check.evaluate(inp, noSignal);
      now += 5_000; // only 5s
      expect((await check.evaluate(inp, noSignal)).outcome).toBe("inconclusive");
    });

    test("a portless container passes on time alone (nothing to bind)", async () => {
      let now = 1_000_000;
      const check = uptimeCheck(10, () => now);
      const inp = input({ ports: [] });
      await check.evaluate(inp, noSignal);
      now += 10_000;
      expect((await check.evaluate(inp, noSignal)).outcome).toBe("pass");
    });

    test("a netns read that throws at the deadline is inconclusive, not a fail", async () => {
      let now = 1_000_000;
      const check = uptimeCheck(10, () => now);
      const netns = new FakeNetnsReader();
      netns.failWith = new Error("ENOENT");
      const inp = input({ ports: [port(8080, 8080, "tcp")], netns });
      await check.evaluate(inp, noSignal);
      now += 10_000;
      expect((await check.evaluate(inp, noSignal)).outcome).toBe("inconclusive");
    });
  });
});

describe("combine", () => {
  test("any fail => roll back, even alongside passes", () => {
    expect(combine([r("pass"), r("fail")], false)).toEqual({ healthy: false, reason: expect.any(String) });
  });

  test("all applicable pass (>=1 pass, none inconclusive) => commit", () => {
    expect(combine([r("pass"), r("skip")], false)).toEqual({ healthy: true });
  });

  test("some inconclusive, none failed => keep polling (undefined)", () => {
    expect(combine([r("pass"), r("inconclusive")], false)).toBeUndefined();
  });

  test("at the timeout, commit unless something failed - inconclusive does NOT veto", () => {
    // The whole design point: a slow/flaky probe must not roll back a good update.
    expect(combine([r("pass"), r("inconclusive")], true)).toEqual({ healthy: true });
    expect(combine([r("inconclusive"), r("inconclusive")], true)).toEqual({ healthy: true });
  });

  test("at the timeout, a fail still rolls back", () => {
    expect(combine([r("inconclusive"), r("fail")], true)).toEqual({ healthy: false, reason: expect.any(String) });
  });

  test("all skipped settles to commit rather than hanging", () => {
    expect(combine([r("skip"), r("skip")], false)).toEqual({ healthy: true });
  });
});

describe("checkByName", () => {
  test("resolves the stateless checks; uptime and port-connect are built per swap", () => {
    expect(checkByName("docker")).toBe(dockerCheck);
    expect(checkByName("port-bound")).toBe(portBoundCheck);
    expect(checkByName("uptime")).toBeUndefined();
    expect(checkByName("port-connect")).toBeUndefined(); // stateful now, built via portConnectCheck()
  });
});

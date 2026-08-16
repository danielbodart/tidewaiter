import { describe, expect, test } from "bun:test";
import {
  dockerHealthProber,
  healthProberByName,
  portHealthProber,
  uptimeHealthProber,
  verdictForDockerHealth,
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
    ...over,
  };
}

describe("dockerHealthProber / verdictForDockerHealth", () => {
  test("healthy is a pass, unhealthy a fail", () => {
    expect(verdictForDockerHealth("healthy")).toEqual({ healthy: true });
    expect(verdictForDockerHealth("unhealthy")?.healthy).toBe(false);
  });

  test("starting is inconclusive (keep waiting)", () => {
    expect(verdictForDockerHealth("starting")).toBeUndefined();
  });

  test("no HEALTHCHECK under health=docker is a fail, not a silent pass", async () => {
    const result = await dockerHealthProber.check(input({ container: runningContainer("app", { health: "none" }) }), noSignal);
    expect(result?.healthy).toBe(false);
  });
});

describe("portHealthProber", () => {
  describe("TCP - a real connect() to the published host port", () => {
    test("a host port that accepts a connection is healthy", async () => {
      // published 18080:80; the prober connects to the HOST port, 18080.
      const tcp = new FakeTcpProbe().listening(18080);
      const result = await portHealthProber.check(
        input({ ports: [port(18080, 80, "tcp")], tcp }),
        noSignal,
      );
      expect(result).toEqual({ healthy: true });
      expect(tcp.probed).toEqual([18080]); // connected to the host port, not the container port
    });

    test("a host port that refuses is inconclusive (keep waiting), not unhealthy", async () => {
      const tcp = new FakeTcpProbe(); // nothing listening
      const result = await portHealthProber.check(input({ ports: [port(18080, 80, "tcp")], tcp }), noSignal);
      expect(result).toBeUndefined();
    });

    test("all published TCP ports must accept before it is healthy", async () => {
      const tcp = new FakeTcpProbe().listening(18080); // 18081 not up yet
      const result = await portHealthProber.check(
        input({ ports: [port(18080, 80, "tcp"), port(18081, 81, "tcp")], tcp }),
        noSignal,
      );
      expect(result).toBeUndefined();
    });
  });

  describe("UDP - a bound-socket check in the container netns", () => {
    test("a bound UDP socket (state 07) is healthy", async () => {
      // 4ABC = 19132, state 07 = bound udp
      const bound = "  sl  local_address\n   0: 00000000:4ABC 00000000:0000 07 0 0 0";
      const netns = new FakeNetnsReader().set(4242, "udp", bound).set(4242, "udp6", "");
      const result = await portHealthProber.check(input({ ports: [port(19132, 19132, "udp")], netns }), noSignal);
      expect(result).toEqual({ healthy: true });
    });

    test("no bound UDP socket is inconclusive (keep waiting)", async () => {
      const netns = new FakeNetnsReader().set(4242, "udp", "  sl\n").set(4242, "udp6", "");
      const result = await portHealthProber.check(input({ ports: [port(19132, 19132, "udp")], netns }), noSignal);
      expect(result).toBeUndefined();
    });

    // Regression from a live-host run: a freshly-started container's
    // /proc/<pid>/net/udp is not there for the first instant, so the read throws
    // ENOENT. That must read as "not ready yet" (keep waiting), never an
    // immediate unhealthy that rolls a perfectly-good update straight back.
    test("a netns read that throws (proc not ready) is inconclusive, not unhealthy", async () => {
      const netns = new FakeNetnsReader();
      netns.failWith = new Error("ENOENT: no such file or directory, open '/proc/999/net/udp'");
      const result = await portHealthProber.check(input({ ports: [port(19132, 19132, "udp")], netns }), noSignal);
      expect(result).toBeUndefined();
    });
  });

  test("no ports to probe is a fail", async () => {
    const result = await portHealthProber.check(input({ ports: [] }), noSignal);
    expect(result?.healthy).toBe(false);
  });
});

describe("uptimeHealthProber", () => {
  test("passes only once the required seconds have elapsed on the injected clock", async () => {
    let now = 1_000_000;
    const prober = uptimeHealthProber(10, () => now);
    const inp = input();

    expect(await prober.check(inp, noSignal)).toBeUndefined(); // first sight
    now += 5_000;
    expect(await prober.check(inp, noSignal)).toBeUndefined(); // 5s < 10s
    now += 5_000;
    expect(await prober.check(inp, noSignal)).toEqual({ healthy: true }); // 10s
  });
});

describe("healthProberByName", () => {
  test("resolves docker and port; uptime is constructed per swap, not resolved here", () => {
    expect(healthProberByName("docker")).toBe(dockerHealthProber);
    expect(healthProberByName("port")).toBe(portHealthProber);
    expect(healthProberByName("uptime")).toBeUndefined();
  });
});

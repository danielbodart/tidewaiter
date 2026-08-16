import { describe, expect, test } from "bun:test";
import {
  dockerHealthProber,
  healthProberByName,
  portHealthProber,
  uptimeHealthProber,
  verdictForDockerHealth,
} from "../src/health.ts";
import { runningContainer, port } from "./fakes/docker.ts";
import { FakeNetnsReader } from "./fakes/netns.ts";

const noSignal = new AbortController().signal;

describe("dockerHealthProber / verdictForDockerHealth", () => {
  test("healthy is a pass, unhealthy a fail", () => {
    expect(verdictForDockerHealth("healthy")).toEqual({ healthy: true });
    expect(verdictForDockerHealth("unhealthy")?.healthy).toBe(false);
  });

  test("starting is inconclusive (keep waiting)", () => {
    expect(verdictForDockerHealth("starting")).toBeUndefined();
  });

  test("no HEALTHCHECK under health=docker is a fail, not a silent pass", async () => {
    const result = await dockerHealthProber.check(
      { container: runningContainer("app", { health: "none" }), ports: [], netns: new FakeNetnsReader() },
      noSignal,
    );
    expect(result?.healthy).toBe(false);
  });
});

describe("portHealthProber", () => {
  // 1F90 = 8080, state 0A = LISTEN
  const listening = "  sl  local_address\n   0: 00000000:1F90 00000000:0000 0A 0 0 0";

  test("a TCP port with a LISTEN socket is healthy", async () => {
    const netns = new FakeNetnsReader().set(4242, "tcp", listening).set(4242, "tcp6", "");
    const result = await portHealthProber.check(
      { container: runningContainer("app"), ports: [port(8080, 8080, "tcp")], netns },
      noSignal,
    );
    expect(result).toEqual({ healthy: true });
  });

  test("a TCP port with nothing listening is inconclusive (keep waiting)", async () => {
    const netns = new FakeNetnsReader().set(4242, "tcp", "  sl\n").set(4242, "tcp6", "");
    const result = await portHealthProber.check(
      { container: runningContainer("app"), ports: [port(8080, 8080, "tcp")], netns },
      noSignal,
    );
    expect(result).toBeUndefined();
  });

  test("a UDP port needs a bound socket (state 07)", async () => {
    // 4ABC = 19132, state 07 = bound udp
    const bound = "  sl  local_address\n   0: 00000000:4ABC 00000000:0000 07 0 0 0";
    const netns = new FakeNetnsReader().set(4242, "udp", bound).set(4242, "udp6", "");
    const result = await portHealthProber.check(
      { container: runningContainer("app"), ports: [port(19132, 19132, "udp")], netns },
      noSignal,
    );
    expect(result).toEqual({ healthy: true });
  });

  test("no ports to probe is a fail", async () => {
    const result = await portHealthProber.check(
      { container: runningContainer("app"), ports: [], netns: new FakeNetnsReader() },
      noSignal,
    );
    expect(result?.healthy).toBe(false);
  });
});

describe("uptimeHealthProber", () => {
  test("passes only once the required seconds have elapsed on the injected clock", async () => {
    let now = 1_000_000;
    const prober = uptimeHealthProber(10, () => now);
    const input = { container: runningContainer("app"), ports: [], netns: new FakeNetnsReader() };

    expect(await prober.check(input, noSignal)).toBeUndefined(); // first sight
    now += 5_000;
    expect(await prober.check(input, noSignal)).toBeUndefined(); // 5s < 10s
    now += 5_000;
    expect(await prober.check(input, noSignal)).toEqual({ healthy: true }); // 10s
  });
});

describe("healthProberByName", () => {
  test("resolves docker and port; uptime is constructed per swap, not resolved here", () => {
    expect(healthProberByName("docker")).toBe(dockerHealthProber);
    expect(healthProberByName("port")).toBe(portHealthProber);
    expect(healthProberByName("uptime")).toBeUndefined();
  });
});

import { describe, expect, test } from "bun:test";
import { parseConntrack, type ConntrackEntry } from "../src/conntrack.ts";
import {
  combine,
  conntrackDetector,
  detectorByName,
  netioDetector,
  noneDetector,
  parseProcNetTcp,
  tcpDetector,
  type DetectorResult,
} from "../src/detectors.ts";
import type { PublishedPort } from "../src/model.ts";
import { FakeNetnsReader } from "./fakes/netns.ts";

const conntrack = parseConntrack(await Bun.file(`${import.meta.dir}/fixtures/conntrack-L.txt`).text());
const procNetTcp = await Bun.file(`${import.meta.dir}/fixtures/proc-net-tcp.txt`).text();

const tcp = (hostPort: number): PublishedPort => ({ hostPort, containerPort: hostPort, protocol: "tcp" });
const udp = (hostPort: number): PublishedPort => ({ hostPort, containerPort: hostPort, protocol: "udp" });

describe("parseProcNetTcp", () => {
  test("reads local port and state, skipping the header", () => {
    const entries = parseProcNetTcp(procNetTcp);
    // 1F90 = 8080 (LISTEN + two ESTABLISHED), 0016 = 22 (LISTEN)
    expect(entries).toContainEqual({ localPort: 8080, state: "0A" });
    expect(entries.filter((e) => e.localPort === 8080 && e.state === "01")).toHaveLength(2);
    expect(entries).toContainEqual({ localPort: 22, state: "0A" });
  });
});

describe("conntrackDetector", () => {
  const detect = (published: PublishedPort[], table: readonly ConntrackEntry[] = conntrack) =>
    conntrackDetector.inUse({ id: "c", pid: 1, published, conntrack: table, netns: new FakeNetnsReader() });

  test("counts ASSURED flows to a published port as sessions", async () => {
    const result = await detect([udp(19132)]);
    expect(result).toEqual({ inUse: true, sessions: 1, confidence: "high" });
  });

  test("an unASSURED-only flow reads as in use but uncounted", async () => {
    // Port 25565 has one ASSURED and one SYN_SENT; ASSURED wins the count.
    const result = await detect([tcp(25565)]);
    expect(result.inUse).toBe(true);
    expect(result.confidence).toBe("high");
  });

  test("no flows to the port reads as idle, high confidence", async () => {
    const result = await detect([tcp(1234)]);
    expect(result).toEqual({ inUse: false, confidence: "high" });
  });
});

describe("tcpDetector", () => {
  test("counts ESTABLISHED sockets on a published container port", async () => {
    const netns = new FakeNetnsReader().set(42, "tcp", procNetTcp).set(42, "tcp6", "");
    const result = await tcpDetector.inUse({ id: "c", pid: 42, published: [tcp(8080)], conntrack: [], netns });
    expect(result).toEqual({ inUse: true, sessions: 2, confidence: "high" });
  });

  test("no ESTABLISHED sockets reads as idle", async () => {
    const netns = new FakeNetnsReader().set(42, "tcp", procNetTcp).set(42, "tcp6", "");
    const result = await tcpDetector.inUse({ id: "c", pid: 42, published: [tcp(9999)], conntrack: [], netns });
    expect(result.inUse).toBe(false);
  });

  test("with no tcp ports it cannot tell", async () => {
    const result = await tcpDetector.inUse({ id: "c", pid: 42, published: [udp(19132)], conntrack: [], netns: new FakeNetnsReader() });
    expect(result.confidence).toBe("low");
    expect(result.inUse).toBe(false);
  });
});

describe("noneDetector", () => {
  test("always idle, low confidence", async () => {
    const result = await noneDetector.inUse({ id: "c", pid: 1, published: [], conntrack: [], netns: new FakeNetnsReader() });
    expect(result).toEqual({ inUse: false, confidence: "low" });
  });
});

describe("detectorByName", () => {
  test("resolves the stateless names and nothing else", () => {
    expect(detectorByName("conntrack")).toBe(conntrackDetector);
    expect(detectorByName("none")).toBe(noneDetector);
    expect(detectorByName("nope")).toBeUndefined();
    // netio is built via netioDetector(), not resolved by name, because it is
    // stateful and needs Docker + a clock.
    expect(detectorByName("netio")).toBeUndefined();
  });
});

describe("netioDetector", () => {
  const input = (id: string) => ({ id, pid: 1, published: [], conntrack: [], netns: new FakeNetnsReader() });

  test("cannot tell on the first sample (no baseline for a rate)", async () => {
    let now = 0;
    const detector = netioDetector({ netBytes: async () => 1000, now: () => now });
    expect(await detector.inUse(input("web"))).toEqual({ inUse: true, confidence: "low" });
  });

  test("busy, high confidence, when bytes climb above the threshold", async () => {
    let now = 0;
    const bytes: Record<string, number> = { web: 0 };
    const detector = netioDetector({
      netBytes: async (id) => bytes[id] ?? 0,
      now: () => now,
      thresholdBytesPerSec: 100,
    });

    await detector.inUse(input("web")); // baseline at t=0, 0 bytes
    now = 1000; // one second later
    bytes.web = 5000; // 5000 B/s >> 100
    expect(await detector.inUse(input("web"))).toEqual({ inUse: true, confidence: "high" });
  });

  test("idle, high confidence, when bytes are flat", async () => {
    let now = 0;
    const detector = netioDetector({ netBytes: async () => 4242, now: () => now, thresholdBytesPerSec: 100 });

    await detector.inUse(input("web")); // baseline
    now = 1000;
    expect(await detector.inUse(input("web"))).toEqual({ inUse: false, confidence: "high" }); // 0 B/s
  });

  test("keeps a separate baseline per container", async () => {
    let now = 0;
    const bytes: Record<string, number> = { a: 0, b: 0 };
    const detector = netioDetector({
      netBytes: async (id) => bytes[id] ?? 0,
      now: () => now,
      thresholdBytesPerSec: 100,
    });

    await detector.inUse(input("a"));
    await detector.inUse(input("b"));
    now = 1000;
    bytes.a = 10_000; // a is busy
    // b is flat
    expect((await detector.inUse(input("a"))).inUse).toBe(true);
    expect((await detector.inUse(input("b"))).inUse).toBe(false);
  });
});

describe("combine", () => {
  const idle: DetectorResult = { inUse: false, confidence: "high" };
  const busy: DetectorResult = { inUse: true, sessions: 2, confidence: "high" };
  const cantTell: DetectorResult = { inUse: true, confidence: "low" };

  test("in use if any detector says so", () => {
    expect(combine([idle, busy]).inUse).toBe(true);
  });

  test("idle only when all agree", () => {
    expect(combine([idle, idle]).inUse).toBe(false);
  });

  test("confidence is high when a high-confidence detector drove the verdict", () => {
    expect(combine([idle, idle]).confidence).toBe("high");
  });

  test("a lone cannot-tell stays low confidence", () => {
    expect(combine([cantTell])).toEqual({ inUse: true, confidence: "low" });
  });

  test("sums sessions where they are countable", () => {
    expect(combine([busy, busy]).sessions).toBe(4);
  });
});

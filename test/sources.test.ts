import { describe, expect, test } from "bun:test";
import { Daemon, DEFAULTS } from "../src/daemon.ts";
import type { Action } from "../src/decide.ts";
import { FakeConntrackSource } from "./fakes/conntrack.ts";
import { FakeInhibitor, FakeSource } from "./fakes/source.ts";

const keep: Action = { kind: "keep", reason: "already on the latest image" };
const defer: Action = { kind: "defer", reason: "busy" };

function make(...names: ("docker" | "systemd")[]) {
  const inhibitor = new FakeInhibitor();
  const conntrack = new FakeConntrackSource();
  const sources = names.map((name) => new FakeSource(name, inhibitor));
  const log: string[] = [];
  const instance = new Daemon(sources, conntrack, inhibitor, DEFAULTS, (message) => log.push(message));
  return { daemon: instance, sources, inhibitor, conntrack, log };
}

describe("the daemon over several sources", () => {
  test("dumps conntrack once and hands every source the same table", async () => {
    const { daemon, sources, conntrack } = both();
    conntrack.text = "udp      17 29 src=10.0.0.5 dst=10.0.0.1 sport=40000 dport=19132 [ASSURED] mark=0 use=1\n";

    await daemon.once();

    expect(conntrack.calls).toBe(1);
    expect(sources[0]!.contexts[0]!.conntrack).toEqual(sources[1]!.contexts[0]!.conntrack);
    expect(sources[0]!.contexts[0]!.conntrack).toHaveLength(1);
  });

  test("a failed dump reaches the sources as could-not-tell, not as an empty table", async () => {
    const { daemon, sources, conntrack, log } = both();
    conntrack.failWith = new Error("Operation not permitted");

    await daemon.once();

    expect(sources[1]!.contexts[0]!.conntrack).toBeUndefined();
    expect(log.join("\n")).toContain("could not read conntrack");
  });

  test("concatenates the outcomes in source order", async () => {
    const { daemon, sources } = both();
    sources[0]!.outcomes = [{ workload: "web", action: keep, busy: false }];
    sources[1]!.outcomes = [{ workload: "game.service", action: keep, busy: false }];

    const outcomes = await daemon.once();

    expect(outcomes.map((o) => o.workload)).toEqual(["web", "game.service"]);
  });

  test("a source that throws does not stop the other, and the error still surfaces", async () => {
    const { daemon, sources, log } = both();
    sources[0]!.failWith = new Error("docker socket gone");
    sources[1]!.outcomes = [{ workload: "game.service", action: keep, busy: false }];

    await expect(daemon.once()).rejects.toThrow("docker socket gone");

    expect(sources[1]!.contexts).toHaveLength(1);
    expect(log.join("\n")).toContain("docker: Error: docker socket gone");
  });

  test("summarises in neutral words across sources", async () => {
    const { daemon, sources, log } = both();
    sources[0]!.outcomes = [{ workload: "web", action: keep, busy: false }];
    sources[1]!.outcomes = [{ workload: "game.service", action: keep, busy: false }];

    await daemon.once();

    expect(log).toContain("2 workloads up to date, nothing to do");
  });

  test("calls units units when systemd is the only source", async () => {
    const { daemon, log } = only("systemd");
    await daemon.once();
    expect(log).toContain("No units to manage");
  });
});

describe("the shutdown inhibitor", () => {
  test("is held for exactly the busy workloads, and released when they go idle", async () => {
    const { daemon, sources, inhibitor } = both();
    sources[0]!.outcomes = [{ workload: "web", action: defer, busy: true }];
    sources[1]!.outcomes = [
      { workload: "bedrock.service", action: defer, busy: true },
      { workload: "hytale.service", action: keep, busy: false },
    ];

    await daemon.once();
    expect(inhibitor.held).toEqual(["bedrock.service", "web"]);

    sources[0]!.outcomes = [{ workload: "web", action: keep, busy: false }];
    sources[1]!.outcomes = [{ workload: "bedrock.service", action: keep, busy: false }];
    await daemon.once();
    expect(inhibitor.held).toEqual([]);
  });

  test("is taken for a swap before it starts, and let go at the end of the pass", async () => {
    const { daemon, sources, inhibitor } = only("systemd");
    sources[0]!.holds = ["bedrock.service"];
    sources[0]!.outcomes = [{ workload: "bedrock.service", action: keep, busy: false }];

    await daemon.once();

    expect(sources[0]!.seenHeld).toEqual([["bedrock.service"]]);
    expect(inhibitor.held).toEqual([]);
  });

  test("keeps a workload's last verdict when a pass could not sample it", async () => {
    const { daemon, sources, inhibitor } = only("docker");
    sources[0]!.outcomes = [{ workload: "web", action: defer, busy: true }];
    await daemon.once();

    // The registry did not answer: no sample this pass.
    sources[0]!.outcomes = [{ workload: "web", action: { kind: "keep", reason: "registry unreachable" } }];
    await daemon.once();

    expect(inhibitor.held).toEqual(["web"]);
  });

  test("keeps a source's verdicts when the source throws", async () => {
    const { daemon, sources, inhibitor } = both();
    sources[1]!.outcomes = [{ workload: "bedrock.service", action: defer, busy: true }];
    await daemon.once();

    sources[1]!.failWith = new Error("bus gone");
    await daemon.once().catch(() => {});

    expect(inhibitor.held).toEqual(["bedrock.service"]);
  });

  test("drops a workload the source no longer reports", async () => {
    const { daemon, sources, inhibitor } = only("docker");
    sources[0]!.outcomes = [{ workload: "web", action: defer, busy: true }];
    await daemon.once();

    sources[0]!.outcomes = [];
    await daemon.once();

    expect(inhibitor.held).toEqual([]);
  });

  test("the activity tick moves the lock between passes", async () => {
    const { daemon, sources, inhibitor, conntrack } = both();
    sources[1]!.outcomes = [{ workload: "bedrock.service", action: keep, busy: false }];
    await daemon.once();
    expect(inhibitor.held).toEqual([]);

    sources[1]!.activities = [{ workload: "bedrock.service", busy: true }];
    await daemon.tick();

    expect(inhibitor.held).toEqual(["bedrock.service"]);
    expect(conntrack.calls).toBe(2);
    // The tick asked for activity only; no pass ran.
    expect(sources[1]!.contexts).toHaveLength(1);
  });

  test("a lock that cannot be taken is warned about once, and the pass goes on", async () => {
    const { daemon, sources, inhibitor, log } = only("systemd");
    inhibitor.failWith = new Error("could not take the shutdown inhibitor");
    sources[0]!.outcomes = [{ workload: "bedrock.service", action: defer, busy: true }];

    await daemon.once();
    await daemon.once();

    expect(log.filter((line) => line.includes("could not take")).length).toBe(1);
  });

  test("run() releases the lock on the way out", async () => {
    const { daemon, sources, inhibitor } = only("systemd");
    sources[0]!.outcomes = [{ workload: "bedrock.service", action: defer, busy: true }];
    const controller = new AbortController();

    const running = daemon.run(controller.signal);
    await Bun.sleep(5);
    expect(inhibitor.held).toEqual(["bedrock.service"]);
    controller.abort();
    await running;

    expect(inhibitor.held).toEqual([]);
  });
});

function both() {
  return make("docker", "systemd");
}

function only(name: "docker" | "systemd") {
  return make(name);
}

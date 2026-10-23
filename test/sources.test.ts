import { describe, expect, test } from "bun:test";
import { type AutoReboot, Daemon, DEFAULTS } from "../src/daemon.ts";
import type { Action } from "../src/decide.ts";
import { parseWindow } from "../src/reboot.ts";
import { FakeConntrackSource } from "./fakes/conntrack.ts";
import { FakeBootCheck, FakeRebooter, FakeSource, FakeStatus } from "./fakes/source.ts";

const keep: Action = { kind: "keep", reason: "already on the latest image" };
const defer: Action = { kind: "defer", reason: "busy" };

function make(...names: ("docker" | "systemd")[]) {
  const status = new FakeStatus();
  const conntrack = new FakeConntrackSource();
  const sources = names.map((name) => new FakeSource(name, status));
  const log: string[] = [];
  const check = new FakeBootCheck();
  const rebooter = new FakeRebooter();
  // 04:30 local time unless a test moves it.
  const clock = { now: new Date(2026, 9, 23, 4, 30) };
  const reboot: AutoReboot = { window: parseWindow("03:00-05:00"), label: "03:00-05:00", check, rebooter, now: () => clock.now };
  const instance = new Daemon(sources, conntrack, DEFAULTS, (message) => log.push(message), status, reboot);
  return { daemon: instance, sources, status, conntrack, log, check, rebooter, clock };
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

describe("the status", () => {
  test("says exactly which workloads are busy, and that they are not once they go idle", async () => {
    const { daemon, sources, status } = both();
    sources[0]!.outcomes = [{ workload: "web", action: defer, busy: true }];
    sources[1]!.outcomes = [
      { workload: "bedrock.service", action: defer, busy: true },
      { workload: "hytale.service", action: keep, busy: false, settled: true },
    ];

    await daemon.once();
    expect(status.last!.busy).toEqual(["bedrock.service", "web"]);
    expect(status.last!.workloads).toEqual([
      { workload: "web", source: "docker", busy: true, settled: false },
      { workload: "bedrock.service", source: "systemd", busy: true, settled: false },
      { workload: "hytale.service", source: "systemd", busy: false, settled: true },
    ]);

    sources[0]!.outcomes = [{ workload: "web", action: keep, busy: false }];
    sources[1]!.outcomes = [{ workload: "bedrock.service", action: keep, busy: false }];
    await daemon.once();
    expect(status.last!.busy).toEqual([]);
  });

  test("says a swap is under way before it starts, and stops at the end of the pass", async () => {
    const { daemon, sources, status } = only("systemd");
    sources[0]!.holds = ["bedrock.service"];
    sources[0]!.outcomes = [{ workload: "bedrock.service", action: keep, busy: false }];

    await daemon.once();

    expect(sources[0]!.seenSwapping).toEqual([["bedrock.service"]]);
    expect(status.last!.swapping).toEqual([]);
  });

  test("keeps a workload's last verdict when a pass could not sample it, and calls it unsettled", async () => {
    const { daemon, sources, status } = only("docker");
    sources[0]!.outcomes = [{ workload: "web", action: defer, busy: true }];
    await daemon.once();

    // The registry did not answer: no sample this pass.
    sources[0]!.outcomes = [{ workload: "web", action: { kind: "keep", reason: "registry unreachable" } }];
    await daemon.once();

    expect(status.last!.busy).toEqual(["web"]);
    expect(status.last!.workloads).toEqual([{ workload: "web", source: "docker", busy: true, settled: false }]);
  });

  test("keeps a source's verdicts when the source throws", async () => {
    const { daemon, sources, status } = both();
    sources[1]!.outcomes = [{ workload: "bedrock.service", action: defer, busy: true }];
    await daemon.once();

    sources[1]!.failWith = new Error("bus gone");
    await daemon.once().catch(() => {});

    expect(status.last!.busy).toEqual(["bedrock.service"]);
  });

  test("drops a workload the source no longer reports", async () => {
    const { daemon, sources, status } = only("docker");
    sources[0]!.outcomes = [{ workload: "web", action: defer, busy: true }];
    await daemon.once();

    sources[0]!.outcomes = [];
    await daemon.once();

    expect(status.last!.busy).toEqual([]);
    expect(status.last!.workloads).toEqual([]);
  });

  test("the activity tick moves it between passes", async () => {
    const { daemon, sources, status, conntrack } = both();
    sources[1]!.outcomes = [{ workload: "bedrock.service", action: keep, busy: false }];
    await daemon.once();
    expect(status.last!.busy).toEqual([]);

    sources[1]!.activities = [{ workload: "bedrock.service", busy: true }];
    await daemon.tick();

    expect(status.last!.busy).toEqual(["bedrock.service"]);
    expect(conntrack.calls).toBe(2);
    // The tick asked for activity only; no pass ran.
    expect(sources[1]!.contexts).toHaveLength(1);
  });

  test("a status that cannot be written is warned about once, and the pass goes on", async () => {
    const { daemon, sources, status, log } = only("systemd");
    status.failWith = new Error("read-only file system");
    sources[0]!.outcomes = [{ workload: "bedrock.service", action: defer, busy: true }];

    await daemon.once();
    await daemon.once();

    expect(log.filter((line) => line.includes("could not write the status")).length).toBe(1);
  });

  test("run() writes it, and clears it on the way out", async () => {
    const { daemon, sources, status } = only("systemd");
    sources[0]!.outcomes = [{ workload: "bedrock.service", action: defer, busy: true }];
    const controller = new AbortController();

    const running = daemon.run(controller.signal);
    await Bun.sleep(5);
    expect(status.last!.busy).toEqual(["bedrock.service"]);
    controller.abort();
    await running;

    expect(status.cleared).toBe(true);
  });
});

describe("an automatic reboot", () => {
  /** A systemd source with one unit idle for its whole debounce, and a reboot needed. */
  function ready() {
    const made = only("systemd");
    made.sources[0]!.outcomes = [{ workload: "bedrock.service", action: keep, busy: false, settled: true }];
    made.sources[0]!.activities = [{ workload: "bedrock.service", busy: false }];
    made.check.reason = "kernel changed since boot";
    return made;
  }

  async function pass(daemon: Daemon) {
    await daemon.once();
    await daemon.considerReboot();
  }

  test("goes ahead when needed, inside the window, with everything settled", async () => {
    const { daemon, rebooter, log, status } = ready();
    await pass(daemon);
    expect(rebooter.reboots).toBe(1);
    expect(log).toContain("Automatic reboot (03:00-05:00): needed, kernel changed since boot; rebooting, as nothing is in use");
    expect(status.last!.reboot!.verdict).toBe("rebooting, as nothing is in use");
  });

  test("is asked for once, not again every pass after", async () => {
    const { daemon, rebooter } = ready();
    await pass(daemon);
    await pass(daemon);
    expect(rebooter.reboots).toBe(1);
  });

  test("is not needed when the booted kernel, initrd and modules are what is installed", async () => {
    const { daemon, rebooter, check, log } = ready();
    check.reason = undefined;
    await pass(daemon);
    expect(rebooter.reboots).toBe(0);
    expect(log).toContain("Automatic reboot (03:00-05:00): not needed");
  });

  test("never happens when it cannot tell whether one is needed", async () => {
    const { daemon, rebooter, check, log } = ready();
    check.failWith = new Error("cannot read /nix/var/nix/profiles/system");
    await pass(daemon);
    expect(rebooter.reboots).toBe(0);
    expect(log.join("\n")).toContain("could not tell whether a reboot is needed");
  });

  test("waits outside the window, however idle everything is", async () => {
    const { daemon, rebooter, clock, log, status } = ready();
    clock.now = new Date(2026, 9, 23, 5, 0);
    await pass(daemon);
    expect(rebooter.reboots).toBe(0);
    expect(log).toContain("Automatic reboot (03:00-05:00): needed, kernel changed since boot; waiting for the window");
    expect(status.last!.reboot).toEqual({
      window: "03:00-05:00",
      needed: "kernel changed since boot",
      verdict: "waiting for the window",
    });

    clock.now = new Date(2026, 9, 24, 3, 0);
    await pass(daemon);
    expect(rebooter.reboots).toBe(1);
  });

  test("waits while anything is in use, and goes once it is not", async () => {
    const { daemon, sources, rebooter, log } = ready();
    sources[0]!.outcomes = [{ workload: "bedrock.service", action: defer, busy: true }];
    await pass(daemon);
    expect(rebooter.reboots).toBe(0);
    expect(log).toContain("Automatic reboot (03:00-05:00): needed, kernel changed since boot; waiting: bedrock.service in use");

    sources[0]!.outcomes = [{ workload: "bedrock.service", action: keep, busy: false, settled: true }];
    await pass(daemon);
    expect(rebooter.reboots).toBe(1);
  });

  test("waits for every source: a busy container holds off a reboot for the units", async () => {
    const { daemon, sources, rebooter, check, log } = both();
    check.reason = "kernel changed since boot";
    sources[0]!.outcomes = [{ workload: "web", action: keep, busy: true }];
    sources[1]!.outcomes = [{ workload: "bedrock.service", action: keep, busy: false, settled: true }];
    await pass(daemon);
    expect(rebooter.reboots).toBe(0);
    expect(log.at(-1)).toContain("waiting: web in use");
  });

  test("waits until everything has been idle for its whole debounce", async () => {
    const { daemon, sources, rebooter, log } = ready();
    sources[0]!.outcomes = [{ workload: "bedrock.service", action: defer, busy: false, settled: false }];
    await pass(daemon);
    expect(rebooter.reboots).toBe(0);
    expect(log.at(-1)).toContain("waiting: bedrock.service not idle for long enough yet");
  });

  test("waits for a workload the pass could not sample", async () => {
    const { daemon, sources, rebooter } = ready();
    sources[0]!.outcomes = [{ workload: "bedrock.service", action: { kind: "keep", reason: "systemd unreachable" } }];
    await pass(daemon);
    expect(rebooter.reboots).toBe(0);
  });

  test("waits while a swap is under way", async () => {
    const { daemon, sources, rebooter, log } = ready();
    // Asked from inside the pass, while the swap is held.
    sources[0]!.holds = ["bedrock.service"];
    const original = sources[0]!.once.bind(sources[0]!);
    sources[0]!.once = async (context) => {
      const outcomes = await original(context);
      await daemon.considerReboot();
      return outcomes;
    };
    await daemon.once();
    expect(rebooter.reboots).toBe(0);
    expect(log.join("\n")).toContain("waiting: swapping bedrock.service");
  });

  test("looks once more right before rebooting, and waits for someone who has just joined", async () => {
    const { daemon, sources, rebooter, log } = ready();
    sources[0]!.activities = [{ workload: "bedrock.service", busy: true }];
    await pass(daemon);
    expect(rebooter.reboots).toBe(0);
    expect(log.at(-1)).toContain("waiting: bedrock.service in use");
  });

  test("waits when the last look could not be taken", async () => {
    const { daemon, sources, rebooter } = ready();
    const original = sources[0]!.activity.bind(sources[0]!);
    sources[0]!.activity = async () => { throw new Error("bus gone"); };
    await pass(daemon);
    expect(rebooter.reboots).toBe(0);
    sources[0]!.activity = original;
  });

  test("refused by logind, is asked for again on the next pass", async () => {
    const { daemon, rebooter, log } = ready();
    rebooter.failWith = new Error("Operation denied due to active block inhibitor");
    await pass(daemon);
    expect(rebooter.reboots).toBe(0);
    expect(log.at(-1)).toContain("the reboot was refused");

    rebooter.failWith = undefined;
    await pass(daemon);
    expect(rebooter.reboots).toBe(1);
  });

  test("never happens from a pass that failed", async () => {
    const { daemon, sources, rebooter } = ready();
    const controller = new AbortController();
    sources[0]!.failWith = new Error("bus gone");
    const running = daemon.run(controller.signal);
    await Bun.sleep(5);
    controller.abort();
    await running;
    expect(rebooter.reboots).toBe(0);
  });

  test("run() reboots at the end of a pass", async () => {
    const { daemon, rebooter } = ready();
    const controller = new AbortController();
    const running = daemon.run(controller.signal);
    await Bun.sleep(5);
    controller.abort();
    await running;
    expect(rebooter.reboots).toBe(1);
  });
});

function both() {
  return make("docker", "systemd");
}

function only(name: "docker" | "systemd") {
  return make(name);
}

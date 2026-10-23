import { describe, expect, test } from "bun:test";
import { parseConntrack } from "../src/conntrack.ts";
import type { PassContext } from "../src/source.ts";
import { SystemdSource } from "../src/systemd-source.ts";
import { FakeConntrackSource } from "./fakes/conntrack.ts";
import { FakeNetnsReader } from "./fakes/netns.ts";
import {
  FakeClosure,
  FakeListeners,
  FakeProfiles,
  FakeSystemd,
  FakeUnits,
  MemoryStateStore,
  unitState,
  type Journal,
} from "./fakes/systemd.ts";
import { FakeTcpProbe } from "./fakes/tcp.ts";

const UNIT = "game.service";
const PROFILE = "/nix/var/nix/profiles/tidewaiter/game";
const V1 = "/nix/store/aaaa-game-1";
const V2 = "/nix/store/bbbb-game-2";
const V3 = "/nix/store/cccc-game-3";

/** A player's UDP flow to the game's port, as `conntrack -L` prints it. */
const PLAYER = "udp      17 29 src=10.0.0.5 dst=10.0.0.1 sport=40000 dport=7777 src=10.0.0.1 dst=10.0.0.5 sport=7777 dport=40000 [ASSURED] mark=0 use=1\n";

/**
 * The systemd source over a unit, a profile, sockets and conntrack that exist
 * only in memory: the real reconcile, decide() and swap sequencing, with the
 * far ends replaced. By default `game.service` is up on V1 with 7777/udp bound,
 * and its units file asks for V2 after one idle sample.
 */
function harness(fields: Record<string, unknown> = {}, store = new MemoryStateStore()) {
  const journal: Journal = [];
  const systemd = new FakeSystemd(journal).add(unitState(UNIT));
  const profiles = new FakeProfiles(journal).seed(PROFILE, V1);
  const closure = new FakeClosure(journal);
  const listeners = new FakeListeners().bind(UNIT, 7777);
  const conntrack = new FakeConntrackSource();
  const units = new FakeUnits().declare(UNIT, {
    desired: V2, profile: PROFILE, ports: ["7777/udp"], idleSamples: 1, healthTimeout: 3, ...fields,
  });
  const log: string[] = [];
  let now = 1_000_000;

  const build = (dryRun = false) =>
    new SystemdSource(
      {
        units, systemd, profiles, listeners, conntrack, store, closure,
        netns: new FakeNetnsReader(),
        tcp: new FakeTcpProbe(),
        log: (message) => log.push(message),
        // Every read of the clock moves it on, so a health gate's grace and
        // deadline pass in a handful of polls.
        now: () => (now += 250),
      },
      { dryRun, healthPollMillis: 0 },
    );
  let source = build();

  /**
   * One pass, with this pass's conntrack table (the shared dump), defaulting to
   * what conntrack holds; null is a dump that failed.
   */
  const pass = async (table: string | null = conntrack.text) => {
    const context: PassContext = {
      conntrack: table === null ? undefined : parseConntrack(table),
      hold: async (workload) => { journal.push(`hold ${workload}`); },
    };
    return source.once(context);
  };

  return {
    systemd, profiles, closure, listeners, conntrack, units, store, journal, log, pass,
    get source() { return source; },
    /** A new source over the same state store: Tidewaiter restarted. */
    restartDaemon(dryRun = false) { source = build(dryRun); },
    kinds: async (table?: string | null) => (await pass(table)).map((outcome) => outcome.action.kind),
  };
}

/** A restart onto whatever the profile now points at, which comes up but never binds its port if that is `bad`. */
function neverBinds(h: ReturnType<typeof harness>, bad: string) {
  h.systemd.onRestart = (name, fake) => {
    fake.patch(name, { activeState: "active", invocationId: fake.nextInvocation() });
    h.listeners.clear();
    void h.profiles.current(PROFILE).then((target) => { if (target !== bad) h.listeners.bind(UNIT, 7777); });
    return "done";
  };
}

describe("deciding", () => {
  test("keeps a unit whose profile is already on the desired version", async () => {
    const h = harness({ desired: V1 });
    expect(await h.kinds()).toEqual(["keep"]);
    expect(h.journal).toEqual([]);
  });

  test("defers while a player's flow is on a declared port, then swaps once idle", async () => {
    const h = harness({ idleSamples: 2 });
    h.conntrack.text = PLAYER;
    expect(await h.kinds()).toEqual(["defer"]);
    expect(await h.kinds()).toEqual(["defer"]);

    h.conntrack.text = "";
    expect(await h.kinds()).toEqual(["defer"]); // idle 1/2
    expect(await h.kinds()).toEqual(["update"]);
    expect(await h.profiles.current(PROFILE)).toBe(V2);
  });

  test("a flow to some other port is not a player", async () => {
    const h = harness();
    h.conntrack.text = PLAYER.replaceAll("7777", "8888");
    expect(await h.kinds()).toEqual(["update"]);
  });

  test("defers when someone arrives between the decision and the swap", async () => {
    const h = harness();
    h.conntrack.text = PLAYER; // what the re-sample right before the swap sees

    const [outcome] = await h.pass(""); // the pass's own dump was empty

    expect(outcome!.action.kind).toBe("defer");
    expect(outcome!.action.reason).toContain("arrived just before the swap");
    expect(h.journal).toEqual(["hold game.service"]);
    expect(await h.profiles.current(PROFILE)).toBe(V1);
  });

  test("a unit with no declared ports cannot be read, so it is busy - and says so once", async () => {
    const h = harness({ ports: [] });
    expect(await h.kinds()).toEqual(["defer"]);
    expect(await h.kinds()).toEqual(["defer"]);
    expect(h.log.filter((line) => line.includes("declares no ports")).length).toBe(1);
  });

  test("a conntrack dump that failed reads as busy, never as an empty table", async () => {
    const h = harness();
    const [outcome] = await h.pass(null);
    expect(outcome!.action.kind).toBe("defer");
    expect(outcome!.busy).toBe(true);
  });

  test("detector none swaps without looking", async () => {
    const h = harness({ detector: "none", ports: [] });
    h.conntrack.text = PLAYER;
    expect(await h.kinds()).toEqual(["update"]);
  });

  test("a unit still activating is left to settle", async () => {
    const h = harness();
    h.systemd.patch(UNIT, { activeState: "activating", subState: "start" });
    const [outcome] = await h.pass();
    expect(outcome!.action.kind).toBe("defer");
    expect(outcome!.action.reason).toContain("activating");
  });

  test("dry run decides but touches nothing", async () => {
    const h = harness();
    h.restartDaemon(true);
    expect(await h.kinds()).toEqual(["update"]);
    expect(h.journal).toEqual([]);
    expect(h.log.join("\n")).toContain("dry run");
  });

  test("speaks of versions and units, not images and containers", async () => {
    const h = harness({ idleSamples: 3 });
    const [outcome] = await h.pass();
    expect(h.log.join("\n")).not.toContain("container");
    expect(outcome!.action.reason).toContain("idle 1/3");
  });
});

describe("swapping", () => {
  test("holds the lock, sets the profile, restarts, gates and prunes - in that order", async () => {
    const h = harness();
    const [outcome] = await h.pass();

    expect(outcome!.action.kind).toBe("update");
    expect(h.journal).toEqual(["hold game.service", `set ${V2}`, "restart game.service", "prune +2"]);
    expect(await h.profiles.current(PROFILE)).toBe(V2);
    expect(h.log.join("\n")).toContain(`updated to ${V2} and it is healthy`);
    expect(h.store.memories[UNIT]).toEqual({ good: 2 });
  });

  test("keeps keepGenerations previous generations", async () => {
    const h = harness({ keepGenerations: 0, desired: V3 });
    h.profiles.seed(PROFILE, V1, V2);
    await h.pass();
    expect(h.profiles.generationsOf(PROFILE)).toEqual([3]);
  });

  test("a restart job that fails rolls the profile back to the generation it left, and pins", async () => {
    const h = harness();
    h.systemd.onRestart = (name, fake) => {
      // The new version never says READY=1; the old one, back on generation 1, does.
      if (h.profiles.profiles.get(PROFILE)!.current === 2) {
        fake.patch(name, { activeState: "failed", result: "timeout" });
        return "failed";
      }
      fake.patch(name, { activeState: "active", invocationId: fake.nextInvocation() });
      return "done";
    };

    await h.pass();

    expect(h.journal).toEqual([
      "hold game.service", `set ${V2}`, "restart game.service", "switch 1", "delete 2", "restart game.service",
    ]);
    expect(await h.profiles.current(PROFILE)).toBe(V1);
    expect(h.profiles.generationsOf(PROFILE)).toEqual([1]);
    expect(h.log.join("\n")).toContain("the restart job ended 'failed'");
    expect(h.log.join("\n")).toContain(`rolled back to ${V1} and pinned ${V2} out`);
    expect(h.store.memories[UNIT]?.pinned).toBe(V2);

    // Pinned: no more restarts while V2 is what is desired.
    expect(await h.kinds()).toEqual(["pinned"]);
    expect(h.journal.filter((line) => line.startsWith("restart"))).toHaveLength(2);
  });

  test("a version that comes up but never binds its port fails uptime and rolls back", async () => {
    const h = harness();
    neverBinds(h, V2);

    await h.pass();

    expect(h.log.join("\n")).toContain("no published port is bound");
    expect(await h.profiles.current(PROFILE)).toBe(V1);
    expect(h.store.memories[UNIT]?.pinned).toBe(V2);
  });

  test("a port bound by some other unit does not count", async () => {
    const h = harness();
    neverBinds(h, V2);
    h.systemd.onRestart = ((original) => (name, fake) => {
      const result = original!(name, fake);
      h.listeners.bind("other.service", 7777);
      return result;
    })(h.systemd.onRestart);

    await h.pass();

    expect(h.store.memories[UNIT]?.pinned).toBe(V2);
  });

  test("the pin survives a restart of Tidewaiter", async () => {
    const h = harness();
    neverBinds(h, V2);
    await h.pass();

    h.restartDaemon();
    expect(await h.kinds()).toEqual(["pinned"]);
    expect(h.journal.filter((line) => line.startsWith("set"))).toHaveLength(1);
  });

  test("a new desired version clears the pin", async () => {
    const h = harness();
    neverBinds(h, V2);
    await h.pass();

    h.units.declare(UNIT, { desired: V3, profile: PROFILE, ports: ["7777/udp"], idleSamples: 1, healthTimeout: 3 });
    expect(await h.kinds()).toEqual(["update"]);
    expect(await h.profiles.current(PROFILE)).toBe(V3);
    expect(h.store.memories[UNIT]?.pinned).toBeUndefined();
  });

  test("a nix-env that fails is undone and retried, and pinned after three in a row", async () => {
    const h = harness();
    for (let attempt = 1; attempt <= 3; attempt++) {
      h.profiles.setThrows = new Error("cannot connect to the Nix daemon");
      expect(await h.kinds()).toEqual(["update"]);
    }

    expect(h.journal.filter((line) => line.startsWith("restart"))).toEqual([]);
    expect(await h.profiles.current(PROFILE)).toBe(V1);
    expect(h.store.memories[UNIT]).toMatchObject({ pinned: V2, swapFailures: 3 });
    expect(await h.kinds()).toEqual(["pinned"]);
  });

  test("a restart that could not be queued puts the profile back without restarting", async () => {
    const h = harness();
    h.systemd.restartThrows = new Error("Connection reset by peer");

    await h.pass();

    expect(h.journal).toEqual(["hold game.service", `set ${V2}`, "restart game.service", "switch 1", "delete 2"]);
    expect(await h.profiles.current(PROFILE)).toBe(V1);
    expect(h.store.memories[UNIT]).toMatchObject({ failing: V2, swapFailures: 1 });
  });

  test("with bindClosure, the closure is bound and systemd reloaded before each restart", async () => {
    const h = harness({ bindClosure: true });
    neverBinds(h, V2);

    await h.pass();

    expect(h.journal).toEqual([
      "hold game.service",
      `bind ${V2}`, "daemon-reload", `set ${V2}`, "restart game.service",
      "switch 1", "delete 2", `bind ${V1}`, "daemon-reload", "restart game.service",
    ]);
  });

  test("a rollback that is itself unhealthy asks for a person rather than looping", async () => {
    const h = harness();
    h.systemd.onRestart = (name, fake) => {
      fake.patch(name, { activeState: "failed" });
      return "failed";
    };

    await h.pass();
    await h.pass();

    expect(h.log.join("\n")).toContain("manual attention needed");
    expect(h.journal.filter((line) => line.startsWith("restart"))).toHaveLength(2);
  });
});

describe("stopped units", () => {
  test("have their profile moved for the next start, with no restart", async () => {
    const h = harness();
    h.systemd.patch(UNIT, { activeState: "inactive", subState: "dead", mainPid: 0, controlGroup: "" });
    h.conntrack.text = PLAYER;

    const [outcome] = await h.pass();

    expect(outcome!.action.kind).toBe("update");
    expect(outcome!.busy).toBe(false);
    expect(h.journal).toEqual([`set ${V2}`]);
    expect(h.log.join("\n")).toContain("updated the profile of a stopped unit");
  });

  test("are not moved onto a pinned version", async () => {
    const store = new MemoryStateStore();
    store.memories = { [UNIT]: { pinned: V2 } };
    const h = harness({}, store);
    h.systemd.patch(UNIT, { activeState: "failed", mainPid: 0, controlGroup: "" });

    expect(await h.kinds()).toEqual(["pinned"]);
    expect(h.journal).toEqual([]);
  });

  test("a unit systemd does not know is left alone, said once", async () => {
    const h = harness();
    h.systemd.units.clear();
    expect(await h.kinds()).toEqual(["keep"]);
    expect(await h.kinds()).toEqual(["keep"]);
    expect(h.journal).toEqual([]);
    expect(h.log.filter((line) => line.includes("does not know")).length).toBe(1);
  });
});

describe("crash recovery", () => {
  test("a unit failed on a generation newer than the last good one is rolled back and pinned", async () => {
    const store = new MemoryStateStore();
    store.memories = { [UNIT]: { good: 1 } };
    const h = harness({}, store);
    h.profiles.seed(PROFILE, V1, V2); // Tidewaiter died after `set V2`
    h.systemd.patch(UNIT, { activeState: "failed", result: "exit-code" });

    const [outcome] = await h.pass();

    expect(outcome!.action.kind).toBe("pinned");
    expect(h.journal).toEqual(["hold game.service", "switch 1", "delete 2", "restart game.service"]);
    expect(await h.profiles.current(PROFILE)).toBe(V1);
    expect(h.store.memories[UNIT]?.pinned).toBe(V2);
  });

  test("a unit up on a generation with no record is adopted as the baseline", async () => {
    const store = new MemoryStateStore();
    store.memories = { [UNIT]: { good: 1 } };
    const h = harness({ desired: V2 }, store);
    h.profiles.seed(PROFILE, V1, V2);

    expect(await h.kinds()).toEqual(["keep"]);
    expect(h.store.memories[UNIT]).toEqual({ good: 2 });
    expect(h.log.join("\n")).toContain("adopting generation 2");
  });
});

describe("a swap Tidewaiter died in the middle of", () => {
  /** good=1 on V1, and a record of a swap to V2 started from it while invocation 0001 ran. */
  function interrupted(fields: Record<string, unknown> = {}) {
    const store = new MemoryStateStore();
    store.memories = { [UNIT]: { good: 1, swapping: { from: 1, fromTarget: V1, to: V2, invocationId: "0001" } } };
    return harness(fields, store);
  }

  test("is recorded, durably, before anything changes, and cleared once it commits", async () => {
    const h = harness();
    const seen: unknown[] = [];
    const set = h.profiles.set.bind(h.profiles);
    h.profiles.set = async (profile, path) => {
      seen.push(h.store.memories[UNIT]?.swapping);
      return set(profile, path);
    };

    expect(await h.kinds()).toEqual(["update"]);

    expect(seen).toEqual([{ from: 1, fromTarget: V1, to: V2, invocationId: "0001" }]);
    expect(h.store.memories[UNIT]).toEqual({ good: 2 });
  });

  test("is not started when it cannot be recorded", async () => {
    const h = harness();
    h.store.save = async () => { throw new Error("read-only file system"); };

    expect(await h.kinds()).toEqual(["defer"]);
    expect(h.journal).toEqual(["hold game.service"]);
    expect(await h.profiles.current(PROFILE)).toBe(V1);
  });

  test("after `set` but before the restart: the profile is put back, not adopted, and the swap redone when idle", async () => {
    const h = interrupted();
    h.profiles.seed(PROFILE, V1, V2);

    expect(await h.kinds()).toEqual(["update"]);

    expect(h.journal).toEqual([
      "switch 1", "delete 2", "hold game.service", `set ${V2}`, "restart game.service", "prune +2",
    ]);
    expect(h.store.memories[UNIT]).toEqual({ good: 2 });
    expect(h.log.join("\n")).not.toContain("adopting");
  });

  test("after `set` but before the restart, with a player on: put back without a restart, and waits", async () => {
    const h = interrupted();
    h.profiles.seed(PROFILE, V1, V2);
    h.conntrack.text = PLAYER;

    expect(await h.kinds()).toEqual(["defer"]);

    expect(h.journal).toEqual(["switch 1", "delete 2"]);
    expect(await h.profiles.current(PROFILE)).toBe(V1);
    expect(h.store.memories[UNIT]).toEqual({ good: 1 });
  });

  test("after the closure was bound but before `set`: the drop-in goes back to what the profile names", async () => {
    const h = interrupted({ bindClosure: true });
    h.conntrack.text = PLAYER;

    expect(await h.kinds()).toEqual(["defer"]);

    expect(h.journal).toEqual([`bind ${V1}`, "daemon-reload"]);
    expect(h.store.memories[UNIT]).toEqual({ good: 1 });
  });

  test("after the restart, before the gate: gated now, and committed when healthy", async () => {
    const h = interrupted();
    h.profiles.seed(PROFILE, V1, V2);
    h.systemd.patch(UNIT, { invocationId: "0002" });

    expect(await h.kinds()).toEqual(["keep"]);

    expect(h.journal).toEqual(["hold game.service", "prune +2"]);
    expect(h.store.memories[UNIT]).toEqual({ good: 2 });
  });

  test("after the restart, before the gate: gated now, and rolled back and pinned when unhealthy", async () => {
    const h = interrupted();
    h.profiles.seed(PROFILE, V1, V2);
    h.systemd.patch(UNIT, { invocationId: "0002" });
    h.listeners.clear(); // V2 came up but never bound its port
    neverBinds(h, V2);

    const [outcome] = await h.pass();

    expect(outcome!.action.kind).toBe("pinned");
    expect(h.journal).toEqual(["hold game.service", "switch 1", "delete 2", "restart game.service"]);
    expect(await h.profiles.current(PROFILE)).toBe(V1);
    expect(h.store.memories[UNIT]).toEqual({ good: 1, pinned: V2 });
  });

  test("after the restart, failed: rolled back and pinned", async () => {
    const h = interrupted();
    h.profiles.seed(PROFILE, V1, V2);
    h.systemd.patch(UNIT, { invocationId: "0002", activeState: "failed", result: "exit-code" });

    expect(await h.kinds()).toEqual(["pinned"]);

    expect(await h.profiles.current(PROFILE)).toBe(V1);
    expect(h.store.memories[UNIT]).toEqual({ good: 1, pinned: V2 });
  });

  test("a rollback's pin is saved straight away, not at the end of the pass", async () => {
    const h = harness();
    neverBinds(h, V2);
    h.units.declare("other.service", { desired: V1, profile: `${PROFILE}-other`, ports: ["7778/udp"] });
    h.systemd.add(unitState("other.service"));
    h.profiles.seed(`${PROFILE}-other`, V1);
    // What is on disk when the pass moves on to the next unit: the moment a crash would lose it.
    let onDisk: unknown;
    const read = h.systemd.unit.bind(h.systemd);
    h.systemd.unit = async (name) => {
      if (name === "other.service" && onDisk === undefined) onDisk = structuredClone(h.store.memories[UNIT]);
      return read(name);
    };

    await h.pass();

    expect(onDisk).toEqual({ good: 1, pinned: V2 });
  });
});

describe("activity and list", () => {
  test("activity reads players, without moving the idle streak", async () => {
    const h = harness({ idleSamples: 2 });
    expect(await h.source.activity(parseConntrack(PLAYER))).toEqual([{ workload: UNIT, busy: true }]);
    expect(await h.source.activity([])).toEqual([{ workload: UNIT, busy: false }]);
    // Two idle ticks did not count as samples: the first pass is still 1/2.
    expect(await h.kinds()).toEqual(["defer"]);
  });

  test("a stopped unit is not busy, and one systemd cannot describe is", async () => {
    const h = harness();
    h.systemd.patch(UNIT, { activeState: "inactive" });
    expect(await h.source.activity(parseConntrack(PLAYER))).toEqual([{ workload: UNIT, busy: false }]);
    h.systemd.failWith = new Error("bus gone");
    expect(await h.source.activity([])).toEqual([{ workload: UNIT, busy: true }]);
  });

  test("list shows the profile, the status and the policy", async () => {
    const h = harness();
    const [line] = await h.source.list();
    expect(line).toBe(
      `${UNIT}  ${PROFILE} -> ${V1}  [update waiting]  desired=${V2} ` +
        "detector=conntrack health=systemd,port-connect,port-bound,uptime idle-samples=1",
    );
  });

  test("a units file that cannot be read is warned about once, and the others carry on", async () => {
    const h = harness();
    h.units.files.set("broken.json", "{ not json");
    expect(await h.kinds()).toEqual(["update"]);
    await h.pass();
    expect(h.log.filter((line) => line.startsWith("broken.json")).length).toBe(1);
  });
});

import { afterEach, describe, expect, test } from "bun:test";
import { existsSync } from "node:fs";
import { DbusError, MessageType, SYSTEM_BUS, Variant, type Message } from "../src/dbus.ts";
import { DbusSystemdClient, toJobRemoved, toUnitState, unitPath } from "../src/systemd.ts";
import { FakeBus } from "./fakes/bus.ts";

const MANAGER = "org.freedesktop.systemd1.Manager";

/** systemd as the fake bus plays it: one unit, and jobs numbered from 7. */
function systemd(bus: FakeBus, overrides: { unit?: Record<string, Variant>; service?: Record<string, Variant> } = {}) {
  bus.answer = (call) => {
    switch (call.member) {
      case "AddMatch":
      case "Subscribe":
      case "Reload":
        return {};
      case "LoadUnit":
        return { signature: "o", body: [unitPath(String(call.body?.[0]))] };
      case "GetAll":
        return call.body?.[0] === "org.freedesktop.systemd1.Unit"
          ? {
              signature: "a{sv}",
              body: [{
                LoadState: new Variant("s", "loaded"),
                ActiveState: new Variant("s", "active"),
                SubState: new Variant("s", "running"),
                InvocationID: new Variant("ay", [0xde, 0xad, 0xbe, 0xef, 0, 1]),
                ...overrides.unit,
              }],
            }
          : {
              signature: "a{sv}",
              body: [{
                Result: new Variant("s", "success"),
                NRestarts: new Variant("u", 2),
                MainPID: new Variant("u", 4242),
                ControlGroup: new Variant("s", "/system.slice/game.service"),
                ...overrides.service,
              }],
            };
      case "RestartUnit":
        return { signature: "o", body: ["/org/freedesktop/systemd1/job/7"] };
      default:
        return new DbusError("org.freedesktop.DBus.Error.UnknownMethod", String(call.member));
    }
  };
}

function jobRemoved(bus: FakeBus, job: string, result: string): void {
  bus.signal({
    path: "/org/freedesktop/systemd1",
    interface: MANAGER,
    member: "JobRemoved",
    signature: "uoss",
    body: [Number(job.split("/").pop()), job, "game.service", result],
  });
}

describe("DbusSystemdClient", () => {
  let bus: FakeBus;
  let client: DbusSystemdClient | undefined;
  afterEach(async () => {
    client?.close();
    client = undefined;
    await bus?.stop();
  });

  test("reads a unit's state from its Unit and Service properties", async () => {
    bus = await FakeBus.start();
    systemd(bus);
    client = new DbusSystemdClient(bus.path);
    expect(await client.unit("game.service")).toEqual({
      name: "game.service",
      loadState: "loaded",
      activeState: "active",
      subState: "running",
      result: "success",
      nRestarts: 2,
      mainPid: 4242,
      controlGroup: "/system.slice/game.service",
      invocationId: "deadbeef0001",
    });
    // LoadUnit, so a unit nothing has started yet is still found.
    expect(bus.calls.find((call) => call.member === "LoadUnit")?.body).toEqual(["game.service"]);
  });

  test("subscribes to job ends before it asks for anything", async () => {
    bus = await FakeBus.start();
    systemd(bus);
    client = new DbusSystemdClient(bus.path);
    await client.unit("game.service");
    const members = bus.calls.map((call) => call.member);
    expect(members.slice(0, 3)).toEqual(["Hello", "AddMatch", "Subscribe"]);
    expect(String(bus.calls[1]!.body?.[0])).toContain("member='JobRemoved'");
  });

  test("a restart waits for its own job to be removed, and returns its result", async () => {
    bus = await FakeBus.start();
    systemd(bus);
    client = new DbusSystemdClient(bus.path);
    await client.unit("game.service");

    let settled = false;
    const restart = client.restart("game.service").then((result) => {
      settled = true;
      return result;
    });
    await Bun.sleep(30);
    // Someone else's job ending says nothing about ours.
    jobRemoved(bus, "/org/freedesktop/systemd1/job/6", "done");
    await Bun.sleep(30);
    expect(settled).toBe(false);
    jobRemoved(bus, "/org/freedesktop/systemd1/job/7", "failed");
    expect(await restart).toBe("failed");
    expect(bus.calls.find((call) => call.member === "RestartUnit")?.body).toEqual(["game.service", "replace"]);
  });

  test("a job that ends before the reply to RestartUnit is still heard", async () => {
    bus = await FakeBus.start();
    systemd(bus);
    const answer = bus.answer;
    bus.answer = (call) => {
      if (call.member === "RestartUnit") jobRemoved(bus, "/org/freedesktop/systemd1/job/7", "done");
      return answer(call);
    };
    client = new DbusSystemdClient(bus.path);
    expect(await client.restart("game.service")).toBe("done");
  });

  test("a restart fails when the bus drops while waiting, rather than waiting forever", async () => {
    bus = await FakeBus.start();
    systemd(bus);
    client = new DbusSystemdClient(bus.path);
    const restart = client.restart("game.service").catch((error: Error) => error);
    await Bun.sleep(30);
    bus.disconnect();
    expect(((await restart) as Error).message).toMatch(/lost the bus/);
    // And the next call opens a new connection.
    expect((await client.unit("game.service")).activeState).toBe("active");
  });

  test("daemon-reload is the Manager's Reload", async () => {
    bus = await FakeBus.start();
    systemd(bus);
    client = new DbusSystemdClient(bus.path);
    await client.daemonReload();
    expect(bus.calls.at(-1)).toMatchObject({ member: "Reload", interface: MANAGER });
  });
});

describe("toJobRemoved", () => {
  const signal = (body: unknown[], member = "JobRemoved"): Message => ({
    type: MessageType.Signal, serial: 1, interface: MANAGER, member, signature: "uoss", body,
  });

  test("reads the job path, unit and result", () => {
    expect(toJobRemoved(signal([7, "/j/7", "game.service", "timeout"]))).toEqual({
      job: "/j/7", unit: "game.service", result: "timeout",
    });
  });

  test("reads a result it does not know as failed", () => {
    expect(toJobRemoved(signal([7, "/j/7", "game.service", "frobnicated"]))?.result).toBe("failed");
  });

  test("ignores every other signal", () => {
    expect(toJobRemoved(signal([7, "/j/7", "game.service", "done"], "JobNew"))).toBeUndefined();
  });
});

describe("toUnitState", () => {
  test("a unit that is not running has no pid, cgroup or result to speak of", () => {
    expect(toUnitState("gone.service", { LoadState: "not-found", ActiveState: "inactive", SubState: "dead" }, {})).toEqual({
      name: "gone.service", loadState: "not-found", activeState: "inactive", subState: "dead",
      result: "", nRestarts: 0, mainPid: 0, controlGroup: "", invocationId: "",
    });
  });
});

describe("unitPath", () => {
  test("escapes as systemd does", () => {
    expect(unitPath("chandlery-bedrock.service")).toBe("/org/freedesktop/systemd1/unit/chandlery_2dbedrock_2eservice");
    expect(unitPath("2fast.service")).toBe("/org/freedesktop/systemd1/unit/_32fast_2eservice");
  });
});

/**
 * Against the host's own systemd, where there is one: reading a unit needs no
 * privilege. Skipped in the Nix sandbox and anywhere without a system bus.
 */
describe.skipIf(!existsSync(SYSTEM_BUS))("DbusSystemdClient against this host's systemd", () => {
  test("reads systemd-journald, which every systemd host runs", async () => {
    const client = new DbusSystemdClient();
    try {
      const state = await client.unit("systemd-journald.service");
      expect(state.loadState).toBe("loaded");
      expect(state.activeState).toBe("active");
      expect(state.mainPid).toBeGreaterThan(0);
      expect(state.controlGroup).toBe("/system.slice/systemd-journald.service");
      expect(state.invocationId).toMatch(/^[0-9a-f]{32}$/);
    } finally {
      client.close();
    }
  });

  test("calls a unit with no file not-found", async () => {
    const client = new DbusSystemdClient();
    try {
      expect((await client.unit("tidewaiter-no-such-unit.service")).loadState).toBe("not-found");
    } finally {
      client.close();
    }
  });
});

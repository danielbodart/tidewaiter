import { afterEach, describe, expect, test } from "bun:test";
import { mkdir, mkdtemp, rm, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DbusError } from "../src/dbus.ts";
import { decideReboot, inWindow, LogindRebooter, NixosBootCheck, parseWindow, type RebootInputs } from "../src/reboot.ts";
import { FakeBus } from "./fakes/bus.ts";

/** A local time on some day. */
const at = (hours: number, minutes = 0) => new Date(2026, 9, 23, hours, minutes);

describe("parseWindow", () => {
  test("reads HH:MM-HH:MM as minutes since midnight", () => {
    expect(parseWindow("03:00-05:00")).toEqual({ start: 180, end: 300 });
    expect(parseWindow("23:30-01:15")).toEqual({ start: 1410, end: 75 });
    expect(parseWindow("00:00-24:00")).toEqual({ start: 0, end: 1440 });
  });

  test("refuses what is not a window", () => {
    for (const text of ["3:00-5:00", "03:00", "03:00-05:00-06:00", "25:00-01:00", "03:60-04:00", "24:00-01:00", "", "late"]) {
      expect(() => parseWindow(text)).toThrow(/not a window/);
    }
  });

  test("refuses an empty window, rather than guessing it means all day", () => {
    expect(() => parseWindow("03:00-03:00")).toThrow(/empty window.*00:00-24:00/);
  });
});

describe("inWindow", () => {
  test("from the start, up to but not including the end", () => {
    const window = parseWindow("03:00-05:00");
    expect(inWindow(window, at(2, 59))).toBe(false);
    expect(inWindow(window, at(3, 0))).toBe(true);
    expect(inWindow(window, at(4, 59))).toBe(true);
    expect(inWindow(window, at(5, 0))).toBe(false);
    expect(inWindow(window, at(15, 0))).toBe(false);
  });

  test("a window that crosses midnight is the end of one day and the start of the next", () => {
    const window = parseWindow("23:00-01:00");
    expect(inWindow(window, at(22, 59))).toBe(false);
    expect(inWindow(window, at(23, 0))).toBe(true);
    expect(inWindow(window, at(0, 0))).toBe(true);
    expect(inWindow(window, at(0, 59))).toBe(true);
    expect(inWindow(window, at(1, 0))).toBe(false);
    expect(inWindow(window, at(12, 0))).toBe(false);
  });

  test("00:00-24:00 is the whole day", () => {
    const window = parseWindow("00:00-24:00");
    expect(inWindow(window, at(0, 0))).toBe(true);
    expect(inWindow(window, at(23, 59))).toBe(true);
  });
});

describe("NixosBootCheck", () => {
  let dir: string;
  afterEach(async () => {
    await rm(dir, { recursive: true, force: true });
  });

  /**
   * Two systems, as the store lays them out: directories of links into
   * "store" paths. `kernel` names the installed system's kernel.
   */
  async function systems(kernel = "linux-1", initrd = true) {
    dir = await mkdtemp(join(tmpdir(), "tidewaiter-boot-"));
    for (const name of ["linux-1", "linux-2", "initrd-1", "modules-1"]) await writeFile(join(dir, name), name);
    const system = async (path: string, kernelName: string) => {
      await mkdir(path);
      await symlink(join(dir, kernelName), join(path, "kernel"));
      if (initrd) await symlink(join(dir, "initrd-1"), join(path, "initrd"));
      await symlink(join(dir, "modules-1"), join(path, "kernel-modules"));
    };
    await system(join(dir, "system-1"), "linux-1");
    await system(join(dir, "system-2"), kernel);
    // /run/booted-system and the profile are themselves links to a system.
    await symlink(join(dir, "system-1"), join(dir, "booted"));
    await symlink(join(dir, "system-2"), join(dir, "current"));
    return new NixosBootCheck(join(dir, "booted"), join(dir, "current"));
  }

  test("nothing needed when the installed system boots the same kernel, initrd and modules", async () => {
    expect(await (await systems()).needed()).toBeUndefined();
  });

  test("needed when the kernel changed, saying what changed", async () => {
    expect(await (await systems("linux-2")).needed()).toBe("kernel changed since boot");
  });

  test("a system with no initrd at all on either side is not a change", async () => {
    expect(await (await systems("linux-1", false)).needed()).toBeUndefined();
  });

  test("cannot tell when the installed system is not there, rather than calling everything changed", async () => {
    const check = await systems();
    await rm(join(dir, "current"));
    await expect(check.needed()).rejects.toThrow(/cannot read .*current/);
  });
});

describe("LogindRebooter", () => {
  let bus: FakeBus;
  afterEach(async () => {
    await bus?.stop();
  });

  test("asks logind to reboot, not interactively", async () => {
    bus = await FakeBus.start();
    await new LogindRebooter(bus.path).reboot();
    const call = bus.calls.find((message) => message.member === "Reboot")!;
    expect(call.destination).toBe("org.freedesktop.login1");
    expect(call.path).toBe("/org/freedesktop/login1");
    expect(call.interface).toBe("org.freedesktop.login1.Manager");
    expect(call.signature).toBe("b");
    expect(call.body).toEqual([false]);
  });

  test("a refusal is an error for the daemon to log", async () => {
    bus = await FakeBus.start();
    bus.answer = (call) => call.member === "Reboot"
      ? new DbusError("org.freedesktop.login1.OperationInProgress", "Operation denied due to active block inhibitor")
      : {};
    await expect(new LogindRebooter(bus.path).reboot()).rejects.toThrow(/block inhibitor/);
  });
});

describe("decideReboot", () => {
  const ready: RebootInputs = { needed: "kernel changed since boot", inWindow: true, busy: [], unsettled: [], swapping: [] };

  test("reboots when needed, in the window, with everything settled and nothing swapping", () => {
    expect(decideReboot(ready)).toEqual({ kind: "reboot" });
  });

  test("not needed outranks everything", () => {
    expect(decideReboot({ ...ready, needed: undefined, busy: ["web"] })).toEqual({ kind: "not-needed" });
  });

  test("waits, and says for what: the window, then a swap, then players, then the debounce", () => {
    expect(decideReboot({ ...ready, inWindow: false, busy: ["web"] })).toEqual({ kind: "wait", reason: "waiting for the window" });
    expect(decideReboot({ ...ready, swapping: ["a.service"], busy: ["web"] }))
      .toEqual({ kind: "wait", reason: "waiting: swapping a.service" });
    expect(decideReboot({ ...ready, busy: ["web", "a.service"], unsettled: ["b"] }))
      .toEqual({ kind: "wait", reason: "waiting: a.service, web in use" });
    expect(decideReboot({ ...ready, unsettled: ["b"] }))
      .toEqual({ kind: "wait", reason: "waiting: b not idle for long enough yet" });
  });
});

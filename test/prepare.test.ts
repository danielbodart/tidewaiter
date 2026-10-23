import { describe, expect, test } from "bun:test";
import { prepare } from "../src/prepare.ts";
import { FakeClosure, FakeProfiles, FakeSystemd, FakeUnits, type Journal } from "./fakes/systemd.ts";

const V1 = "/nix/store/aaaa-game-1";
const V2 = "/nix/store/bbbb-game-2";
const PROFILE = "/nix/var/nix/profiles/tidewaiter/game";

function world() {
  const journal: Journal = [];
  const logs: string[] = [];
  const deps = {
    units: new FakeUnits(),
    profiles: new FakeProfiles(journal),
    closure: new FakeClosure(journal),
    systemd: new FakeSystemd(journal),
    log: (message: string) => logs.push(message),
  };
  return { deps, journal, logs };
}

describe("prepare", () => {
  test("creates a missing profile on the desired version", async () => {
    const { deps, journal } = world();
    deps.units.declare("game.service", { desired: V1, profile: PROFILE });
    const result = await prepare(deps);
    expect(result).toEqual({ seeded: ["game.service"], bound: [], failed: [] });
    expect(await deps.profiles.current(PROFILE)).toBe(V1);
    // Nothing to reload when no drop-in was written.
    expect(journal).toEqual([`set ${V1}`]);
  });

  test("never moves a profile that exists: that waits for the unit to be idle", async () => {
    const { deps, journal } = world();
    deps.profiles.seed(PROFILE, V1);
    deps.units.declare("game.service", { desired: V2, profile: PROFILE });
    expect((await prepare(deps)).seeded).toEqual([]);
    expect(await deps.profiles.current(PROFILE)).toBe(V1);
    expect(journal).toEqual([]);
  });

  test("binds the closure of what the profile points at now, not of desired, then reloads once", async () => {
    const { deps, journal } = world();
    deps.profiles.seed(PROFILE, V1);
    deps.units.declare("game.service", { desired: V2, profile: PROFILE, bindClosure: true });
    deps.units.declare("other.service", { desired: V2, profile: `${PROFILE}-other`, bindClosure: true });
    const result = await prepare(deps);
    expect(result.bound).toEqual(["game.service", "other.service"]);
    expect(journal).toEqual([`bind ${V1}`, `set ${V2}`, `bind ${V2}`, "daemon-reload"]);
  });

  test("one unit failing does not stop the next, and is reported", async () => {
    const { deps, logs } = world();
    deps.profiles.setThrows = new Error("cannot connect to the Nix daemon");
    deps.units.declare("a.service", { desired: V1, profile: `${PROFILE}-a` });
    deps.units.declare("b.service", { desired: V1, profile: `${PROFILE}-b` });
    const result = await prepare(deps);
    expect(result.failed).toEqual(["a.service"]);
    expect(result.seeded).toEqual(["b.service"]);
    expect(logs.some((line) => line.includes("a.service: could not prepare it (cannot connect"))).toBe(true);
  });

  test("a reload that fails fails every unit it was for", async () => {
    const { deps } = world();
    deps.systemd.daemonReload = async () => {
      throw new Error("no bus");
    };
    deps.units.declare("game.service", { desired: V1, profile: PROFILE, bindClosure: true });
    expect((await prepare(deps)).failed).toEqual(["game.service"]);
  });

  test("skips a file it cannot use, with its warning", async () => {
    const { deps, logs } = world();
    deps.units.files.set("broken.json", "{");
    expect(await prepare(deps)).toEqual({ seeded: [], bound: [], failed: [] });
    expect(logs[0]).toContain("broken.json: not valid JSON");
  });
});

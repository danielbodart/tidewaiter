import { afterEach, describe, expect, test } from "bun:test";
import { chmod, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { CLOSURE_DROP_IN, closureDropIn, dropInProfile, NixClosureBinder } from "../src/closure.ts";
import { FileStateStore } from "../src/state-store.ts";
import { systemdVerdict, type JobResult } from "../src/systemd.ts";
import { unitState } from "./fakes/systemd.ts";

describe("systemdVerdict", () => {
  const baseline = unitState("game.service", { invocationId: "aa", nRestarts: 0 });

  test("passes a unit active on the invocation the job started", () => {
    expect(systemdVerdict("done", baseline, baseline)).toEqual({ outcome: "pass" });
    expect(systemdVerdict("done", baseline, { ...baseline, activeState: "reloading" }).outcome).toBe("pass");
  });

  test("waits while it is still activating", () => {
    expect(systemdVerdict("done", baseline, { ...baseline, activeState: "activating" }).outcome).toBe("inconclusive");
  });

  test("fails any job that did not end done", () => {
    for (const job of ["canceled", "timeout", "failed", "dependency", "skipped"] as JobResult[]) {
      expect(systemdVerdict(job, baseline, baseline)).toEqual({ outcome: "fail", reason: `the restart job ended '${job}'` });
    }
  });

  test("fails a unit that has stopped, with systemd's reason", () => {
    expect(systemdVerdict("done", baseline, { ...baseline, activeState: "failed", result: "exit-code" })).toEqual({
      outcome: "fail", reason: "the unit is failed (exit-code)",
    });
    expect(systemdVerdict("done", baseline, { ...baseline, activeState: "inactive" }).outcome).toBe("fail");
    expect(systemdVerdict("done", baseline, { ...baseline, activeState: "deactivating" }).outcome).toBe("fail");
  });

  test("fails a unit Restart= quietly brought back: a new invocation, or more restarts", () => {
    expect(systemdVerdict("done", baseline, { ...baseline, invocationId: "bb" }).outcome).toBe("fail");
    expect(systemdVerdict("done", baseline, { ...baseline, nRestarts: 1 }).reason).toContain("crashed");
  });
});

describe("closureDropIn", () => {
  test("binds the profile's directory and the closure, read-only, sorted and once each", () => {
    expect(closureDropIn("/nix/var/nix/profiles/chandlery/bedrock", [
      "/nix/store/bbb-bedrock-server", "/nix/store/aaa-glibc", "/nix/store/bbb-bedrock-server",
    ])).toBe([
      "# Written by tidewaiter before each restart: what the profile points at, and its closure.",
      "# Profile: /nix/var/nix/profiles/chandlery/bedrock",
      "[Service]",
      "BindReadOnlyPaths=/nix/var/nix/profiles/chandlery",
      "BindReadOnlyPaths=/nix/store/aaa-glibc",
      "BindReadOnlyPaths=/nix/store/bbb-bedrock-server",
      "",
    ].join("\n"));
  });
});

describe("NixClosureBinder.ensure", () => {
  const PROFILE = "/nix/var/nix/profiles/tidewaiter/game";
  const dirs: string[] = [];
  afterEach(async () => {
    for (const dir of dirs.splice(0)) await rm(dir, { recursive: true, force: true });
  });

  /** A binder over a scratch runtime directory, with a nix-store that answers each path as its own closure. */
  async function binder() {
    const dir = await mkdtemp(join(tmpdir(), "tidewaiter-ensure-"));
    dirs.push(dir);
    const nixStore = join(dir, "nix-store");
    await writeFile(nixStore, `#!${Bun.which("sh") ?? "/bin/sh"}\necho "$3"\n`);
    await chmod(nixStore, 0o755);
    const file = join(dir, "game.service.d", CLOSURE_DROP_IN);
    return { binder: new NixClosureBinder(dir, nixStore), read: () => readFile(file, "utf8") };
  }

  test("writes a drop-in that is missing", async () => {
    const { binder: closure, read } = await binder();
    expect(await closure.ensure("game.service", PROFILE, "/nix/store/aaaa-game-1")).toBe(true);
    expect(await read()).toContain("BindReadOnlyPaths=/nix/store/aaaa-game-1\n");
    expect(dropInProfile(await read())).toBe(PROFILE);
  });

  test("leaves the daemon's drop-in for the same profile alone, whatever it binds", async () => {
    const { binder: closure, read } = await binder();
    await closure.bind("game.service", PROFILE, "/nix/store/bbbb-game-2");
    expect(await closure.ensure("game.service", PROFILE, "/nix/store/aaaa-game-1")).toBe(false);
    expect(await read()).toContain("BindReadOnlyPaths=/nix/store/bbbb-game-2\n");
  });

  test("replaces one written for another profile", async () => {
    const { binder: closure, read } = await binder();
    await closure.bind("game.service", `${PROFILE}-old`, "/nix/store/bbbb-game-2");
    expect(await closure.ensure("game.service", PROFILE, "/nix/store/aaaa-game-1")).toBe(true);
    expect(dropInProfile(await read())).toBe(PROFILE);
  });
});

describe("FileStateStore", () => {
  const dirs: string[] = [];
  afterEach(async () => {
    for (const dir of dirs.splice(0)) await rm(dir, { recursive: true, force: true });
  });
  const fresh = async () => {
    const dir = await mkdtemp(join(tmpdir(), "tidewaiter-state-"));
    dirs.push(dir);
    return join(dir, "state", "systemd.json");
  };

  test("no file is no memories", async () => {
    expect(await new FileStateStore(await fresh()).load()).toEqual({});
  });

  test("round-trips, creating the directory", async () => {
    const path = await fresh();
    const store = new FileStateStore(path);
    await store.save({ "game.service": { pinned: "/nix/store/x", good: 3 } });
    expect(await new FileStateStore(path).load()).toEqual({ "game.service": { pinned: "/nix/store/x", good: 3 } });
    expect(await readFile(path, "utf8")).toEndWith("}\n");
  });

  test("a file that cannot be read is an error, never silently forgotten pins", async () => {
    const path = await fresh();
    await new FileStateStore(path).save({});
    await writeFile(path, "{ half");
    await expect(new FileStateStore(path).load()).rejects.toThrow();
  });
});

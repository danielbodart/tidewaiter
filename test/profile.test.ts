import { afterAll, describe, expect, test } from "bun:test";
import { existsSync } from "node:fs";
import { mkdtemp, readdir, realpath, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { generationOf, NixProfiles } from "../src/profile.ts";

describe("generationOf", () => {
  test("reads the number out of a generation link", () => {
    expect(generationOf("/nix/var/nix/profiles/chandlery/bedrock", "bedrock-12-link")).toBe(12);
    expect(generationOf("/p/bedrock", "/p/bedrock-3-link")).toBe(3);
  });

  test("is undefined for anything else", () => {
    expect(generationOf("/p/bedrock", "/nix/store/abc-bedrock")).toBeUndefined();
    expect(generationOf("/p/bedrock", "java-3-link")).toBeUndefined();
    expect(generationOf("/p/bedrock", "bedrock-x-link")).toBeUndefined();
    expect(generationOf("/p/bedrock", "bedrock-0-link")).toBeUndefined();
  });
});

/**
 * Two store paths that are certainly there, for a real profile to point at:
 * whatever `sh` and `ls` resolve to on a Nix host.
 */
async function storePaths(): Promise<string[]> {
  const paths = new Set<string>();
  for (const command of ["sh", "ls", "env"]) {
    const found = Bun.which(command);
    if (!found) continue;
    const real = await realpath(found).catch(() => "");
    const match = /^\/nix\/store\/[^/]+/.exec(real);
    if (match) paths.add(match[0]);
  }
  return [...paths];
}

// Against the real nix-env, where there is one with a daemon to talk to; the
// Nix build's sandbox has neither, and skips.
const paths = await storePaths();
const real = Bun.which("nix-env") !== null && existsSync("/nix/var/nix/daemon-socket/socket") && paths.length >= 2;

describe.skipIf(!real)("NixProfiles against nix-env", () => {
  const dirs: string[] = [];
  afterAll(async () => {
    for (const dir of dirs) await rm(dir, { recursive: true, force: true });
  });
  const [A, B] = paths as [string, string];

  test("set creates the parent, numbers generations, and switch, delete and prune behave", async () => {
    const dir = await mkdtemp(join(tmpdir(), "tidewaiter-profile-"));
    dirs.push(dir);
    const profile = join(dir, "not-yet", "game");
    const profiles = new NixProfiles();

    expect(await profiles.current(profile)).toBeUndefined();
    expect(await profiles.generation(profile)).toBeUndefined();

    await profiles.set(profile, A);
    await profiles.set(profile, B);
    await profiles.set(profile, A);
    expect(await profiles.generation(profile)).toBe(3);
    expect(await profiles.current(profile)).toBe(A);
    expect(await profiles.target(profile, 2)).toBe(B);

    await profiles.switchGeneration(profile, 2);
    await profiles.deleteGeneration(profile, 3);
    expect(await profiles.current(profile)).toBe(B);
    expect((await readdir(join(dir, "not-yet"))).sort()).toEqual(["game", "game-1-link", "game-2-link"]);

    await profiles.set(profile, A); // nix-env numbers it one past the highest left: 3 again
    await profiles.prune(profile, 1);
    expect((await readdir(join(dir, "not-yet"))).sort()).toEqual(["game", "game-2-link", "game-3-link"]);
  });

  test("a failure says what nix-env said", async () => {
    const dir = await mkdtemp(join(tmpdir(), "tidewaiter-profile-"));
    dirs.push(dir);
    const profile = join(dir, "game");
    await new NixProfiles().set(profile, A);
    await expect(new NixProfiles().switchGeneration(profile, 9)).rejects.toThrow("does not exist");
  });
});

describe.skipIf(!real)("NixClosureBinder against nix-store", () => {
  test("writes the drop-in with the target's whole closure", async () => {
    const { NixClosureBinder, CLOSURE_DROP_IN } = await import("../src/closure.ts");
    const dir = await mkdtemp(join(tmpdir(), "tidewaiter-closure-"));
    try {
      await new NixClosureBinder(dir).bind("game.service", "/nix/var/nix/profiles/tidewaiter/game", paths[0]!);
      const text = await Bun.file(join(dir, "game.service.d", CLOSURE_DROP_IN)).text();
      expect(text).toContain("BindReadOnlyPaths=/nix/var/nix/profiles/tidewaiter\n");
      expect(text).toContain(`BindReadOnlyPaths=${paths[0]}\n`);
      expect(text.split("\n").filter((line) => line.startsWith("BindReadOnlyPaths=/nix/store/")).length).toBeGreaterThan(1);
    } finally {
      await rm(dir, { recursive: true, force: true });
    }
  });
});

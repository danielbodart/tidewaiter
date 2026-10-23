import { afterEach, describe, expect, test } from "bun:test";
import { chmod, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { inhibitReason, SystemdInhibitor } from "../src/inhibit.ts";

/**
 * A stand-in for systemd-inhibit: records its arguments and its pid, then runs
 * the command it was given (cat) - exactly the shape of the real one once it
 * holds the lock. `refuse` makes it fail the way systemd-inhibit does with no
 * logind to talk to.
 */
async function fakeInhibit(refuse = false) {
  const dir = await mkdtemp(join(tmpdir(), "tidewaiter-inhibit-"));
  const script = join(dir, "systemd-inhibit");
  await writeFile(
    script,
    refuse
      ? `#!/bin/sh\necho "Failed to inhibit: Access denied" >&2\nexit 1\n`
      : `#!/bin/sh\necho "$$ $*" >> "${dir}/calls"\nfor last; do :; done\nexec "$last"\n`,
  );
  await chmod(script, 0o755);
  dirs.push(dir);
  const calls = async () =>
    (await readFile(join(dir, "calls"), "utf8").catch(() => "")).trim().split("\n").filter(Boolean)
      .map((line) => {
        const [pid, ...args] = line.split(" ");
        return { pid: Number(pid), args: args.join(" ") };
      });
  return { script, calls };
}

const dirs: string[] = [];
afterEach(async () => {
  for (const dir of dirs.splice(0)) await rm(dir, { recursive: true, force: true });
});

function alive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

describe("inhibitReason", () => {
  test("names the workloads, sorted and once each", () => {
    expect(inhibitReason(["web", "bedrock.service", "web"])).toBe("in use: bedrock.service, web");
  });

  test("is nothing at all when nothing is busy", () => {
    expect(inhibitReason([])).toBeUndefined();
  });
});

describe("SystemdInhibitor", () => {
  test("takes a shutdown:sleep block lock, with who and why", async () => {
    const { script, calls } = await fakeInhibit();
    const inhibitor = new SystemdInhibitor([script]);

    await inhibitor.set(["bedrock.service"]);

    const [call] = await calls();
    expect(call!.args).toBe(
      "--what=shutdown:sleep --mode=block --who=tidewaiter --why=in use: bedrock.service cat",
    );
    expect(alive(call!.pid)).toBe(true);
    await inhibitor.set([]);
  });

  test("the same workloads again take no new lock", async () => {
    const { script, calls } = await fakeInhibit();
    const inhibitor = new SystemdInhibitor([script]);

    await inhibitor.set(["web"]);
    await inhibitor.set(["web"]);

    expect(await calls()).toHaveLength(1);
    await inhibitor.set([]);
  });

  test("a new reason takes the new lock before dropping the old", async () => {
    const { script, calls } = await fakeInhibit();
    const inhibitor = new SystemdInhibitor([script]);

    await inhibitor.set(["web"]);
    await inhibitor.set(["web", "bedrock.service"]);

    const [first, second] = await calls();
    expect(second!.args).toContain("--why=in use: bedrock.service, web");
    expect(alive(first!.pid)).toBe(false);
    expect(alive(second!.pid)).toBe(true);
    expect(inhibitor.reason).toBe("in use: bedrock.service, web");
    await inhibitor.set([]);
  });

  test("releasing ends the child, and with it the lock", async () => {
    const { script, calls } = await fakeInhibit();
    const inhibitor = new SystemdInhibitor([script]);

    await inhibitor.set(["web"]);
    await inhibitor.set([]);

    const [call] = await calls();
    expect(alive(call!.pid)).toBe(false);
    expect(inhibitor.reason).toBeUndefined();
  });

  test("a lock whose child died is taken again", async () => {
    const { script, calls } = await fakeInhibit();
    const inhibitor = new SystemdInhibitor([script]);

    await inhibitor.set(["web"]);
    const [first] = await calls();
    process.kill(first!.pid, "SIGKILL");
    await Bun.sleep(20);
    await inhibitor.set(["web"]);

    expect(await calls()).toHaveLength(2);
    await inhibitor.set([]);
  });

  test("a lock that cannot be taken is an error, not a silent no-op", async () => {
    const { script } = await fakeInhibit(true);
    const inhibitor = new SystemdInhibitor([script]);

    await expect(inhibitor.set(["web"])).rejects.toThrow("Access denied");
    expect(inhibitor.reason).toBeUndefined();
  });
});

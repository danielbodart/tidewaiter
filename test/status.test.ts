import { afterEach, describe, expect, spyOn, test } from "bun:test";
import { mkdtemp, readdir, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { main } from "../src/main.ts";
import { describeStatus, FileStatus, isBusy, readStatus, type Status } from "../src/status.ts";

const NOW = Date.parse("2026-10-23T03:30:10Z");

function status(fields: Partial<Status> = {}): Status {
  return { updated: "2026-10-23T03:30:00Z", busy: [], swapping: [], workloads: [], ...fields };
}

describe("describeStatus", () => {
  test("idle: says so, with each workload and how old the news is", () => {
    expect(describeStatus(status({
      workloads: [
        { workload: "bedrock.service", source: "systemd", busy: false, settled: true },
        { workload: "web", source: "docker", busy: false, settled: false },
      ],
    }), NOW)).toEqual([
      "Idle: nothing in use, nothing being swapped",
      "  bedrock.service (systemd)  idle",
      "  web (docker)  idle, not yet for long enough",
      "As of 10s ago",
    ]);
  });

  test("busy: names what is in use", () => {
    const lines = describeStatus(status({
      busy: ["bedrock.service"],
      workloads: [{ workload: "bedrock.service", source: "systemd", busy: true, settled: false }],
    }), NOW);
    expect(lines[0]).toBe("Busy: bedrock.service in use");
    expect(lines[1]).toBe("  bedrock.service (systemd)  in use");
  });

  test("a swap under way comes first", () => {
    const lines = describeStatus(status({
      busy: ["web"],
      swapping: ["bedrock.service"],
      workloads: [{ workload: "bedrock.service", source: "systemd", busy: false, settled: false }],
    }), NOW);
    expect(lines[0]).toBe("Busy: swapping bedrock.service");
    expect(lines[1]).toBe("  bedrock.service (systemd)  swapping");
  });

  test("where an automatic reboot stands", () => {
    expect(describeStatus(status({ reboot: { window: "03:00-05:00", verdict: "not needed" } }), NOW))
      .toContain("Automatic reboot (03:00-05:00): not needed");
    expect(describeStatus(status({
      reboot: { window: "03:00-05:00", needed: "kernel changed since boot", verdict: "waiting: web in use" },
    }), NOW)).toContain("Automatic reboot (03:00-05:00): needed, kernel changed since boot; waiting: web in use");
  });

  test("busy is anything in use or swapping", () => {
    expect(isBusy(status())).toBe(false);
    expect(isBusy(status({ busy: ["web"] }))).toBe(true);
    expect(isBusy(status({ swapping: ["web"] }))).toBe(true);
  });
});

describe("the status file", () => {
  let dir: string;
  afterEach(async () => {
    await rm(dir, { recursive: true, force: true });
  });

  test("is written whole, creating its directory, and read back; cleared, it is gone", async () => {
    dir = await mkdtemp(join(tmpdir(), "tidewaiter-status-"));
    const path = join(dir, "run", "status.json");
    const file = new FileStatus(path);

    await file.write(status({ busy: ["web"] }));
    expect(await readStatus(path)).toEqual(status({ busy: ["web"] }));
    // Nothing left behind from the rename.
    expect(await readdir(join(dir, "run"))).toEqual(["status.json"]);

    await file.clear();
    expect(await readStatus(path)).toBeUndefined();
    await file.clear();
  });
});

describe("tidewaiter status", () => {
  let dir: string;
  afterEach(async () => {
    await rm(dir, { recursive: true, force: true });
  });

  async function run(written: Status | undefined): Promise<{ code: number; output: string }> {
    dir = await mkdtemp(join(tmpdir(), "tidewaiter-status-"));
    const path = join(dir, "status.json");
    if (written) await new FileStatus(path).write(written);
    const printed: string[] = [];
    const log = spyOn(console, "log").mockImplementation((line: string) => { printed.push(line); });
    try {
      return { code: await main(["status", "--status-file", path]), output: printed.join("\n") };
    } finally {
      log.mockRestore();
    }
  }

  test("exits 0 when nothing is in use or swapping, with no banner", async () => {
    const { code, output } = await run(status());
    expect(code).toBe(0);
    expect(output.startsWith("Idle:")).toBe(true);
  });

  test("exits 1 when something is in use", async () => {
    expect((await run(status({ busy: ["bedrock.service"] }))).code).toBe(1);
  });

  test("exits 1 while a swap is under way", async () => {
    expect((await run(status({ swapping: ["bedrock.service"] }))).code).toBe(1);
  });

  test("exits 2 when there is no status: no daemon to ask", async () => {
    const { code, output } = await run(undefined);
    expect(code).toBe(2);
    expect(output).toContain("No status at");
  });
});

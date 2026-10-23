import { afterEach, describe, expect, test } from "bun:test";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DEFAULT_UNIT_HEALTH, DirUnitsDirectory, parseUnitPolicy } from "../src/units.ts";

const REQUIRED = { unit: "chandlery-bedrock.service", desired: "/nix/store/abc-bedrock", profile: "/nix/var/nix/profiles/chandlery/bedrock" };
const parse = (fields: Record<string, unknown>) => parseUnitPolicy(JSON.stringify(fields), "bedrock.json");

describe("parseUnitPolicy", () => {
  test("reads a full file", () => {
    const { policy, warnings } = parse({
      ...REQUIRED,
      ports: ["19132/udp", "19133/udp"],
      detector: "conntrack",
      health: ["systemd", "port-bound"],
      idleSamples: 5,
      healthTimeout: 60,
      keepGenerations: 2,
      bindClosure: true,
    });
    expect(warnings).toEqual([]);
    expect(policy).toEqual({
      ...REQUIRED,
      ports: [
        { hostPort: 19132, containerPort: 19132, protocol: "udp" },
        { hostPort: 19133, containerPort: 19133, protocol: "udp" },
      ],
      detector: "conntrack",
      health: ["systemd", "port-bound"],
      idleSamples: 5,
      healthTimeoutSeconds: 60,
      keepGenerations: 2,
      bindClosure: true,
    });
  });

  test("defaults everything but the three required fields", () => {
    const { policy, warnings } = parse(REQUIRED);
    expect(warnings).toEqual([]);
    expect(policy).toMatchObject({
      ports: [], detector: "conntrack", health: DEFAULT_UNIT_HEALTH, idleSamples: 3,
      healthTimeoutSeconds: 120, keepGenerations: 1, bindClosure: false,
    });
  });

  test("a bare port means both protocols", () => {
    expect(parse({ ...REQUIRED, ports: ["25565", 7777] }).policy!.ports).toEqual([
      { hostPort: 25565, containerPort: 25565, protocol: "tcp" },
      { hostPort: 25565, containerPort: 25565, protocol: "udp" },
      { hostPort: 7777, containerPort: 7777, protocol: "tcp" },
      { hostPort: 7777, containerPort: 7777, protocol: "udp" },
    ]);
  });

  test("a bad port is warned about and dropped, keeping the good ones", () => {
    const { policy, warnings } = parse({ ...REQUIRED, ports: ["19132/udp", "99999", "http"] });
    expect(policy!.ports).toHaveLength(1);
    expect(warnings).toEqual([
      "bedrock.json: ports: ignoring '99999', ports must be between 1 and 65535",
      "bedrock.json: ports: ignoring unrecognised port 'http'",
    ]);
  });

  test("each bad setting is warned about and defaulted", () => {
    const { policy, warnings } = parse({
      ...REQUIRED, detector: "netio", idleSamples: 0, healthTimeout: "soon", keepGenerations: -1, bindClosure: "yes", ports: "7777",
    });
    expect(policy).toMatchObject({ detector: "conntrack", idleSamples: 3, healthTimeoutSeconds: 120, keepGenerations: 1, bindClosure: false, ports: [] });
    expect(warnings).toHaveLength(6);
    expect(warnings[0]).toContain("'netio' is not one of conntrack, tcp, none");
  });

  test("docker is not a check a unit can have", () => {
    const { policy, warnings } = parse({ ...REQUIRED, health: ["docker", "uptime", "bogus"] });
    expect(policy!.health).toEqual(["uptime"]);
    expect(warnings[0]).toContain("'docker' needs a container's HEALTHCHECK");
    expect(warnings[1]).toContain("'bogus' is not a known check");
  });

  test("an empty or all-unknown health list keeps the default gate", () => {
    expect(parse({ ...REQUIRED, health: [] }).policy!.health).toEqual(DEFAULT_UNIT_HEALTH);
    expect(parse({ ...REQUIRED, health: ["docker"] }).policy!.health).toEqual(DEFAULT_UNIT_HEALTH);
  });

  test("a unit named without a suffix is a service", () => {
    expect(parse({ ...REQUIRED, unit: "chandlery-bedrock" }).policy!.unit).toBe("chandlery-bedrock.service");
    expect(parse({ ...REQUIRED, unit: "game.scope" }).policy!.unit).toBe("game.scope");
  });

  test("missing a required field skips the file", () => {
    for (const field of ["unit", "desired", "profile"] as const) {
      const fields: Record<string, unknown> = { ...REQUIRED };
      delete fields[field];
      const { policy, warnings } = parse(fields);
      expect(policy).toBeUndefined();
      expect(warnings[0]).toBe(`bedrock.json: '${field}' must be a non-empty string; skipping it`);
    }
  });

  test("a relative desired or profile skips the file", () => {
    expect(parse({ ...REQUIRED, desired: "result" }).policy).toBeUndefined();
    expect(parse({ ...REQUIRED, profile: "profiles/x" }).policy).toBeUndefined();
  });

  test("not JSON, or not an object, skips the file", () => {
    expect(parseUnitPolicy("{ nope", "x.json").warnings[0]).toStartWith("x.json: not valid JSON");
    expect(parseUnitPolicy("[1]", "x.json").warnings).toEqual(["x.json: not a JSON object; skipping it"]);
  });
});

describe("DirUnitsDirectory", () => {
  const dirs: string[] = [];
  afterEach(async () => {
    for (const dir of dirs.splice(0)) await rm(dir, { recursive: true, force: true });
  });

  test("reads every .json file, sorted, and nothing else", async () => {
    const dir = await mkdtemp(join(tmpdir(), "tidewaiter-units-"));
    dirs.push(dir);
    await writeFile(join(dir, "b.service.json"), "{}");
    await writeFile(join(dir, "a.service.json"), "[]");
    await writeFile(join(dir, "README"), "not a unit");

    expect(await new DirUnitsDirectory(dir).read()).toEqual([
      { name: "a.service.json", text: "[]" },
      { name: "b.service.json", text: "{}" },
    ]);
  });

  test("a directory that is not there declares nothing", async () => {
    expect(await new DirUnitsDirectory("/nonexistent/tidewaiter/units").read()).toEqual([]);
  });
});

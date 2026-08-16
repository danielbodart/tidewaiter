import { describe, expect, test } from "bun:test";
import { parseArguments } from "../src/main.ts";
import { DEFAULTS } from "../src/daemon.ts";

describe("parseArguments", () => {
  test("defaults to the run command", () => {
    expect(parseArguments([]).command).toBe("run");
  });

  test("takes a bare command word", () => {
    expect(parseArguments(["check"]).command).toBe("check");
    expect(parseArguments(["list"]).command).toBe("list");
  });

  test("reads the interval flag and its short form", () => {
    expect(parseArguments(["-d", "60"]).options.interval).toBe(60);
    expect(parseArguments(["--interval", "120"]).options.interval).toBe(120);
  });

  test("falls back to the env interval, overridden by the flag", () => {
    expect(parseArguments([], { TIDEWAITER_INTERVAL: "45" }).options.interval).toBe(45);
    expect(parseArguments(["-d", "10"], { TIDEWAITER_INTERVAL: "45" }).options.interval).toBe(10);
  });

  test("reads the docker socket from flag and env", () => {
    expect(parseArguments(["--docker-socket", "/tmp/d.sock"]).socket).toBe("/tmp/d.sock");
    expect(parseArguments([], { DOCKER_SOCKET: "/env.sock" }).socket).toBe("/env.sock");
  });

  test("dry-run and label", () => {
    const parsed = parseArguments(["-n", "-l", "my.label", "check"]);
    expect(parsed.options.dryRun).toBe(true);
    expect(parsed.options.label).toBe("my.label");
    expect(parsed.command).toBe("check");
  });

  test("version and help flags", () => {
    expect(parseArguments(["--version"]).showVersion).toBe(true);
    expect(parseArguments(["-h"]).help).toBe(true);
  });

  test("an unknown option throws", () => {
    expect(() => parseArguments(["--nope"])).toThrow(/unknown option/);
  });

  test("a flag missing its value throws", () => {
    expect(() => parseArguments(["--interval"])).toThrow(/needs a value/);
  });

  test("the default interval is the daemon default", () => {
    expect(parseArguments([]).options.interval).toBe(DEFAULTS.interval);
  });
});

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

describe("parseArguments: sources and the systemd side", () => {
  test("docker is the only source by default, so the image behaves as it always has", () => {
    const parsed = parseArguments([]);
    expect(parsed.sources).toEqual(["docker"]);
    expect(parsed.inhibit).toBe(false);
  });

  test("--source repeats or takes a comma-separated list, each named once", () => {
    expect(parseArguments(["--source", "systemd"]).sources).toEqual(["systemd"]);
    expect(parseArguments(["-s", "systemd", "-s", "docker"]).sources).toEqual(["systemd", "docker"]);
    expect(parseArguments(["--source", "systemd,docker,systemd"]).sources).toEqual(["systemd", "docker"]);
  });

  test("TIDEWAITER_SOURCES, overridden by the flag", () => {
    expect(parseArguments([], { TIDEWAITER_SOURCES: "systemd" }).sources).toEqual(["systemd"]);
    expect(parseArguments(["-s", "docker"], { TIDEWAITER_SOURCES: "systemd" }).sources).toEqual(["docker"]);
  });

  test("an unknown source is refused", () => {
    expect(() => parseArguments(["--source", "podman"])).toThrow(/'podman' is not a source/);
    expect(() => parseArguments(["--source", ","])).toThrow(/needs a source name/);
  });

  test("the units and state directories, from flags and the environment", () => {
    const defaults = parseArguments([]);
    expect(defaults.unitsDir).toBe("/etc/tidewaiter/units");
    expect(defaults.stateDir).toBe("/var/lib/tidewaiter");
    expect(parseArguments(["--units-dir", "/u"], { TIDEWAITER_UNITS_DIR: "/e" }).unitsDir).toBe("/u");
    expect(parseArguments([], { TIDEWAITER_UNITS_DIR: "/e" }).unitsDir).toBe("/e");
    expect(parseArguments(["--state-dir", "/s"]).stateDir).toBe("/s");
    // StateDirectory=a b is passed as a colon-separated list; the first is ours.
    expect(parseArguments([], { STATE_DIRECTORY: "/var/lib/tidewaiter:/var/lib/other" }).stateDir).toBe("/var/lib/tidewaiter");
  });

  test("--inhibit and --no-inhibit, last one wins, and the environment", () => {
    expect(parseArguments(["--inhibit"]).inhibit).toBe(true);
    expect(parseArguments(["--inhibit", "--no-inhibit"]).inhibit).toBe(false);
    expect(parseArguments([], { TIDEWAITER_INHIBIT: "true" }).inhibit).toBe(true);
    expect(parseArguments(["--no-inhibit"], { TIDEWAITER_INHIBIT: "1" }).inhibit).toBe(false);
    expect(() => parseArguments([], { TIDEWAITER_INHIBIT: "maybe" })).toThrow(/true or false/);
  });

  test("--activity-interval, which must be more than nothing", () => {
    expect(parseArguments([]).options.activityInterval).toBe(DEFAULTS.activityInterval);
    expect(parseArguments(["--activity-interval", "5"]).options.activityInterval).toBe(5);
    expect(() => parseArguments(["--activity-interval", "0"])).toThrow(/more than 0/);
  });

  test("prepare is a command", () => {
    expect(parseArguments(["prepare"]).command).toBe("prepare");
  });
});

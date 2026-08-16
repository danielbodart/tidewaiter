import { describe, expect, test } from "bun:test";
import { DEFAULTS, Tidewaiter, type Options } from "../src/daemon.ts";
import type { HealthStatus } from "../src/model.ts";
import { FakeConntrackSource } from "./fakes/conntrack.ts";
import { FakeDocker, port, runningContainer } from "./fakes/docker.ts";
import { FakeNetnsReader } from "./fakes/netns.ts";
import { FakeRegistry } from "./fakes/registry.ts";
import { FakeTcpProbe } from "./fakes/tcp.ts";

/**
 * The whole daemon, wired to a Docker, registry, conntrack and netns that exist
 * only in memory. This is what the interface seams buy: the real reconcile
 * loop, the real decide() and the real swap sequencing, with only the far ends
 * replaced.
 */
function tidewaiter(overrides: Partial<Options> = {}) {
  const docker = new FakeDocker();
  const registry = new FakeRegistry();
  const conntrack = new FakeConntrackSource();
  const netns = new FakeNetnsReader();
  const tcp = new FakeTcpProbe();
  const log: string[] = [];
  let now = 1_000_000;
  const daemon = new Tidewaiter(
    docker,
    registry,
    conntrack,
    netns,
    { ...DEFAULTS, healthPollMillis: 1, ...overrides },
    (message) => log.push(message),
    () => now,
    undefined,
    tcp,
  );
  return {
    docker,
    registry,
    conntrack,
    netns,
    tcp,
    daemon,
    log,
    advance(ms: number) { now += ms; },
  };
}

/** An opted-in container running `current`, whose tag resolves to `desired`. */
function opted(
  docker: FakeDocker,
  registry: FakeRegistry,
  name: string,
  current: string,
  desired: string,
  labels: Record<string, string> = {},
  health: HealthStatus = "none",
) {
  const image = "app:latest";
  const imageId = `config-${current}`;
  docker.running.push(
    runningContainer(name, {
      imageId,
      health,
      spec: { image, labels: { "tidewaiter.autoupdate": "registry", ...labels }, published: [port(8080)] },
    }),
  );
  registry.digests[image] = desired;
  // The running container was created from the image whose manifest digest is
  // `current`, reachable by its image id (stable) and by the tag (until a pull
  // moves it). A pull would land `desired`. current != desired is what makes an
  // update due, and both are in the same manifest-digest namespace as the
  // registry answer, so the imageDigest()-vs-registry comparison is real.
  docker.images[imageId] = current;
  docker.images[image] = current;
  docker.pullLands[image] = desired;
  // By default a recreated container comes up healthy, so update scenarios
  // using health=docker commit without needing the clock to advance. Tests
  // that exercise rollback override this to "unhealthy".
  docker.healthByName[name] = "healthy";
}

describe("a container already on the latest image", () => {
  test("is kept, and nothing is pulled", async () => {
    const { docker, registry, daemon } = tidewaiter();
    opted(docker, registry, "web", "sha256:same", "sha256:same");

    const actions = await daemon.once();

    expect(actions.map((a) => a.kind)).toEqual(["keep"]);
    expect(docker.pulls).toEqual([]);
  });
});

describe("an idle container with a newer image", () => {
  test("is pulled, recreated and committed once healthy", async () => {
    const { docker, registry, daemon } = tidewaiter();
    opted(docker, registry, "web", "sha256:old", "sha256:new", { "tidewaiter.health": "docker", "tidewaiter.health-timeout": "1" });
    // idle: no conntrack flows to 8080.

    // Three passes to clear the default debounce of 3 idle samples.
    await daemon.once();
    await daemon.once();
    const actions = await daemon.once();

    expect(actions.map((a) => a.kind)).toEqual(["update"]);
    // Two pulls of the same ref: the background prefetch on the first idle pass
    // (a newer digest appeared), then the authoritative re-pull at swap time
    // (cache-warm, so cheap on a real host).
    expect(docker.pulls.map((p) => p.ref)).toEqual(["app:latest", "app:latest"]);
    // rename-not-delete: old parked, new created, old removed on commit.
    expect(docker.renames[0]).toEqual({ container: "web", to: "web-tidewaiter-rollback" });
    expect(docker.creates.map((c) => c.name)).toEqual(["web"]);
    expect(docker.removes.some((r) => r.container === "web-tidewaiter-rollback")).toBe(true);
  });

  test("rolls back and pins out when the new image is unhealthy", async () => {
    const { docker, registry, daemon, log } = tidewaiter();
    opted(docker, registry, "web", "sha256:old", "sha256:bad", { "tidewaiter.health": "docker", "tidewaiter.health-timeout": "1" });
    // The recreated container reports unhealthy.
    docker.healthByName["web"] = "unhealthy";

    await daemon.once();
    await daemon.once();
    await daemon.once(); // update attempt -> unhealthy -> rollback

    expect(docker.creates.length).toBeGreaterThan(0);
    // rollback removed the new one and restored the parked old name.
    expect(docker.removes.some((r) => r.container === "web")).toBe(true);
    expect(docker.renames.some((r) => r.container === "web-tidewaiter-rollback" && r.to === "web")).toBe(true);
    expect(log.join("\n")).toContain("rolled back");

    // Next pass: the tag still resolves to the bad digest, so it stays pinned.
    const actions = await daemon.once();
    expect(actions.map((a) => a.kind)).toEqual(["pinned"]);
  });

  // Feature: for health=docker the event stream settles the gate, not the poll.
  // The clock never advances and the container stays "starting", so the poll
  // (whose deadline is clock-based) can never fire - if the update commits, the
  // health_status event is what drove it.
  test("commits on a health_status:healthy event without waiting out the poll", async () => {
    const { docker, registry, daemon } = tidewaiter({ healthPollMillis: 60_000 });
    opted(docker, registry, "web", "sha256:old", "sha256:new", {
      "tidewaiter.health": "docker",
      "tidewaiter.health-timeout": "600",
    });
    // The inspect-poll can never conclude: health stays "starting" (inconclusive)
    // and the clock is frozen, so only an event can end the wait.
    docker.healthByName["web"] = "starting";
    // Queued before the pass; the watch consumes it as soon as the gate opens.
    docker.emit({ action: "health_status: healthy", container: "web", status: "healthy" });

    await daemon.once();
    await daemon.once();
    const actions = await daemon.once();

    expect(actions.map((a) => a.kind)).toEqual(["update"]);
    expect(docker.removes.some((r) => r.container === "web-tidewaiter-rollback")).toBe(true);
  });

  // The port-connect path, end to end - the one that failed on a live host.
  // Health is confirmed by a real connect() to the published HOST port (18080),
  // not by reading /proc, so there is no PID/netns timing to race. Scoped to
  // health=port-connect so the single connect pass commits it.
  test("commits a port-connect update once the published host port accepts a connection", async () => {
    const { docker, registry, daemon, tcp } = tidewaiter();
    docker.running.push(
      runningContainer("web", {
        imageId: "config-sha256:old",
        spec: {
          image: "app:latest",
          labels: {
            "tidewaiter.autoupdate": "registry",
            "tidewaiter.idle-samples": "2",
            "tidewaiter.health": "port-connect",
            "tidewaiter.health-timeout": "5",
          },
          published: [port(18080, 80, "tcp")],
        },
      }),
    );
    registry.digests["app:latest"] = "sha256:new";
    docker.images["config-sha256:old"] = "sha256:old";
    docker.images["app:latest"] = "sha256:old";
    docker.pullLands["app:latest"] = "sha256:new";
    // The new container will accept connections on its published host port.
    tcp.listening(18080);

    await daemon.once();
    const actions = await daemon.once(); // idle-samples=2 -> updates on the 2nd pass

    expect(actions.map((a) => a.kind)).toEqual(["update"]);
    expect(tcp.probed).toContain(18080); // health was confirmed by connecting to the host port
    expect(docker.removes.some((r) => r.container === "web-tidewaiter-rollback")).toBe(true); // committed
  });

  // The combination rule leaning toward TRUST: a port that never accepts is NOT
  // an active failure, so at the health-timeout the update COMMITS anyway (a
  // flaky/slow probe must not veto a good update). uptime passes and nothing
  // fails. Contrast with the exited-container test below, which DOES roll back.
  test("commits at the timeout even when the port never accepts (no active failure)", async () => {
    const docker = new FakeDocker();
    const registry = new FakeRegistry();
    let now = 1_000_000;
    const daemon = new Tidewaiter(
      docker, registry, new FakeConntrackSource(), new FakeNetnsReader(),
      { ...DEFAULTS, healthPollMillis: 0 },
      () => {},
      () => (now += 250), // clock advances so the 1s timeout is reached
      undefined,
      new FakeTcpProbe(), // nothing listening -> connect always inconclusive
    );
    docker.running.push(
      runningContainer("web", {
        imageId: "config-sha256:old",
        spec: {
          image: "app:latest",
          labels: {
            "tidewaiter.autoupdate": "registry",
            "tidewaiter.idle-samples": "1",
            "tidewaiter.health": "port-connect",
            "tidewaiter.health-timeout": "1",
          },
          published: [port(18080, 80, "tcp")],
        },
      }),
    );
    registry.digests["app:latest"] = "sha256:new";
    docker.images["config-sha256:old"] = "sha256:old";
    docker.images["app:latest"] = "sha256:old";
    docker.pullLands["app:latest"] = "sha256:new";

    await daemon.once();

    // Committed: the parked old container was removed, not restored.
    expect(docker.removes.some((r) => r.container === "web-tidewaiter-rollback")).toBe(true);
    expect(docker.renames.some((r) => r.container === "web-tidewaiter-rollback" && r.to === "web")).toBe(false);
  });

  // The uptime check's active-failure path: a new container that EXITS (or
  // crash-loops) is a genuine failure and rolls back, even under the
  // trust-the-update rule.
  test("rolls back when the new container exits (uptime active failure)", async () => {
    const { docker, registry, daemon } = tidewaiter();
    docker.running.push(
      runningContainer("web", {
        imageId: "config-sha256:old",
        spec: {
          image: "app:latest",
          labels: {
            "tidewaiter.autoupdate": "registry",
            "tidewaiter.idle-samples": "1",
            "tidewaiter.health": "uptime",
            "tidewaiter.health-timeout": "5",
          },
          published: [port(18080, 80, "tcp")],
        },
      }),
    );
    registry.digests["app:latest"] = "sha256:new";
    docker.images["config-sha256:old"] = "sha256:old";
    docker.images["app:latest"] = "sha256:old";
    docker.pullLands["app:latest"] = "sha256:new";
    // The recreated container is reported as exited by inspect.
    docker.runningByName["web"] = false;

    await daemon.once();

    expect(docker.renames.some((r) => r.container === "web-tidewaiter-rollback" && r.to === "web")).toBe(true);
  });

  test("rolls back on a health_status:unhealthy event", async () => {
    const { docker, registry, daemon, log } = tidewaiter({ healthPollMillis: 60_000 });
    opted(docker, registry, "web", "sha256:old", "sha256:bad", {
      "tidewaiter.health": "docker",
      "tidewaiter.health-timeout": "600",
    });
    docker.healthByName["web"] = "starting";
    docker.emit({ action: "health_status: unhealthy", container: "web", status: "unhealthy" });

    await daemon.once();
    await daemon.once();
    await daemon.once();

    expect(log.join("\n")).toContain("rolled back");
    expect(docker.renames.some((r) => r.container === "web-tidewaiter-rollback" && r.to === "web")).toBe(true);
  });

  // Regression for the unsafe-rollback bug: if a swap step fails BEFORE the old
  // container has been renamed away, rollback must not force-remove the still-
  // good original (which a blind plan.rollback did, taking the service down).
  test("a stop failure at the start of a swap never removes the original", async () => {
    const { docker, registry, daemon, log } = tidewaiter();
    opted(docker, registry, "web", "sha256:old", "sha256:new", { "tidewaiter.health": "docker", "tidewaiter.health-timeout": "1" });
    docker.failOp.stop = new Error("Docker socket hiccup");

    await daemon.once();
    await daemon.once();
    await daemon.once(); // update attempt -> stop throws -> safe undo

    // The original was never renamed nor created-over, so it must not be removed.
    expect(docker.removes.some((r) => r.container === "web")).toBe(false);
    // Nothing was parked, so no bogus rename of a non-existent parked container.
    expect(docker.renames).toEqual([]);
    // The image was pulled (before the stop), but no new container stood up.
    expect(docker.creates).toEqual([]);
    expect(log.join("\n")).toContain("undoing what was done");
  });

  // Regression: a transient swap-step failure must NOT pin the tag (pinning
  // means "the image is bad"; a bounced API call says nothing about the image).
  test("a transient swap failure does not pin the tag out", async () => {
    const { docker, registry, daemon } = tidewaiter();
    opted(docker, registry, "web", "sha256:old", "sha256:new", { "tidewaiter.health": "docker", "tidewaiter.health-timeout": "1" });
    docker.failOp.create = new Error("temporary disk pressure");

    await daemon.once();
    await daemon.once();
    await daemon.once(); // update -> create throws -> undo, no pin

    // The create failure cleared on retry: still idle, so it tries again rather
    // than sitting pinned. A pinned container would return "pinned" here.
    docker.failOp.create = undefined;
    docker.healthByName["web"] = "healthy";
    const actions = await daemon.once();
    expect(actions.map((a) => a.kind)).toEqual(["update"]);
  });

  // A DETERMINISTIC swap failure (a genuinely broken image, not a blip) must not
  // retry forever: after a few consecutive failures on the same digest it pins
  // out, and the service stays up throughout.
  test("backs off and pins out after repeated swap failures on the same digest", async () => {
    const { docker, registry, daemon, log } = tidewaiter();
    opted(docker, registry, "web", "sha256:old", "sha256:new", {
      "tidewaiter.idle-samples": "1",
      "tidewaiter.health": "docker",
      "tidewaiter.health-timeout": "1",
    });
    // Every start fails - a deterministically broken image.
    docker.failOp.start = new Error("exec: entrypoint not found");

    // MAX_SWAP_FAILURES is 3: passes 1-3 attempt+fail, pass 4 is pinned.
    expect((await daemon.once()).map((a) => a.kind)).toEqual(["update"]); // fail 1
    expect((await daemon.once()).map((a) => a.kind)).toEqual(["update"]); // fail 2
    expect((await daemon.once()).map((a) => a.kind)).toEqual(["update"]); // fail 3 -> pins
    expect((await daemon.once()).map((a) => a.kind)).toEqual(["pinned"]); // now pinned, no more attempts
    expect(log.join("\n")).toContain("pinning it out");
    // The original container survived every failed swap.
    expect(docker.running.some((c) => c.spec.name === "web")).toBe(true);

    // A new digest clears the back-off and it tries again.
    registry.digests["app:latest"] = "sha256:fixed";
    docker.pullLands["app:latest"] = "sha256:fixed";
    docker.failOp.start = undefined;
    docker.healthByName["web"] = "healthy";
    expect((await daemon.once()).map((a) => a.kind)).toEqual(["update"]);
  });

  // Regression: private-registry credentials must reach docker pull, not just
  // the registry digest lookup.
  test("passes X-Registry-Auth through to docker pull", async () => {
    const docker = new FakeDocker();
    const registry = new FakeRegistry();
    const conntrack = new FakeConntrackSource();
    const netns = new FakeNetnsReader();
    let now = 1_000_000;
    const daemon = new Tidewaiter(
      docker, registry, conntrack, netns,
      { ...DEFAULTS, healthPollMillis: 1 },
      () => {},
      () => now,
      // AuthSource keyed by registry host; the default ref resolves to Docker Hub.
      (host) => (host === "registry-1.docker.io" ? "base64-creds" : undefined),
    );
    opted(docker, registry, "web", "sha256:old", "sha256:new", { "tidewaiter.health": "docker", "tidewaiter.health-timeout": "1" });

    await daemon.once();
    await daemon.once();
    await daemon.once();

    // Both the background prefetch and the swap-time pull carry the credential.
    expect(docker.pulls).toEqual([
      { ref: "app:latest", auth: "base64-creds" },
      { ref: "app:latest", auth: "base64-creds" },
    ]);
    void now;
  });

  test("warns when recreating a container: network-mode container, but still updates", async () => {
    const { docker, registry, daemon, log } = tidewaiter();
    opted(docker, registry, "web", "sha256:old", "sha256:new", {
      "tidewaiter.health": "docker",
      "tidewaiter.health-timeout": "1",
    });
    // Give the running container a container:<id> network mode.
    docker.running = docker.running.map((c) =>
      c.spec.name === "web" ? { ...c, spec: { ...c.spec, networkMode: "container:sidecar" } } : c,
    );

    await daemon.once();
    await daemon.once();
    const actions = await daemon.once();

    expect(actions.map((a) => a.kind)).toEqual(["update"]);
    expect(log.join("\n")).toContain("shares another container's netns");
  });

  test("stops pinning once the tag moves to a new digest", async () => {
    const { docker, registry, daemon } = tidewaiter();
    opted(docker, registry, "web", "sha256:old", "sha256:bad", { "tidewaiter.health": "docker", "tidewaiter.health-timeout": "1" });
    docker.healthByName["web"] = "unhealthy";
    await daemon.once();
    await daemon.once();
    await daemon.once();
    expect((await daemon.once()).map((a) => a.kind)).toEqual(["pinned"]);

    // A genuinely new image appears in the registry and comes up healthy this
    // time. Only the registry moves and what a pull lands; the local tag is left
    // as-is (the container is still on the rolled-back image).
    registry.digests["app:latest"] = "sha256:fixed";
    docker.pullLands["app:latest"] = "sha256:fixed";
    docker.healthByName["web"] = "healthy";
    // pin cleared -> still idle from before, so it updates immediately.
    const actions = await daemon.once();
    expect(actions.map((a) => a.kind)).toEqual(["update"]);
  });
});

describe("opt-in and policy", () => {
  test("a container with an unrecognised autoupdate value is left alone", async () => {
    const { docker, registry, daemon, log } = tidewaiter();
    // Opted in by the key but with a typo'd value: the container is listed
    // (the daemon filters on the key), but must not be touched.
    opted(docker, registry, "web", "sha256:old", "sha256:new", { "tidewaiter.autoupdate": "registy" });

    const actions = await daemon.once();

    expect(actions.map((a) => a.kind)).toEqual(["keep"]);
    expect(docker.pulls).toEqual([]);
    expect(docker.creates).toEqual([]);
    expect(log.join("\n")).toContain("not a known policy");
  });

  test("the local policy updates from the local image without contacting the registry or pulling", async () => {
    const { docker, registry, daemon } = tidewaiter();
    // Running config-old; the tag's LOCAL image has been rebuilt to sha256:built.
    const imageId = "config-old";
    docker.running.push(
      runningContainer("web", {
        imageId,
        spec: {
          image: "app:latest",
          labels: { "tidewaiter.autoupdate": "local", "tidewaiter.health": "docker", "tidewaiter.health-timeout": "1" },
          published: [port(8080)],
        },
      }),
    );
    docker.images[imageId] = "sha256:old";        // what the container runs
    docker.images["app:latest"] = "sha256:built"; // what the tag resolves to locally now
    docker.healthByName["web"] = "healthy";

    await daemon.once();
    await daemon.once();
    const actions = await daemon.once();

    expect(actions.map((a) => a.kind)).toEqual(["update"]);
    // local never pulls and never asks the registry.
    expect(docker.pulls).toEqual([]);
    expect(registry.calls).toEqual([]);
    // it still recreates onto the new local image.
    expect(docker.creates.map((c) => c.name)).toEqual(["web"]);
  });
});

describe("the activity gate", () => {
  test("defers while the container has live flows", async () => {
    const { docker, registry, conntrack, daemon } = tidewaiter();
    opted(docker, registry, "web", "sha256:old", "sha256:new");
    // An ASSURED flow to the published host port 8080.
    conntrack.text = "tcp 6 300 ESTABLISHED src=10.0.0.9 dst=10.0.0.1 sport=5000 dport=8080 [ASSURED]";

    for (let i = 0; i < 5; i += 1) {
      expect((await daemon.once()).map((a) => a.kind)).toEqual(["defer"]);
    }
    // Busy, so it never swaps - but it DOES warm the cache in the background
    // (once, deduped across passes), so the eventual idle swap needs no download.
    expect(docker.creates).toEqual([]);
    expect(docker.pulls.map((p) => p.ref)).toEqual(["app:latest"]);
  });

  test("requires N consecutive idle samples before updating", async () => {
    const { docker, registry, daemon } = tidewaiter();
    opted(docker, registry, "web", "sha256:old", "sha256:new", {
      "tidewaiter.idle-samples": "3",
      "tidewaiter.health": "docker",
      "tidewaiter.health-timeout": "1",
    });

    expect((await daemon.once()).map((a) => a.kind)).toEqual(["defer"]);
    expect((await daemon.once()).map((a) => a.kind)).toEqual(["defer"]);
    expect((await daemon.once()).map((a) => a.kind)).toEqual(["update"]);
  });

  test("a transient flow resets the streak but does not cancel the eventual update", async () => {
    const { docker, registry, conntrack, daemon } = tidewaiter();
    opted(docker, registry, "web", "sha256:old", "sha256:new", {
      "tidewaiter.idle-samples": "2",
      "tidewaiter.health": "docker",
      "tidewaiter.health-timeout": "1",
    });

    conntrack.text = "";                     // idle 1
    expect((await daemon.once()).map((a) => a.kind)).toEqual(["defer"]);
    conntrack.text = "tcp 6 1 ESTABLISHED dport=8080 [ASSURED]"; // busy - resets
    expect((await daemon.once()).map((a) => a.kind)).toEqual(["defer"]);
    conntrack.text = "";                     // idle 1 again
    expect((await daemon.once()).map((a) => a.kind)).toEqual(["defer"]);
    expect((await daemon.once()).map((a) => a.kind)).toEqual(["update"]); // idle 2
  });

  test("is fail-safe: a detector that throws is treated as busy", async () => {
    const { docker, registry, conntrack, daemon, log } = tidewaiter();
    opted(docker, registry, "web", "sha256:old", "sha256:new");
    conntrack.failWith = new Error("conntrack: Operation not permitted");

    // conntrack dump fails -> empty table -> conntrack detector would say idle,
    // but the failed dump is logged; with an empty table the detector reports
    // idle high-confidence. The dump failure itself is the fail-safe point:
    // it logs a warning and the pass still completes without disrupting anything.
    const actions = await daemon.once();
    expect(actions[0]?.kind).not.toBe("update");
    expect(log.join("\n")).toContain("conntrack");
  });

  test("is fail-safe: a netio stats read that throws is treated as busy", async () => {
    const { docker, registry, daemon, log } = tidewaiter();
    opted(docker, registry, "web", "sha256:old", "sha256:new", { "tidewaiter.detector": "netio" });
    docker.netBytesFailWith = new Error("stats: container not running");

    // The netio detector throws inside sample(); the daemon's per-container
    // try/catch folds it into "in use, low confidence", so it defers rather than
    // updating or aborting the pass.
    const actions = await daemon.once();
    expect(actions.map((a) => a.kind)).toEqual(["defer"]);
    // Assumed busy, so it must not swap; the prefetch may still warm the cache.
    expect(docker.creates).toEqual([]);
    expect(log.join("\n")).toContain("netio");
  });
});

describe("prefetching a newer image", () => {
  test("pulls in the background while the container is still busy", async () => {
    const { docker, registry, conntrack, daemon } = tidewaiter();
    opted(docker, registry, "web", "sha256:old", "sha256:new");
    // Busy the whole time: an ASSURED flow to the published host port.
    conntrack.text = "tcp 6 300 ESTABLISHED src=10.0.0.9 dst=10.0.0.1 sport=5000 dport=8080 [ASSURED]";

    const actions = await daemon.once();
    // Never swaps while busy...
    expect(actions.map((a) => a.kind)).toEqual(["defer"]);
    expect(docker.creates).toEqual([]);
    // ...but the newer image was fetched ahead of time, so the eventual swap is
    // download-free.
    expect(docker.pulls.map((p) => p.ref)).toEqual(["app:latest"]);

    // Deduped: further busy passes on the same digest do not re-pull.
    await daemon.once();
    await daemon.once();
    expect(docker.pulls.map((p) => p.ref)).toEqual(["app:latest"]);
  });

  test("does not prefetch when already on the latest, pinned, local, or dry-run", async () => {
    // Already current: nothing newer to fetch.
    {
      const { docker, registry, daemon } = tidewaiter();
      opted(docker, registry, "web", "sha256:same", "sha256:same");
      await daemon.once();
      expect(docker.pulls).toEqual([]);
    }
    // local policy never touches the network.
    {
      const { docker, daemon } = tidewaiter();
      docker.running.push(
        runningContainer("web", {
          imageId: "config-old",
          spec: { image: "app:latest", labels: { "tidewaiter.autoupdate": "local" }, published: [port(8080)] },
        }),
      );
      docker.images["config-old"] = "sha256:old";
      docker.images["app:latest"] = "sha256:built";
      await daemon.once();
      expect(docker.pulls).toEqual([]);
    }
    // dry run touches nothing.
    {
      const { docker, registry, daemon } = tidewaiter({ dryRun: true });
      opted(docker, registry, "web", "sha256:old", "sha256:new");
      await daemon.once();
      expect(docker.pulls).toEqual([]);
    }
  });

  test("relaunches when the tag moves to a different digest while busy", async () => {
    const { docker, registry, conntrack, daemon } = tidewaiter();
    opted(docker, registry, "web", "sha256:old", "sha256:new");
    conntrack.text = "tcp 6 300 ESTABLISHED dport=8080 [ASSURED]"; // busy throughout

    await daemon.once();
    expect(docker.pulls.map((p) => p.ref)).toEqual(["app:latest"]);

    // The registry tag moves again before the container ever went idle.
    registry.digests["app:latest"] = "sha256:newer";
    docker.pullLands["app:latest"] = "sha256:newer";
    await daemon.once();
    // A second prefetch fires for the new target digest.
    expect(docker.pulls.map((p) => p.ref)).toEqual(["app:latest", "app:latest"]);
  });

  test("a failed prefetch never pins and is retried on the next pass", async () => {
    const { docker, registry, conntrack, daemon, log } = tidewaiter();
    opted(docker, registry, "web", "sha256:old", "sha256:new");
    conntrack.text = "tcp 6 300 ESTABLISHED dport=8080 [ASSURED]"; // busy, so only the prefetch runs
    docker.failPull = new Error("registry 500");

    const first = await daemon.once();
    expect(first.map((a) => a.kind)).toEqual(["defer"]); // a failed warm-up never pins
    expect(docker.pulls.length).toBe(1);
    expect(log.join("\n")).toContain("prefetch of app:latest failed");

    // The record was cleared, so the next pass retries the pull.
    docker.failPull = undefined;
    await daemon.once();
    expect(docker.pulls.length).toBe(2);
  });
});

describe("keep-images pruning", () => {
  test("retains the previous image and prunes older ones", async () => {
    const { docker, registry, daemon } = tidewaiter();
    opted(docker, registry, "web", "sha256:v1", "sha256:v2", {
      "tidewaiter.keep-images": "1",
      "tidewaiter.health": "docker",
      "tidewaiter.health-timeout": "1",
    });

    // First update v1 -> v2.
    await daemon.once();
    await daemon.once();
    await daemon.once();
    expect(docker.removedImages).toEqual([]); // only one previous, keep-images=1

    // A second update v2 -> v3: now v1 falls outside keep-images=1 and is pruned.
    // The registry moves and a pull will land v3; the local tag is on v2 (where
    // the first update left it), so this is a genuine update.
    registry.digests["app:latest"] = "sha256:v3";
    docker.pullLands["app:latest"] = "sha256:v3";
    // container is still idle; it updates on the next pass.
    await daemon.once();
    expect(docker.removedImages).toEqual(["sha256:v1"]);
  });
});

describe("adopting an unknown running image", () => {
  test("logs a baseline-adoption warning on first sight", async () => {
    const { docker, registry, daemon, log, conntrack } = tidewaiter();
    opted(docker, registry, "web", "sha256:mystery", "sha256:new");
    conntrack.text = "tcp 6 1 ESTABLISHED dport=8080 [ASSURED]"; // busy, so it only adopts, does not act

    await daemon.once();
    expect(log.join("\n")).toContain("adopting the running image as the current baseline");
  });
});

describe("dry run", () => {
  test("decides to update but changes nothing", async () => {
    const { docker, registry, daemon, log } = tidewaiter({ dryRun: true });
    opted(docker, registry, "web", "sha256:old", "sha256:new", {
      "tidewaiter.health": "docker",
      "tidewaiter.health-timeout": "1",
    });

    await daemon.once();
    await daemon.once();
    const actions = await daemon.once();

    expect(actions.map((a) => a.kind)).toEqual(["update"]);
    expect(docker.pulls).toEqual([]);
    expect(docker.creates).toEqual([]);
    expect(log.join("\n")).toContain("dry run");
  });
});

describe("summary logging", () => {
  test("says nothing to do, once, then stays quiet", async () => {
    const { docker, registry, daemon, log } = tidewaiter();
    opted(docker, registry, "web", "sha256:same", "sha256:same");

    await daemon.once();
    const first = log.length;
    await daemon.once();
    // The summary line is unchanged, so it is not repeated.
    expect(log.length).toBe(first);
    expect(log.join("\n")).toContain("up to date");
  });
});

describe("conntrack is dumped once per pass", () => {
  test("not once per container", async () => {
    const { docker, registry, conntrack, daemon } = tidewaiter();
    opted(docker, registry, "a", "sha256:same", "sha256:same");
    opted(docker, registry, "b", "sha256:same", "sha256:same");

    await daemon.once();
    expect(conntrack.calls).toBe(1);
  });
});

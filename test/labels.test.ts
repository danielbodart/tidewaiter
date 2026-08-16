import { describe, expect, test } from "bun:test";
import { parseLabels, POLICY_DEFAULTS, type ContainerPolicy } from "../src/labels.ts";
import type { PublishedPort } from "../src/model.ts";

const port = (hostPort: number, protocol: "tcp" | "udp" = "tcp"): PublishedPort => ({
  hostPort,
  containerPort: hostPort,
  protocol,
});

/** Parse and assert the container is opted in, returning the (non-undefined) policy. */
function opted(labels: Record<string, string>, published: PublishedPort[] = []): ContainerPolicy {
  const { policy } = parseLabels(labels, published);
  if (policy === undefined) throw new Error("expected the container to be opted in");
  return policy;
}

describe("parseLabels", () => {
  describe("opt-in", () => {
    test("an unlabelled container is not opted in", () => {
      const { policy, warnings } = parseLabels({}, []);
      expect(policy).toBeUndefined();
      // Silent: the common case for most containers on a host.
      expect(warnings).toEqual([]);
    });

    test("tidewaiter.autoupdate=registry opts in with all defaults", () => {
      const policy = opted({ "tidewaiter.autoupdate": "registry" });
      expect(policy).toMatchObject({
        autoupdate: "registry",
        detector: POLICY_DEFAULTS.detector,
        health: POLICY_DEFAULTS.health,
        idleSamples: POLICY_DEFAULTS.idleSamples,
        keepImages: POLICY_DEFAULTS.keepImages,
      });
    });

    test("tidewaiter.autoupdate=local opts in with the local policy", () => {
      expect(opted({ "tidewaiter.autoupdate": "local" }).autoupdate).toBe("local");
    });

    test("an unrecognised autoupdate value is not opted in, and warns", () => {
      const { policy, warnings } = parseLabels({ "tidewaiter.autoupdate": "registy" }, []);
      expect(policy).toBeUndefined();
      expect(warnings.join("\n")).toContain("not a known policy");
    });

    test("the value is case-insensitive and trimmed", () => {
      expect(opted({ "tidewaiter.autoupdate": " Registry " }).autoupdate).toBe("registry");
    });
  });

  test("reads every override", () => {
    const policy = opted({
      "tidewaiter.autoupdate": "registry",
      "tidewaiter.detector": "tcp",
      "tidewaiter.health": "docker",
      "tidewaiter.idle-samples": "5",
      "tidewaiter.health-timeout": "60",
      "tidewaiter.grace": "20",
      "tidewaiter.keep-images": "3",
    });
    expect(policy).toMatchObject({
      detector: "tcp",
      health: "docker",
      idleSamples: 5,
      healthTimeoutSeconds: 60,
      graceSeconds: 20,
      keepImages: 3,
    });
  });

  test("an unknown detector is a warning and falls back to the default", () => {
    const { policy, warnings } = parseLabels(
      { "tidewaiter.autoupdate": "registry", "tidewaiter.detector": "psychic" },
      [],
    );
    expect(policy?.detector).toBe(POLICY_DEFAULTS.detector);
    expect(warnings.join("\n")).toContain("tidewaiter.detector");
  });

  test("a non-numeric idle-samples is a warning and falls back", () => {
    const { policy, warnings } = parseLabels(
      { "tidewaiter.autoupdate": "registry", "tidewaiter.idle-samples": "lots" },
      [],
    );
    expect(policy?.idleSamples).toBe(POLICY_DEFAULTS.idleSamples);
    expect(warnings.length).toBeGreaterThan(0);
  });

  test("keep-images may be zero", () => {
    const { policy, warnings } = parseLabels(
      { "tidewaiter.autoupdate": "registry", "tidewaiter.keep-images": "0" },
      [],
    );
    expect(policy?.keepImages).toBe(0);
    expect(warnings).toEqual([]);
  });

  describe("tidewaiter.ports override", () => {
    test("selects the matching published ports", () => {
      const policy = opted(
        { "tidewaiter.autoupdate": "registry", "tidewaiter.ports": "8080" },
        [port(8080), port(9090)],
      );
      expect(policy.ports).toEqual([port(8080)]);
    });

    test("honours a protocol qualifier", () => {
      const policy = opted(
        { "tidewaiter.autoupdate": "registry", "tidewaiter.ports": "19132/udp" },
        [port(19132, "tcp"), port(19132, "udp")],
      );
      expect(policy.ports).toEqual([port(19132, "udp")]);
    });

    test("a port the container does not publish is a warning and dropped", () => {
      const { policy, warnings } = parseLabels(
        { "tidewaiter.autoupdate": "registry", "tidewaiter.ports": "1234" },
        [port(8080)],
      );
      expect(policy?.ports).toEqual([]);
      expect(warnings.join("\n")).toContain("not a port this container publishes");
    });
  });
});

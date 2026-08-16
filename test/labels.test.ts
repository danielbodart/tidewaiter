import { describe, expect, test } from "bun:test";
import { DEFAULT_POLICY, parseLabels } from "../src/labels.ts";
import type { PublishedPort } from "../src/model.ts";

const port = (hostPort: number, protocol: "tcp" | "udp" = "tcp"): PublishedPort => ({
  hostPort,
  containerPort: hostPort,
  protocol,
});

describe("parseLabels", () => {
  test("an unlabelled container is not opted in", () => {
    const { policy } = parseLabels({}, []);
    expect(policy.enable).toBe(false);
  });

  test("tidewaiter.enable=true opts a container in with all defaults", () => {
    const { policy } = parseLabels({ "tidewaiter.enable": "true" }, []);
    expect(policy).toMatchObject({
      enable: true,
      detector: DEFAULT_POLICY.detector,
      health: DEFAULT_POLICY.health,
      idleSamples: DEFAULT_POLICY.idleSamples,
      keepImages: DEFAULT_POLICY.keepImages,
    });
  });

  test("reads every override", () => {
    const { policy } = parseLabels(
      {
        "tidewaiter.enable": "true",
        "tidewaiter.detector": "tcp",
        "tidewaiter.health": "docker",
        "tidewaiter.idle-samples": "5",
        "tidewaiter.health-timeout": "60",
        "tidewaiter.grace": "20",
        "tidewaiter.keep-images": "3",
      },
      [],
    );
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
      { "tidewaiter.enable": "true", "tidewaiter.detector": "psychic" },
      [],
    );
    expect(policy.detector).toBe(DEFAULT_POLICY.detector);
    expect(warnings.join("\n")).toContain("tidewaiter.detector");
  });

  test("a non-numeric idle-samples is a warning and falls back", () => {
    const { policy, warnings } = parseLabels(
      { "tidewaiter.enable": "true", "tidewaiter.idle-samples": "lots" },
      [],
    );
    expect(policy.idleSamples).toBe(DEFAULT_POLICY.idleSamples);
    expect(warnings.length).toBeGreaterThan(0);
  });

  test("keep-images may be zero", () => {
    const { policy, warnings } = parseLabels(
      { "tidewaiter.enable": "true", "tidewaiter.keep-images": "0" },
      [],
    );
    expect(policy.keepImages).toBe(0);
    expect(warnings).toEqual([]);
  });

  describe("tidewaiter.ports override", () => {
    test("selects the matching published ports", () => {
      const { policy } = parseLabels(
        { "tidewaiter.enable": "true", "tidewaiter.ports": "8080" },
        [port(8080), port(9090)],
      );
      expect(policy.ports).toEqual([port(8080)]);
    });

    test("honours a protocol qualifier", () => {
      const { policy } = parseLabels(
        { "tidewaiter.enable": "true", "tidewaiter.ports": "19132/udp" },
        [port(19132, "tcp"), port(19132, "udp")],
      );
      expect(policy.ports).toEqual([port(19132, "udp")]);
    });

    test("a port the container does not publish is a warning and dropped", () => {
      const { policy, warnings } = parseLabels(
        { "tidewaiter.enable": "true", "tidewaiter.ports": "1234" },
        [port(8080)],
      );
      expect(policy.ports).toEqual([]);
      expect(warnings.join("\n")).toContain("not a port this container publishes");
    });
  });
});

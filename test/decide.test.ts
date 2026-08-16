import { describe, expect, test } from "bun:test";
import { decide, type ContainerState, type Snapshot } from "../src/decide.ts";
import { DEFAULT_POLICY, type ContainerPolicy } from "../src/labels.ts";
import { runningContainer } from "./fakes/docker.ts";

function snapshot(overrides: Partial<Snapshot> = {}, policy: Partial<ContainerPolicy> = {}): Snapshot {
  return {
    container: runningContainer("app"),
    // The manifest digest the container is on now - a real digest in the same
    // namespace as `desired`, so the two genuinely compare.
    currentDigest: "sha256:old",
    policy: { ...DEFAULT_POLICY, enable: true, idleSamples: 3, ...policy },
    desired: { container: "app", ref: "app:latest", digest: "sha256:new" },
    inUse: false,
    idleStreak: 3,
    ...overrides,
  };
}

const state = (overrides: Partial<ContainerState> = {}): ContainerState => ({ ...overrides });

describe("decide", () => {
  test("keeps a container already on the desired digest", () => {
    const action = decide(
      snapshot({ desired: { container: "app", ref: "app:latest", digest: "sha256:old" } }),
      state(),
    );
    expect(action.kind).toBe("keep");
  });

  test("updates an idle container when a newer image is available", () => {
    const action = decide(snapshot(), state());
    expect(action.kind).toBe("update");
    if (action.kind === "update") {
      expect(action.from).toBe("sha256:old");
      expect(action.to).toBe("sha256:new");
    }
  });

  test("defers when the container is in use, however long the idle streak", () => {
    const action = decide(snapshot({ inUse: true, idleStreak: 99 }), state());
    expect(action.kind).toBe("defer");
  });

  test("defers until the idle streak reaches the debounce threshold", () => {
    expect(decide(snapshot({ idleStreak: 2 }), state()).kind).toBe("defer");
    expect(decide(snapshot({ idleStreak: 3 }), state()).kind).toBe("update");
  });

  test("stays pinned to a digest a previous update failed on", () => {
    const action = decide(snapshot(), state({ pinnedDigest: "sha256:new" }));
    expect(action.kind).toBe("pinned");
  });

  test("the pin does not block a different, newer digest", () => {
    const action = decide(
      snapshot({ desired: { container: "app", ref: "app:latest", digest: "sha256:newer" } }),
      state({ pinnedDigest: "sha256:new" }),
    );
    expect(action.kind).toBe("update");
  });

  test("the pin wins even when the local tag digest equals the pinned digest", () => {
    // This is the post-rollback state: the pull moved the local tag to the bad
    // digest, so currentDigest == desired == pinned, but the container was put
    // back on the old image. The pin must win over keep, or the failed update
    // would be forgotten and the container treated as up to date on a bad image.
    const action = decide(
      snapshot({ desired: { container: "app", ref: "app:latest", digest: "sha256:bad" }, currentDigest: "sha256:bad" }),
      state({ pinnedDigest: "sha256:bad" }),
    );
    expect(action.kind).toBe("pinned");
  });

  // Regression: the decision must be driven by the manifest digest passed in as
  // currentDigest, never by the container's config ID (imageId). An earlier
  // version compared the registry manifest digest against inspect's `.Image`
  // config ID - two different hash namespaces that never match - so every
  // container read as permanently out of date and was updated on every pass.
  test("compares currentDigest, not the container's config id", () => {
    const container = runningContainer("app", { imageId: "config-abcdef" });
    // currentDigest equals desired: nothing to do, regardless of imageId.
    const action = decide(
      snapshot({
        container,
        currentDigest: "sha256:same",
        desired: { container: "app", ref: "app:latest", digest: "sha256:same" },
      }),
      state(),
    );
    expect(action.kind).toBe("keep");
  });

  test("updates when the image is not present locally at all", () => {
    // currentDigest undefined means nothing is pulled yet - a reason to update.
    const action = decide(snapshot({ currentDigest: undefined }), state());
    expect(action.kind).toBe("update");
    if (action.kind === "update") expect(action.from).toBeUndefined();
  });
});

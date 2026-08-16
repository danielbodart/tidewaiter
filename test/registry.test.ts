import { describe, expect, test } from "bun:test";
import { dockerConfigAuth, HttpRegistryClient, parseRef, retryAfterMillis } from "../src/registry.ts";
import type { Handler } from "../src/http.ts";

describe("parseRef", () => {
  test("a bare name is a Docker Hub library image on :latest", () => {
    expect(parseRef("nginx")).toEqual({ registry: "registry-1.docker.io", repository: "library/nginx", reference: "latest" });
  });

  test("a user/repo:tag on Docker Hub", () => {
    expect(parseRef("danielbodart/portical:1.2.3")).toEqual({
      registry: "registry-1.docker.io",
      repository: "danielbodart/portical",
      reference: "1.2.3",
    });
  });

  test("a private registry with a port", () => {
    expect(parseRef("registry.example.com:5000/team/app:2")).toEqual({
      registry: "registry.example.com:5000",
      repository: "team/app",
      reference: "2",
    });
  });

  test("a digest reference", () => {
    expect(parseRef("nginx@sha256:abc").reference).toBe("sha256:abc");
  });
});

describe("HttpRegistryClient", () => {
  test("returns the Docker-Content-Digest from a HEAD manifest", async () => {
    const handler: Handler = async () =>
      new Response("", { status: 200, headers: { "Docker-Content-Digest": "sha256:manifest" } });
    const digest = await new HttpRegistryClient(handler).digest("nginx:1.27");
    expect(digest).toBe("sha256:manifest");
  });

  test("follows a 401 Bearer challenge to a token then retries", async () => {
    const seen: string[] = [];
    const handler: Handler = async (req) => {
      const url = req.url;
      seen.push(url);
      if (url.includes("/manifests/") && !req.headers.get("Authorization")) {
        return new Response("unauthorized", {
          status: 401,
          headers: { "WWW-Authenticate": 'Bearer realm="https://auth.example/token",service="reg",scope="repository:library/nginx:pull"' },
        });
      }
      if (url.startsWith("https://auth.example/token")) {
        return Response.json({ token: "tok123" });
      }
      // Authorized manifest retry
      expect(req.headers.get("Authorization")).toBe("Bearer tok123");
      return new Response("", { status: 200, headers: { "Docker-Content-Digest": "sha256:authed" } });
    };
    const digest = await new HttpRegistryClient(handler).digest("nginx:1.27");
    expect(digest).toBe("sha256:authed");
    expect(seen.some((u) => u.startsWith("https://auth.example/token"))).toBe(true);
  });

  test("a missing digest header is an error naming the ref", async () => {
    const handler: Handler = async () => new Response("", { status: 200 });
    await expect(new HttpRegistryClient(handler).digest("nginx:1.27")).rejects.toThrow(/nginx:1.27/);
  });

  test("waits out a 429 with Retry-After then succeeds", async () => {
    let calls = 0;
    const handler: Handler = async () => {
      calls += 1;
      if (calls === 1) return new Response("slow down", { status: 429, headers: { "Retry-After": "1" } });
      return new Response("", { status: 200, headers: { "Docker-Content-Digest": "sha256:after-wait" } });
    };
    const waited: number[] = [];
    const client = new HttpRegistryClient(handler, undefined, async (ms) => { waited.push(ms); });

    const digest = await client.digest("nginx:1.27");
    expect(digest).toBe("sha256:after-wait");
    expect(waited).toEqual([1000]); // Retry-After: 1 second
  });

  test("uses a default backoff when a 429 carries no Retry-After", async () => {
    let calls = 0;
    const handler: Handler = async () => {
      calls += 1;
      if (calls === 1) return new Response("slow down", { status: 429 });
      return new Response("", { status: 200, headers: { "Docker-Content-Digest": "sha256:ok" } });
    };
    const waited: number[] = [];
    const client = new HttpRegistryClient(handler, undefined, async (ms) => { waited.push(ms); });

    await client.digest("nginx:1.27");
    expect(waited).toEqual([1000]); // default backoff
  });

  test("gives up with a rate-limited error after persistent 429s", async () => {
    const handler: Handler = async () => new Response("slow down", { status: 429, headers: { "Retry-After": "0" } });
    const client = new HttpRegistryClient(handler, undefined, async () => {});
    await expect(client.digest("nginx:1.27")).rejects.toThrow(/rate-limited/);
  });
});

describe("retryAfterMillis", () => {
  test("reads a seconds value", () => {
    expect(retryAfterMillis("5")).toBe(5000);
  });

  test("falls back to the default when absent or unparseable", () => {
    expect(retryAfterMillis(null)).toBe(1000);
    expect(retryAfterMillis("", 2000)).toBe(2000);
    expect(retryAfterMillis("not-a-date", 2000)).toBe(2000);
  });

  test("a past HTTP-date clamps to zero", () => {
    expect(retryAfterMillis("Thu, 01 Jan 1970 00:00:00 GMT")).toBe(0);
  });
});

describe("dockerConfigAuth", () => {
  test("encodes a base64 user:pass entry as X-Registry-Auth JSON", async () => {
    const config = {
      auths: { "https://index.docker.io/v1/": { auth: Buffer.from("alice:secret").toString("base64") } },
    };
    const path = `${import.meta.dir}/fixtures/tmp-docker-config.json`;
    await Bun.write(path, JSON.stringify(config));
    try {
      const auth = await dockerConfigAuth(path);
      const value = auth("registry-1.docker.io");
      expect(value).toBeDefined();
      const decoded = JSON.parse(Buffer.from(value!, "base64").toString("utf8"));
      expect(decoded).toMatchObject({ username: "alice", password: "secret" });
    } finally {
      await Bun.file(path).delete().catch(() => {});
    }
  });

  test("a missing config file means anonymous everywhere", async () => {
    const auth = await dockerConfigAuth(`${import.meta.dir}/fixtures/does-not-exist.json`);
    expect(auth("registry-1.docker.io")).toBeUndefined();
  });
});

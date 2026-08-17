import { describe, expect, test } from "bun:test";
import { HttpDockerClient, netBytesOf, toRunning, toSpec, type RawContainerInspect } from "../src/docker.ts";
import type { Handler } from "../src/http.ts";

const nginx = (await Bun.file(`${import.meta.dir}/fixtures/inspect-nginx.json`).json()) as RawContainerInspect;
const multinet = (await Bun.file(`${import.meta.dir}/fixtures/inspect-multinet.json`).json()) as RawContainerInspect;

describe("toRunning", () => {
  test("reads id, pid, config id and health off an inspect", () => {
    const running = toRunning(nginx);
    expect(running.id).toBe("abc123def456");
    expect(running.pid).toBe(4242);
    // `.Image` is the config ID, kept for diagnostics only - deliberately NOT
    // the manifest digest an update decision uses (that comes from imageDigest).
    expect(running.imageId).toBe("sha256:aaaa1111");
    expect(running.health).toBe("healthy");
  });

  test("maps an unknown health status to none", () => {
    const running = toRunning({ ...nginx, State: { Pid: 1 } });
    expect(running.health).toBe("none");
  });
});

describe("toSpec", () => {
  test("strips the leading slash from the name", () => {
    expect(toSpec(nginx).name).toBe("web");
  });

  test("reads published ports from PortBindings, de-duplicating the v4/v6 pair", () => {
    expect(toSpec(nginx).published).toEqual([{ hostPort: 8080, containerPort: 80, protocol: "tcp" }]);
  });

  test("reads a bind mount", () => {
    expect(toSpec(nginx).mounts).toEqual([
      { type: "bind", source: "/srv/site", target: "/usr/share/nginx/html", readonly: true, anonymous: false },
    ]);
  });

  test("reads multiple networks with their aliases and static addresses", () => {
    const spec = toSpec(multinet);
    expect(spec.networks).toEqual([
      // frontend: no static pin, but a runtime IP IPAM handed out (what a client
      // reaches, and what port-connect probes). backend: statically pinned.
      { name: "frontend", aliases: ["api"], ipv4Address: undefined, ipv6Address: undefined, ipAddress: "172.27.0.9" },
      { name: "backend", aliases: ["api-internal"], ipv4Address: "172.28.0.5", ipv6Address: undefined, ipAddress: "172.28.0.5" },
    ]);
  });

  test("drops a 'no' restart policy to undefined", () => {
    const spec = toSpec({ ...nginx, HostConfig: { ...nginx.HostConfig, RestartPolicy: { Name: "no" } } });
    expect(spec.restartPolicy).toBeUndefined();
  });
});

describe("HttpDockerClient", () => {
  /** A Handler that records every request and replies from a table. */
  function recording(routes: (req: Request) => Response | Promise<Response>) {
    const requests: Request[] = [];
    const handler: Handler = async (req) => {
      requests.push(req);
      return routes(req);
    };
    return { handler, requests };
  }

  test("imageDigest reads the first RepoDigest, stripping the repo", async () => {
    const { handler } = recording(() =>
      Response.json({ RepoDigests: ["nginx@sha256:beef"] }),
    );
    const digest = await new HttpDockerClient(handler).imageDigest("nginx:1.27");
    expect(digest).toBe("sha256:beef");
  });

  test("networkDriver reads .Driver and asks for the network by name", async () => {
    const { handler, requests } = recording(() => Response.json({ Driver: "macvlan" }));
    const driver = await new HttpDockerClient(handler).networkDriver("pub-net");
    expect(driver).toBe("macvlan");
    expect(requests[0]?.url).toContain("/networks/pub-net");
  });

  test("networkDriver returns undefined when the network is gone (404)", async () => {
    const { handler } = recording(() => new Response("no such network", { status: 404 }));
    expect(await new HttpDockerClient(handler).networkDriver("ghost-net")).toBeUndefined();
  });

  test("imageDigest returns undefined for an image not present locally", async () => {
    const { handler } = recording(() => new Response("no such image", { status: 404 }));
    expect(await new HttpDockerClient(handler).imageDigest("ghost:latest")).toBeUndefined();
  });

  test("pull sends X-Registry-Auth and fully drains the streamed body", async () => {
    let drained = false;
    const { handler, requests } = recording(
      () =>
        new Response(
          new ReadableStream({
            start(controller) {
              controller.enqueue(new TextEncoder().encode('{"status":"Pulling"}\n'));
              controller.enqueue(new TextEncoder().encode('{"status":"Done"}\n'));
              drained = true;
              controller.close();
            },
          }),
        ),
    );
    await new HttpDockerClient(handler).pull("nginx:1.27", "authblob");
    expect(requests[0]?.headers.get("X-Registry-Auth")).toBe("authblob");
    expect(drained).toBe(true);
  });

  test("stop passes the grace period as ?t=", async () => {
    const { handler, requests } = recording(() => new Response("", { status: 204 }));
    await new HttpDockerClient(handler).stop("web", 15);
    expect(requests[0]?.url).toContain("/stop?t=15");
  });

  test("a Docker error names the endpoint that failed", async () => {
    const { handler } = recording(() => new Response("boom", { status: 500 }));
    await expect(new HttpDockerClient(handler).start("web")).rejects.toThrow(/start/);
  });

  test("netBytes sums rx+tx across interfaces from a single /stats snapshot", async () => {
    const { handler, requests } = recording(() =>
      Response.json({ networks: { eth0: { rx_bytes: 100, tx_bytes: 50 }, eth1: { rx_bytes: 5, tx_bytes: 1 } } }),
    );
    const bytes = await new HttpDockerClient(handler).netBytes("web");
    expect(bytes).toBe(156);
    expect(requests[0]?.url).toContain("/stats?stream=false");
  });
});

describe("netBytesOf", () => {
  test("totals rx+tx over every interface", () => {
    expect(netBytesOf({ networks: { eth0: { rx_bytes: 10, tx_bytes: 20 } } })).toBe(30);
  });

  test("a container with no networks block totals zero", () => {
    expect(netBytesOf({})).toBe(0);
    expect(netBytesOf({ networks: null })).toBe(0);
  });
});

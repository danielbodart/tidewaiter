import { describe, expect, test } from "bun:test";
import { toSpec, type RawContainerInspect } from "../src/docker.ts";
import {
  extraNetworks,
  planSwap,
  recreatable,
  rollbackName,
  toCreatePayload,
} from "../src/recreate.ts";
import { spec } from "./fakes/docker.ts";

const nginx = toSpec((await Bun.file(`${import.meta.dir}/fixtures/inspect-nginx.json`).json()) as RawContainerInspect);
const multinet = toSpec((await Bun.file(`${import.meta.dir}/fixtures/inspect-multinet.json`).json()) as RawContainerInspect);
const anonvol = toSpec((await Bun.file(`${import.meta.dir}/fixtures/inspect-anonvol.json`).json()) as RawContainerInspect);

describe("toCreatePayload", () => {
  test("carries env, labels, cmd and entrypoint verbatim", () => {
    const payload = toCreatePayload(nginx) as Record<string, unknown>;
    expect(payload.Env).toEqual(["PATH=/usr/local/sbin:/usr/local/bin", "NGINX_VERSION=1.27"]);
    expect(payload.Cmd).toEqual(["nginx", "-g", "daemon off;"]);
    expect(payload.Entrypoint).toEqual(["/docker-entrypoint.sh"]);
    expect((payload.Labels as Record<string, string>)["tidewaiter.enable"]).toBe("true");
  });

  test("puts bind mounts in Binds with the read-only flag", () => {
    const payload = toCreatePayload(nginx) as { HostConfig: { Binds: string[] } };
    expect(payload.HostConfig.Binds).toEqual(["/srv/site:/usr/share/nginx/html:ro"]);
  });

  test("reproduces port bindings by container port and protocol", () => {
    const payload = toCreatePayload(nginx) as { HostConfig: { PortBindings: Record<string, { HostPort: string }[]> } };
    expect(payload.HostConfig.PortBindings["80/tcp"]).toEqual([{ HostPort: "8080" }]);
  });

  test("carries restart policy, stop signal and stop timeout", () => {
    const payload = toCreatePayload(nginx) as Record<string, unknown>;
    expect(payload.StopSignal).toBe("SIGQUIT");
    expect(payload.StopTimeout).toBe(15);
    expect((payload.HostConfig as { RestartPolicy: unknown }).RestartPolicy).toEqual({
      Name: "unless-stopped",
      MaximumRetryCount: 0,
    });
  });

  test("carries the healthcheck", () => {
    const payload = toCreatePayload(nginx) as { Healthcheck?: { Test: string[] } };
    expect(payload.Healthcheck?.Test).toEqual(["CMD-SHELL", "curl -f http://localhost/ || exit 1"]);
  });

  test("named volumes go in Mounts long-form, not Binds", () => {
    const payload = toCreatePayload(multinet) as { HostConfig: { Mounts: Record<string, unknown>[] } };
    expect(payload.HostConfig.Mounts).toEqual([{ Type: "volume", Source: "api-data", Target: "/data", ReadOnly: false }]);
  });

  test("carries caps, sysctls, extra hosts and user", () => {
    const payload = toCreatePayload(multinet) as Record<string, unknown>;
    const host = payload.HostConfig as Record<string, unknown>;
    expect(host.CapAdd).toEqual(["NET_ADMIN"]);
    expect(host.CapDrop).toEqual(["MKNOD"]);
    expect(host.Sysctls).toEqual({ "net.core.somaxconn": "1024" });
    expect(host.ExtraHosts).toEqual(["db:10.0.0.9"]);
    expect(payload.User).toBe("app");
  });

  test("attaches only the first network at create time", () => {
    const payload = toCreatePayload(multinet) as { NetworkingConfig?: { EndpointsConfig: Record<string, unknown> } };
    const endpoints = payload.NetworkingConfig?.EndpointsConfig ?? {};
    expect(Object.keys(endpoints)).toEqual(["frontend"]);
  });

  test("never leaks a runtime-only field into the payload", () => {
    const payload = toCreatePayload(nginx) as Record<string, unknown>;
    expect(payload.Id).toBeUndefined();
    expect(payload.State).toBeUndefined();
    expect(payload.NetworkSettings).toBeUndefined();
  });
});

describe("extraNetworks", () => {
  test("is every network beyond the first", () => {
    expect(extraNetworks(multinet).map((n) => n.name)).toEqual(["backend"]);
  });

  test("is empty for a single-network container", () => {
    expect(extraNetworks(nginx)).toEqual([]);
  });
});

describe("recreatable", () => {
  test("refuses a container with an anonymous volume rather than lose its data", () => {
    const verdict = recreatable(anonvol);
    expect(verdict.ok).toBe(false);
    if (!verdict.ok) expect(verdict.reason).toContain("anonymous volume");
  });

  test("allows a container with only named volumes and binds", () => {
    expect(recreatable(nginx).ok).toBe(true);
    expect(recreatable(multinet).ok).toBe(true);
  });
});

describe("planSwap", () => {
  const base = spec("web", {
    networks: [
      { name: "frontend", aliases: [] },
      { name: "backend", aliases: [] },
    ],
  });
  const plan = planSwap(base, { ...base, image: "web:new" }, 12);

  test("stops, parks (renames) the old, creates and starts the new under the original name", () => {
    expect(plan.swap.map((s) => s.op)).toEqual(["stop", "rename", "create", "connect", "start"]);
    const rename = plan.swap.find((s) => s.op === "rename");
    expect(rename).toEqual({ op: "rename", container: "web", to: rollbackName("web") });
  });

  test("connects the extra networks after create", () => {
    const connect = plan.swap.find((s) => s.op === "connect");
    expect(connect).toEqual({ op: "connect", container: "web", network: { name: "backend", aliases: [] } });
  });

  test("commit removes the parked old container", () => {
    expect(plan.commit).toEqual([{ op: "remove", container: rollbackName("web"), force: true }]);
  });

  test("rollback removes the new one and restores the old name", () => {
    expect(plan.rollback.map((s) => s.op)).toEqual(["remove", "rename", "start"]);
    expect(plan.rollback[1]).toEqual({ op: "rename", container: rollbackName("web"), to: "web" });
  });
});

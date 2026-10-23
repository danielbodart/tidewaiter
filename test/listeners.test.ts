import { describe, expect, test } from "bun:test";
import { parseListeners, unitBoundPorts, type Listener } from "../src/listeners.ts";

const fixture = await Bun.file(new URL("./fixtures/ss-Hlntu-e.txt", import.meta.url)).text();

describe("parseListeners", () => {
  // Captured with `ss -Hlntu -e` on a NixOS host (systemd 260, iproute2 7.0).
  test("reads protocol, port and cgroup off real output", () => {
    const listeners = parseListeners(fixture);
    expect(listeners).toHaveLength(15);
    expect(listeners[0]).toEqual({ protocol: "udp", port: 53, cgroup: "/system.slice/frisket-dns.socket" });
  });

  test("reads the port after an interface suffix and from a bracketed v6 address", () => {
    const listeners = parseListeners(fixture);
    expect(listeners).toContainEqual({ protocol: "udp", port: 53, cgroup: "/system.slice/systemd-resolved.service" });
    expect(listeners).toContainEqual({ protocol: "udp", port: 546, cgroup: "/system.slice/NetworkManager.service" });
    expect(listeners).toContainEqual({
      protocol: "tcp", port: 64325, cgroup: "/user.slice/user-1000.slice/user@1000.service/app.slice/docker.service",
    });
  });

  test("a dual-stack wildcard, and a line with no cgroup field", () => {
    const text = [
      "tcp LISTEN 0 4096 *:25565 *:* ino:9 sk:9 cgroup:/system.slice/chandlery-java.service <->",
      "tcp LISTEN 0 128 0.0.0.0:22 0.0.0.0:* ino:1234 sk:30 <->",
      "icmp6 UNCONN 0 0 *:58 *:* ino:1 sk:1",
    ].join("\n");
    expect(parseListeners(text)).toEqual([
      { protocol: "tcp", port: 25565, cgroup: "/system.slice/chandlery-java.service" },
      { protocol: "tcp", port: 22, cgroup: "" },
    ]);
  });
});

describe("unitBoundPorts", () => {
  const listeners: Listener[] = [
    { protocol: "udp", port: 19132, cgroup: "/system.slice/chandlery-bedrock.service" },
    { protocol: "udp", port: 19133, cgroup: "/system.slice/chandlery-bedrock.service/payload" },
    { protocol: "tcp", port: 8080, cgroup: "/system.slice/chandlery-bedrock.service" },
    { protocol: "udp", port: 19134, cgroup: "/system.slice/chandlery-bedrock.service2" },
  ];
  const bound = unitBoundPorts(async () => listeners, "/system.slice/chandlery-bedrock.service");

  test("the unit's own cgroup and those beneath it, by family", async () => {
    expect([...await bound("udp")].sort()).toEqual([19132, 19133]);
    expect([...await bound("tcp")]).toEqual([8080]);
  });

  test("a sibling sharing a prefix is not the unit", async () => {
    expect(await bound("udp")).not.toContain(19134);
  });

  test("cannot tell, rather than nothing bound, when the unit has no cgroup", async () => {
    await expect(unitBoundPorts(async () => listeners, "")("udp")).rejects.toThrow("no cgroup");
  });

  test("cannot tell when ss gives no cgroups at all", async () => {
    const old = unitBoundPorts(async () => [{ protocol: "udp", port: 19132, cgroup: "" }], "/system.slice/x.service");
    await expect(old("udp")).rejects.toThrow("no socket cgroups");
  });
});

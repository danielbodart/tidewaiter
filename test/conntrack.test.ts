import { describe, expect, test } from "bun:test";
import { parseConntrack } from "../src/conntrack.ts";

const fixture = await Bun.file(`${import.meta.dir}/fixtures/conntrack-L.txt`).text();

describe("parseConntrack", () => {
  test("reads tcp and udp flows and drops everything else", () => {
    const entries = parseConntrack(fixture);
    expect(entries.every((e) => e.protocol === "tcp" || e.protocol === "udp")).toBe(true);
    // Seven lines, one of which is ICMP - so six entries.
    expect(entries).toHaveLength(6);
  });

  test("takes the original-direction destination port, not the reply's", () => {
    const entries = parseConntrack(fixture);
    const ports = entries.map((e) => e.destinationPort).sort((a, b) => a - b);
    // 8080, 19132, 19132, 25565, 25565, 28080 - never a client's ephemeral source port.
    expect(ports).toEqual([8080, 19132, 19132, 25565, 25565, 28080]);
  });

  test("marks ASSURED flows", () => {
    const entries = parseConntrack(fixture);
    const assured25565 = entries.filter((e) => e.destinationPort === 25565 && e.assured);
    const unassured25565 = entries.filter((e) => e.destinationPort === 25565 && !e.assured);
    expect(assured25565).toHaveLength(1);
    expect(unassured25565).toHaveLength(1);
  });

  test("captures the TCP state, and leaves UDP flows stateless", () => {
    const entries = parseConntrack(fixture);
    const established = entries.find((e) => e.destinationPort === 8080);
    const timeWait = entries.find((e) => e.destinationPort === 28080);
    const udp = entries.find((e) => e.protocol === "udp");
    expect(established?.state).toBe("ESTABLISHED");
    expect(timeWait?.state).toBe("TIME_WAIT"); // a closed connection cooling down
    expect(udp?.state).toBeUndefined();
  });

  test("ignores blank lines and garbage", () => {
    expect(parseConntrack("\n   \nnonsense line\n")).toEqual([]);
  });
});

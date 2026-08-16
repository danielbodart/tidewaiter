import { describe, expect, test } from "bun:test";
import { parseConntrack } from "../src/conntrack.ts";

const fixture = await Bun.file(`${import.meta.dir}/fixtures/conntrack-L.txt`).text();

describe("parseConntrack", () => {
  test("reads tcp and udp flows and drops everything else", () => {
    const entries = parseConntrack(fixture);
    expect(entries.every((e) => e.protocol === "tcp" || e.protocol === "udp")).toBe(true);
    // Six lines, one of which is ICMP - so five entries.
    expect(entries).toHaveLength(5);
  });

  test("takes the original-direction destination port, not the reply's", () => {
    const entries = parseConntrack(fixture);
    const ports = entries.map((e) => e.destinationPort).sort((a, b) => a - b);
    // 8080, 19132, 19132, 25565, 25565 - never a client's ephemeral source port.
    expect(ports).toEqual([8080, 19132, 19132, 25565, 25565]);
  });

  test("marks ASSURED flows", () => {
    const entries = parseConntrack(fixture);
    const assured25565 = entries.filter((e) => e.destinationPort === 25565 && e.assured);
    const unassured25565 = entries.filter((e) => e.destinationPort === 25565 && !e.assured);
    expect(assured25565).toHaveLength(1);
    expect(unassured25565).toHaveLength(1);
  });

  test("ignores blank lines and garbage", () => {
    expect(parseConntrack("\n   \nnonsense line\n")).toEqual([]);
  });
});

import type { ConnectOutcome, TcpProbe } from "../../src/health.ts";

/**
 * A stand-in for the real connect()-to-an-address TCP probe.
 *
 * A test declares what each endpoint answers. The ergonomic default matches how
 * most tests think - "these host PORTS are up" via `listening(...)`, matching any
 * host - while `acceptingAt`/`refusingAt`/`unreachableAt` pin a specific
 * `host:port` when a test cares about the exact address (bridge IP vs loopback).
 * Anything not declared reads as `refused` (a wired route with nothing serving),
 * which is the common "not up yet" case.
 */
export class FakeTcpProbe implements TcpProbe {
  /** host:port -> outcome, the address-specific answers. */
  private readonly answers = new Map<string, ConnectOutcome>();
  /** Ports that accept on ANY host, the port-only convenience. */
  private readonly openPorts = new Set<number>();
  /** Records every port probed, for assertions (host-agnostic). */
  readonly probed: number[] = [];
  /** Records every full endpoint probed, for address-level assertions. */
  readonly probedTargets: { host: string; port: number }[] = [];

  /** Mark host ports as accepting on any address (the common case). */
  listening(...ports: number[]): this {
    for (const port of ports) this.openPorts.add(port);
    return this;
  }

  acceptingAt(host: string, port: number): this {
    this.answers.set(`${host}:${port}`, "accepted");
    return this;
  }

  refusingAt(host: string, port: number): this {
    this.answers.set(`${host}:${port}`, "refused");
    return this;
  }

  unreachableAt(host: string, port: number): this {
    this.answers.set(`${host}:${port}`, "unreachable");
    return this;
  }

  async connect(host: string, port: number): Promise<ConnectOutcome> {
    this.probed.push(port);
    this.probedTargets.push({ host, port });
    const pinned = this.answers.get(`${host}:${port}`);
    if (pinned !== undefined) return pinned;
    return this.openPorts.has(port) ? "accepted" : "refused";
  }
}

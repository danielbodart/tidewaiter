import type { TcpProbe } from "../../src/health.ts";

/**
 * A stand-in for the real connect()-to-a-host-port TCP probe.
 *
 * `open` is the set of host ports a test says are accepting connections; any
 * port not in it reads as refused. `failWith` makes connectable() itself throw,
 * to check that a probe error is handled (the real probe swallows it to false).
 */
export class FakeTcpProbe implements TcpProbe {
  readonly open = new Set<number>();
  failWith?: Error;
  /** Records every port probed, for assertions. */
  readonly probed: number[] = [];

  listening(...ports: number[]): this {
    for (const port of ports) this.open.add(port);
    return this;
  }

  async connectable(hostPort: number): Promise<boolean> {
    this.probed.push(hostPort);
    if (this.failWith) throw this.failWith;
    return this.open.has(hostPort);
  }
}

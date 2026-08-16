import type { RegistryClient } from "../../src/registry.ts";
import type { Digest } from "../../src/model.ts";

/**
 * The registry as an in-memory digest map.
 *
 * Tests set `digests[ref]` to what the tag should resolve to; a ref with no
 * entry throws, as an unreachable registry would, which exercises the daemon's
 * "leave it alone" path. `calls` records every lookup for assertions.
 */
export class FakeRegistry implements RegistryClient {
  digests: Record<string, Digest> = {};
  failWith?: Error;
  readonly calls: string[] = [];

  async digest(ref: string): Promise<Digest> {
    this.calls.push(ref);
    if (this.failWith) throw this.failWith;
    const digest = this.digests[ref];
    if (digest === undefined) throw new Error(`no digest configured for ${ref}`);
    return digest;
  }
}

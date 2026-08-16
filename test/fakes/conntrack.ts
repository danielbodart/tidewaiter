import type { ConntrackSource } from "../../src/conntrack.ts";

/**
 * The host conntrack table as canned `conntrack -L` text.
 *
 * Tests set `text` to captured output (or the empty default for "no flows");
 * `calls` counts dumps so a test can prove the daemon dumps once per pass rather
 * than once per container. Set `failWith` to exercise the fail-safe path.
 */
export class FakeConntrackSource implements ConntrackSource {
  text = "";
  failWith?: Error;
  calls = 0;

  async dump(): Promise<string> {
    this.calls += 1;
    if (this.failWith) throw this.failWith;
    return this.text;
  }
}

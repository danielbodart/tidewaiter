import type { NetnsReader, NetTable } from "../../src/netns.ts";

/**
 * A stand-in for reading /proc/<pid>/net/{tcp,udp} tables.
 *
 * Tables are keyed `${pid}:${table}`, so a test hands the exact socket-table
 * text a detector or health probe should see for a given container, with no
 * real PID and no filesystem.
 */
export class FakeNetnsReader implements NetnsReader {
  readonly tables = new Map<string, string>();
  /**
   * Make read() throw, as the real reader does when /proc/<pid>/net/* is not
   * there yet - a freshly-started or just-exited container. Models the ENOENT a
   * live host throws that an in-memory default of "" would hide.
   */
  failWith?: Error;

  set(pid: number, table: NetTable, text: string): this {
    this.tables.set(`${pid}:${table}`, text);
    return this;
  }

  async read(pid: number, table: NetTable): Promise<string> {
    if (this.failWith) throw this.failWith;
    return this.tables.get(`${pid}:${table}`) ?? "";
  }
}

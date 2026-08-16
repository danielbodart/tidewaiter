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

  set(pid: number, table: NetTable, text: string): this {
    this.tables.set(`${pid}:${table}`, text);
    return this;
  }

  async read(pid: number, table: NetTable): Promise<string> {
    return this.tables.get(`${pid}:${table}`) ?? "";
  }
}

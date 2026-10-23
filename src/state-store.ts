import { mkdir, readFile, rename, writeFile } from "node:fs/promises";
import { dirname } from "node:path";

/**
 * What the systemd source must remember across its own restarts, per unit.
 *
 * Unlike Docker's in-memory state, this is persisted: a rollback leaves the
 * failed version as a store path the module still asks for, and a forgotten
 * pin would retry it - and restart the unit under its players' feet - once on
 * every daemon restart. The idle streak is not here: after a restart it counts
 * up again, which only ever delays.
 */
export interface UnitMemory {
  /** The desired path whose update failed its health check; left alone until desired moves. */
  readonly pinned?: string;
  /** The desired path swap steps have been failing on, and how many times running. */
  readonly failing?: string;
  readonly swapFailures?: number;
  /**
   * The profile generation last known good: committed by a healthy swap, or
   * the one already running when the unit was first seen healthy. What crash
   * recovery rolls back to.
   */
  readonly good?: number;
}

export type Memories = Readonly<Record<string, UnitMemory>>;

export interface StateStore {
  load(): Promise<Memories>;
  save(memories: Memories): Promise<void>;
}

/**
 * A JSON file, written whole and atomically (a temp file renamed over it), so
 * a crash mid-write leaves the previous state rather than half of one. A file
 * that is not there is no memories; one that cannot be parsed is an error,
 * because silently forgetting pins is exactly what this file exists to stop.
 */
export class FileStateStore implements StateStore {
  constructor(readonly path: string) {}

  async load(): Promise<Memories> {
    let text: string;
    try {
      text = await readFile(this.path, "utf8");
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === "ENOENT") return {};
      throw error;
    }
    const json = JSON.parse(text) as unknown;
    if (typeof json !== "object" || json === null || Array.isArray(json)) {
      throw new Error(`${this.path} is not a JSON object`);
    }
    return json as Memories;
  }

  async save(memories: Memories): Promise<void> {
    await mkdir(dirname(this.path), { recursive: true });
    const temporary = `${this.path}.tmp`;
    await writeFile(temporary, `${JSON.stringify(memories, null, 2)}\n`, { mode: 0o644 });
    await rename(temporary, this.path);
  }
}

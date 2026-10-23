import { mkdir, readFile, rename, rm, writeFile } from "node:fs/promises";
import { dirname } from "node:path";
import type { SourceName } from "./source.ts";

/** Where the NixOS module has the daemon write its status, and where `tidewaiter status` looks. */
export const DEFAULT_STATUS_FILE = "/run/tidewaiter/status.json";

/** One workload as the daemon last saw it. */
export interface WorkloadStatus {
  readonly workload: string;
  readonly source: SourceName;
  /** In use right now, by the last pass or tick; could-not-tell reads as busy. */
  readonly busy: boolean;
  /** Idle for its whole idle-samples debounce at the last pass. */
  readonly settled: boolean;
}

/** Where an automatic reboot stands, when the daemon has one configured. */
export interface RebootStatus {
  /** The window, as given: "03:00-05:00". */
  readonly window: string;
  /** Why a reboot is needed ("kernel changed since boot"), or undefined when it is not. */
  readonly needed?: string;
  /** What the last pass concluded: "not needed", "waiting for ...", "rebooting". */
  readonly verdict: string;
}

/**
 * What the daemon knows right now, for `tidewaiter status` and for scripts.
 *
 * Written whole after every pass, every activity tick and the start of every
 * swap, so a reader sees one consistent moment and never a half-written file.
 */
export interface Status {
  /** When this was written, ISO 8601. */
  readonly updated: string;
  /**
   * Seconds after `updated` by which the daemon will have written again: a
   * status older than that was left by a daemon that is no longer running.
   */
  readonly staleAfter?: number;
  /** The workloads in use, sorted. */
  readonly busy: readonly string[];
  /** The workloads being swapped right now, sorted. */
  readonly swapping: readonly string[];
  readonly workloads: readonly WorkloadStatus[];
  readonly reboot?: RebootStatus;
}

/** Where the daemon puts its status; NoStatus when nobody asked for it. */
export interface StatusSink {
  write(status: Status): Promise<void>;
  /** The daemon is stopping: what it said is no longer true. */
  clear(): Promise<void>;
}

export const NoStatus: StatusSink = {
  async write() {},
  async clear() {},
};

/**
 * A JSON file, replaced by rename so a reader never sees it half-written.
 *
 * Plain JSON in a world-readable directory - /run/tidewaiter, systemd's
 * RuntimeDirectory= under the module - so anyone can ask whether the host is
 * in use without root, without D-Bus and without the daemon answering.
 */
export class FileStatus implements StatusSink {
  constructor(readonly path: string) {}

  async write(status: Status): Promise<void> {
    await mkdir(dirname(this.path), { recursive: true });
    const temporary = `${this.path}.${process.pid}.tmp`;
    await writeFile(temporary, `${JSON.stringify(status, null, 2)}\n`);
    await rename(temporary, this.path);
  }

  async clear(): Promise<void> {
    await rm(this.path, { force: true });
  }
}

/** The status at `path`, or undefined when there is none - no daemon writing it. */
export async function readStatus(path: string): Promise<Status | undefined> {
  let text: string;
  try {
    text = await readFile(path, "utf8");
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return undefined;
    throw error;
  }
  return JSON.parse(text) as Status;
}

/**
 * Whether the daemon that wrote this has stopped writing: one killed outside
 * systemd leaves its last status behind, which says nothing about now.
 */
export function isStale(status: Status, now: number): boolean {
  return status.staleAfter !== undefined && now - Date.parse(status.updated) > status.staleAfter * 1000;
}

/** Whether anything is in use or being swapped: what `tidewaiter status` exits 1 for. */
export function isBusy(status: Status): boolean {
  return status.busy.length > 0 || status.swapping.length > 0;
}

/**
 * `tidewaiter status`, for a person: a headline, one line per workload, and
 * where an automatic reboot stands.
 */
export function describeStatus(status: Status, now: number): string[] {
  const list = (names: readonly string[]) => names.join(", ");
  const age = Math.max(0, Math.round((now - Date.parse(status.updated)) / 1000));
  const lines = [
    status.swapping.length > 0 ? `Busy: swapping ${list(status.swapping)}`
      : status.busy.length > 0 ? `Busy: ${list(status.busy)} in use`
      : "Idle: nothing in use, nothing being swapped",
  ];
  for (const { workload, source, busy, settled } of status.workloads) {
    const state = status.swapping.includes(workload) ? "swapping"
      : busy ? "in use"
      : settled ? "idle"
      : "idle, not yet for long enough";
    lines.push(`  ${workload} (${source})  ${state}`);
  }
  if (status.reboot) {
    const { window, needed, verdict } = status.reboot;
    lines.push(needed === undefined
      ? `Automatic reboot (${window}): not needed`
      : `Automatic reboot (${window}): needed, ${needed}; ${verdict}`);
  }
  lines.push(`As of ${age}s ago`);
  return lines;
}

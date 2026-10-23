import type { ClosureBinder } from "../../src/closure.ts";
import type { ListenerSource } from "../../src/listeners.ts";
import type { Profiles } from "../../src/profile.ts";
import type { Memories, StateStore } from "../../src/state-store.ts";
import type { JobResult, SystemdClient, UnitState } from "../../src/systemd.ts";
import type { UnitFile, UnitsDirectory } from "../../src/units.ts";

/**
 * Every fake below writes what it was asked to do into one shared journal, so
 * a test can assert the ORDER across them - closure bound before the reload,
 * the reload before the profile moves, the profile before the restart.
 */
export type Journal = string[];

/** A running unit, as systemd would describe it. */
export function unitState(name: string, overrides: Partial<UnitState> = {}): UnitState {
  return {
    name,
    loadState: "loaded",
    activeState: "active",
    subState: "running",
    result: "success",
    nRestarts: 0,
    mainPid: 4242,
    controlGroup: `/system.slice/${name}`,
    invocationId: "0001",
    ...overrides,
  };
}

/**
 * systemd in memory: units by name, and a restart that, by default, brings the
 * unit back up healthy on a new invocation. `onRestart` replaces that for a
 * test that wants a failed job, a crash, or a unit that never binds; it may
 * change the unit's state and returns the job's result.
 */
export class FakeSystemd implements SystemdClient {
  readonly units = new Map<string, UnitState>();
  onRestart?: (name: string, fake: FakeSystemd) => JobResult;
  /** Make the next restart throw, as a job that could not be queued does. */
  restartThrows?: Error;
  failWith?: Error;
  private invocations = 1;

  constructor(readonly journal: Journal = []) {}

  add(state: UnitState): this {
    this.units.set(state.name, state);
    return this;
  }

  /** Change a unit's state in place. */
  patch(name: string, changes: Partial<UnitState>): void {
    this.units.set(name, { ...this.units.get(name)!, ...changes });
  }

  /** A fresh invocation id, as each start gets. */
  nextInvocation(): string {
    this.invocations += 1;
    return String(this.invocations).padStart(4, "0");
  }

  async unit(name: string): Promise<UnitState> {
    if (this.failWith) throw this.failWith;
    return this.units.get(name) ?? unitState(name, {
      loadState: "not-found", activeState: "inactive", subState: "dead", mainPid: 0, controlGroup: "", invocationId: "",
    });
  }

  async restart(name: string): Promise<JobResult> {
    this.journal.push(`restart ${name}`);
    if (this.restartThrows) {
      const error = this.restartThrows;
      this.restartThrows = undefined;
      throw error;
    }
    if (this.onRestart) return this.onRestart(name, this);
    this.patch(name, { activeState: "active", subState: "running", result: "success", invocationId: this.nextInvocation() });
    return "done";
  }

  async daemonReload(): Promise<void> {
    this.journal.push("daemon-reload");
  }
}

/**
 * Nix profiles in memory: numbered generations per profile, the current one,
 * and nix-env's numbering (a new generation is one past the highest).
 */
export class FakeProfiles implements Profiles {
  readonly profiles = new Map<string, { generations: Map<number, string>; current: number }>();
  /** Make the next set() throw, as nix-env does when the Nix daemon is unreachable. */
  setThrows?: Error;

  constructor(readonly journal: Journal = []) {}

  /** Seed a profile with generations 1..n of these paths, the last current. */
  seed(profile: string, ...paths: string[]): this {
    this.profiles.set(profile, {
      generations: new Map(paths.map((path, index) => [index + 1, path])),
      current: paths.length,
    });
    return this;
  }

  generationsOf(profile: string): number[] {
    return [...(this.profiles.get(profile)?.generations.keys() ?? [])].sort((a, b) => a - b);
  }

  async current(profile: string): Promise<string | undefined> {
    const entry = this.profiles.get(profile);
    return entry?.generations.get(entry.current);
  }

  async generation(profile: string): Promise<number | undefined> {
    return this.profiles.get(profile)?.current;
  }

  async target(profile: string, generation: number): Promise<string | undefined> {
    return this.profiles.get(profile)?.generations.get(generation);
  }

  async set(profile: string, storePath: string): Promise<void> {
    this.journal.push(`set ${storePath}`);
    if (this.setThrows) {
      const error = this.setThrows;
      this.setThrows = undefined;
      throw error;
    }
    const entry = this.profiles.get(profile) ?? { generations: new Map<number, string>(), current: 0 };
    const next = Math.max(0, ...entry.generations.keys()) + 1;
    entry.generations.set(next, storePath);
    entry.current = next;
    this.profiles.set(profile, entry);
  }

  async switchGeneration(profile: string, generation: number): Promise<void> {
    this.journal.push(`switch ${generation}`);
    const entry = this.profiles.get(profile)!;
    if (!entry.generations.has(generation)) throw new Error(`profile version ${generation} does not exist`);
    entry.current = generation;
  }

  async deleteGeneration(profile: string, generation: number): Promise<void> {
    this.journal.push(`delete ${generation}`);
    const entry = this.profiles.get(profile)!;
    if (generation !== entry.current) entry.generations.delete(generation);
  }

  async prune(profile: string, keep: number): Promise<void> {
    this.journal.push(`prune +${keep + 1}`);
    const entry = this.profiles.get(profile)!;
    const newest = [...entry.generations.keys()].sort((a, b) => b - a).slice(0, keep + 1);
    for (const generation of [...entry.generations.keys()]) {
      if (!newest.includes(generation) && generation !== entry.current) entry.generations.delete(generation);
    }
  }
}

/** Records each drop-in it would write. */
export class FakeClosure implements ClosureBinder {
  readonly bound: { unit: string; profile: string; target: string }[] = [];

  constructor(readonly journal: Journal = []) {}

  async bind(unit: string, profile: string, target: string): Promise<void> {
    this.journal.push(`bind ${target}`);
    this.bound.push({ unit, profile, target });
  }
}

/** `ss -Hlntu -e` as canned text. */
export class FakeListeners implements ListenerSource {
  text = "";

  /** Add a socket in a unit's cgroup. */
  bind(unit: string, port: number, protocol: "tcp" | "udp" = "udp"): this {
    const state = protocol === "tcp" ? "LISTEN" : "UNCONN";
    this.text += `${protocol} ${state} 0 0 0.0.0.0:${port} 0.0.0.0:* ino:1 sk:1 cgroup:/system.slice/${unit} <->\n`;
    return this;
  }

  clear(): void {
    this.text = "";
  }

  async dump(): Promise<string> {
    return this.text;
  }
}

/** The units directory, in memory: file name to contents. */
export class FakeUnits implements UnitsDirectory {
  readonly files = new Map<string, string>();

  declare(unit: string, fields: Record<string, unknown>): this {
    this.files.set(`${unit}.json`, JSON.stringify({ unit, ...fields }));
    return this;
  }

  async read(): Promise<UnitFile[]> {
    return [...this.files.entries()].sort(([a], [b]) => a.localeCompare(b)).map(([name, text]) => ({ name, text }));
  }
}

/** The state file, in memory; `saves` counts writes. */
export class MemoryStateStore implements StateStore {
  memories: Memories = {};
  saves = 0;

  async load(): Promise<Memories> {
    return structuredClone(this.memories);
  }

  async save(memories: Memories): Promise<void> {
    this.saves += 1;
    this.memories = structuredClone(memories);
  }
}

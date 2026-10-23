import { mkdir, readlink, realpath } from "node:fs/promises";
import { basename, dirname } from "node:path";

/**
 * Nix profiles: the stable path a unit's ExecStart runs from, and the
 * generations behind it that make a rollback exact.
 *
 * A profile `P` is a symlink to `P-<N>-link`, which links to a store path.
 * Every generation link is a GC root, so whatever a rollback goes back to is
 * still in the store.
 */
export interface Profiles {
  /** The store path the profile points at now, or undefined if there is no profile. */
  current(profile: string): Promise<string | undefined>;
  /** The current generation number, or undefined if there is no profile. */
  generation(profile: string): Promise<number | undefined>;
  /** The store path a generation points at, or undefined if it is gone. */
  target(profile: string, generation: number): Promise<string | undefined>;
  /** Point the profile at a store path, as a new generation. */
  set(profile: string, storePath: string): Promise<void>;
  /** Point the profile back at an existing generation. */
  switchGeneration(profile: string, generation: number): Promise<void>;
  /** Delete one generation, never the current one. */
  deleteGeneration(profile: string, generation: number): Promise<void>;
  /** Keep the current generation and the `keep` newest before it; delete the rest. */
  prune(profile: string, keep: number): Promise<void>;
}

/**
 * The generation number in a profile's link name: `bedrock-12-link` -> 12.
 *
 * Exported and pure, for the tests and for anything reading a profile without
 * a Nix to ask.
 */
export function generationOf(profile: string, link: string): number | undefined {
  const prefix = `${basename(profile)}-`;
  const name = basename(link);
  if (!name.startsWith(prefix) || !name.endsWith("-link")) return undefined;
  const number = Number(name.slice(prefix.length, -"-link".length));
  return Number.isInteger(number) && number > 0 ? number : undefined;
}

/**
 * Profiles through `nix-env`, which every Nix install has and whose
 * generations are what `nix-env --list-generations` shows an operator.
 *
 * Reads are in-process (two readlinks); writes go through nix-env, which with
 * NIX_REMOTE=daemon registers the GC roots through the Nix daemon. A rollback
 * is a switch to the generation recorded before the swap, not `--rollback`:
 * after a re-`--set` of a path that already had a generation, "the one before
 * the current" is not necessarily the one we left.
 */
export class NixProfiles implements Profiles {
  constructor(private readonly nixEnv: string = "nix-env") {}

  async current(profile: string): Promise<string | undefined> {
    try {
      return await realpath(profile);
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === "ENOENT") return undefined;
      throw error;
    }
  }

  async generation(profile: string): Promise<number | undefined> {
    let link: string;
    try {
      link = await readlink(profile);
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === "ENOENT") return undefined;
      throw error;
    }
    const generation = generationOf(profile, link);
    if (generation === undefined) throw new Error(`${profile} links to '${link}', which is not a profile generation`);
    return generation;
  }

  async target(profile: string, generation: number): Promise<string | undefined> {
    return this.current(`${profile}-${generation}-link`);
  }

  async set(profile: string, storePath: string): Promise<void> {
    // nix-env will not create the profile's directory ("cannot read directory").
    await mkdir(dirname(profile), { recursive: true });
    await this.run(["--profile", profile, "--set", storePath]);
  }

  async switchGeneration(profile: string, generation: number): Promise<void> {
    await this.run(["--profile", profile, "--switch-generation", String(generation)]);
  }

  async deleteGeneration(profile: string, generation: number): Promise<void> {
    await this.run(["--profile", profile, "--delete-generations", String(generation)]);
  }

  async prune(profile: string, keep: number): Promise<void> {
    // `+N` keeps the N newest generations by number, and never the current one
    // is deleted; the failed generations a rollback leaves are deleted by the
    // rollback itself, so the newest are the good ones.
    await this.run(["--profile", profile, "--delete-generations", `+${keep + 1}`]);
  }

  private async run(args: readonly string[]): Promise<void> {
    const process = Bun.spawn([this.nixEnv, ...args], { stdout: "pipe", stderr: "pipe" });
    const [stderr, exitCode] = await Promise.all([new Response(process.stderr).text(), process.exited]);
    if (exitCode !== 0) {
      throw new Error(`nix-env ${args.join(" ")} exited with status ${exitCode}: ${stderr.trim().slice(0, 300)}`);
    }
  }
}

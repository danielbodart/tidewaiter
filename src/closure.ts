import { link, mkdir, readFile, rename, unlink, writeFile } from "node:fs/promises";
import { dirname, join } from "node:path";

/**
 * Lets a confined unit see the program its profile points at.
 *
 * NixOS confinement gives a unit a root with only its own closure bound in,
 * computed from the unit's definition at evaluation time. A unit whose
 * ExecStart runs from a profile references no store path at all, and after a
 * rollback the profile points at a path the definition has never heard of - so
 * without help both would start in a root with no program in it. Before every
 * restart, Tidewaiter rewrites a runtime drop-in binding the target's closure
 * and the profile's directory, then reloads systemd.
 */
export interface ClosureBinder {
  /** Bind `target`'s closure and `profile`'s directory into `unit`. The caller reloads systemd. */
  bind(unit: string, profile: string, target: string): Promise<void>;
  /**
   * Bind as `bind` does, but only if `unit` has no drop-in for `profile` yet -
   * none at all (after a reboot, or a newly declared unit), or one for a
   * different profile. True when it wrote one; the caller reloads systemd.
   *
   * For `tidewaiter prepare`, so it never overwrites the daemon's: the daemon
   * rewrites the drop-in before every restart, and a prepare that read the
   * profile just before the daemon moved it would otherwise put the old
   * closure back under the new version's restart.
   */
  ensure(unit: string, profile: string, target: string): Promise<boolean>;
}

/** The drop-in's file name: early enough that a unit's own later drop-ins can still add to it. */
export const CLOSURE_DROP_IN = "50-tidewaiter-closure.conf";

/**
 * The drop-in's text: the profile's directory (so the profile symlink and its
 * generation link resolve inside the root) and every path of the closure,
 * read-only.
 */
export function closureDropIn(profile: string, closure: readonly string[]): string {
  const paths = [dirname(profile), ...[...new Set(closure)].sort()];
  return [
    "# Written by tidewaiter before each restart: what the profile points at, and its closure.",
    `${PROFILE_LINE}${profile}`,
    "[Service]",
    ...paths.map((path) => `BindReadOnlyPaths=${path}`),
    "",
  ].join("\n");
}

/** The comment line naming the profile a drop-in was written for. */
const PROFILE_LINE = "# Profile: ";

/** The profile a drop-in was written for, if it says. */
export function dropInProfile(text: string): string | undefined {
  const line = text.split("\n").find((candidate) => candidate.startsWith(PROFILE_LINE));
  return line?.slice(PROFILE_LINE.length);
}

/**
 * The closure from `nix-store --query --requisites`, written under
 * /run/systemd/system/<unit>.d - runtime, so it is gone after a reboot until
 * `tidewaiter prepare` writes it again, before anything starts.
 */
export class NixClosureBinder implements ClosureBinder {
  constructor(
    private readonly runtimeDir: string = "/run/systemd/system",
    private readonly nixStore: string = "nix-store",
  ) {}

  async bind(unit: string, profile: string, target: string): Promise<void> {
    const file = this.file(unit);
    const temporary = await this.write(file, profile, target);
    await rename(temporary, file);
  }

  async ensure(unit: string, profile: string, target: string): Promise<boolean> {
    const file = this.file(unit);
    const existing = await readFile(file, "utf8").catch((error: NodeJS.ErrnoException) => {
      if (error.code === "ENOENT") return undefined;
      throw error;
    });
    if (existing !== undefined && dropInProfile(existing) === profile) return false;

    const temporary = await this.write(file, profile, target);
    if (existing !== undefined) {
      // For another profile: the declaration moved, and what was bound is no use.
      await rename(temporary, file);
      return true;
    }
    // None there: link() rather than rename(), so a drop-in the daemon wrote
    // in the meantime wins.
    try {
      await link(temporary, file);
      return true;
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === "EEXIST") return false;
      throw error;
    } finally {
      await unlink(temporary).catch(() => {});
    }
  }

  private file(unit: string): string {
    return join(this.runtimeDir, `${unit}.d`, CLOSURE_DROP_IN);
  }

  /** The drop-in's text in a temporary file of this process's own, ready to move into place. */
  private async write(file: string, profile: string, target: string): Promise<string> {
    const closure = await this.requisites(target);
    await mkdir(dirname(file), { recursive: true });
    const temporary = `${file}.${process.pid}.tmp`;
    await writeFile(temporary, closureDropIn(profile, closure), { mode: 0o644 });
    return temporary;
  }

  private async requisites(target: string): Promise<string[]> {
    const process = Bun.spawn([this.nixStore, "--query", "--requisites", target], { stdout: "pipe", stderr: "pipe" });
    const [stdout, stderr, exitCode] = await Promise.all([
      new Response(process.stdout).text(),
      new Response(process.stderr).text(),
      process.exited,
    ]);
    if (exitCode !== 0) {
      throw new Error(`nix-store --query --requisites ${target} exited with status ${exitCode}: ${stderr.trim().slice(0, 300)}`);
    }
    return stdout.split("\n").map((line) => line.trim()).filter((line) => line !== "");
  }
}

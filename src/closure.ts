import { mkdir, rename, writeFile } from "node:fs/promises";
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
    "[Service]",
    ...paths.map((path) => `BindReadOnlyPaths=${path}`),
    "",
  ].join("\n");
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
    const closure = await this.requisites(target);
    const file = join(this.runtimeDir, `${unit}.d`, CLOSURE_DROP_IN);
    await mkdir(dirname(file), { recursive: true });
    const temporary = `${file}.tmp`;
    await writeFile(temporary, closureDropIn(profile, closure), { mode: 0o644 });
    await rename(temporary, file);
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

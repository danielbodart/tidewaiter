import { realpath } from "node:fs/promises";
import { join } from "node:path";
import { DbusConnection, SYSTEM_BUS } from "./dbus.ts";

/**
 * A daily stretch of local time, in minutes since midnight.
 *
 * `end` may be 1440 (24:00), and may be before `start`, for a window that
 * crosses midnight: 23:00-01:00 is the last hour of one day and the first of
 * the next.
 */
export interface Window {
  readonly start: number;
  readonly end: number;
}

const TIME = /^([01][0-9]|2[0-3]):([0-5][0-9])$/;

/** "03:00-05:00" to a Window. 24:00 is allowed as an end; an empty window is refused. */
export function parseWindow(text: string): Window {
  const [from, to, ...rest] = text.split("-");
  const minutes = (time: string | undefined, end: boolean): number => {
    if (end && time === "24:00") return 24 * 60;
    const match = time === undefined ? null : TIME.exec(time);
    if (!match) throw new Error(`'${text}' is not a window: give HH:MM-HH:MM, such as 03:00-05:00`);
    return Number(match[1]) * 60 + Number(match[2]);
  };
  if (rest.length > 0) throw new Error(`'${text}' is not a window: give HH:MM-HH:MM, such as 03:00-05:00`);
  const window = { start: minutes(from, false), end: minutes(to, true) };
  if (window.start === window.end) {
    throw new Error(`'${text}' is an empty window; 00:00-24:00 is the whole day`);
  }
  return window;
}

/** Whether `date`, in local time, is inside the window: start inclusive, end exclusive. */
export function inWindow(window: Window, date: Date): boolean {
  const minute = date.getHours() * 60 + date.getMinutes();
  return window.start < window.end
    ? minute >= window.start && minute < window.end
    : minute >= window.start || minute < window.end;
}

/** Whether the machine needs a reboot to run what is installed. */
export interface BootCheck {
  /** Why a reboot is needed, or undefined when it is not. Throws when it cannot tell. */
  needed(): Promise<string | undefined>;
}

/** What nixpkgs' system.autoUpgrade compares to decide that a switch is not enough. */
const BOOT_FILES = ["initrd", "kernel", "kernel-modules"] as const;

/**
 * The test system.autoUpgrade makes with allowReboot: the booted system's
 * initrd, kernel and kernel modules against the installed system's. A
 * difference is something only a reboot puts in place; everything else a
 * nightly `switch` already has.
 *
 * Both paths are arguments, so a test can stand a different "installed"
 * system up without installing one.
 */
export class NixosBootCheck implements BootCheck {
  constructor(
    private readonly booted = "/run/booted-system",
    private readonly current = "/nix/var/nix/profiles/system",
  ) {}

  async needed(): Promise<string | undefined> {
    // Neither system there at all is "cannot tell", never "everything changed".
    for (const system of [this.booted, this.current]) {
      await realpath(system).catch((error: Error) => {
        throw new Error(`cannot read ${system} (${error.message})`);
      });
    }
    const changed: string[] = [];
    for (const name of BOOT_FILES) {
      const [was, is] = await Promise.all([resolved(join(this.booted, name)), resolved(join(this.current, name))]);
      if (was !== is) changed.push(name);
    }
    return changed.length === 0 ? undefined : `${changed.join(", ")} changed since boot`;
  }
}

/** Where a path finally leads, or undefined when it leads nowhere - a system with no initrd. */
async function resolved(path: string): Promise<string | undefined> {
  try {
    return await realpath(path);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return undefined;
    throw error;
  }
}

/** Reboots the machine. */
export interface Rebooter {
  reboot(): Promise<void>;
}

/**
 * logind's Manager.Reboot, as `systemctl reboot` asks for it: the same
 * shutdown everything else takes, and the same block inhibitors - someone
 * else's lock still refuses it, and the next pass in the window tries again.
 * Not interactive: there is nobody to ask for a password.
 */
export class LogindRebooter implements Rebooter {
  constructor(private readonly path: string = SYSTEM_BUS) {}

  async reboot(): Promise<void> {
    const bus = await DbusConnection.open(this.path);
    try {
      await bus.call(
        "org.freedesktop.login1",
        "/org/freedesktop/login1",
        "org.freedesktop.login1.Manager",
        "Reboot",
        "b",
        [false],
      );
    } finally {
      bus.close();
    }
  }
}

/** What stands between the machine and a reboot it needs. */
export interface RebootInputs {
  /** Why a reboot is needed; undefined when it is not. */
  readonly needed: string | undefined;
  readonly inWindow: boolean;
  /** In use right now. */
  readonly busy: readonly string[];
  /** Idle, but not yet for their idle-samples debounce - or never sampled. */
  readonly unsettled: readonly string[];
  readonly swapping: readonly string[];
}

export type RebootVerdict =
  | { readonly kind: "not-needed" }
  | { readonly kind: "wait"; readonly reason: string }
  | { readonly kind: "reboot" };

/**
 * Reboot only when one is needed, inside the window, with nothing swapping and
 * everything idle for as long as a swap would wait. A window that is busy
 * throughout simply passes; the next day's is tried the same way.
 */
export function decideReboot(inputs: RebootInputs): RebootVerdict {
  if (inputs.needed === undefined) return { kind: "not-needed" };
  if (!inputs.inWindow) return { kind: "wait", reason: "waiting for the window" };
  const list = (names: readonly string[]) => [...names].sort().join(", ");
  if (inputs.swapping.length > 0) return { kind: "wait", reason: `waiting: swapping ${list(inputs.swapping)}` };
  if (inputs.busy.length > 0) return { kind: "wait", reason: `waiting: ${list(inputs.busy)} in use` };
  if (inputs.unsettled.length > 0) {
    return { kind: "wait", reason: `waiting: ${list(inputs.unsettled)} not idle for long enough yet` };
  }
  return { kind: "reboot" };
}

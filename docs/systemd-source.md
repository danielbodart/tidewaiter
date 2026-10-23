# Updating systemd units

Tidewaiter was written to update Docker containers. What makes it worth having
- wait until nobody is using the thing, swap it, health-gate the swap, roll back
on failure - has nothing to do with Docker. This is the design for a second
source of things to update: **systemd units whose program lives in a Nix
profile**, so a native service on NixOS gets the same care a labelled container
does. Docker behaviour does not change.

Status: built. Sections 1-9 are implemented, unit-tested, and proven end to
end by the NixOS VM test of section 11 (see "Where this stands", section 12,
for where building it changed this design). Section 13 lists what is still
open.

The shape, end to end, for one unit:

```
nixos-rebuild switch (or system.autoUpgrade)
  -> /etc/tidewaiter/units/chandlery-bedrock.service.json  {"desired": "/nix/store/...-bedrock-1.21.2", ...}
  -> the unit itself is NOT restarted (restartIfChanged = false)

tidewaiter, every pass:
  current = realpath /nix/var/nix/profiles/chandlery/bedrock
  desired != current?
    players on 19132/udp (conntrack)?            -> defer; `tidewaiter status` says in use
    idle for idleSamples passes?                 -> swap:
      nix-env --profile P --set DESIRED
      systemctl restart chandlery-bedrock.service   (the unit's own ExecStop saves and quits)
      health gate: systemd state, port-bound by cgroup, port-connect, uptime
        healthy   -> keep, prune old generations
        unhealthy -> nix-env --profile P --switch-generation PREVIOUS, delete the failed
                     generation, restart, pin DESIRED out
```

## Contents

1. Sources and the shared core
2. The systemd source
3. Activity: conntrack for a host unit
4. Health for a unit
5. The swap state machine
6. Reboots: status, and rebooting automatically
7. Confinement and the closure a unit can see
8. Command line
9. Nix: flake, package, module
10. How each shared contract is met
11. Test plan
12. Where this stands
13. Open questions

---

## 1. Sources and the shared core

Today `Tidewaiter` (`src/daemon.ts`) is the Docker updater and the loop in one.
It splits into a **daemon** that owns everything shared and **sources** that own
how one kind of workload is read, swapped and checked. This follows the same
split Portical made on its `systemd-source` branch (`Source` in its
`src/source.ts`), adapted: Portical's sources only *describe* desired state,
whereas a Tidewaiter source also *acts* (a swap is source-specific), so here a
source runs its own pass.

```ts
// src/source.ts
export interface PassContext {
  /** The host conntrack table, dumped once per pass; undefined when the dump failed. */
  readonly conntrack: readonly ConntrackEntry[] | undefined;
  /**
   * Mark `workload` as being swapped until the end of the pass. Called before
   * any swap starts, so the status says so and an automatic reboot waits.
   */
  hold(workload: string): Promise<void>;
}

export interface Outcome {
  /** "chandlery-bedrock.service", or a container name. Unique within a source. */
  readonly workload: string;
  readonly action: Action;
  /**
   * The activity verdict this pass, fail-safe (could-not-tell reads as busy).
   * Undefined when the pass never sampled it (registry or systemd unreachable):
   * the daemon keeps the workload's last verdict.
   */
  readonly busy?: boolean;
  /** Idle for its whole idle-samples debounce this pass; undefined reads as not. */
  readonly settled?: boolean;
}

export interface Source {
  readonly name: "docker" | "systemd";
  /** Reconcile every workload once. One workload failing never fails the pass. */
  once(context: PassContext): Promise<Outcome[]>;
  /** Activity only - no digests, no swaps - for the faster tick between passes. */
  activity(conntrack: readonly ConntrackEntry[] | undefined): Promise<{ workload: string; busy: boolean }[]>;
  /** One line per workload for `tidewaiter list`. */
  list(): Promise<string[]>;
}
```

```ts
// src/daemon.ts
export class Daemon {
  constructor(
    sources: readonly Source[],
    conntrack: ConntrackSource,
    options: DaemonOptions,        // interval, activityInterval
    log: Log,
    status: StatusSink,            // NoStatus without --status-file
    autoReboot?: AutoReboot,       // with --auto-reboot-window
  );
  once(): Promise<Outcome[]>;      // dump conntrack, each source in order, summary, status
  considerReboot(): Promise<void>; // section 6
  run(signal: AbortSignal): Promise<void>;  // runEvery(interval, once + considerReboot) + the activity tick
}
```

What is shared, and where it lives:

| Concern | Where | Notes |
|---|---|---|
| Interval loop, single-flight | `src/loop.ts` `runEvery` | Already extracted. Both sources' passes run in one queue, so two swaps never overlap. |
| Keep / update / defer / pinned | `src/decide.ts` `decide(Decidable, ContainerState)` | Already generalised. For systemd the "digests" are store paths, equally opaque. |
| Debounce (idle streak) | the source's per-workload state | Same rule: `idleStreak = busy ? 0 : previous + 1`. |
| Detectors | `src/detectors.ts` | `conntrack`, `tcp`, `none` reused as is (section 3). |
| Health checks and their combination | `src/health.ts` | `port-connect`, `port-bound`, `uptime` reused through the new `HealthSubject` and `BoundPorts` seams; a new `systemd` check. |
| Health gate polling | `src/gate.ts` `pollVerdict` | Already extracted. |
| Pinning and swap-failure back-off | per-source state, same rules | `MAX_SWAP_FAILURES` moves to a shared constant. |
| Status and automatic reboot | `src/status.ts`, `src/reboot.ts` | Daemon-owned, fed by every source (section 6). |

`DockerSource` (`src/docker-source.ts`) is the old `Tidewaiter` class renamed,
with `once()` taking the `PassContext` instead of dumping conntrack itself, and
`run()` moving to the `Daemon`. Its logic, messages and tests are otherwise
untouched: `daemon.test.ts` keeps driving it through the same fakes, behind a
`Daemon` with one source. A failed conntrack dump reaches it as `undefined`,
which it reads as an empty table - exactly what it did before (see open
question 12).

If a source throws, the others still run, its workloads keep their last busy
verdicts (so a source that cannot be read never reads as idle), and the
first error is rethrown at the end of the pass, so the loop logs it and `check`
exits non-zero as before.

The summary line stays per pass across all sources, counting "containers"
when Docker is the only source (unchanged), "units" when systemd is, and
"workloads" for both. `decide()` takes its reasons' wording as an optional
`Words` argument - the Docker wording by default, `UNIT_WORDS` for units - so a
unit's log talks about versions and profiles, not images.

## 2. The systemd source

### Declaring a unit: the units directory

A unit opts in by a JSON file, one per unit, in a directory (default
`/etc/tidewaiter/units`, `--units-dir`):

```json
// /etc/tidewaiter/units/chandlery-bedrock.service.json
{
  "unit": "chandlery-bedrock.service",
  "desired": "/nix/store/abc...-bedrock-server-1.21.2",
  "profile": "/nix/var/nix/profiles/chandlery/bedrock",
  "ports": ["19132/udp", "19133/udp"],
  "detector": "conntrack",
  "health": ["systemd", "port-connect", "port-bound", "uptime"],
  "idleSamples": 3,
  "healthTimeout": 120,
  "keepGenerations": 1,
  "bindClosure": true
}
```

Why a file and not `X-Tidewaiter-*` keys in the unit (Portical's choice):

- `desired` changes on every deploy, and the unit must *not* change when it does
  - an unchanged unit is what keeps `nixos-rebuild switch` from even considering
  a restart, and what keeps the unit's `ExecStart` pointing at the stable
  profile path.
- Nothing has to be enumerated over D-Bus or parsed out of unit files: the
  directory *is* the list of managed units, read each pass.
- It is just as usable without Nix: any deployment tool can drop a file.

Only `unit`, `desired` and `profile` are required. Every other field has the
same default and the same "warn and use the default" handling as the labels
(`labels.ts`), through a `parseUnitPolicy(json, fileName)` beside
`parseLabels`, returning `{ policy?, warnings }`. A file that is not JSON, or is
missing a required field, is warned about and skipped, never guessed at. The
file name is informative only; `unit` is the identity. A `unit` without a
suffix gets `.service`.

`ports` uses the `tidewaiter.ports` grammar (`25565`, `25565/tcp`, `19132/udp`),
but is not validated against "what is published", because a host unit publishes
nothing: the declared ports *are* the unit's ports. A bare port means both
protocols. Each becomes a `PublishedPort` with `hostPort = containerPort`.

`UnitPolicy` is `ContainerPolicy` minus `autoupdate`, `graceSeconds` and
`keepImages`, plus `unit`, `desired`, `profile`, `keepGenerations` and
`bindClosure`; `health` is a list of `"systemd" | "port-connect" |
"port-bound" | "uptime"`, `detector` one of `"conntrack" | "tcp" | "none"`
(`netio` reads Docker's `/stats` and has no unit equivalent).

### Talking to systemd

Over the system bus, with the hand-written D-Bus client from Portical's
`systemd-source` branch (`src/dbus.ts`, ~600 lines, zero dependencies) copied
in - the same "lift from the sibling" PLAN.md already used for `http.ts`. It
keeps Tidewaiter at zero runtime dependencies, so the Nix package stays a plain
`bun build` with nothing to fetch.

```ts
// src/systemd.ts
export interface UnitState {
  readonly name: string;
  readonly loadState: string;      // "loaded", "not-found", ...
  readonly activeState: string;    // "active", "activating", "deactivating", "inactive", "failed", "reloading"
  readonly subState: string;       // "running", "start-pre", ...
  readonly result: string;         // Service.Result: "success", "exit-code", "timeout", ...
  readonly nRestarts: number;      // Service.NRestarts
  readonly mainPid: number;        // Service.MainPID, 0 when none
  readonly controlGroup: string;   // Service.ControlGroup, "" when not running
  readonly invocationId: string;   // Unit.InvocationID, hex; changes every start
}

export type JobResult = "done" | "canceled" | "timeout" | "failed" | "dependency" | "skipped";

export interface SystemdClient {
  unit(name: string): Promise<UnitState>;
  /** RestartUnit(name, "replace"), then wait for its JobRemoved. No timeout of its own: the unit's TimeoutStopSec/TimeoutStartSec bound it. */
  restart(name: string): Promise<JobResult>;
  daemonReload(): Promise<void>;
}
```

`DbusSystemdClient`: `GetUnit`/`LoadUnit` for the path, `Properties.GetAll` on
`org.freedesktop.systemd1.Unit` and `...Service`, `Subscribe` plus an
`AddMatch` on `JobRemoved` before calling `RestartUnit`, so the job's end is
never missed. For a `Type=notify` unit (Chandlery's), the restart job finishes
only when the new process has sent `READY=1` - or fails on `TimeoutStartSec` -
so `"done"` already means "the game said it is up".

The restart is a *restart*, not stop-then-start by Tidewaiter: graceful stop is
the unit's own business (Chandlery's `ExecStop` types the save-and-quit into the
console FIFO and waits, under `TimeoutStopSec=5min`).

### Profiles

```ts
// src/profile.ts
export interface Profiles {
  /** The store path the profile points at now, or undefined if there is no profile. */
  current(profile: string): Promise<string | undefined>;
  /** The current generation number, for an exact rollback. */
  generation(profile: string): Promise<number | undefined>;
  /** The store path a generation points at. */
  target(profile: string, generation: number): Promise<string | undefined>;
  set(profile: string, storePath: string): Promise<void>;
  switchGeneration(profile: string, generation: number): Promise<void>;
  deleteGeneration(profile: string, generation: number): Promise<void>;
  /** Keep the current generation and `keep` before it; delete the rest. */
  prune(profile: string, keep: number): Promise<void>;
}
```

`NixProfiles` shells out, as `CliConntrackSource` does:

| Method | Command |
|---|---|
| `current` | `realpath` of the profile (a `readlink` chain: `P -> P-N-link -> /nix/store/...`), in-process |
| `generation` | parse `N` from `readlink P` (`P-N-link`), in-process |
| `target` | `realpath` of `P-N-link`, in-process |
| `set` | `mkdir -p $(dirname P)`, then `nix-env --profile P --set PATH` |
| `switchGeneration` | `nix-env --profile P --switch-generation N` |
| `deleteGeneration` | `nix-env --profile P --delete-generations N` |
| `prune` | `nix-env --profile P --delete-generations +(keep+1)` |

Checked on the development host: `nix-env --set` needs the profile's parent
directory to exist ("cannot read directory"), hence the `mkdir`; `--rollback`
goes to the generation *before* the current one, which after a re-`--set` of a
path that already had a generation is not necessarily the one we left, so the
rollback is by the generation number recorded before the swap, not
`--rollback`. Every generation link under `/nix/var/nix/profiles` is a GC root,
so the version rolled back to is always still in the store.

`--delete-generations +N` keeps the N highest-*numbered* generations, and a new
generation is numbered one past the highest left. So a rollback deletes the
generation it leaves (the failed version) straight away: otherwise the failed
generation, being newest, would be what `prune` keeps as "the previous one" and
the good version before it would be pruned. Checked against `nix-env` on the
development host, and in `test/profile.test.ts` wherever a Nix daemon is
reachable.

`nix build --profile P PATH` would also do, but `nix-env` is in every Nix
install and its generations are what `nix-env --list-generations` shows an
operator.

### Sockets by cgroup

For port-bound health (section 4):

```ts
// src/listeners.ts
export interface Listener { readonly protocol: Protocol; readonly port: number; readonly cgroup: string }
export interface ListenerSource { dump(): Promise<string> }    // `ss -Hlntu -e`
export function parseListeners(text: string): Listener[];
export function unitBoundPorts(listeners: () => Promise<Listener[]>, controlGroup: string): BoundPorts;
```

`ss -Hlntu -e` prints each listening TCP and bound UDP socket with
`cgroup:/system.slice/<unit>` (checked on the development host, systemd 260,
iproute2 with sock_diag cgroup support: Linux 5.7+, iproute2 5.9+). A socket
belongs to the unit when its cgroup is the unit's `ControlGroup` or beneath it.
Any local address counts - for health, a socket bound only to loopback is still
the unit serving. Portical's branch parses the same output for its `published`;
the parser is shared in spirit, not by import (the repos do not depend on each
other).

`unitBoundPorts` throws - which port-bound and uptime read as "cannot tell yet",
never "nothing bound" - when the unit has no `ControlGroup` (not running) or
when `ss` printed no `cgroup:` field on any line (a kernel or iproute2 too old),
so a host that cannot attribute sockets never fails a healthy update for it.

## 3. Activity: conntrack for a host unit

The conntrack detector needs no change. It already matches flows by their
original-direction destination port against `PublishedPort.hostPort`; for a
unit the declared ports are host ports, and every flow to them - a LAN client
(`src=10.x`), a client DNAT'd in from the WAN, or one over loopback - is in the
host's table. UDP players show up exactly as they do for a container on host
networking, which is the case PLAN.md built the detector for.

- `ports` empty: "cannot tell", so busy (fail-safe), with a warning - the same as
  an empty `tidewaiter.ports` override today.
- `tcp`: counts `ESTABLISHED` sockets in `/proc/<MainPID>/net/tcp[6]` on the
  declared ports. In the host netns that table is host-wide, but filtered to the
  unit's own ports it is still the unit's traffic. A unit with no `MainPID`
  (inactive) is "cannot tell".
- `none`: always idle.

One addition, for both sources: **re-sample right before the swap.** Today the
idle verdict can be up to a pass old by the time the swap starts. The systemd
source dumps conntrack once more, immediately before `nix-env --set`, and defers
if anyone has arrived. Cheap, and it closes most of the "joined in the last
interval" window. (Back-porting it to Docker is an open question.)

## 4. Health for a unit

The gate is the Docker gate - `pollVerdict` over a set of checks, roll back on
any active failure, at the deadline commit unless something failed - with the
unit presented through the seams added in this branch:

```ts
// what the systemd source builds per poll
const subject: HealthSubject = {
  running: state.activeState === "active" || state.activeState === "reloading",
  health: "none",                                   // no Docker HEALTHCHECK
  spec: { networks: [], networkMode: "host" },      // host network
};
const input: HealthInput = {
  container: subject,
  ports: policy.ports,
  bound: unitBoundPorts(listeners, state.controlGroup),
  tcp, networkDriver: async () => undefined,
};
```

| Check | For a unit |
|---|---|
| `systemd` (new) | **fail** if the restart job did not end `done`, if `ActiveState` is `failed`, `inactive` or `deactivating` after the job, if `NRestarts` rose, or if `InvocationID` changed since the job (it crashed and `Restart=` brought it back). **pass** when `active`/`running` with the job `done`. **inconclusive** while `activating`/`reloading`. |
| `port-bound` | A socket in the unit's cgroup on any declared port. Unchanged check; reads through `BoundPorts`. |
| `port-connect` | `connect()` to `127.0.0.1:<port>` for each declared TCP port, truthful (no proxy on the host network), so a refusal after the grace fails. Unchanged check: `clientEndpoints` already does this for `networkMode: "host"`. Skips when no TCP port is declared - a Bedrock server is UDP only. |
| `uptime` | `running` stays true (the `systemd` check's crash signals feed `running` too: `false` once `NRestarts` or `InvocationID` moved) for the grace, and by then a declared port is bound in the cgroup. Unchanged check. |
| `docker` | Not accepted in a unit's `health`; warned and dropped. |

A restart job that did not end `done` is unhealthy at once, whichever checks
the policy names - for a `Type=notify` unit it means the program never said it
was up.

Default: all four. `systemd` is the cheapest and most authoritative for a
`Type=notify` unit; `port-bound` is the only generic UDP signal and, through the
cgroup, is not fooled by another process on the same port.

## 5. The swap state machine

Per managed unit, each pass (`SystemdSource.once`):

1. **Read** the unit's file and policy, `UnitState`, and `current =
   profiles.current(profile)`. A unit systemd does not know (`not-found`) is
   left alone with a warning.
2. **Not running** (`inactive`, `failed`): nobody is playing, but it is not
   Tidewaiter's to start either. If `current != desired` and `desired` is not
   pinned, `set` the profile (binding the closure and reloading first when
   `bindClosure`) so the next start, by whoever starts it, runs `desired`; no
   restart, no gate. Logged as "updated the profile of a stopped unit". (Open
   question 4.) **In transition** (`activating`, `deactivating`): defer, and
   the idle streak does not count.
3. **Sample activity** with the policy's detector over the shared conntrack dump;
   `idleStreak` as for Docker.
4. **Decide**: `decide({ policy, currentDigest: current, desired: { digest:
   desired }, inUse, idleStreak }, state)`.
5. **Update** (not dry-run):
   1. `context.hold(unit)` - the status says swapping; no automatic reboot.
   2. Re-sample conntrack (section 3); busy now means defer, streak reset.
   3. `previous = profiles.generation(profile)`.
   4. If `bindClosure`: write the closure drop-in for `desired` (section 7) and
      `daemonReload()`.
   5. `profiles.set(profile, desired)`.
   6. `jobResult = systemd.restart(unit)`.
   7. Gate: `pollVerdict` with `healthTimeout`.
   8. **Healthy**: `profiles.prune(profile, keepGenerations)`; clear failure
      counters; log `updated to <desired> and it is healthy`.
   9. **Unhealthy**: `profiles.switchGeneration(profile, previous)` and delete
      the generation it leaves (section 2); closure drop-in for the previous
      path; `daemonReload()`; `restart(unit)`; wait for it with the same gate (a
      failed rollback is logged as "manual attention needed", never retried in
      a loop); **pin** `desired`.
   10. **A step failed** before the gate (`nix-env` error, the restart could not
      be queued): undo what was done - switch the profile back if it was set,
      put the previous closure drop-in back if it was rewritten, and restart
      only if the unit's `InvocationID` shows it restarted anyway (the bus
      dropped after the job was queued) - and count it against
      `MAX_SWAP_FAILURES` exactly as Docker's swap-step failures are. A failed
      *job* is not a step failure: the new version did not come up, so it rolls
      back and pins.
6. **Pinned** while `desired` is the pinned path; a new `desired` clears it.

State that must survive a restart of Tidewaiter is persisted, unlike Docker's
in-memory state, because a rollback leaves the failed generation in the profile
and a forgotten pin would retry it once on every daemon restart:

```json
// /var/lib/tidewaiter/systemd.json  (StateDirectory)
{ "chandlery-bedrock.service": { "pinned": "/nix/store/...", "failing": "/nix/store/...", "swapFailures": 1, "good": 7 } }
```

Written atomically (temp file and rename) after any pass that changed it. A
file that cannot be parsed stops the systemd source (logged every pass) rather
than starting over with no pins. The
idle streak is not persisted: after a daemon restart it counts up again, which
only ever delays.

There is no prefetch: `desired` is in the system closure, so it was built (or
substituted) by the `nixos-rebuild` that wrote the file - "builds the new version
as soon as it's published" is the system build itself.

**Crash recovery.** If Tidewaiter dies mid-swap the profile may point at
`desired` with the gate never run - or never even restarted into, the unit still
running the old version. So before the swap changes anything it writes, and
saves, a record of it: the generation it starts from, that generation's target,
`desired`, and the unit's `InvocationID`. The record is cleared, and saved, once
the gate commits or the rollback ends. On start, a unit with a record is
resumed by whether its invocation moved on:

| The unit | What it means | What is done |
| --- | --- | --- |
| same invocation | the restart never happened (or its stop is still running) | profile switched back to `from`, drop-in rebound for it, reload - no restart; the idle-gated path swaps again |
| new invocation, `failed` | the new version did not come up | rolled back to `from` (restart, gate), `desired` pinned out |
| new invocation, up or activating | restarted, never gated | gated now; committed if healthy, else rolled back and pinned |
| new invocation, stopped | someone stopped it since | record dropped; the stopped-unit path moves the profile |

Without a record, a unit that is up is adopted as it is (with a warning if its
generation differs from the last good one: someone moved the profile by hand),
and a unit `failed` on a generation newer than the last recorded good one is
rolled back to that one and the version pinned. "Last good generation" is
recorded per unit after every healthy swap, and on first sight of a unit that is
up with no record. Pins, `good` and the record are each saved the moment they
change, not at the end of the pass.

## 6. Reboots: status, and rebooting automatically

**Humans stay in charge.** Nothing Tidewaiter does stands between a person
and a reboot they ask for. What it offers instead is a way to know whether a
reboot would cut anyone off, and a way to have the machine reboot itself only
when it would not.

*How this changed.* The first build held a logind **block** inhibitor
(`shutdown:sleep`, through a `systemd-inhibit` child) while anything was busy,
so that `system.autoUpgrade`'s `shutdown -r +1` was refused under players.
Since systemd 257 a block lock is enforced against root too, which is what made
that work - and also what refused a person's own `systemctl reboot`. The
owner's call: root should still be able to reboot; only *automatic* reboots
should wait. So the lock is gone, `allowReboot` is left off, and the waiting
moved into Tidewaiter, which knows what "idle" means here.

### `tidewaiter status`

With `--status-file PATH` (the module passes `/run/tidewaiter/status.json`,
in a `RuntimeDirectory` systemd creates world-readable and removes on stop),
`run` writes what it knows, as JSON, after every pass, every activity tick and
the moment a swap starts:

```json
{
  "updated": "2026-10-23T03:30:00.000Z",
  "busy": ["chandlery-bedrock.service"],
  "swapping": [],
  "workloads": [
    { "workload": "chandlery-bedrock.service", "source": "systemd", "busy": true, "settled": false }
  ],
  "reboot": { "window": "03:00-05:00", "needed": "kernel changed since boot", "verdict": "waiting: chandlery-bedrock.service in use" }
}
```

written to a temporary file and renamed, so a reader never sees half of one.
`tidewaiter status` reads it - no root, no D-Bus, no daemon to answer - prints
a headline, one line per workload and where an automatic reboot stands, and
exits **0** when nothing is in use or being swapped, **1** when something is,
**2** when there is no file (no daemon writing one). `busy` is the latest
verdict from a pass or a tick, fail-safe (could-not-tell is busy); `settled` is
whether the last pass found it idle for its whole `idleSamples`. Only `run`
writes it: a `check` beside a running daemon must not overwrite what the daemon
says. A file that cannot be written is warned about once and never fatal.

A file rather than a D-Bus service or a socket: the daemon already knows the
answer every few seconds, a file needs nothing listening, and the hand-written
D-Bus client does not serve methods.

**The activity tick** (`--activity-interval`, default 15 s) dumps conntrack and
runs each source's `activity()` - no digests, no swaps, no effect on idle
streaks - so a player who joins between passes shows up within one tick. It
runs whenever there is a status file or an automatic reboot to keep current;
the image, which has neither by default, does not tick, as before.

### Rebooting automatically

With `--auto-reboot-window HH:MM-HH:MM` (module: `autoReboot = { enable;
window; }`), every pass of `run` ends with `considerReboot()`:

1. **Is a reboot needed?** The test `system.autoUpgrade` makes with
   `allowReboot`: `realpath` of `initrd`, `kernel` and `kernel-modules` under
   `/run/booted-system` against the same under `/nix/var/nix/profiles/system`
   (`--booted-system`, `--current-system`; tests point them elsewhere). One
   missing on both sides is no change; either system itself unreadable is
   "cannot tell", logged, and never a reboot.
2. **Inside the window?** Local time, start inclusive, end exclusive. A start
   after the end crosses midnight (`23:00-01:00`); `00:00-24:00` is the whole
   day; an empty window (`03:00-03:00`) is refused at start and by the module.
3. **Anything being swapped?** The pass's held workloads (`context.hold`).
4. **Anything in use?** The latest busy verdicts, from every source.
5. **Anything not settled?** Each source reports `settled` per workload: idle
   for its whole `idleSamples` debounce this pass. A workload the pass could
   not sample (registry or systemd unreachable), one just swapped, a unit on
   its way up or down, all count as not settled. A stopped or unknown unit, and
   a container whose label is not a policy, have nobody on them and are.
6. **One last look.** Activity is read again, as a swap re-samples before it
   starts; anyone there now, or a source that cannot answer, means wait.
7. **Reboot**: logind's `Manager.Reboot(false)` over the system bus, as
   `systemctl reboot` asks for it. Once accepted, no later pass asks again.
   Refused (another program's block lock, no logind) is logged, and the next
   pass in the window tries again.

`decideReboot` is the pure part (steps 1-5's inputs to a verdict); the daemon
gathers them. A pass in which a source threw never reboots: the error is
rethrown before `considerReboot` runs. The verdict is logged when it changes -
`Automatic reboot (03:00-05:00): needed, kernel changed since boot; waiting:
chandlery-bedrock.service in use` - and is in the status.

**Never outside the window, and no catching up.** A window busy from start to
end passes with no reboot; the next day's is tried the same way. A box whose
players never leave never reboots by itself - which is what a person asked for
by declaring them; `tidewaiter status` says so, and a person can reboot by hand.

**With `system.autoUpgrade`.** Run it with `allowReboot = false`, which makes
it a nightly `nixos-rebuild switch`: units with `tidewaiter.desired` have
`restartIfChanged = false`, so the switch moves their declarations and
Tidewaiter restarts them when idle; a new kernel waits in the system profile
until Tidewaiter's window finds everyone gone. The module **asserts** that
`autoReboot.enable` and `autoUpgrade.allowReboot` are not both on: the two
would fight, and autoUpgrade's waits for no one. Schedule the upgrade before
the window (autoUpgrade's default `dates` is `04:40`, so with the default
`03:00-05:00` the reboot can follow the same night's upgrade).

## 7. Confinement and the closure a unit can see

Chandlery runs each server under `confinement.enable` (a `RootDirectory` with
only the unit's closure bound in). The confinement module computes that closure
from the unit's own definition at evaluation time - but a unit whose `ExecStart`
is `/nix/var/nix/profiles/chandlery/bedrock/bin/...` references no store path at
all, and after a rollback the profile points at a path the current definition
has never heard of. Without help, both the update and the rollback would start
in a root with no program in it.

So for a unit with `bindClosure = true` (the module defaults it to
`config.confinement.enable`), Tidewaiter maintains a runtime drop-in:

```ini
# /run/systemd/system/chandlery-bedrock.service.d/50-tidewaiter-closure.conf
[Service]
BindReadOnlyPaths=/nix/var/nix/profiles/chandlery
BindReadOnlyPaths=/nix/store/...-bedrock-server-1.21.2
BindReadOnlyPaths=/nix/store/...-glibc-2.40
...
```

- The list is `nix-store --query --requisites <target>`, plus the profile's
  directory (so the profile symlink and its generation link resolve inside the
  root).
- Rewritten, followed by `daemonReload()`, before the restart into `desired` and
  again before the restart back into the previous generation (steps 5.4 and 5.9).
- At boot, and on every switch that changes a declaration, the module's
  `tidewaiter-prepare.service` runs `tidewaiter prepare`, which writes the
  drop-in for each unit's *current* profile target (and seeds a missing
  profile, below) and reloads systemd, before any managed unit starts - `/run`
  is empty after a reboot. (Designed as an activation snippet; see section 9
  for why it is a unit.)
- `prepare` writes a drop-in only when there is none, or the one there names
  another profile (a `# Profile:` line records which); a missing one is
  created with `link()`, so it never replaces one the daemon wrote meanwhile.
  Once a drop-in exists the daemon is its only writer: a switch restarts
  `prepare` while the daemon may be swapping, and a `prepare` that read the
  profile before the daemon's `set` and wrote after the daemon's bind would put
  the old closure back under the new version's restart. To have `prepare`
  rewrite a drop-in by hand (after moving a profile yourself), delete it and
  `systemctl restart tidewaiter-prepare`.

This keeps the confinement as tight as Chandlery wants it - only the running
version's closure - while letting the version change underneath. The simpler
alternative, binding all of `/nix/store` read-only, is noted as open question 5.

**Seeding.** A new unit's profile does not exist yet, and its `ExecStart` would
fail. `tidewaiter prepare` (from `tidewaiter-prepare.service`) does, for every
units file: if the profile does not exist, `nix-env --profile P --set DESIRED`.
A first install is not activity-gated: nothing is running to disturb. An
existing profile is never moved by `prepare`.

A closure path that is itself a symlink in the store is bound as written;
nixpkgs' confinement binds such a path's target instead, because systemd
cannot bind a symlink. No store path a program's closure is made of is a
symlink in practice, so this is not handled (open question 13).

## 8. Command line

| Flag | Env | Meaning |
|---|---|---|
| `--source NAME` | `TIDEWAITER_SOURCES` | `docker` or `systemd`, repeatable or comma separated. Default `docker`, so the image and every existing compose file behave exactly as now. |
| `--units-dir DIR` | `TIDEWAITER_UNITS_DIR` | Where the systemd source reads unit files. Default `/etc/tidewaiter/units`. |
| `--state-dir DIR` | `STATE_DIRECTORY` | Where the systemd source persists pins. Default `/var/lib/tidewaiter`; systemd's `StateDirectory=` sets the env. |
| `--status-file PATH` | `TIDEWAITER_STATUS_FILE` | Where `run` writes its status and `status` reads it (section 6). `run` writes none by default; `status` reads `/run/tidewaiter/status.json`, where the module has it written. |
| `--activity-interval SECONDS` | | The tick between passes. Default 15. Only runs with a status file or an automatic reboot. |
| `--auto-reboot-window HH:MM-HH:MM` | `TIDEWAITER_AUTO_REBOOT_WINDOW` | Reboot when needed, inside this daily window, once idle (section 6). Checked at start. |
| `--booted-system PATH`, `--current-system PATH` | | What "reboot needed" compares. Default `/run/booted-system` and `/nix/var/nix/profiles/system`. |

`--label` and `--docker-socket` only mean anything with the Docker source;
without it Tidewaiter never opens the Docker socket.

Commands: `run`, `check`, `list` work across every source. New: `prepare`, which
seeds missing profiles and writes the closure drop-ins (section 7), reloads
systemd if it wrote any, then exits; it reads only the units directory and
needs no Tidewaiter daemon. And `status` (section 6), which reads only the
status file, prints no banner, and exits 0 idle, 1 busy, 2 no daemon.

The usage line becomes "update Docker containers and systemd units only when
they are idle".

## 9. Nix

### Flake

`flake.nix`, one input (`nixpkgs`, `nixos-unstable` for its Bun - see open
question 6), for `x86_64-linux` and `aarch64-linux`, laid out as Portical's
branch does:

```
flake.nix
nix/package.nix        # bun build --compile, bun test in checkPhase
nix/module.nix         # services.tidewaiter + systemd.services.<n>.tidewaiter
nix/module-check.nix   # evaluation-only assertions on a configuration
nix/vm-test.nix        # the NixOS VM test (section 11)
```

Outputs:

- `packages.<system>.default` - `bun build --compile --minify --sourcemap
  --define 'TIDEWAITER_VERSION="<version>"' src/main.ts`, version from
  `package.json`'s major, `self.revCount` and `self.lastModifiedDate` (the
  `run.ts` scheme with the date for the run number). Tidewaiter has no runtime
  dependencies, so it builds in the sandbox from `src`, `test`, `package.json`
  and `tsconfig.json` alone; `typescript` and `@types/bun` are only for `tsc`,
  which the package does not run. `dontStrip` and `dontPatchELF`: a compiled
  Bun binary is Bun with the bundle appended, and stripping loses the bundle.
  `makeWrapper` *suffixes* `PATH` with `conntrack-tools` (conntrack),
  `iproute2` (ss) and `nix` (nix-env, nix-store), so the host's own come
  first: the module puts `config.nix.package` on the services' `path` rather
  than overriding the package. systemd and logind are only spoken to over the
  bus.
- `checks.<system>` - `package` (which runs `bun test`), `module`, `vm`.
- `nixosModules.default`.
- `formatter.<system>` - `nixfmt`.

### Module: `services.tidewaiter`

| Option | Type | Default | Meaning |
|---|---|---|---|
| `enable` | bool | `false` | Run the daemon. |
| `package` | package | this flake's | The Tidewaiter to run. |
| `sources` | nonEmptyListOf (enum ["systemd" "docker"]) | `["systemd"]` | One Tidewaiter per host runs every source. |
| `interval` | ints.positive | `60` | Seconds between update passes. Shorter than Docker's 300 because a pass costs no registry calls. |
| `activityInterval` | ints.positive | `15` | Seconds between activity ticks, keeping the status current. |
| `autoReboot.enable` | bool | `false` | Reboot when needed, inside the window, once everything is idle (section 6). |
| `autoReboot.window` | strMatching `HH:MM-HH:MM` (end may be `24:00`) | `"03:00-05:00"` | Local time; may cross midnight. |
| `dryRun` | bool | `false` | Report, change nothing. |
| `dockerSocket` | str | `"/run/docker.sock"` | With `"docker"` in `sources`. |
| `label` | str | `"tidewaiter.autoupdate"` | With `"docker"` in `sources`. |
| `extraArgs` | listOf str | `[]` | Passed through. |

### Module: `systemd.services.<name>.tidewaiter`

Declared by re-declaring `options.systemd.services` as `attrsOf (submodule
...)`, which merges into every service, as nixpkgs' confinement module and
Portical's `portical.forward` do. Declared whether or not
`services.tidewaiter.enable` is set, so Chandlery can declare it unconditionally.

| Option | Type | Default | Meaning |
|---|---|---|---|
| `desired` | nullOr package | `null` | The version the profile should point at. Non-null opts the unit in. |
| `profile` | strMatching "/nix/var/nix/profiles/.+" | `"/nix/var/nix/profiles/tidewaiter/<name>"` | The profile the unit's `ExecStart` runs from. |
| `ports` | listOf (strMatching "[0-9]{1,5}(/(tcp\|udp))?") | `[]` | The ports whose flows mean "in use", and that health probes. |
| `detector` | enum ["conntrack" "tcp" "none"] | `"conntrack"` | |
| `health` | listOf (enum ["systemd" "port-connect" "port-bound" "uptime"]) | all four | Combined; any active failure rolls back. |
| `idleSamples` | ints.positive | `3` | Consecutive idle passes before a swap. |
| `healthTimeout` | ints.positive | `120` | Seconds to wait for healthy. |
| `keepGenerations` | ints.unsigned | `1` | Previous profile generations kept for rollback. |
| `bindClosure` | bool | `config.confinement.enable` | Maintain the closure drop-in (section 7). |

When `desired != null`, the submodule sets, on that service:

- `restartIfChanged = false` - a switch never restarts it; Tidewaiter does,
  when idle. (`reloadIfChanged` untouched, `stopIfChanged` irrelevant.)

and the top level of the module sets:

- `environment.etc."tidewaiter/units/<name>.service.json".text` - the JSON of
  section 2, `desired` as `"${desired}"`, which also puts it in the system
  closure (built at switch, and kept from GC by the system generation).
- `tidewaiter-prepare.service` - a oneshot (`RemainAfterExit`) running
  `tidewaiter --units-dir /etc/tidewaiter/units prepare` through the Nix
  daemon, `wantedBy` multi-user and before every managed service, which each
  get `wants` and `after` it - `wants`, not `requires`, so its restart on a
  switch never restarts them. Its `restartTriggers` are the declarations, so a
  switch that changes one re-runs it.

  *Not* an activation snippet, as first designed: activation at boot runs in
  stage 2 before systemd, so before the Nix daemon - and in a NixOS VM test
  before the store's paths are even registered (`boot.postBootCommands` loads
  the database after activation), so `nix-env --set` fails "path is not
  valid". A unit also has to reload systemd after writing the drop-ins at boot,
  which `prepare` does.

  The unit is always defined and only `enable`d when a service opts in: whether
  a `systemd.services` attribute exists cannot depend on the other services'
  options without infinite recursion.
- a warning if any unit opts in while `services.tidewaiter.enable` is false: the
  unit will never be updated, only seeded; and one if `sources` lacks
  `"systemd"`.
- an assertion that `autoReboot.enable` and `system.autoUpgrade.allowReboot`
  (with `autoUpgrade.enable`) are not both on, and one that the window is not
  empty.
- `environment.systemPackages` gets the package, for `tidewaiter status`.
- an assertion that each unit's `ports` is non-empty when its `detector` is not
  `"none"` - the runtime fail-safe would otherwise defer it forever.

`ExecStart` is the unit's own: Chandlery's `packagePath` is what points it at
`profile`. Tidewaiter's module never rewrites another service's command.

### The daemon's unit

Root, because it must restart other units, write system profiles and read the
host conntrack table - and confined as far as that allows:

- `wantedBy = [ "multi-user.target" ]`, `after = [ "network.target"
  "nix-daemon.socket" ]`, `Restart = "on-failure"`, `StateDirectory =
  "tidewaiter"`, and `RuntimeDirectory = "tidewaiter"` for the status file
  (`--status-file /run/tidewaiter/status.json`).
- `Environment = "NIX_REMOTE=daemon"`, so `nix-env` goes through the Nix daemon
  instead of writing the store itself - no `CAP_CHOWN`/`CAP_FOWNER` needed -
  and `HOME=/var/lib/tidewaiter`, since root's home is out of sight.
- `TimeoutStopSec = "15min"`: a stop waits for the pass in flight, and a swap
  can take a game's whole `TimeoutStopSec`, its start and two gates; killing it
  halfway leaves a profile moved with no gate run. `KillMode = "mixed"`, so a
  `nix-env` it is running is not signalled before Tidewaiter has finished with
  it.
- `CapabilityBoundingSet = [ "CAP_NET_ADMIN" ]` (conntrack over netlink),
  `AmbientCapabilities` not needed (uid 0). Everything Tidewaiter writes is
  root-owned, so uid 0 without `CAP_DAC_OVERRIDE` suffices.
- `ProtectSystem = "strict"` with `ReadWritePaths = [ "/nix/var/nix/profiles"
  "/run/systemd/system" ]`; `ProtectHome`, `PrivateTmp`, `PrivateDevices`,
  `ProtectKernelTunables`, `ProtectKernelModules`, `ProtectKernelLogs`,
  `ProtectControlGroups` (`ss` only reads), `ProtectClock`, `ProtectHostname`,
  `RestrictNamespaces`, `RestrictRealtime`, `RestrictSUIDSGID`,
  `LockPersonality`, `NoNewPrivileges`, `SystemCallArchitectures = "native"`,
  `SystemCallFilter = [ "@system-service" "~@privileged" ]`, `UMask = "0022"` (the drop-ins
  must be world-readable for systemd's manager - it reads as root anyway, but
  operators should be able to).
- `RestrictAddressFamilies = [ "AF_UNIX" "AF_NETLINK" "AF_INET" "AF_INET6" ]`:
  the bus and Nix daemon, conntrack and `sock_diag`, and port-connect.
- `IPAddressAllow = "localhost"`, `IPAddressDeny = "any"` when `sources` is only
  `["systemd"]` - the only IP it ever opens is port-connect to loopback. With
  Docker, the registry needs the internet, so no IP filter.
- With `"docker"`: `SupplementaryGroups = [ "docker" ]`.
- Not `MemoryDenyWriteExecute` (Bun's JIT), not `PrivateNetwork` (conntrack and
  port-connect are the host's), not `PrivateUsers` (it must be real root to the
  bus and to `nix-env`).
- `ProcSubset`/`ProtectProc` left default: the `tcp` detector reads
  `/proc/<MainPID>/net/*`.

`systemd-analyze security tidewaiter` is run in the VM test: **2.0 OK**
(recorded in the README). Uid 0 with only `CAP_NET_ADMIN` is enough for
systemd's `RestartUnit` and `Reload`, and for logind's `Reboot` - both
recognise root by uid - as the VM test shows.

## 10. How each shared contract is met

| Contract | How |
|---|---|
| Opt-in shape `systemd.services.<unit>.tidewaiter = { desired; profile; ports; detector; health; idleSamples; }` | Exactly those names, plus `healthTimeout`, `keepGenerations`, `bindClosure` (section 9). |
| Module sets `restartIfChanged = false` | In the submodule, when `desired != null`. |
| Module writes the desired store path to `/etc/tidewaiter/<unit>.json` "or similar" | `/etc/tidewaiter/units/<unit>.json`, with the whole policy, not just `desired` - the directory is the list of managed units. |
| Daemon: profile != desired -> wait idle -> `nix-env --set` -> restart -> health -> on failure roll back and restart | Section 5. Rollback is `--switch-generation <recorded>`, which is `--rollback` made exact. |
| Inhibitor while players are on, mode decided and documented | Built as a `block` lock, then replaced at the owner's request: humans can always reboot. `tidewaiter status` says whether anyone is on, and `autoReboot` reboots only inside a window once everything is idle; autoUpgrade runs with `allowReboot = false` (section 6). |
| Chandlery `packagePath` defaulting to `"${package}"` | A Chandlery server sets `packagePath = profile` and `tidewaiter.desired = package`; `tidewaiter-prepare.service` seeds the profile before the unit first starts, so it starts on day one. |
| Chandlery confinement | `bindClosure` (section 7) binds the profile dir and the *current* target's closure, and rewrites it before every restart, so update and rollback both start. **Needs Chandlery to not also hard-code its package's closure as the only store path in the root** - it may, harmlessly, but then an old version's closure lingers until the next switch. |
| Chandlery `ExecStop` save-and-quit, `TimeoutStopSec=5min`, `Type=notify` | Tidewaiter only `RestartUnit`s; the job waits for ExecStop and then READY=1. The `systemd` check reads a failed job as unhealthy. |
| Chandlery console FIFO socket unit | Untouched. Restarting the service leaves `chandlery-<n>.socket` and its FIFO up. |
| Treesinger socket, ExecStartPre minting | Untouched: it runs on each start, including Tidewaiter's restart. A failed mint degrades to unauthenticated per its own contract, which the `systemd` check sees as a normal start. |
| Portical `systemd.services.<unit>.portical.forward` | Independent. Typically the same ports appear in both; a unit can share a `let ports = ...` between `portical.forward` and `tidewaiter.ports` only while no forward remaps a port. For `external:internal/proto` (e.g. `19133:19132/udp`), `tidewaiter.ports` takes the internal port (`19132/udp`) - the one the unit binds and conntrack sees flows reach - written out or derived with `map (p: lib.last (lib.splitString ":" p))`. Nothing depends on Portical. |

A Chandlery server, in full, for the README:

```nix
systemd.services."chandlery-bedrock".tidewaiter = {
  desired = config.services.chandlery.servers.bedrock.package;
  profile = "/nix/var/nix/profiles/chandlery/bedrock";
  ports = [ "19132/udp" "19133/udp" ];
};
services.chandlery.servers.bedrock.packagePath = "/nix/var/nix/profiles/chandlery/bedrock";
services.tidewaiter.enable = true;
```

## 11. Test plan

All new TypeScript in the existing style: pure functions tested against
captured real output, everything with I/O behind an interface with an in-memory
fake. Docker tests unchanged and green throughout.

- **`parseUnitPolicy`** - a full file; only required fields (defaults); each
  bad field warned and defaulted; missing `desired`/`profile`/`unit` skipped;
  not JSON skipped; `docker` in `health` dropped with a warning; `netio`
  rejected; bare `unit` gets `.service`; ports grammar shared with labels.
- **`parseListeners`** - captured `ss -Hlntu -e` from a real host (fixture):
  TCP LISTEN and UDP UNCONN, v4/v6/wildcard/`%lo`, a socket-unit cgroup, no
  `cgroup:` field at all. **`unitBoundPorts`** - cgroup equal, beneath,
  sibling with a shared prefix (`foo.service` vs `foo.service2`) not matched.
- **`NixProfiles`** - against a temporary directory and real `nix-env` where
  available (skipped otherwise): set creates the parent, generation numbers,
  switchGeneration, prune keeps `+N`; and pure `generationOf("P-12-link")`.
- **`systemd` health check** - each `UnitState`/`JobResult` combination to
  pass/fail/inconclusive; `NRestarts` and `InvocationID` movement.
- **`SystemdSource`** with `FakeSystemd`, `FakeProfiles`, `FakeListeners`,
  `FakeConntrackSource`, `FakeTcpProbe` and an in-memory units directory and
  state store:
  - keep when `current == desired`;
  - defer while conntrack shows a UDP flow to a declared port; the debounce;
  - a flow arriving between the decision and the swap defers (re-sample);
  - healthy swap: set, restart, gate, prune; order of calls asserted;
  - job `failed` -> switch back to the recorded generation, restart, pin;
  - port never bound -> uptime fails -> rollback;
  - pin survives a new `SystemdSource` over the same state store;
  - a new `desired` clears the pin;
  - `nix-env` failing -> undo, counted, pinned after `MAX_SWAP_FAILURES`;
  - stopped unit -> profile set, no restart;
  - `not-found` unit -> warned, left alone;
  - `hold()` called before the swap;
  - `bindClosure`: drop-in written for desired before restart and for the
    previous path before the rollback restart, each followed by daemonReload.
- **`Daemon`** - two sources in one pass share one conntrack dump; outcomes
  concatenated; the status names exactly the busy workloads, and the swapping
  one before its swap starts; the activity tick updates it without touching
  idle streaks; a source that throws does not stop the other; `run` clears it
  on the way out.
- **Automatic reboot** (`FakeBootCheck`, `FakeRebooter`, a settable clock) -
  reboots when needed, in the window and settled, once only; not when not
  needed or when it cannot tell; waits outside the window and goes in the next
  day's; waits for a busy workload of either source, an unsettled or unsampled
  one, a swap, someone found by the last look, a last look that failed; asks
  again after a refusal; never from a failed pass. `parseWindow` and
  `inWindow`, midnight crossing included; `NixosBootCheck` over temporary
  system directories; `LogindRebooter` against the fake bus; `decideReboot`.
- **Status** - `describeStatus`, `FileStatus` round trip, and `main(["status"])`
  exiting 0, 1 and 2.
- **D-Bus** - Portical's wire-format tests come with `dbus.ts`; new golden bytes
  for `RestartUnit`, `GetAll` on a Service, and a `JobRemoved` signal decoded.
- **`prepare`** - seeds only missing profiles; writes the drop-in only where it is missing or names another profile, never over the daemon's.
- **Arguments** - `--source` repeated and comma separated, unknown rejected,
  `--units-dir`, `--state-dir`/`STATE_DIRECTORY`, `--status-file`,
  `--auto-reboot-window` (checked), `--booted-system`, `--current-system`.
- **Nix**
  - `checks.package`: the build, with `bun test` in `checkPhase`.
  - `checks.module`: evaluate a configuration with an opted-in unit and assert
    the rendered `/etc/tidewaiter/units/x.service.json`, `restartIfChanged =
    false`, the ordering after `tidewaiter-prepare.service` (wants, not
    requires) and that unit itself, and the daemon's `ExecStart` and
    sandbox; and that importing the module with nothing opted in and the
    daemon off adds nothing.
  - `checks.vm`, one node:
    - `echo-game`: a tiny Python UDP+TCP echo built three ways - `v1`, `v2`
      (answers "v2") and `bad` (exits after 2 s without binding) - with
      `ExecStart = "${profile}/bin/echo-game"`, `Type=notify` by writing
      `READY=1` to `NOTIFY_SOCKET` itself after binding (systemd binds the
      socket into the confined root), `confinement.enable = true` so
      `bindClosure` is exercised, `tidewaiter = { desired = v1; ports = [
      "7777/udp" "7777/tcp" ]; idleSamples = 2; healthTimeout = 20; }`.
    - specialisations `v2` and `bad` that only change `tidewaiter.desired`.
    - `services.tidewaiter = { enable = true; interval = 2; activityInterval =
      1; autoReboot = { enable = true; window = "00:00-24:00"; }; }`, with
      `--current-system /run/installed-system`, a tmpfiles link to
      `/run/booted-system` until the test replaces it;
      `net.netfilter.nf_conntrack_udp_timeout{,_stream} = 5` so an idle UDP
      flow ages out within the test; a persistent journal.
    - Script: boots on v1 (profile seeded by prepare, closure drop-in written,
      confined, answers "v1" on UDP and TCP); start a UDP player sending every
      second (a transient unit); assert `tidewaiter status`, run as `nobody`,
      exits 0 before and 1 with `echo-game.service` in use after; switch to
      `v2` with `switch-to-configuration test` and assert the unit's
      `InvocationID` did not change (restartIfChanged); assert no inhibitor of
      tidewaiter's is listed and that `shutdown -r +5` is accepted (then
      cancelled); wait five passes and assert still v1 and not restarted; stop
      the player; wait for the profile to point at v2, a new `InvocationID`,
      "v2" on the port, "updated to v2 and it is healthy" in the journal, and
      status exiting 0 again.
    - Switch to `bad`: wait for "rolled back to v2" in the journal; profile
      back on v2, unit active, answers "v2"; five more passes with no further
      restart, and the pin in `/var/lib/tidewaiter/systemd.json`; the bad
      generation deleted (`--list-generations` shows two). Restart
      `tidewaiter.service` and assert it still does not retry (persisted pin).
    - `systemd-analyze security tidewaiter.service` printed into the log.
    - Automatic reboot: a second player; replace `/run/installed-system` with
      one whose kernel differs; wait for "needed, kernel changed since boot;
      waiting: echo-game.service in use" in the journal and in the status, and
      five passes with the machine still up; stop the player; the machine
      powers off (the test driver runs QEMU with `-no-reboot`); start it again
      and find "rebooting, as nothing is in use" in the previous boot's
      journal, and "not needed" in this one's.
    - Runs in about 90 s of test script on a KVM host.
- Also: `bun run.ts check` (tsc) and the Docker image build unchanged.

## 12. Where this stands

Refactors made first, behaviour unchanged:

- **`src/loop.ts` `runEvery`** - the single-flight interval loop.
- **`src/gate.ts` `pollVerdict`, `sleep`, `uptimeSeconds`** - the health gate's
  poll-until-settled loop and the uptime grace, shared by both sources.
- **`src/decide.ts` `Decidable`, `Words`** - the part of a `Snapshot` `decide()`
  reads, and the wording of its reasons.
- **`src/health.ts` `HealthSubject`, `BoundPorts`, `netnsBoundPorts`** - the
  checks read a subject and a bound-ports function, not a container and its
  netns.
- **`src/labels.ts`** - `parsePortTerm` and the field validators exported, so a
  units file is read with the labels' rules.

Built on them, each with in-memory fakes (`test/fakes/systemd.ts`,
`test/fakes/source.ts`):

| File | What |
|---|---|
| `src/source.ts` | `Source`, `PassContext`, `Outcome`, `MAX_SWAP_FAILURES` |
| `src/daemon.ts` | `Daemon`: shared dump, sources in turn, summary, status, activity tick, automatic reboot |
| `src/docker-source.ts` | `DockerSource`, plus `activity()` and `list()` |
| `src/status.ts` | `Status`, `StatusSink`, `FileStatus`, `describeStatus` |
| `src/reboot.ts` | `parseWindow`, `inWindow`, `NixosBootCheck`, `LogindRebooter`, `decideReboot` |
| `src/units.ts` | `parseUnitPolicy`, `DirUnitsDirectory` |
| `src/listeners.ts` | `parseListeners`, `unitBoundPorts`, `SsListenerSource` |
| `src/profile.ts` | `Profiles`, `NixProfiles`, `generationOf` |
| `src/systemd.ts` | `UnitState`, `SystemdClient`, `systemdVerdict`, `DbusSystemdClient` |
| `src/dbus.ts` | the D-Bus wire format and connection, copied from Portical's branch |
| `src/prepare.ts` | `prepare`: seed missing profiles, write closure drop-ins, reload |
| `src/closure.ts` | `ClosureBinder`, `NixClosureBinder`, `closureDropIn` |
| `src/state-store.ts` | `UnitMemory`, `FileStateStore` |
| `src/systemd-source.ts` | `SystemdSource`: sections 2-5 and 7's swap side |

`main.ts` builds the sources section 8's flags name - Docker alone by
default, so the image behaves as before - the `FileStatus` with
`--status-file`, and the automatic reboot with `--auto-reboot-window`. `flake.nix` and `nix/` are section 9, and `nix/vm-test.nix` the VM
test of section 11.

`DbusSystemdClient` subscribes to `JobRemoved` once per connection, before its
first call, and a `JobRemoved` that arrives before the reply to `RestartUnit`
is kept for the restart that asked; a bus that drops while a restart waits
rejects it, which the source treats as a swap-step failure (undo, and restart
back only if the `InvocationID` moved). It uses `LoadUnit` rather than
`GetUnit`, so a unit nothing has started yet is found, and a name with no unit
file reads `not-found`.

Where building part 2 changed the design: `prepare` runs from a unit, not an
activation snippet (section 9); the wrapper suffixes `PATH` instead of the
module overriding `nix`; the daemon's unit gained `TimeoutStopSec`,
`KillMode=mixed`, `HOME` and `~@privileged`; logind was found to refuse a
reboot at schedule time - and then, because a block lock refused a person's
reboot too, the lock was replaced by `tidewaiter status` and Tidewaiter's own
automatic reboot (section 6).

Checked: `bun test` (all pass; the tests needing a Nix daemon or the system bus
run on a development host and skip in the sandbox), `tsc --noEmit`, `nix flake
check` (package with `bun test` under nixpkgs' Bun 1.4.2, module, VM).

## 13. Open questions

1. **No catching up.** A host busy through every window never reboots by
   itself, however old its kernel. Add a deadline - reboot in the window
   anyway once a reboot has been needed for N days, with a loud log - or stay
   strict? The design is strict: a person can always reboot by hand, and
   `tidewaiter status` says why it has not.
2. **A broken detector, or an unreachable registry, holds automatic reboots
   off.** Fail-safe says busy; a container whose registry did not answer is
   never sampled that pass, so is not settled. Sample Docker activity before
   resolving digests, or cap how long an unreadable workload may hold a reboot
   off? Strict for now.
3. **A status that outlives its daemon.** Under the module systemd removes
   `/run/tidewaiter` when the daemon stops, and `run` clears the file on the
   way out, but a daemon killed outside systemd leaves the last one behind;
   `status` shows its age, not a verdict on it. Exit 2 when it is older than a
   few intervals?
4. **Stopped units.** The design moves a stopped unit's profile to `desired`
   with no gate. Leave its profile alone instead, so a manually-stopped unit
   comes back on what it last ran?
5. **Closure drop-in vs binding `/nix/store`.** The drop-in keeps confinement
   tight but adds `nix-store -qR`, a file in `/run` and a daemon-reload per
   swap. Binding `/nix/store` read-only in Chandlery's units is simpler, and the
   store is world-readable anyway. Which does Chandlery prefer? (Needs agreeing
   with the Chandlery branch.)
6. **Which Bun builds the flake.** `mise.toml` pins `1.3` (1.3.14 locally),
   the flake builds with nixpkgs-unstable's 1.4.2, and Portical's branch moved
   its `mise.toml` to 1.4.2. The whole suite passes on both, so the flake just
   uses 1.4.2; bump `mise.toml` (and the image) to match?
7. **Unit config changes.** With `restartIfChanged = false`, a change to the
   unit's *own* definition (a new flag in `ExecStart`) waits for the next
   version change to take effect. Should the module include a hash of the
   unit's definition in the JSON, and Tidewaiter treat a new hash like a new
   `desired` - restart when idle?
8. **Re-sample before the swap for Docker too** (section 3)? Small, and closes
   the same window, but it changes Docker behaviour, which this branch promises
   not to.
9. **Notification.** "Log/notify": the design logs to the journal. Worth an
   `--on-rollback COMMAND` hook, or `STATUS=` via `sd_notify` so `systemctl
   status tidewaiter` shows busy/pinned units?
10. **A `symlink` profile kind** for plain-systemd hosts without Nix: the same
    flow over `/opt/app/current -> /opt/app/releases/X`, keeping the previous
    target for rollback. Small once `Profiles` is an interface; worth shipping
    in v1 for the "useful beyond NixOS" goal?
11. **Outbound flows to a declared port.** conntrack matches by destination port
    only, so the host itself talking *out* to another server on 19132 would
    read as a player. Also match the flow's original destination address
    against the host's own addresses?
12. **Docker reads a failed conntrack dump as an empty table** in a pass - so
    as idle, though it logs "assuming busy where it matters". That predates
    this branch and is kept, as promised; the systemd source reads it as busy.
    The activity tick does read it as busy for a `conntrack`-detector
    container, and an automatic reboot's last look is a tick's, so a reboot is
    never taken on a read that failed. Make Docker's swaps fail safe too?
13. **Symlinked store paths in a closure** are bound as written, where nixpkgs'
    confinement binds their target, since systemd cannot bind a symlink
    (section 7). Not seen in a real closure; handle it anyway?
14. **A service newly declared in a switch** relies on
    `switch-to-configuration` restarting `tidewaiter-prepare.service` (its
    triggers changed) before it starts the new service, which is ordered after
    it. The VM test covers boot and a changed `desired`, not a unit added by a
    switch. Worth a VM subtest?
15. **`tidewaiter-prepare` is not hardened.** It runs as plain root for a few
    seconds per switch. Give it the daemon's sandbox too?

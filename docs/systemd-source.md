# Updating systemd units

Tidewaiter was written to update Docker containers. What makes it worth having
- wait until nobody is using the thing, swap it, health-gate the swap, roll back
on failure - has nothing to do with Docker. This is the design for a second
source of things to update: **systemd units whose program lives in a Nix
profile**, so a native service on NixOS gets the same care a labelled container
does. Docker behaviour does not change.

Status: sections 1-7 are built and unit-tested (see "Where this stands",
section 12): the `Daemon` over sources, `DockerSource`, `SystemdSource`, the
inhibitor, profiles, listeners, the closure drop-in and the persisted state.
Not yet: the D-Bus `SystemdClient`, the command line of section 8, `tidewaiter
prepare`, and all of section 9 (flake, module, VM test).

The shape, end to end, for one unit:

```
nixos-rebuild switch (or system.autoUpgrade)
  -> /etc/tidewaiter/units/chandlery-bedrock.service.json  {"desired": "/nix/store/...-bedrock-1.21.2", ...}
  -> the unit itself is NOT restarted (restartIfChanged = false)

tidewaiter, every pass:
  current = realpath /nix/var/nix/profiles/chandlery/bedrock
  desired != current?
    players on 19132/udp (conntrack)?            -> defer, hold the shutdown inhibitor
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
6. Holding off reboots: the logind inhibitor
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
   * Take the shutdown inhibitor for `workload` until the end of the pass.
   * Called before any swap starts, so a reboot cannot land mid-swap.
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
}

export interface Source {
  readonly name: "docker" | "systemd";
  /** Reconcile every workload once. One workload failing never fails the pass. */
  once(context: PassContext): Promise<Outcome[]>;
  /** Activity only - no digests, no swaps - for the inhibitor's faster tick. */
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
    inhibitor: Inhibitor,          // NoInhibitor when --no-inhibit
    options: DaemonOptions,        // interval, activityInterval, dryRun
    log: Log,
  );
  once(): Promise<Outcome[]>;      // dump conntrack, each source in order, summary, inhibitor
  run(signal: AbortSignal): Promise<void>;  // runEvery(interval, once) + the activity tick
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
| Shutdown inhibitor | `src/inhibit.ts` | Daemon-owned, fed by every source (section 6). |

`DockerSource` (`src/docker-source.ts`) is the old `Tidewaiter` class renamed,
with `once()` taking the `PassContext` instead of dumping conntrack itself, and
`run()` moving to the `Daemon`. Its logic, messages and tests are otherwise
untouched: `daemon.test.ts` keeps driving it through the same fakes, behind a
`Daemon` with one source. A failed conntrack dump reaches it as `undefined`,
which it reads as an empty table - exactly what it did before (see open
question 12).

If a source throws, the others still run, its workloads keep their last busy
verdicts (so a source that cannot be read never drops a lock it held), and the
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
   1. `context.hold(unit)` - the inhibitor covers the swap.
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
`desired` with the gate never run. On start it cannot tell that from a healthy
earlier swap, so it does what Docker does - adopts what is running as the
baseline, with a warning - but it does have one more fact: if the unit is
`failed` and the profile's current generation is newer than the daemon's last
recorded good one, it rolls back to that one (holding the inhibitor, with the
same gate) and pins the version it found. "Last good generation" is recorded per
unit after every healthy swap, and on first sight of a unit that is up.

## 6. Holding off reboots: the logind inhibitor

While any managed workload (unit or container) is busy or being swapped,
Tidewaiter holds a logind inhibitor lock:

- `what = shutdown:sleep`, `who = tidewaiter`, `why = "in use:
  chandlery-bedrock.service, chandlery-hytale.service"`.
- **`mode = block`.** `delay` only defers for `InhibitDelayMaxSec` (5 s by
  default) - useless against a session that lasts hours. `block-weak` is
  ignored by privileged callers, and `system.autoUpgrade` reboots as root.
  Since **systemd 257**, `block` locks are enforced against privileged callers
  too (org.freedesktop.login1(5): "Since systemd version 257 inhibitors of type
  "block" are enforced on operations requested by both privileged and
  unprivileged users"); bypassing one takes an explicit
  `SD_LOGIND_SKIP_INHIBITORS` / `systemctl --check-inhibitors=no` / `-i`.
  NixOS 25.05 and later ship 257+; the development host is on 260.

**How it is held.** `Manager.Inhibit` returns the lock as a file descriptor,
passed with `SCM_RIGHTS`; neither the hand-written D-Bus client nor Bun's
`node:net` can receive one. So the lock is held by a child:

```sh
systemd-inhibit --what=shutdown:sleep --mode=block --who=tidewaiter --why="..." cat
```

with `cat`'s stdin a pipe from Tidewaiter. `systemd-inhibit` only runs `cat`
once it holds the lock, so Tidewaiter writes a newline and waits for `cat` to
echo it back: that echo is the acknowledgement, and a `systemd-inhibit` that
could not take the lock exits without it, which `set()` reports as an error
(logged once by the daemon; never fatal). A child that has died is noticed on
the next `set()` and replaced. Releasing is closing the pipe; and if
Tidewaiter dies for any reason, the kernel closes it, `cat` exits,
`systemd-inhibit` exits and the lock is gone - no lock can outlive the daemon.
Changing the `why` takes the new lock before dropping the old, so there is never
a gap.

```ts
// src/inhibit.ts
export interface Inhibitor {
  /** Hold the lock for exactly these workloads; empty releases it. Idempotent. */
  set(workloads: readonly string[]): Promise<void>;
}
export class SystemdInhibitor implements Inhibitor { /* spawns systemd-inhibit as above */ }
export const NoInhibitor: Inhibitor;
```

**When it is refreshed.** On every `hold()` (a swap is about to start), at the
end of every pass (the busy workloads; holds end with the pass, because the
swap they covered is over), and on a faster **activity tick**
(`--activity-interval`, default 15 s) that only dumps conntrack and runs each
source's `activity()` - no digests, no swaps, no effect on idle streaks. A
player who joins between update passes is covered within one tick, not one
`interval`.

**Fail-safe applies.** A workload whose activity cannot be read counts as busy,
so it holds the lock too. That is the same rule as the swap gate, but it means a
broken detector blocks reboots until it is fixed; see open question 2.

**How `system.autoUpgrade` respects it.** With `allowReboot = true`, nixpkgs'
`nixos-upgrade.service` runs `nixos-rebuild boot` and then, when the kernel,
initrd or modules changed and it is inside `rebootWindow`, `shutdown -r +1`.
That is a logind `ScheduleShutdown`, checked against block inhibitors like any
other shutdown request. While the lock is held the reboot is refused, the
upgrade unit logs the refusal, and the new generation stays installed for the
next boot. The next night's run finds the booted and built kernels still differ
and tries again - so a busy server reboots at the first window in which nobody
is playing, with nothing to configure in `system.autoUpgrade`.

Whether logind refuses at *schedule* time or holds the scheduled shutdown until
the lock goes is verified by the VM test (section 11) rather than assumed; both
keep players safe. If it refuses, `nixos-upgrade.service` ends `failed` that
night, which is noisy but accurate. Not done: wrapping the reboot in an
`ExecCondition` - it would also skip the switch-only upgrades, which never
disrupt anyone.

A person at a terminal running `systemctl reboot` is told who holds the lock and
why (systemctl checks inhibitors client-side on a TTY), and `-i` overrides it
deliberately. `systemd-inhibit --list` shows the lock.

The inhibitor works for the Docker source too when Tidewaiter runs natively;
in the container it has no logind, so the CLI default is `--no-inhibit` and the
NixOS module turns it on.

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
- At boot and on every switch the module's activation snippet runs `tidewaiter
  prepare`, which writes the drop-in for each unit's *current* profile target
  (and seeds a missing profile, below) before systemd starts anything - `/run`
  is empty after a reboot.

This keeps the confinement as tight as Chandlery wants it - only the running
version's closure - while letting the version change underneath. The simpler
alternative, binding all of `/nix/store` read-only, is noted as open question 5.

**Seeding.** A new unit's profile does not exist yet, and its `ExecStart` would
fail. `tidewaiter prepare` (from the activation snippet) does, for every units
file: if the profile does not exist, `nix-env --profile P --set DESIRED`. A
first install is not activity-gated: nothing is running to disturb.

## 8. Command line

| Flag | Env | Meaning |
|---|---|---|
| `--source NAME` | `TIDEWAITER_SOURCES` | `docker` or `systemd`, repeatable or comma separated. Default `docker`, so the image and every existing compose file behave exactly as now. |
| `--units-dir DIR` | `TIDEWAITER_UNITS_DIR` | Where the systemd source reads unit files. Default `/etc/tidewaiter/units`. |
| `--state-dir DIR` | `STATE_DIRECTORY` | Where the systemd source persists pins. Default `/var/lib/tidewaiter`; systemd's `StateDirectory=` sets the env. |
| `--inhibit` / `--no-inhibit` | `TIDEWAITER_INHIBIT` | Hold the logind lock while busy. Default off (the container has no logind); the module turns it on. |
| `--activity-interval SECONDS` | | The inhibitor's tick. Default 15. Only meaningful with `--inhibit`. |

`--label` and `--docker-socket` only mean anything with the Docker source;
without it Tidewaiter never opens the Docker socket.

Commands: `run`, `check`, `list` work across every source. New: `prepare`, which
seeds missing profiles and writes the closure drop-ins (section 7), then exits;
it reads only the units directory and needs no daemon.

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
  `makeWrapper` puts on `PATH`: `conntrack-tools` (conntrack), `iproute2` (ss),
  `systemd` (systemd-inhibit), `nix` (nix-env, nix-store), `coreutils`.
  The module overrides `nix` with `config.nix.package`.
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
| `activityInterval` | ints.positive | `15` | Seconds between activity ticks for the inhibitor. |
| `inhibit` | bool | `true` | Hold the shutdown/sleep block lock while anything managed is busy. |
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
- `system.activationScripts.tidewaiter` - `${package}/bin/tidewaiter prepare
  --units-dir /etc/static/tidewaiter/units` (the files' store copy, readable
  before `/etc` is linked), with `nix` on `PATH`.
- a warning if any unit opts in while `services.tidewaiter.enable` is false: the
  unit will never be updated, only seeded.
- an assertion that each unit's `ports` is non-empty when its `detector` is not
  `"none"` - the runtime fail-safe would otherwise defer it forever.

`ExecStart` is the unit's own: Chandlery's `packagePath` is what points it at
`profile`. Tidewaiter's module never rewrites another service's command.

### The daemon's unit

Root, because it must restart other units, write system profiles and read the
host conntrack table - and confined as far as that allows:

- `wantedBy = [ "multi-user.target" ]`, `after = [ "network.target" ]`,
  `Restart = "on-failure"`, `StateDirectory = "tidewaiter"`.
- `Environment = "NIX_REMOTE=daemon"`, so `nix-env` goes through the Nix daemon
  instead of writing the store itself - no `CAP_CHOWN`/`CAP_FOWNER` needed.
- `CapabilityBoundingSet = [ "CAP_NET_ADMIN" ]` (conntrack over netlink),
  `AmbientCapabilities` not needed (uid 0). Everything Tidewaiter writes is
  root-owned, so uid 0 without `CAP_DAC_OVERRIDE` suffices.
- `ProtectSystem = "strict"` with `ReadWritePaths = [ "/nix/var/nix/profiles"
  "/run/systemd/system" ]`; `ProtectHome`, `PrivateTmp`, `PrivateDevices`,
  `ProtectKernelTunables`, `ProtectKernelModules`, `ProtectKernelLogs`,
  `ProtectControlGroups` (`ss` only reads), `ProtectClock`, `ProtectHostname`,
  `RestrictNamespaces`, `RestrictRealtime`, `RestrictSUIDSGID`,
  `LockPersonality`, `NoNewPrivileges`, `SystemCallArchitectures = "native"`,
  `SystemCallFilter = [ "@system-service" ]`, `UMask = "0022"` (the drop-ins
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

`systemd-analyze security tidewaiter` is run in the VM test and its exposure
level recorded in the README.

## 10. How each shared contract is met

| Contract | How |
|---|---|
| Opt-in shape `systemd.services.<unit>.tidewaiter = { desired; profile; ports; detector; health; idleSamples; }` | Exactly those names, plus `healthTimeout`, `keepGenerations`, `bindClosure` (section 9). |
| Module sets `restartIfChanged = false` | In the submodule, when `desired != null`. |
| Module writes the desired store path to `/etc/tidewaiter/<unit>.json` "or similar" | `/etc/tidewaiter/units/<unit>.json`, with the whole policy, not just `desired` - the directory is the list of managed units. |
| Daemon: profile != desired -> wait idle -> `nix-env --set` -> restart -> health -> on failure roll back and restart | Section 5. Rollback is `--switch-generation <recorded>`, which is `--rollback` made exact. |
| Inhibitor while players are on, mode decided and documented | `shutdown:sleep`, `block`, held by a `systemd-inhibit` child; section 6, including how autoUpgrade's `shutdown -r +1` is refused. |
| Chandlery `packagePath` defaulting to `"${package}"` | A Chandlery server sets `packagePath = profile` and `tidewaiter.desired = package`; Tidewaiter seeds the profile on first activation, so the unit starts on day one. |
| Chandlery confinement | `bindClosure` (section 7) binds the profile dir and the *current* target's closure, and rewrites it before every restart, so update and rollback both start. **Needs Chandlery to not also hard-code its package's closure as the only store path in the root** - it may, harmlessly, but then an old version's closure lingers until the next switch. |
| Chandlery `ExecStop` save-and-quit, `TimeoutStopSec=5min`, `Type=notify` | Tidewaiter only `RestartUnit`s; the job waits for ExecStop and then READY=1. The `systemd` check reads a failed job as unhealthy. |
| Chandlery console FIFO socket unit | Untouched. Restarting the service leaves `chandlery-<n>.socket` and its FIFO up. |
| Treesinger socket, ExecStartPre minting | Untouched: it runs on each start, including Tidewaiter's restart. A failed mint degrades to unauthenticated per its own contract, which the `systemd` check sees as a normal start. |
| Portical `systemd.services.<unit>.portical.forward` | Independent. Typically the same port list appears in both; a unit can share a `let ports = ...` between `portical.forward` and `tidewaiter.ports`. Nothing depends on Portical. |

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
  concatenated; the inhibitor is set to exactly the busy plus held workloads and
  released when they go idle; the activity tick updates the lock without
  touching idle streaks; a source that throws does not stop the other.
- **`SystemdInhibitor`** - with a fake `systemd-inhibit` script on `PATH` that
  records its arguments and lives while its stdin is open: acquire, change why
  (new before old), release; the child exits when the daemon's pipe closes.
- **D-Bus** - Portical's wire-format tests come with `dbus.ts`; new golden bytes
  for `RestartUnit`, `GetAll` on a Service, and a `JobRemoved` signal decoded.
- **`prepare`** - seeds only missing profiles; writes the drop-in for each.
- **Arguments** - `--source` repeated and comma separated, unknown rejected,
  `--units-dir`, `--state-dir`/`STATE_DIRECTORY`, `--inhibit`/`--no-inhibit`.
- **Nix**
  - `checks.package`: the build, with `bun test` in `checkPhase`.
  - `checks.module`: evaluate a configuration with an opted-in unit and assert
    the rendered `/etc/tidewaiter/units/x.service.json`, `restartIfChanged =
    false`, the activation snippet, and the daemon's `ExecStart`.
  - `checks.vm`, one node:
    - `echo-game`: a tiny UDP+TCP echo built three ways - `v1`, `v2` (answers
      "v2") and `bad` (exits after 2 s without binding) - with `ExecStart =
      "${profile}/bin/echo-game"`, `Type=notify` via `systemd-notify --ready`
      after binding (`NotifyAccess=all`), `confinement.enable = true` so
      `bindClosure` is exercised, `tidewaiter = { desired = v1; ports = [
      "7777/udp" "7777/tcp" ]; idleSamples = 2; healthTimeout = 20; }`.
    - specialisations `v2` and `bad` that only change `tidewaiter.desired`.
    - `services.tidewaiter = { enable = true; interval = 2; activityInterval =
      1; }`; `net.netfilter.nf_conntrack_udp_timeout{,_stream} = 5` so an idle
      UDP flow ages out within the test.
    - Script: boots on v1 (profile seeded, answers "v1"); start a UDP client
      sending every second and a TCP client holding a connection; switch to
      `v2` with `switch-to-configuration test` and assert the unit's
      `InvocationID` did not change (restartIfChanged); assert
      `systemd-inhibit --list` shows tidewaiter, and that `shutdown -r +1` and
      `systemctl reboot` (non-TTY) are refused - or, if logind accepts the
      schedule, that `ScheduledShutdown` never fires while the lock is held -
      recording which in the README; wait several passes and assert still v1;
      stop the clients; wait for the profile to point at v2, a new
      `InvocationID`, "v2" on the port, and the lock released.
    - Switch to `bad`: wait for "rolled back" in the journal; profile back on
      v2's generation, unit active, answers "v2"; then several more passes with
      no further restart (pinned). Restart `tidewaiter.service` and assert it
      still does not retry (persisted pin).
    - `systemd-analyze security tidewaiter.service` printed into the log.
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
| `src/daemon.ts` | `Daemon`: shared dump, sources in turn, summary, inhibitor, activity tick |
| `src/docker-source.ts` | `DockerSource`, plus `activity()` and `list()` |
| `src/inhibit.ts` | `Inhibitor`, `NoInhibitor`, `SystemdInhibitor` |
| `src/units.ts` | `parseUnitPolicy`, `DirUnitsDirectory` |
| `src/listeners.ts` | `parseListeners`, `unitBoundPorts`, `SsListenerSource` |
| `src/profile.ts` | `Profiles`, `NixProfiles`, `generationOf` |
| `src/systemd.ts` | `UnitState`, `SystemdClient` (interface only), `systemdVerdict` |
| `src/closure.ts` | `ClosureBinder`, `NixClosureBinder`, `closureDropIn` |
| `src/state-store.ts` | `UnitMemory`, `FileStateStore` |
| `src/systemd-source.ts` | `SystemdSource`: sections 2-5 and 7's swap side |

`main.ts` runs a `Daemon` over `DockerSource` with `NoInhibitor`, so the binary
and the image behave as before.

Still to do: `DbusSystemdClient` (with Portical's `dbus.ts`), section 8's
flags and `prepare`, and section 9.

## 13. Open questions

1. **The inhibitor on older systemd.** Before 257, `block` is ignored by root,
   so `system.autoUpgrade`'s reboot would go ahead regardless. Require 257+
   (an assertion on `config.systemd.package.version`), or add an
   `ExecCondition` fallback to `nixos-upgrade.service` on older hosts?
2. **A broken detector blocks reboots forever.** Fail-safe says busy; busy says
   hold the lock. Cap it - e.g. stop holding for a workload whose activity has
   been unreadable for longer than N hours, with a loud log - or stay strict?
   The design is strict.
3. **`nixos-upgrade.service` failing** on a night it was refused. Accept, or
   have the module add `SuccessExitStatus`/a wrapper so a refused reboot is not
   a failure? Wrapping nixpkgs' script is brittle; the design accepts.
4. **Stopped units.** The design moves a stopped unit's profile to `desired`
   with no gate. Leave its profile alone instead, so a manually-stopped unit
   comes back on what it last ran?
5. **Closure drop-in vs binding `/nix/store`.** The drop-in keeps confinement
   tight but adds `nix-store -qR`, a file in `/run` and a daemon-reload per
   swap. Binding `/nix/store` read-only in Chandlery's units is simpler, and the
   store is world-readable anyway. Which does Chandlery prefer? (Needs agreeing
   with the Chandlery branch.)
6. **Which Bun builds the flake.** `mise.toml` pins `1.3` (1.3.14 locally),
   nixpkgs-unstable has 1.4.2 and Portical's branch moved its `mise.toml` to
   1.4.2. Bump Tidewaiter's pin to match, or fetch 1.3 as a fixed-output
   derivation?
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
12. **Docker reads a failed conntrack dump as an empty table** - so as idle,
    though it logs "assuming busy where it matters". That predates this branch
    and is kept, as promised; the systemd source reads it as busy. Make Docker
    fail safe too?

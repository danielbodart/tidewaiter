<p align="center"><img src="logo.png" alt="Logo" width="600"></p>


# Tidewaiter

**Auto-updater that waits for the tide to go out.** It watches for a newer version — a newer image in the registry for a Docker container, or a new store path in the NixOS configuration for a [systemd unit](#systemd-units) — updates **only when nobody is using it** (no live network flows), health-checks the swap, and **rolls back** on failure. On NixOS it can also reboot into a new kernel the same way: inside a nightly window, once nobody is on.

> A *tidewaiter* was a customs officer who boarded an incoming ship only once the tide allowed. This one waits for the tide of traffic to ebb before it goes aboard — and steps back the instant anyone's on.

## Why

Watchtower is archived, Podman auto-update isn't available on Flatcar, and nothing for plain Docker/compose combines *registry-watch + activity-gating + health-gated rollback*. Kubernetes' [Agones](https://agones.dev) proves the "don't disrupt a busy server" idea at scale; Tidewaiter is the lightweight version — it infers "busy" from **conntrack**, so any image qualifies with no SDK, no Kubernetes, no Podman. Paired with Flatcar's OS auto-update, the box stays current end to end, hands-off.

## How it works

Once per interval, for each container opted in with `tidewaiter.autoupdate=registry` (or `=local`):

1. **Compare digests.** Resolve what the tag should be — the registry's manifest digest (`registry`) or the locally-stored image for the tag (`local`) — and compare it against the digest of the image the container is actually running. Equal ⇒ nothing to do.
2. **Wait for the tide to go out.** If a newer image exists, check whether the container is idle — no live network flows to its published ports. Busy, or not yet idle for long enough ⇒ defer to the next pass. *If it can't tell, it assumes busy and defers* — a live session is never kicked to chase a version. **Meanwhile the newer image is pulled in the background** (`registry` policy), the moment it's noticed — even while the container is still busy — so that when the tide finally goes out the swap is a fast local recreate, not a multi-minute download inside the idle window. The prefetch is speculative: a failed pull never pins and is retried, and the swap still does its own authoritative pull (cache-warm, so cheap).
3. **Swap, health-gate, commit or roll back.** When idle: pull (usually already cached from the prefetch) → graceful-stop → recreate faithfully (same name, env, mounts, ports, networks, labels, restart policy, caps) → wait for healthy up to a timeout → keep it; otherwise **roll back** to the exact previous image and pin that digest out until the tag moves on.

The recreate is **rename-not-delete**: the old container is parked under `<name>-tidewaiter-rollback` while the new one stands up under the original name, so the name never resolves to nothing and a failed swap always has something to restore.

## Activity detection

A container is "in use" if it has live inbound flows to its published ports.

- **conntrack** (default) — reads the host conntrack table over netlink (needs `CAP_NET_ADMIN`). The only detector that sees **UDP** peers, since a UDP server shares one socket across all clients. Prefers `ASSURED` flows so a lone probe packet doesn't read as a session.
- **tcp** — backup: counts `ESTABLISHED` TCP sockets in the container's netns via `/proc/<pid>/net/tcp[6]`. TCP only (UDP has no socket-state fallback).
- **netio** — a zero-privilege signal from the Docker `/stats` net-I/O rate; two samples an interval apart. Never green-lights an update on its own until it has a rate.
- **none** — opt out of the gate; always reads as idle.

A debounce (`tidewaiter.idle-samples`, default 3) requires several consecutive idle passes before an update, so a brief lull between packets doesn't count as empty.

## Health strategy

`tidewaiter.health` is a **comma-separated set of checks, run together** — the swap commits only if none of them actively fails, and rolls back the moment one does. Each check covers a different blind spot (empirically: no single check is enough — an app's own healthcheck can pass while the port is unreachable; a TCP connect can pass through docker-proxy with no backend behind it). Default is all four:

- **docker** — read `.State.Health.Status` (authoritative about the app's *internal* self-check, when the image defines a `HEALTHCHECK`; watched live via the Docker event stream). Blind to external reachability.
- **port-connect** — a real `connect()` to a published host port, the path a client takes. Blind to whether there's actually a backend (docker-proxy answers the handshake).
- **port-bound** — reads the container's own netns (`/proc/<pid>/net/*`) for a bound/listening socket on a published port. Sees the *inside* truth — bypasses docker-proxy — and is the only generic signal for UDP.
- **uptime** — the container stays up, and by the time it has (a short grace), if it publishes ports at least one must be bound. This is what catches "the process runs and the port accepts, but nothing is actually serving."

The gate leans deliberately toward *trusting the update*: a check that merely never confirms (a slow probe) does not veto — only an active failure (the app reports unhealthy, the container exits, or it comes up not serving) rolls back. A wrongly-rejected good update is the worse error; a genuinely-broken one is caught next cycle by the activity gate too.

Override per container, e.g. `tidewaiter.health=docker,uptime`.

## Labels

| Label | Meaning | Default |
| --- | --- | --- |
| `tidewaiter.autoupdate` | **the opt-in.** `registry` (compare against the registry) or `local` (compare against the locally-stored image, no network). A container is managed only if it carries this label with a known value; absence — or an unrecognised value — means it is left completely alone. | *(none — required to opt in)* |
| `tidewaiter.detector` | `conntrack` \| `tcp` \| `netio` \| `none` | `conntrack` |
| `tidewaiter.ports` | scope the idle check and health probe to specific published ports — use it when a container also exposes a metrics/admin port that gets constant automated traffic (which would otherwise keep it looking "busy" forever) | all published ports |
| `tidewaiter.idle-samples` | consecutive idle passes required before updating | `3` |
| `tidewaiter.health` | comma-separated checks: `docker`, `port-connect`, `port-bound`, `uptime` (run together, any active failure rolls back) | all four |
| `tidewaiter.health-timeout` | seconds to wait for healthy before rolling back | `120` |
| `tidewaiter.grace` | stop-grace (drain) seconds | container's own stop timeout |
| `tidewaiter.keep-images` | previous images to retain for rollback before pruning | `1` |

A malformed label is reported and the default used — one bad label never stops a container being managed.

`tidewaiter.ports` takes a comma-separated list where each term is a port with an optional protocol: `25565/tcp` (TCP only), `19132/udp` (UDP only), or a bare `25565` (both, or whichever the container actually publishes) — e.g. `tidewaiter.ports=25565,19132/udp`.

## Deploy

```yaml
services:
  tidewaiter:
    image: danielbodart/tidewaiter:latest
    container_name: tidewaiter
    network_mode: host          # share the net ns: conntrack + connect to host ports
    cap_add:
      - NET_ADMIN               # ACTIVITY GATE: dump the host conntrack table (netlink)
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock   # rw: it pulls, recreates, stops, removes
      - /proc:/host/proc:ro     # DETECT LISTENING: read /proc/<pid>/net/* to see what a
                                # container has bound inside its own netns (host networking
                                # shares the net ns but NOT the pid ns; a read-only /proc
                                # mount is a smaller grant than `pid: host`)
    restart: unless-stopped
```

Two grants, two jobs: **`CAP_NET_ADMIN`** is the *activity gate* (conntrack — "is anyone using this container?"); the read-only **`/proc` mount** is the *health/detector* read ("is it actually listening inside?"). Tidewaiter never needs `CAP_SYS_ADMIN` — it reads `/proc`, it doesn't enter namespaces. Without the `/proc` mount, `port-bound` health and the `tcp` detector degrade to inconclusive; conntrack, `port-connect`, `docker` and `uptime` still work.

Then opt any container in with `tidewaiter.autoupdate=registry`. See [`docker-compose.yaml`](./docker-compose.yaml) for a worked example.

Private registries: Tidewaiter reads `~/.docker/config.json` for credentials and sends them both when resolving digests and when pulling.

## systemd units

The same care for a native service: a systemd unit whose program lives in a **Nix profile**. The unit's `ExecStart` runs from the profile (`/nix/var/nix/profiles/…/bin/server`), so the unit itself never changes when the version does. A declaration names the store path the profile *should* point at; when it differs, Tidewaiter:

1. **Waits for the tide to go out** — conntrack flows to the unit's declared ports, UDP included, exactly as for a container on host networking. Debounced by `idleSamples`, and looked at once more right before anything changes.
2. **Swaps** — `nix-env --profile P --set DESIRED`, then `systemctl restart` the unit (over D-Bus). A graceful stop is the unit's own `ExecStop`, however long its `TimeoutStopSec` allows.
3. **Health-gates** — `systemd`: the restart job finished `done` (for a `Type=notify` unit, the program said `READY=1`) and the unit stayed up on that invocation, with no `Restart=` crash loop hidden behind `active`; `port-bound`: a socket **in the unit's own cgroup** is on a declared port (`ss -e`, so another process on the same port doesn't count); `port-connect` and `uptime` as for containers.
4. **Commits or rolls back** — healthy: prune old generations. Unhealthy: switch the profile back to the exact generation it left, delete the failed one, restart, and pin the version out until the declaration moves on. Pins survive a restart of Tidewaiter; a swap it died in the middle of is rolled back when it next sees the unit failed.

A stopped unit just has its profile moved, for whoever starts it next.

### NixOS

```nix
{
  inputs.tidewaiter.url = "github:danielbodart/tidewaiter";
  # ...
  imports = [ inputs.tidewaiter.nixosModules.default ];

  services.tidewaiter.enable = true;

  systemd.services.minecraft = {
    wantedBy = [ "multi-user.target" ];
    serviceConfig.ExecStart = "/nix/var/nix/profiles/tidewaiter/minecraft/bin/minecraft-server";
    tidewaiter = {
      desired = pkgs.minecraft-server;   # the version to move to; setting it opts in
      ports = [ "25565/tcp" ];
    };
  };
}
```

Setting `tidewaiter.desired` on a service:

- sets `restartIfChanged = false` — `nixos-rebuild switch` (or `system.autoUpgrade`) no longer restarts it; Tidewaiter does, when it's idle;
- writes `/etc/tidewaiter/units/<name>.service.json`, the declaration the daemon reads, which also puts `desired` in the system closure — built by the switch that asked for it;
- orders the service after `tidewaiter-prepare.service`, which creates its profile on `desired` the first time it's declared, so it starts on day one.

| `systemd.services.<name>.tidewaiter.` | Meaning | Default |
| --- | --- | --- |
| `desired` | the package the profile should point at; non-null opts the service in | `null` |
| `profile` | the profile `ExecStart` runs from | `/nix/var/nix/profiles/tidewaiter/<name>` |
| `ports` | ports whose flows mean "in use", and that health probes: `19132/udp`, `25565/tcp`, or bare `25565` for both | `[]` (an assertion requires some unless `detector = "none"`) |
| `detector` | `conntrack` \| `tcp` \| `none` | `conntrack` |
| `health` | any of `systemd`, `port-connect`, `port-bound`, `uptime`, run together | all four |
| `idleSamples` | consecutive idle passes before updating | `3` |
| `healthTimeout` | seconds to wait for healthy | `120` |
| `keepGenerations` | earlier profile generations kept after a healthy update | `1` |
| `bindClosure` | bind the profile's current target and its closure into a confined service (see below) | the service's `confinement.enable` |

| `services.tidewaiter.` | Meaning | Default |
| --- | --- | --- |
| `enable` | run the daemon | `false` |
| `sources` | `systemd`, `docker`, or both — one daemon per host, so two swaps never overlap | `[ "systemd" ]` |
| `interval` | seconds between passes | `60` |
| `activityInterval` | seconds between activity checks between passes, keeping `tidewaiter status` current | `15` |
| `autoReboot.enable` | reboot when the installed system needs it, inside the window, once nothing is in use (see below) | `false` |
| `autoReboot.window` | daily window in local time, `HH:MM-HH:MM`; may cross midnight, `00:00-24:00` is all day | `"03:00-05:00"` |
| `dryRun`, `dockerSocket`, `label`, `extraArgs`, `package` | as they say | |

The daemon runs as root — it restarts other units and writes system profiles — and is otherwise confined: `CAP_NET_ADMIN` only (conntrack), `ProtectSystem=strict` with only `/nix/var/nix/profiles` and `/run/systemd/system` writable, profiles written through the Nix daemon, no `@privileged` system calls, and IP limited to localhost when systemd is the only source. `systemd-analyze security tidewaiter` rates it **2.0 OK**.

**Confined services.** NixOS `confinement.enable` gives a service a root holding only the closure of its own definition — which names the profile but no version. With `bindClosure` (on by default for a confined service), Tidewaiter writes a runtime drop-in binding the profile's directory and its current target's closure before every restart, rollbacks included, and `tidewaiter-prepare` writes it at boot (only where none is there, so it never races the daemon's).

**Combining with Portical.** A unit forwarded by [Portical](https://github.com/danielbodart/portical) typically lists the same ports twice. While no forward remaps a port, share one list: `let ports = [ "19132/udp" ]; in { portical.forward = ports; tidewaiter.ports = ports; }`. A remapped forward (`external:internal/proto`) is not a valid `tidewaiter.ports` entry: list the internal port — the one the unit binds, and the one conntrack sees the players' flows reach — e.g. `portical.forward = [ "19133:19132/udp" ]; tidewaiter.ports = [ "19132/udp" ];`, or derive it with `map (p: lib.last (lib.splitString ":" p)) forwards`.

### Is anyone on? `tidewaiter status`

Humans stay in charge: Tidewaiter never stands in the way of a reboot someone asks for. To see whether one would cut anybody off first:

```
$ tidewaiter status
Busy: minecraft.service in use
  minecraft.service (systemd)  in use
  web (docker)  idle
Automatic reboot (03:00-05:00): needed, kernel changed since boot; waiting: minecraft.service in use
As of 4s ago
```

It exits **0** when nothing is in use or being swapped, **1** when something is, and **2** when there is no daemon to ask — so `tidewaiter status && reboot` does what it says. It needs no root: the daemon writes `/run/tidewaiter/status.json` (plain JSON, for scripts too) after every pass, every activity check (`activityInterval`) and the start of every swap, and the CLI only reads it. A workload is listed as "idle, not yet for long enough" until it has been idle for its `idleSamples`.

### Rebooting for a new kernel

`system.autoUpgrade` with `allowReboot = true` reboots whoever is on. Let Tidewaiter do it instead:

```nix
system.autoUpgrade = {
  enable = true;
  allowReboot = false;   # a plain nightly switch; opted-in services are not restarted by it
};
services.tidewaiter.autoReboot = {
  enable = true;
  window = "03:00-05:00";
};
```

Each pass, Tidewaiter reboots (through logind, as `systemctl reboot` does) when all of these hold:

- **a reboot is needed** — the test `autoUpgrade` itself makes: `/run/booted-system/{initrd,kernel,kernel-modules}` differ from `/nix/var/nix/profiles/system`'s;
- **it is inside the window**, in local time — never outside it;
- **every managed unit and container** has been idle for its whole `idleSamples`, Docker's included, and activity read once more right before the reboot still says so;
- **nothing is being swapped**.

A window busy from start to end simply passes, and the next night's is tried the same way. What it is waiting for is in the journal (once per change) and in `tidewaiter status`. The module refuses `autoReboot` together with `autoUpgrade.allowReboot`, since the two would fight. Both the wait and the reboot are proven by the VM test (`nix build .#checks.x86_64-linux.vm`).

### Without NixOS

Nothing here needs NixOS, only Nix profiles and systemd. Drop a JSON file per unit in `/etc/tidewaiter/units/`:

```json
{
  "unit": "minecraft.service",
  "desired": "/nix/store/…-minecraft-server-1.21.4",
  "profile": "/nix/var/nix/profiles/tidewaiter/minecraft",
  "ports": ["25565/tcp"]
}
```

Every other field (`detector`, `health`, `idleSamples`, `healthTimeout`, `keepGenerations`) is optional, with the defaults above; `bindClosure` defaults to `false` in a file. Point the unit's `ExecStart` at the profile, have your deployment change only the file (never restart the unit itself), run `tidewaiter prepare` to create missing profiles, and run `tidewaiter --source systemd --status-file /run/tidewaiter/status.json run` as root (add `--auto-reboot-window 03:00-05:00` on NixOS to reboot as above).

The design, and what is still open, is in [`docs/systemd-source.md`](./docs/systemd-source.md).

## Commands

```
tidewaiter run      # reconcile continuously on an interval (default)
tidewaiter check    # reconcile once and exit
tidewaiter list     # show managed containers and units, and whether an update is waiting
tidewaiter status   # what the running daemon sees in use or swapping; exit 0 idle, 1 busy, 2 no daemon or a stale status
tidewaiter prepare  # create missing unit profiles and missing closure drop-ins, then exit
```

Options: `-s/--source docker|systemd` (repeat or comma-separate; default `docker`), `-d/--interval SECONDS` (default 300), `-n/--dry-run`, `--status-file PATH` (`run` writes none unless given one; `status` reads `/run/tidewaiter/status.json` by default), `--activity-interval SECONDS` (default 15), `--auto-reboot-window HH:MM-HH:MM` with `--booted-system PATH` and `--current-system PATH` (defaults `/run/booted-system`, `/nix/var/nix/profiles/system`), `--version`. Docker: `-l/--label LABEL`, `--docker-socket PATH`. systemd: `--units-dir DIR` (default `/etc/tidewaiter/units`), `--state-dir DIR` (default `/var/lib/tidewaiter`). Env: `TIDEWAITER_SOURCES`, `TIDEWAITER_INTERVAL`, `TIDEWAITER_STATUS_FILE`, `TIDEWAITER_AUTO_REBOOT_WINDOW`, `DOCKER_SOCKET`, `TIDEWAITER_UNITS_DIR`, `STATE_DIRECTORY`.

## Building

`bun run.ts check` and `bun run.ts test` for development; `nix build` builds the binary reproducibly (running the tests on the way) and `nix flake check` adds the module check and the VM test.

## Known limitations (v1)

- **Anonymous volumes block updates.** A container with an anonymous volume can't be recreated without losing the volume's data, so Tidewaiter defers and says so — give it a named volume to enable updates.
- **`container:` network mode is copied verbatim.** A container sharing another's netns (`network_mode: "container:x"`) is recreated as-is; if `x` itself was recreated, the reference can break. Logged when detected.
- **No crash-recovery of an in-flight container swap.** If Tidewaiter restarts mid-swap it has no memory of it; it adopts the running image as the current baseline (with a warning) rather than guessing. (A systemd unit's swap is recovered: see above.)
- **`port`/bound-socket health is "listening", not "fully functional."** It matches what most real TCP/HTTP healthchecks test, protocol-aware probes should be baked into images

## Relationship to portical

Tidewaiter is the sibling of [**portical**](https://github.com/danielbodart/portical) and shares its spine — a label-driven reconcile loop over the Docker Engine API in Bun/TypeScript
## License

Apache-2.0.

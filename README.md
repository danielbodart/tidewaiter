<p align="center"><img src="logo.png" alt="Logo" width="600"></p>


# Tidewaiter

**Auto-updater that waits for the tide to go out.** It watches for a newer version — a newer image in the registry for a Docker container, or a new store path in the NixOS configuration for a [systemd unit](https://github.com/danielbodart/tidewaiter/blob/master/docs/systemd.md) — updates **only when nobody is using it** (no live network flows), health-checks the swap, and **rolls back** on failure. On NixOS it can also reboot into a new kernel the same way: inside a nightly window, once nobody is on.

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

## Running it

How to opt something in, and run Tidewaiter, depends on where your services run:

- **[Run it in Docker](https://github.com/danielbodart/tidewaiter/blob/master/docs/docker.md)**
  to update Docker containers, opted in with a label. The image is on Docker Hub as
  `danielbodart/tidewaiter` and on GitHub as `ghcr.io/danielbodart/tidewaiter`.
- **[Run it as a systemd service](https://github.com/danielbodart/tidewaiter/blob/master/docs/systemd.md)**
  to update systemd units whose program lives in a Nix profile - from the flake's
  NixOS module, or from a release binary on any systemd distribution with Nix. A
  native Tidewaiter can update Docker containers too, and is the one to use on a
  host that has both: one Tidewaiter per host, so two swaps never overlap.

It runs on Linux on `amd64`, `arm64` or 32-bit `armv7`: the published images and
binaries cover all three, so a Pi - on 64-bit or 32-bit Raspberry Pi OS - or an ARM
NAS gets the right one. Each is a single static binary, with no runtime and no libc.
It asks the kernel for the conntrack table and for sockets over netlink itself, so
nothing else has to be installed for it - only Nix, for the systemd source. The
systemd source's `port-bound` health needs Linux 5.7 or later, whose socket
diagnostics say which cgroup a socket belongs to.

## Commands

```
tidewaiter run      # reconcile continuously on an interval (default)
tidewaiter check    # reconcile once and exit
tidewaiter list     # show managed containers and units, and whether an update is waiting
tidewaiter status   # what the running daemon sees in use or swapping; exit 0 idle, 1 busy, 2 no daemon or a stale status
tidewaiter prepare  # create missing unit profiles and missing closure drop-ins, then exit
```

Options: `-s/--source docker|systemd` (repeat or comma-separate; default `docker`), `-d/--interval SECONDS` (default 300), `-n/--dry-run`, `--status-file PATH` (`run` writes none unless given one; `status` reads `/run/tidewaiter/status.json` by default), `--activity-interval SECONDS` (default 15), `--auto-reboot-window HH:MM-HH:MM` with `--booted-system PATH` and `--current-system PATH` (defaults `/run/booted-system`, `/nix/var/nix/profiles/system`), `--version`. Docker: `-l/--label LABEL`, `--docker-socket PATH`. systemd: `--units-dir DIR` (default `/etc/tidewaiter/units`), `--state-dir DIR` (default `/var/lib/tidewaiter`). Env: `TIDEWAITER_SOURCES`, `TIDEWAITER_INTERVAL`, `TIDEWAITER_STATUS_FILE`, `TIDEWAITER_AUTO_REBOOT_WINDOW`, `DOCKER_SOCKET`, `TIDEWAITER_UNITS_DIR`, `STATE_DIRECTORY`.

## Development

Tidewaiter is Go, and the standard library apart from two modules:
[godbus](https://github.com/godbus/dbus) for the systemd source's system bus and
logind, and the author's own [gokit](https://github.com/danielbodart/gokit) -
itself standard library only - for httpx, clock and sockdiag. The conntrack
table is read over ctnetlink with the standard library's syscall package.

```shell
mise install       # installs the pinned Go
./run test         # go test, every package
./run check        # gofmt and go vet
./run build        # a static binary for this machine, into dist/
./run release      # every architecture's binary, the unit and checksums, into dist/release/
./run image        # the image, for every architecture, locally
nix build          # the Nix package, running the tests on the way
nix flake check    # also the module check and the NixOS VM test (needs KVM)
```

`./run` is a POSIX shell script and the only place any of this is defined, so CI
runs the same commands you do. The tests that need real infrastructure - the
system bus, a Docker daemon, cgroupfs, or permission to dump conntrack - skip
themselves where it is missing, as it all is in Nix's build sandbox; the VM test
is where conntrack meets a real kernel.

### Versions

Every push to `master` publishes an image, tagged `:latest` and with an exact
version, and a [GitHub release](https://github.com/danielbodart/tidewaiter/releases)
of the same version, tagged `v<version>`, with the standalone binaries for `x64`,
`arm64` and `armv7`, the systemd unit, and their `SHA256SUMS`.

The number is derived from the repository rather than stored in it. Only the
major is committed - in `MAJOR`, because that one is a decision, and it moves
when compatibility breaks; the Go rewrite did not move it, since its flags,
labels, status file and state directory are v1's. The minor is the commit count,
so it only goes up and names exactly one commit; the patch is the CI run number,
which separates two builds of the same commit. Locally the patch becomes a
timestamp, so a developer build sorts after CI's and is obviously not one.

## Known limitations (v1)

The limits that are a container's own - anonymous volumes, `container:` network
mode, and a container swap interrupted by a restart - are in
[Running Tidewaiter in Docker](https://github.com/danielbodart/tidewaiter/blob/master/docs/docker.md#known-limitations).

- **`port`/bound-socket health is "listening", not "fully functional."** It matches what most real TCP/HTTP healthchecks test, protocol-aware probes should be baked into images

## Relationship to portical

Tidewaiter is the sibling of [**portical**](https://github.com/danielbodart/portical) and shares its spine — a label-driven reconcile loop over the Docker Engine API, in Go, on the same [gokit](https://github.com/danielbodart/gokit).

## License

Apache-2.0.

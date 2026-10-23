# Running Tidewaiter as a systemd service

The same care for a native service: a systemd unit whose program lives in a **Nix profile**. The unit's `ExecStart` runs from the profile (`/nix/var/nix/profiles/…/bin/server`), so the unit itself never changes when the version does. A declaration names the store path the profile *should* point at; when it differs, Tidewaiter:

1. **Waits for the tide to go out** — conntrack flows to the unit's declared ports, UDP included, exactly as for a container on host networking. Debounced by `idleSamples`, and looked at once more right before anything changes.
2. **Swaps** — `nix-env --profile P --set DESIRED`, then `systemctl restart` the unit (over D-Bus). A graceful stop is the unit's own `ExecStop`, however long its `TimeoutStopSec` allows.
3. **Health-gates** — `systemd`: the restart job finished `done` (for a `Type=notify` unit, the program said `READY=1`) and the unit stayed up on that invocation, with no `Restart=` crash loop hidden behind `active`; `port-bound`: a socket **in the unit's own cgroup** is on a declared port (the kernel's socket diagnostics name each socket's cgroup, so another process on the same port doesn't count); `port-connect` and `uptime` as for containers.
4. **Commits or rolls back** — healthy: prune old generations. Unhealthy: switch the profile back to the exact generation it left, delete the failed one, restart, and pin the version out until the declaration moves on. Pins survive a restart of Tidewaiter; a swap it died in the middle of is rolled back when it next sees the unit failed.

A stopped unit just has its profile moved, for whoever starts it next.

Updating units needs **Nix** - its profiles and its daemon - and systemd, but not
NixOS. A native Tidewaiter can update Docker containers too, and for those alone
it needs no Nix at all - see [Docker containers too](#docker-containers-too).

There are two ways to run it: the [NixOS module](#nixos), which is the whole setup
in a few lines of configuration, or the [release binary and unit
file](#without-nixos), on any other systemd distribution with Nix installed. How an
update is detected and health-gated, the commands and the options are in the
[README](../README.md). For a host whose services are all in Docker, [running
Tidewaiter in Docker](docker.md) is simpler.

## NixOS

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

## Is anyone on? `tidewaiter status`

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

## Rebooting for a new kernel

NixOS only. `system.autoUpgrade` with `allowReboot = true` reboots whoever is on. Let Tidewaiter do it instead:

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

## Without NixOS

Nothing here needs NixOS, only Nix profiles and systemd. Every build of `master`
is published as a [GitHub release](https://github.com/danielbodart/tidewaiter/releases),
with a standalone binary for each architecture and a unit file to run it:

| Asset | |
| --- | --- |
| `tidewaiter-linux-x64` | The binary, for 64-bit Intel and AMD. |
| `tidewaiter-linux-arm64` | The binary, for 64-bit ARM - a Pi 3/4/5 on 64-bit Raspberry Pi OS, or an ARM NAS. |
| `tidewaiter-linux-armv7` | The binary, for 32-bit ARMv7 - a Pi 2 onwards on 32-bit Raspberry Pi OS, or an armhf NAS. |
| `tidewaiter.service` | The unit file. |
| `SHA256SUMS` | Checksums of the above. |

The binaries are static, so they run on any distribution, glibc or musl. The
conntrack table and the sockets are asked of the kernel over netlink, so the only
thing they need from the host is:

- **Nix**, installed as a multi-user daemon, for the systemd source. The unit looks
  for `nix-env` and `nix-store` in `/nix/var/nix/profiles/default/bin`, where that
  install puts them, and writes profiles through the daemon.

`port-bound` health on a unit needs Linux 5.7 or later, whose socket diagnostics
carry the socket's cgroup.

### Install

```shell
cd "$(mktemp -d)"
arch=$(uname -m | sed 's/x86_64/x64/; s/aarch64/arm64/; s/armv7l/armv7/')
base=https://github.com/danielbodart/tidewaiter/releases/latest/download
curl -fsSLO "$base/tidewaiter-linux-$arch" -O "$base/tidewaiter.service" -O "$base/SHA256SUMS"
sha256sum -c --ignore-missing SHA256SUMS

sudo install -m 755 "tidewaiter-linux-$arch" /usr/local/bin/tidewaiter
sudo install -m 644 tidewaiter.service /etc/systemd/system/tidewaiter.service
sudo systemctl daemon-reload
```

### Declare the units

Drop a JSON file per unit in `/etc/tidewaiter/units/`:

```json
{
  "unit": "minecraft.service",
  "desired": "/nix/store/…-minecraft-server-1.21.4",
  "profile": "/nix/var/nix/profiles/tidewaiter/minecraft",
  "ports": ["25565/tcp"]
}
```

Every other field (`detector`, `health`, `idleSamples`, `healthTimeout`, `keepGenerations`) is optional, with the defaults above; `bindClosure` defaults to `false` in a file. Point the unit's `ExecStart` at the profile, and have your deployment change only the file (never restart the unit itself). A newly declared unit has no profile yet, so create it before the unit first starts - as root, with Nix on the path, since sudo's own usually leaves it out:

```shell
sudo env PATH="/nix/var/nix/profiles/default/bin:$PATH" tidewaiter prepare
```

`tidewaiter list` shows what the daemon would make of the declarations, and
whether an update is waiting, without changing anything.

### Configure and start

The unit runs `tidewaiter run` as root, confined as on NixOS, reading systemd
units only (`TIDEWAITER_SOURCES=systemd`) and writing `/run/tidewaiter/status.json`
for `tidewaiter status`. Settings go in a drop-in, so they survive replacing the
unit file on an upgrade:

```shell
sudo systemctl edit tidewaiter
```

```ini
[Service]
Environment=TIDEWAITER_INTERVAL=300
```

`TIDEWAITER_SOURCES`, `TIDEWAITER_INTERVAL`, `TIDEWAITER_STATUS_FILE`,
`TIDEWAITER_UNITS_DIR` and `DOCKER_SOCKET` can all be set with `Environment=` this
way. Any other option is passed on the command line, by replacing `ExecStart` in
the same drop-in - the empty line clears the unit's own:

```ini
[Service]
ExecStart=
ExecStart=/usr/local/bin/tidewaiter --activity-interval 30 run
```

Leave `--auto-reboot-window` out: whether a reboot is needed is NixOS's own test,
which another distribution has nothing to compare.

Then start it, and have it start on boot:

```shell
sudo systemctl enable --now tidewaiter
journalctl -u tidewaiter -f
```

### Docker containers too

A native Tidewaiter updates labelled Docker containers exactly as the image does,
reading the host's `/proc` directly rather than through a mount. Run one
Tidewaiter per host, so two swaps never overlap: on a host with both systemd units
and containers, that is this one, with both sources.

Docker's socket belongs to the `docker` group, and Docker's registries are on the
internet, where the shipped unit does not let it reach. So the drop-in adds the
group as well as the source, clears the address restriction, and orders
Tidewaiter after Docker:

```ini
[Unit]
After=docker.service

[Service]
Environment=TIDEWAITER_SOURCES=systemd,docker
SupplementaryGroups=docker
IPAddressAllow=
IPAddressDeny=
```

For containers alone, `TIDEWAITER_SOURCES=docker` needs no Nix at all. The
shipped unit leaves the group out because a host with no `docker` group would fail
to start it at all, with `216/GROUP`.

Private registries' credentials are read from `~/.docker/config.json`, and the
unit's home is `/var/lib/tidewaiter` (root's is hidden from it), so they go in
`/var/lib/tidewaiter/.docker/config.json`.

### Upgrade

Replace the binary and restart. A swap in flight is finished first:

```shell
arch=$(uname -m | sed 's/x86_64/x64/; s/aarch64/arm64/; s/armv7l/armv7/')
curl -fsSL -o tidewaiter "https://github.com/danielbodart/tidewaiter/releases/latest/download/tidewaiter-linux-$arch"
sudo install -m 755 tidewaiter /usr/local/bin/tidewaiter
sudo systemctl restart tidewaiter
```

### Uninstall

```shell
sudo systemctl disable --now tidewaiter
sudo rm -rf /etc/systemd/system/tidewaiter.service /etc/systemd/system/tidewaiter.service.d \
  /usr/local/bin/tidewaiter
sudo systemctl daemon-reload
```

The profiles stay where they are, pointing at whatever version each unit last
ran, so the units carry on as they were.

## Design

The design, and what is still open, is in [`systemd-source.md`](systemd-source.md).

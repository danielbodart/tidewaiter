![Tidewaiter Logo](https://raw.githubusercontent.com/danielbodart/tidewaiter/master/logo.png)


# Tidewaiter

**Docker auto-updater that waits for the tide to go out.** It watches the registry for a newer image, updates a container **only when it's idle** (no live network flows), health-checks the swap, and **rolls back** on failure.

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

## Commands

```
tidewaiter run     # reconcile continuously on an interval (default)
tidewaiter check   # reconcile once and exit
tidewaiter list    # show opted-in containers and whether an update is waiting
```

Options: `-d/--interval SECONDS` (default 300), `-l/--label LABEL`, `--docker-socket PATH`, `-n/--dry-run`, `--version`. Env: `DOCKER_SOCKET`, `TIDEWAITER_INTERVAL`.

## Known limitations (v1)

- **Anonymous volumes block updates.** A container with an anonymous volume can't be recreated without losing the volume's data, so Tidewaiter defers and says so — give it a named volume to enable updates.
- **`container:` network mode is copied verbatim.** A container sharing another's netns (`network_mode: "container:x"`) is recreated as-is; if `x` itself was recreated, the reference can break. Logged when detected.
- **No crash-recovery of an in-flight swap.** If Tidewaiter restarts mid-swap it has no memory of it; it adopts the running image as the current baseline (with a warning) rather than guessing.
- **`port`/bound-socket health is "listening", not "fully functional."** It matches what most real TCP/HTTP healthchecks test, but a protocol-aware probe (RakNet/A2S) is a future addition.

## Relationship to portical

Tidewaiter is the sibling of [**portical**](https://github.com/danielbodart/portical) and shares its spine — a label-driven reconcile loop over the Docker Engine API in Bun/TypeScript
## License

Apache-2.0.

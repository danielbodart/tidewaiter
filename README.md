![Tidewaiter Logo](https://raw.githubusercontent.com/danielbodart/tidewaiter/master/logo.png)


# Tidewaiter

**Label-driven Docker auto-updater that waits for the tide to go out.** It watches the registry for a newer image, updates a container **only when it's idle** (no live network flows), health-checks the swap, and **rolls back** on failure.

> A *tidewaiter* was a customs officer who boarded an incoming ship only once the tide allowed. This one waits for the tide of traffic to ebb before it goes aboard — and steps back the instant anyone's on.

## Why

Watchtower is archived, Podman auto-update isn't available on Flatcar, and nothing for plain Docker/compose combines *registry-watch + activity-gating + health-gated rollback*. Kubernetes' [Agones](https://agones.dev) proves the "don't disrupt a busy server" idea at scale; Tidewaiter is the lightweight version — it infers "busy" from **conntrack**, so any image qualifies with no SDK, no Kubernetes, no Podman. Paired with Flatcar's OS auto-update, the box stays current end to end, hands-off.

## How it works

Once per interval, for each container opted in with `tidewaiter.autoupdate=registry` (or `=local`):

1. **Compare digests.** Resolve what the tag should be — the registry's manifest digest (`registry`) or the locally-stored image for the tag (`local`) — and compare it against the digest of the image the container is actually running. Equal ⇒ nothing to do.
2. **Wait for the tide to go out.** If a newer image exists, check whether the container is idle — no live network flows to its published ports. Busy, or not yet idle for long enough ⇒ defer to the next pass. *If it can't tell, it assumes busy and defers* — a live session is never kicked to chase a version.
3. **Swap, health-gate, commit or roll back.** When idle: pull → graceful-stop → recreate faithfully (same name, env, mounts, ports, networks, labels, restart policy, caps) → wait for healthy up to a timeout → keep it; otherwise **roll back** to the exact previous image and pin that digest out until the tag moves on.

The recreate is **rename-not-delete**: the old container is parked under `<name>-tidewaiter-rollback` while the new one stands up under the original name, so the name never resolves to nothing and a failed swap always has something to restore.

## Activity detection

A container is "in use" if it has live inbound flows to its published ports.

- **conntrack** (default) — reads the host conntrack table over netlink (needs `CAP_NET_ADMIN`). The only detector that sees **UDP** peers, since a UDP server shares one socket across all clients. Prefers `ASSURED` flows so a lone probe packet doesn't read as a session.
- **tcp** — backup: counts `ESTABLISHED` TCP sockets in the container's netns via `/proc/<pid>/net/tcp[6]`. TCP only (UDP has no socket-state fallback).
- **netio** — a zero-privilege signal from the Docker `/stats` net-I/O rate; two samples an interval apart. Never green-lights an update on its own until it has a rate.
- **none** — opt out of the gate; always reads as idle.

A debounce (`tidewaiter.idle-samples`, default 3) requires several consecutive idle passes before an update, so a brief lull between packets doesn't count as empty.

## Health strategy

What "healthy" means for the commit/rollback gate, chosen per container by `tidewaiter.health`:

- **port** (default) — probe from outside: for TCP, a LISTEN socket on the published port; for UDP, a bound socket in the container's netns. Needs nothing baked into the image, which is why it's the default.
- **docker** — read `.State.Health.Status`; use it when the container defines a `HEALTHCHECK`. Watched live via the Docker event stream during the gate.
- **uptime** — weakest: the container simply stays running for a while. Last resort.

## Labels

| Label | Meaning | Default |
| --- | --- | --- |
| `tidewaiter.autoupdate` | **the opt-in.** `registry` (compare against the registry) or `local` (compare against the locally-stored image, no network). A container is managed only if it carries this label with a known value; absence — or an unrecognised value — means it is left completely alone. | *(none — required to opt in)* |
| `tidewaiter.detector` | `conntrack` \| `tcp` \| `netio` \| `none` | `conntrack` |
| `tidewaiter.ports` | override which published ports count as "in use" and get health-probed | container's published ports |
| `tidewaiter.idle-samples` | consecutive idle passes required before updating | `3` |
| `tidewaiter.health` | `docker` \| `port` \| `uptime` | `port` |
| `tidewaiter.health-timeout` | seconds to wait for healthy before rolling back | `120` |
| `tidewaiter.grace` | stop-grace (drain) seconds | container's own stop timeout |
| `tidewaiter.keep-images` | previous images to retain for rollback before pruning | `1` |

A malformed label is reported and the default used — one bad label never stops a container being managed.

## Deploy

```yaml
services:
  tidewaiter:
    image: danielbodart/tidewaiter:latest
    container_name: tidewaiter
    network_mode: host          # where the flows live, and where /proc/<pid>/net/* is reachable
    cap_add:
      - NET_ADMIN               # conntrack netlink dump; not in Docker's default set
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock   # rw: it pulls, recreates, stops, removes
    restart: unless-stopped
```

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

Tidewaiter is the sibling of [**portical**](https://github.com/danielbodart/portical) and shares its spine — a label-driven reconcile loop over the Docker Engine API in Bun/TypeScript, with a pure decision core wrapped by a thin impure shell and an in-memory fake for every I/O seam. It was built by lifting and adapting portical's `http.ts` (the `Handler` seam), `docker.ts`, the daemon loop, CI and versioning; the doc-comments throughout `src/` note where a pattern came from. That harvesting is done, so Tidewaiter no longer vendors portical — it stands on its own.

## License

Apache-2.0.

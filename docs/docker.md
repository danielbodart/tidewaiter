# Running Tidewaiter in Docker

The image updates Docker containers: it reads their labels over the Docker socket
and recreates them when a newer image is due. For systemd units, run Tidewaiter
natively instead - see [Running Tidewaiter as a systemd service](systemd.md). How
an update is decided, detected and health-gated is the same either way, and is in
the [README](../README.md).

## Images

The same image is published to two registries:

- Docker Hub: `danielbodart/tidewaiter`
- GitHub Container Registry: `ghcr.io/danielbodart/tidewaiter`

Each carries `:latest` and an exact version. The examples here use Docker Hub;
`ghcr.io/danielbodart/tidewaiter` can be used in its place anywhere. Both are built
for `amd64`, `arm64` and 32-bit `armv7`, and hold nothing but Tidewaiter itself - a
static binary that speaks the Docker Engine API, the registry and netlink on its
own, and CA certificates for the registry.

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

Then opt any container in with `tidewaiter.autoupdate=registry`. See [`docker-compose.yaml`](../docker-compose.yaml) for a worked example.

Private registries: Tidewaiter reads `~/.docker/config.json` for credentials and sends them both when resolving digests and when pulling. In the image that is `/root/.docker/config.json`; mount yours there read-only.

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

## Only the docker source

The image updates containers and nothing else. Updating systemd units from inside
the container is not supported: it would need the host's system bus, its Nix
daemon and its profiles, none of which a container has. Run one Tidewaiter per
host, so two swaps never overlap: on a host with both containers and units to
update, that is a native Tidewaiter with both sources - see
[Docker containers too](systemd.md#docker-containers-too).

## Known limitations

- **Anonymous volumes block updates.** A container with an anonymous volume can't be recreated without losing the volume's data, so Tidewaiter defers and says so — give it a named volume to enable updates.
- **`container:` network mode is copied verbatim.** A container sharing another's netns (`network_mode: "container:x"`) is recreated as-is; if `x` itself was recreated, the reference can break. Logged when detected.
- **No crash-recovery of an in-flight container swap.** If Tidewaiter restarts mid-swap it has no memory of it; it adopts the running image as the current baseline (with a warning) rather than guessing. (A systemd unit's swap is recovered: see [Running Tidewaiter as a systemd service](systemd.md).)

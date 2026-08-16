# Tidewaiter

**Label-driven Docker auto-updater that waits for the tide to go out.** It watches the registry for a newer image, updates a container **only when it's idle** (no live network flows), health-checks the swap, and **rolls back** on failure.

> A *tidewaiter* was a customs officer who boarded an incoming ship only once the tide allowed. This one waits for the tide of traffic to ebb before it goes aboard — and steps back the instant anyone's on.

Status: **planning** — see [PLAN.md](./PLAN.md).

## Why

Watchtower is archived, Podman auto-update isn't available on Flatcar, and nothing for plain Docker/compose combines *registry-watch + activity-gating + health-gated rollback*. Kubernetes' [Agones](https://agones.dev) proves the "don't disrupt a busy server" idea at scale; Tidewaiter is the lightweight version — it infers "busy" from **conntrack**, so any image qualifies with no SDK, no Kubernetes, no Podman. Paired with Flatcar's OS auto-update, the box stays current end to end, hands-off.

## Design in one breath

Single container, `network_mode: host`, `CAP_NET_ADMIN`, Docker socket. Per opted-in container: compare the running image digest to the registry; if newer **and** the container is idle, pull → graceful-stop → recreate → wait healthy → keep, else roll back to the previous image. Idle is measured from live conntrack flows (UDP + TCP) with a TCP/net-I/O fallback.

## Relationship to portical

Tidewaiter is the sibling of [**portical**](https://github.com/danielbodart/portical) and shares its spine — a label-driven reconcile loop over the Docker Engine API in Bun/TypeScript. portical is vendored at [`vendor/portical`](./vendor/portical) as a git submodule to build against and lift from (`http.ts`, `docker.ts`, the daemon loop, CI, versioning). Clone with:

```bash
git clone --recurse-submodules https://github.com/danielbodart/tidewaiter
```

## License

Apache-2.0.

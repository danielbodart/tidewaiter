# Tidewaiter — design & build plan

> A *tidewaiter* was a customs officer who boarded a ship on arrival, **waiting for the tide** before going aboard. This one waits for the tide of traffic to go out, then quietly swaps your container for a newer one — and backs off the moment anyone's aboard.

Status: **planning**. This document is the brief for the build session. Nothing here is built yet.

---

## 1. What it is

A small, single-container daemon that keeps your Docker containers up to date **safely and unobtrusively**:

1. **Watches the registry** for a new image digest for each container that opts in via a label.
2. **Waits until the container is idle** — no live network flows — before touching it. (The differentiator.)
3. **Health-gates the swap**: pull → recreate → wait for healthy → keep; otherwise **roll back** to the exact previous image.

General purpose — any image, not just game servers. Paired with Flatcar's own OS auto-update, the goal is a server that stays current end to end with no hands on it.

It is the sibling of [`portical`](https://github.com/danielbodart/portical) (same author, same shape: a label-driven reconcile loop over the Docker Engine API). **We vendor portical as a git submodule at `vendor/portical` and lift the parts that matter** (see §7).

---

## 2. Why build it (the gap)

Researched thoroughly; the exact combination does not exist for plain Docker/compose:

| Need | State of the art | Gap for us |
| --- | --- | --- |
| Registry-watch + auto-update | **Watchtower** (archived Dec 2025), **WUD**, **Diun** (notify-only), **Dockcheck** | Watchtower is dead; the rest don't gate on activity |
| Health-gated rollback | Docker **Swarm** (`--update-failure-action=rollback`, [buggy](https://github.com/moby/moby/issues/37972)), **Podman auto-update** (unit-failure rollback) | Swarm-only or Podman-only; **Podman isn't on Flatcar** (docker + containerd only) |
| **Don't update while in use** | **Agones** (k8s): `Ready`/`Allocated` states + PodDisruptionBudgets; per-game scripts (lloesche Valheim, palworld) | Agones needs Kubernetes **and** the app to self-report via its SDK; per-game scripts are single-game |

**Tidewaiter is "Agones-lite for Docker/compose"**: it borrows Agones' idea that a busy server must not be disrupted, but infers "busy" from the **network (conntrack)** instead of requiring the app to self-report — so any image qualifies with zero integration, no Kubernetes, no Podman.

### Prior art to reference (always look before inventing)
- **[Agones](https://agones.dev/site/docs/advanced/controlling-disruption/)** — the conceptual north star for activity-aware, disruption-safe game-server lifecycle. Borrow the `Ready`/`Allocated` mental model; reject the k8s + SDK weight.
- **[Podman auto-update](https://docs.podman.io/en/latest/markdown/podman-auto-update.1.html)** — `io.containers.autoupdate=registry` policy + rollback-on-unit-failure via sdnotify. Mirror the *registry digest* policy and the rollback intent.
- **Watchtower** (archived) — the label-driven auto-update UX everyone knows; match the ergonomics, fix the "no rollback / no activity gate / dead" parts.
- **[WUD / What's Up Docker](https://getwud.github.io/wud/)** — mature registry-watch + triggers; reference its registry/watch design (and consider it as the "commodity 80%" if we ever want less to maintain).
- **[Dockcheck](https://github.com/mag37/dockcheck)** — image-backup rollback approach in a shell tool; simplest existing rollback pattern.
- **[itzg/mc-server-runner](https://github.com/itzg/mc-server-runner)** — reference for graceful stop (translate SIGTERM → in-band `stop`, wait). Relevant to how we *drain/stop* a container before recreate.
- **Bun fetch 300s timeout** — [oh-my-pi#2422](https://github.com/can1357/oh-my-pi/issues/2422): Bun's fetch has a hard ~300s timeout, `AbortSignal` can't extend it, disable with `timeout: false`. Already handled in portical's `http.ts` (which we vendor) for the Docker `/events` stream; the registry client must do the same where it long-polls.

Whenever a new sub-problem appears in the build, **check for prior art first and cite it** — that's the house style.

---

## 3. Shape & constraints (decided)

- **One container.** No sidecars. Runs with `network_mode: host`, the Docker socket mounted read-write, and **`CAP_NET_ADMIN`** (required to read conntrack via netlink). Capabilities are a *runtime* grant — set in the deploy compose (`cap_add: [NET_ADMIN]`), **not** the image, and **not** Flatcar/Ignition. See §6.
- **Bun + TypeScript**, compiled to a single static binary, same as portical. Alpine final image + `tini` as PID 1 + `conntrack-tools`.
- **Label-driven**, reconcile loop, pure decision core — same architecture as portical.
- **Trunk-based** (`master`), CI tests-gated build+push to Docker Hub + GHCR, version derived from git (copy portical's `run.ts`).

---

## 4. Activity detection ("is the tide in?")

This is the novel core. A container is **in use** if it has live inbound network flows to its published ports.

### Scope (decided with the user)
- **Primary detector — conntrack via netlink: UDP *and* TCP.** Count flows whose destination is one of the container's published ports. This is the only thing that sees **UDP** peers (a UDP server uses one socket for all clients, so there is no per-peer socket state to count — conntrack is the *only* way to see UDP players). Proven working on the target Flatcar host: `/proc/net/nf_conntrack` is compiled out, but `nf_conntrack_netlink` is loaded and `conntrack -L` / libnetfilter_conntrack work over netlink (needs `CAP_NET_ADMIN`).
- **Backup detector — TCP only.** When conntrack is unavailable, fall back to counting `ESTABLISHED` TCP sockets (`/proc/net/tcp[6]` in the container's netns, reached via the Docker-reported PID) — and/or the Docker `/stats` net-I/O **rate** as a zero-privilege signal. **TCP only, because UDP has no socket-state fallback.**
- **Ignore all other protocols** (ICMP, etc.) and all non-published ports.

### Rules
- **Fail safe: if we cannot tell, assume in use** and defer the update. Never kick a live session to chase a version.
- Debounce: require **N consecutive idle samples** over a window so a brief lull between packets doesn't read as empty.
- Distinguish a short status ping / port scan (single packet, transient flow) from a real session (sustained / `ASSURED` conntrack entry). Prefer `ASSURED` flows and/or a minimum flow age.
- LAN and WAN both count — the host's own conntrack sees a LAN client (`src=10.x`) *and* a DNAT'd WAN client, which is why host-side conntrack beats asking the gateway (gateway sees WAN only).

### Detector interface (pluggable, but ship only the above)
```ts
interface Detector {
  // players/sessions undefined => "presence only, can't count"
  inUse(container: Container): Promise<{ inUse: boolean; sessions?: number; confidence: "low" | "high" }>;
}
```
Registry keyed by name; a container's label may pick one; default = conntrack with the TCP/net-io backup. Compose conservatively (in use if **any** says so). Keep the interface so a native `game-query` detector can be added later, but do not build it now.

---

## 5. Update flow (per opted-in container, each reconcile pass)

```
for each container with tidewaiter.enable=true:
  desired  = registry digest for its image ref     # HEAD/manifest, honour :tag
  current  = running image digest (docker inspect)
  if desired == current: keep, done
  if detector.inUse(container): defer (log "tide is in, N sessions"), try again next pass
  else:
    record previous image id/digest                 # for rollback
    pull desired
    graceful stop (SIGTERM, honour stop_grace_period; drain)
    recreate with identical config (image = desired)
    wait for health (HEALTHCHECK) up to a timeout
    if healthy: commit (prune nothing; keep prev image for one cycle)
    else: recreate with previous image, wait healthy, log FAILED + keep desired pinned-out until it changes
```

Notes:
- **Recreate faithfully.** Preserve name, env, mounts, ports, networks, labels, restart policy, caps — read them off the running container's inspect and re-apply. (This is the fiddliest part; portical's `docker.ts` already models containers/inspect — extend it with create/remove.)
- **Rollback** mirrors Podman's "restart failed → previous image": we keep the previous image ID and, on health failure, put it straight back. Health signal = Docker `HEALTHCHECK` status (fall back to "container stays running for T seconds" when no healthcheck is defined).
- **Registry auth**: support anonymous + `~/.docker/config.json` creds for private registries; Docker Hub rate-limit awareness (Dockcheck does this — reference it).
- **Digest, not tag-chasing**: compare manifest digests for the pinned tag (e.g. `:latest`, `:2`), like Podman's registry policy — do not silently jump tags.

---

## 6. Deploy (target: the Flatcar homelab)

```yaml
services:
  tidewaiter:
    image: danielbodart/tidewaiter:latest
    container_name: tidewaiter
    network_mode: host            # share the netns where the flows live
    cap_add:
      - NET_ADMIN                 # conntrack netlink dump; NOT in Docker's default set
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
    restart: unless-stopped
```
Opt a container in with labels (schema in §8). Nothing changes on the Flatcar OS side — this is app-level compose, version-controlled with the rest of the fleet (`danielbodart/server`). Flatcar's own locksmithd handles the OS; Tidewaiter handles the containers → an always-current box.

---

## 7. What to lift from portical (`vendor/portical`)

The submodule is there to **read and steal from while building**, not to depend on at runtime. Copy/adapt into `src/`:

- **`http.ts`** — the unix-socket `Handler`, `withTimeout`, and the **`timeout: false`** fix for long-poll streams (Docker `/events`, and any registry long-poll). Take as-is.
- **`docker.ts`** — Docker Engine API client (containers, events, inspect, networks). **Extend** with: image pull (`POST /images/create`), image inspect/digest, container create/remove/rename, and reading a container's full config for faithful recreate.
- **`daemon.ts`** — the reconcile loop: event-stream + interval, queued single-flight passes, reconnect-hardened, summary-on-change logging. Reshape the per-item action from "port mapping" to "update decision".
- **`reconcile.ts`** — keep the **pure decision function** pattern: `decide(desired, actual, options) -> Action[]`, tested without Docker or a clock.
- **`main.ts`** — arg/env parsing, subcommands (`run` default, `list`, `check` one-shot), signal handling, startup banner with version.
- **`run.ts` + `version.ts`** — git-derived version baking. Take as-is.
- **`Dockerfile`** — Bun `--compile` cross-arch → alpine + `tini`. **Add** `conntrack-tools` (or link libnetfilter_conntrack) and document `CAP_NET_ADMIN`.
- **`.github/workflows/docker.yml`** — tests-gated build+push, multi-arch, version step. Take as-is (retarget image name).
- **`test/fakes/*`, `mise.toml`, `tsconfig.json`, `package.json`** — test harness + Bun setup. Extend the fake Docker with pull/recreate and a fake conntrack source.

Attribution: portical is the author's own (Apache-2.0); copying is fine. Keep a note in the README that Tidewaiter is portical's sibling and shares its spine.

---

## 8. Labels (draft — refine in build)

| Label | Meaning | Default |
| --- | --- | --- |
| `tidewaiter.enable` | opt this container in | `false` |
| `tidewaiter.policy` | `registry` (digest) — room for `local` later | `registry` |
| `tidewaiter.detector` | `conntrack` \| `tcp` \| `netio` \| `none` | `conntrack` |
| `tidewaiter.ports` | override which ports count as "in use" | container's published ports |
| `tidewaiter.idle-samples` | consecutive idle checks required | e.g. `3` |
| `tidewaiter.health-timeout` | seconds to wait for healthy before rollback | e.g. `120` |
| `tidewaiter.grace` | stop grace (drain) seconds | container's `stop_grace_period` |

Mirror portical's convention (`portical.upnp.forward`) so the two read as a family.

---

## 9. Build order (milestones)

1. **Skeleton**: copy portical's Bun/CI/Docker scaffolding; `main.ts` with `run`/`list`/`check`; version baking; green empty test run.
2. **Docker client**: extend `docker.ts` — inspect full config, pull image, compare digests, create/remove/rename. Fake-Docker tests for a faithful recreate.
3. **Update engine**: pure `decide()` (keep / update / defer / rollback) + the apply loop (pull → stop → recreate → health → commit/rollback). Tests with fake Docker, no clock.
4. **Detectors**: `conntrack` (netlink, UDP+TCP) + `tcp`/`netio` backup; the debounce + fail-safe + `ASSURED`/age filtering. Fake conntrack source for tests.
5. **Wire the gate** into the loop; conservative combination; structured logging.
6. **Image + deploy**: Dockerfile (+conntrack-tools, tini), compose with `cap_add: NET_ADMIN`, ship to Docker Hub/GHCR via CI.
7. **Adopt on the homelab**: label one low-stakes container first, watch a real update + a forced rollback, then roll out.

---

## 10. Assumptions & open questions (confirm during build)

- **"UDP and TCP, backup TCP only, ignore all others"** is read as: conntrack sees UDP+TCP; the *fallback* is TCP-only (UDP has no socket-state fallback); other protocols ignored. Confirm this matches intent.
- Health signal when a container has **no HEALTHCHECK**: use "stays up for T seconds" — is that enough, or require an explicit health label to be eligible for auto-update?
- Rollback bookkeeping: keep the previous image for one cycle then prune, vs keep last-known-good indefinitely.
- Multi-arch / private-registry auth scope for v1.
- Does the reconcile also want an `/events`-driven fast path (like portical) or is a plain interval enough for updates? (Interval is probably fine; events matter less for image changes.)

## 11. Non-goals (v1)
- No web UI (that's WUD's turf; add later if wanted).
- No orchestration / multi-host (single Docker host, like the homelab).
- No building images — Tidewaiter *consumes* images; the image=version build pipelines live in each app's repo.
- No native game-query detector yet (interface leaves room; conntrack covers it).

import type { ContainerSpec, NetworkAttachment } from "./model.ts";

/**
 * The /containers/create request body, as Docker's Engine API wants it.
 *
 * A loose shape on purpose - it is JSON handed straight to Docker, and pinning
 * every nested field would only duplicate Docker's own schema. What matters is
 * that toCreatePayload builds it from a fixed allowlist of ContainerSpec fields
 * and nothing else, so no runtime-only inspect field can ever leak in.
 */
export interface CreatePayload {
  readonly [key: string]: unknown;
}

/**
 * The single throwaway name an old container is parked under during a swap.
 *
 * One fixed suffix rather than a timestamp, so a swap interrupted half-way
 * leaves a predictable name the next pass (or an operator) can find and clean
 * up, instead of a litter of unique corpses.
 */
export function rollbackName(name: string): string {
  return `${name}-tidewaiter-rollback`;
}

/**
 * Build a faithful create payload for a container, changing only the image.
 *
 * A hand-picked allowlist, deliberately NOT a deep-copy of the inspect blob:
 * inspect carries runtime-only state (the container's id, current IP, PID,
 * State) that Docker would reject or misapply on create. Every field here was
 * chosen because it is part of what the container *is*, not part of what it is
 * *doing right now*. recreate.test.ts is the living checklist - a new inspect
 * field that should survive a recreate is a failing test until it is added
 * here.
 *
 * Only the *first* network is attached at create time: Docker's create API
 * takes exactly one endpoint. Any further networks are attached afterwards with
 * connectNetwork - see extraNetworks below.
 */
export function toCreatePayload(spec: ContainerSpec): CreatePayload {
  const config: Record<string, unknown> = {
    Image: spec.image,
    Env: [...spec.env],
    Labels: { ...spec.labels },
  };
  if (spec.cmd) config.Cmd = [...spec.cmd];
  if (spec.entrypoint) config.Entrypoint = [...spec.entrypoint];
  if (spec.exposed.length > 0) {
    config.ExposedPorts = Object.fromEntries(spec.exposed.map((port) => [port, {}]));
  }
  if (spec.user) config.User = spec.user;
  if (spec.workingDir) config.WorkingDir = spec.workingDir;
  if (spec.stopSignal) config.StopSignal = spec.stopSignal;
  if (spec.stopTimeout !== undefined) config.StopTimeout = spec.stopTimeout;
  if (spec.healthcheck) {
    config.Healthcheck = {
      Test: [...spec.healthcheck.test],
      ...(spec.healthcheck.intervalNanos !== undefined ? { Interval: spec.healthcheck.intervalNanos } : {}),
      ...(spec.healthcheck.timeoutNanos !== undefined ? { Timeout: spec.healthcheck.timeoutNanos } : {}),
      ...(spec.healthcheck.retries !== undefined ? { Retries: spec.healthcheck.retries } : {}),
      ...(spec.healthcheck.startPeriodNanos !== undefined ? { StartPeriod: spec.healthcheck.startPeriodNanos } : {}),
    };
  }

  const hostConfig: Record<string, unknown> = {
    Binds: bindsOf(spec),
    Mounts: volumeMountsOf(spec),
    PortBindings: portBindingsOf(spec),
    Privileged: spec.privileged,
  };
  if (spec.restartPolicy) {
    hostConfig.RestartPolicy = {
      Name: spec.restartPolicy.name,
      MaximumRetryCount: spec.restartPolicy.maximumRetryCount,
    };
  }
  if (spec.capAdd.length > 0) hostConfig.CapAdd = [...spec.capAdd];
  if (spec.capDrop.length > 0) hostConfig.CapDrop = [...spec.capDrop];
  if (spec.networkMode) hostConfig.NetworkMode = spec.networkMode;
  if (spec.devices.length > 0) {
    hostConfig.Devices = spec.devices.map((device) => ({
      PathOnHost: device.pathOnHost,
      PathInContainer: device.pathInContainer,
      CgroupPermissions: device.cgroupPermissions,
    }));
  }
  if (spec.extraHosts.length > 0) hostConfig.ExtraHosts = [...spec.extraHosts];
  if (Object.keys(spec.sysctls).length > 0) hostConfig.Sysctls = { ...spec.sysctls };

  const payload: Record<string, unknown> = { ...config, HostConfig: hostConfig };

  const [primary] = spec.networks;
  if (primary) {
    payload.NetworkingConfig = { EndpointsConfig: { [primary.name]: endpointOf(primary) } };
  }

  return payload;
}

/** Every network beyond the first, which the daemon attaches after create. */
export function extraNetworks(spec: ContainerSpec): readonly NetworkAttachment[] {
  return spec.networks.slice(1);
}

/**
 * Whether a spec can be faithfully recreated at all.
 *
 * An anonymous volume cannot: it has no name to reattach, so recreating from it
 * would mint a fresh empty volume and silently drop the data. Rather than do
 * that, the daemon defers the update and says why - the container keeps running
 * its current image, which is the safe outcome.
 */
export function recreatable(spec: ContainerSpec): { ok: true } | { ok: false; reason: string } {
  const anonymous = spec.mounts.find((mount) => mount.anonymous);
  if (anonymous) {
    return {
      ok: false,
      reason:
        `has an anonymous volume at ${anonymous.target}, which cannot be recreated without ` +
        "losing its data; deferring - give it a named volume to enable updates",
    };
  }
  return { ok: true };
}

/**
 * The ordered Docker calls that carry out a swap, as data.
 *
 * Returned as a plan rather than executed, so the *sequencing* - which is the
 * subtle part - is unit-testable with no daemon and no Docker. The daemon walks
 * the list, and on a health failure walks the rollback list instead.
 *
 * The shape is rename-not-delete: the old container is parked under
 * rollbackName() rather than removed, so at every moment exactly one real
 * container wears the original name and there is never a gap where the name
 * resolves to nothing. Commit removes the parked old one; rollback removes the
 * freshly-created new one and restores the old name.
 */
export type SwapStep =
  | { readonly op: "stop"; readonly container: string; readonly graceSeconds: number }
  | { readonly op: "rename"; readonly container: string; readonly to: string }
  | { readonly op: "create"; readonly spec: ContainerSpec }
  | { readonly op: "connect"; readonly container: string; readonly network: NetworkAttachment }
  | { readonly op: "start"; readonly container: string }
  | { readonly op: "remove"; readonly container: string; readonly force: boolean };

export interface SwapPlan {
  /** Stop the old container, park it, and stand the new one up in its place. */
  readonly swap: readonly SwapStep[];
  /** After a healthy new container: drop the parked old one. */
  readonly commit: readonly SwapStep[];
  /** After an unhealthy new container: drop the new one, restore the old. */
  readonly rollback: readonly SwapStep[];
}

/**
 * Plan a swap from the running container to `newSpec`.
 *
 * `newSpec` is the same faithful spec with the new image; `newId` is a
 * placeholder the daemon substitutes once create returns the real id (the plan
 * cannot know it in advance, so commit/rollback refer to the new container by
 * the name it is created under - the original name - which is unambiguous
 * because the old one has been renamed away first).
 */
export function planSwap(spec: ContainerSpec, newSpec: ContainerSpec, graceSeconds: number): SwapPlan {
  const name = spec.name;
  const parked = rollbackName(name);

  const swap: SwapStep[] = [
    { op: "stop", container: name, graceSeconds },
    { op: "rename", container: name, to: parked },
    { op: "create", spec: newSpec },
    ...extraNetworks(newSpec).map(
      (network): SwapStep => ({ op: "connect", container: name, network }),
    ),
    { op: "start", container: name },
  ];

  const commit: SwapStep[] = [{ op: "remove", container: parked, force: true }];

  const rollback: SwapStep[] = [
    { op: "remove", container: name, force: true },
    { op: "rename", container: parked, to: name },
    { op: "start", container: name },
  ];

  return { swap, commit, rollback };
}

function bindsOf(spec: ContainerSpec): string[] {
  return spec.mounts
    .filter((mount) => mount.type === "bind")
    .map((mount) => `${mount.source}:${mount.target}${mount.readonly ? ":ro" : ""}`);
}

/** Named volumes and tmpfs go in Mounts long-form; binds stay in Binds. */
function volumeMountsOf(spec: ContainerSpec): Record<string, unknown>[] {
  return spec.mounts
    .filter((mount) => mount.type !== "bind")
    .map((mount) => ({
      Type: mount.type,
      Source: mount.type === "volume" ? mount.source : undefined,
      Target: mount.target,
      ReadOnly: mount.readonly,
    }));
}

function portBindingsOf(spec: ContainerSpec): Record<string, { HostPort: string }[]> {
  const bindings: Record<string, { HostPort: string }[]> = {};
  for (const port of spec.published) {
    const key = `${port.containerPort}/${port.protocol}`;
    (bindings[key] ??= []).push({ HostPort: String(port.hostPort) });
  }
  return bindings;
}

/**
 * The Engine API EndpointConfig for attaching one network.
 *
 * The same shape whether it goes in a create payload's NetworkingConfig or a
 * later /networks/{id}/connect, so it lives here and docker.ts reuses it for the
 * connect calls rather than keeping a second copy.
 */
export function endpointOf(network: NetworkAttachment): Record<string, unknown> {
  const endpoint: Record<string, unknown> = {};
  if (network.aliases.length > 0) endpoint.Aliases = [...network.aliases];
  const ipam: Record<string, string> = {};
  if (network.ipv4Address) ipam.IPv4Address = network.ipv4Address;
  if (network.ipv6Address) ipam.IPv6Address = network.ipv6Address;
  if (Object.keys(ipam).length > 0) endpoint.IPAMConfig = ipam;
  return endpoint;
}

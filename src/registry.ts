import { ok, type Handler } from "./http.ts";
import type { Digest } from "./model.ts";

/**
 * Resolves the digest a tag currently points at, in the registry.
 *
 * The whole basis of "is there anything new?" - Tidewaiter never chases a tag
 * blindly, it compares the digest the tag resolves to against the digest the
 * container is running, exactly Podman's registry policy. An interface so the
 * daemon runs against a fake with a canned digest map in tests.
 */
export interface RegistryClient {
  digest(ref: string, signal?: AbortSignal): Promise<Digest>;
}

/**
 * Where credentials come from, keyed by registry host.
 *
 * Returns a ready X-Registry-Auth header value (base64 JSON) for a host, or
 * undefined for anonymous. The same value serves both the manifest request here
 * and docker.pull() on the socket, so "where do creds live" stays in one place.
 */
export type AuthSource = (registry: string) => string | undefined;

/** No credentials for anyone - anonymous everywhere. */
export const anonymous: AuthSource = () => undefined;

interface ImageRef {
  readonly registry: string;
  readonly repository: string;
  readonly reference: string;
}

/**
 * Talks to an OCI/Docker v2 registry over a Handler.
 *
 * Resolves a manifest digest with a HEAD (falling back to GET) against the
 * manifests endpoint, reading the Docker-Content-Digest header - the digest the
 * registry itself vouches for, so it matches what a pull would land and what
 * Docker records in RepoDigests.
 */
/** How Docker Hub throttling reaches us, and the backoff for it. */
const MAX_RATELIMIT_RETRIES = 2;
/** Fallback wait when a 429 carries no Retry-After, in milliseconds. */
const DEFAULT_BACKOFF_MILLIS = 1000;

export class HttpRegistryClient implements RegistryClient {
  constructor(
    private readonly handler: Handler,
    private readonly auth: AuthSource = anonymous,
    /**
     * Injected so a rate-limit backoff is deterministic in tests: the real
     * client waits on a timer, a test passes a recording no-op and asserts on
     * how long it was asked to wait.
     */
    private readonly sleep: (millis: number) => Promise<void> = defaultSleep,
  ) {}

  async digest(ref: string, signal?: AbortSignal): Promise<Digest> {
    const image = parseRef(ref);
    const url = `https://${image.registry}/v2/${image.repository}/manifests/${image.reference}`;

    try {
      let response = await this.request(image, url, "HEAD", signal);

      // Docker Hub (and any token-auth registry) answers an anonymous manifest
      // request with 401 and a Bearer challenge naming where to get a token.
      // Fetch one and retry - this is the standard registry auth dance.
      if (response.status === 401) {
        const token = await this.tokenFor(response, image, signal);
        response = await this.request(image, url, "HEAD", signal, token);
      }

      // A few registries do not answer HEAD on manifests; fall back to GET.
      if (response.status === 405) {
        response = await this.request(image, url, "GET", signal);
        if (response.status === 401) {
          const token = await this.tokenFor(response, image, signal);
          response = await this.request(image, url, "GET", signal, token);
        }
      }

      await ok(response);
      const digest = response.headers.get("docker-content-digest");
      await response.text();
      if (!digest) {
        throw new Error("registry returned no Docker-Content-Digest header");
      }
      return digest;
    } catch (cause) {
      throw new Error(`resolving digest for ${ref} failed: ${(cause as Error).message}`, { cause });
    }
  }

  /**
   * Make a request, retrying through Docker Hub's rate limit.
   *
   * A 429 is not an error to surface immediately - Docker Hub throttles
   * anonymous pulls routinely, and the fix is simply to wait the Retry-After it
   * names and try again. Retries a small number of times, then gives up with a
   * clear "rate-limited" error the daemon logs (and leaves the container alone
   * for). The 429 body is drained each time for cleanliness. Every actual HTTP
   * call funnels through here, so both the manifest and token requests get the
   * same treatment.
   */
  private request(
    image: ImageRef,
    url: string,
    method: "HEAD" | "GET",
    signal?: AbortSignal,
    bearer?: string,
  ): Promise<Response> {
    return this.retrying(url, () => this.send(image, url, method, signal, bearer));
  }

  /**
   * Run a request, waiting out Docker Hub's rate limit and retrying.
   *
   * Every actual HTTP call - manifest and token alike - funnels through here, so
   * a 429 on either is handled the same way: drain it, wait the Retry-After it
   * names (or a default), and try again a few times before giving up with a
   * clear "rate-limited" error the daemon logs and leaves the container alone
   * for.
   */
  private async retrying(url: string, make: () => Promise<Response>): Promise<Response> {
    for (let attempt = 0; ; attempt += 1) {
      const response = await make();
      if (response.status !== 429) return response;

      await response.text(); // drain before discarding
      if (attempt >= MAX_RATELIMIT_RETRIES) {
        throw new Error(`registry rate-limited ${url} after ${attempt + 1} attempts`);
      }
      await this.sleep(retryAfterMillis(response.headers.get("retry-after")));
    }
  }

  /** One HTTP call to the registry, no retry logic. */
  private send(
    image: ImageRef,
    url: string,
    method: "HEAD" | "GET",
    signal?: AbortSignal,
    bearer?: string,
  ): Promise<Response> {
    const headers: Record<string, string> = {
      // Ask for the shapes a digest lives behind: v2 manifests, manifest lists
      // and their OCI equivalents. Without this a registry may hand back a v1
      // manifest with a different digest.
      Accept: [
        "application/vnd.docker.distribution.manifest.v2+json",
        "application/vnd.docker.distribution.manifest.list.v2+json",
        "application/vnd.oci.image.manifest.v1+json",
        "application/vnd.oci.image.index.v1+json",
      ].join(", "),
    };
    if (bearer) headers.Authorization = `Bearer ${bearer}`;
    else {
      const basic = this.auth(image.registry);
      // A stored credential is X-Registry-Auth's base64 JSON; for a direct
      // manifest request we need Basic auth instead, so decode it back.
      const basicHeader = toBasicAuth(basic);
      if (basicHeader) headers.Authorization = basicHeader;
    }
    return this.handler(new Request(url, { method, headers, signal }));
  }

  /**
   * Follow a 401's Bearer challenge to a token.
   *
   * The WWW-Authenticate header names the realm, service and scope; the token
   * endpoint hands back a bearer token for exactly that scope. Anonymous pulls
   * of public images work because the token endpoint grants a scoped anonymous
   * token - no credentials needed for the common case.
   */
  private async tokenFor(challenge: Response, image: ImageRef, signal?: AbortSignal): Promise<string | undefined> {
    await challenge.text();
    const header = challenge.headers.get("www-authenticate");
    if (!header || !/^bearer /i.test(header)) return undefined;

    const params = parseChallenge(header);
    const realm = params.realm;
    if (!realm) return undefined;

    const url = new URL(realm);
    if (params.service) url.searchParams.set("service", params.service);
    url.searchParams.set("scope", params.scope ?? `repository:${image.repository}:pull`);

    const headers: Record<string, string> = {};
    const basic = toBasicAuth(this.auth(image.registry));
    if (basic) headers.Authorization = basic;

    const response = await ok(
      await this.retrying(url.toString(), () => this.handler(new Request(url.toString(), { headers, signal }))),
    );
    const body = (await response.json()) as { token?: string; access_token?: string };
    return body.token ?? body.access_token;
  }
}

/**
 * Docker's default registry is Docker Hub, under an odd pair of hostnames: refs
 * name `docker.io` but the API lives at `registry-1.docker.io`, and a bare
 * `nginx` means `library/nginx`. Normalising both here keeps the rest simple.
 */
const DEFAULT_REGISTRY = "registry-1.docker.io";
const DOCKER_HUB_ALIASES = new Set(["docker.io", "index.docker.io", "registry-1.docker.io"]);

export function parseRef(ref: string): ImageRef {
  let remainder = ref;
  let registry = DEFAULT_REGISTRY;

  const slash = remainder.indexOf("/");
  const firstPart = slash === -1 ? "" : remainder.slice(0, slash);
  // A registry host is the bit before the first slash *if* it looks like a host
  // (has a dot or colon, or is localhost) - otherwise the whole thing is a Hub
  // repository like `library/nginx` and there is no registry prefix.
  if (firstPart && (firstPart.includes(".") || firstPart.includes(":") || firstPart === "localhost")) {
    registry = DOCKER_HUB_ALIASES.has(firstPart) ? DEFAULT_REGISTRY : firstPart;
    remainder = remainder.slice(slash + 1);
  }

  // Split the tag or digest off the end. A digest uses `@`, a tag uses the last
  // `:` that is not part of a registry:port (already stripped above).
  let repository = remainder;
  let reference = "latest";
  const at = remainder.indexOf("@");
  if (at !== -1) {
    repository = remainder.slice(0, at);
    reference = remainder.slice(at + 1);
  } else {
    const colon = remainder.lastIndexOf(":");
    if (colon !== -1) {
      repository = remainder.slice(0, colon);
      reference = remainder.slice(colon + 1);
    }
  }

  // A single-segment repository on Docker Hub lives under `library/`.
  if (registry === DEFAULT_REGISTRY && !repository.includes("/")) {
    repository = `library/${repository}`;
  }

  return { registry, repository, reference };
}

/**
 * Turn a Retry-After header into milliseconds to wait.
 *
 * The header is either a number of seconds or an HTTP-date. A missing, empty or
 * unparseable value falls back to a fixed backoff rather than hammering the
 * registry immediately. A date in the past clamps to zero.
 */
export function retryAfterMillis(header: string | null, defaultMillis = DEFAULT_BACKOFF_MILLIS): number {
  if (header === null || header.trim() === "") return defaultMillis;

  const seconds = Number(header);
  if (Number.isFinite(seconds)) return Math.max(0, seconds * 1000);

  const date = Date.parse(header);
  if (Number.isNaN(date)) return defaultMillis;
  return Math.max(0, date - Date.now());
}

function defaultSleep(millis: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, millis));
}

/** Parse a `Bearer realm="...",service="...",scope="..."` challenge. */
function parseChallenge(header: string): Record<string, string> {
  const params: Record<string, string> = {};
  const body = header.replace(/^bearer /i, "");
  for (const match of body.matchAll(/(\w+)="([^"]*)"/g)) {
    const key = match[1];
    const value = match[2];
    if (key !== undefined && value !== undefined) params[key] = value;
  }
  return params;
}

/**
 * Turn a stored X-Registry-Auth value back into a Basic auth header.
 *
 * dockerConfigAuth encodes credentials as the base64 JSON Docker's socket
 * wants; a direct registry request needs Basic instead, so decode the username
 * and password back out. An identity-token credential has no Basic form, so it
 * is skipped (the Bearer path handles those registries).
 */
function toBasicAuth(xRegistryAuth: string | undefined): string | undefined {
  if (!xRegistryAuth) return undefined;
  try {
    const json = JSON.parse(Buffer.from(xRegistryAuth, "base64").toString("utf8")) as {
      username?: string;
      password?: string;
    };
    if (!json.username || json.password === undefined) return undefined;
    return `Basic ${Buffer.from(`${json.username}:${json.password}`).toString("base64")}`;
  } catch {
    return undefined;
  }
}

/**
 * Build an AuthSource from ~/.docker/config.json.
 *
 * Reads the `auths` map: each entry's `auth` is base64 `user:password`, which
 * Docker's socket wants re-encoded as base64 JSON `{username,password,
 * serveraddress}` for X-Registry-Auth. An `identitytoken` (from `docker login`
 * with a token) is passed through as `{identitytoken}`. Missing file or a
 * registry with no entry means anonymous, which is the common homelab case.
 */
export async function dockerConfigAuth(path?: string): Promise<AuthSource> {
  const configPath = path ?? `${homeDir()}/.docker/config.json`;

  let config: DockerConfig;
  try {
    config = (await Bun.file(configPath).json()) as DockerConfig;
  } catch {
    return anonymous;
  }

  const byRegistry = new Map<string, string>();
  for (const [host, entry] of Object.entries(config.auths ?? {})) {
    const value = encodeAuth(host, entry);
    if (value) byRegistry.set(normaliseHost(host), value);
  }

  return (registry) => byRegistry.get(normaliseHost(registry));
}

interface DockerConfig {
  auths?: Record<string, { auth?: string; identitytoken?: string }>;
}

function encodeAuth(host: string, entry: { auth?: string; identitytoken?: string }): string | undefined {
  const serveraddress = normaliseHost(host);

  if (entry.identitytoken) {
    return base64Json({ identitytoken: entry.identitytoken, serveraddress });
  }
  if (entry.auth) {
    const decoded = Buffer.from(entry.auth, "base64").toString("utf8");
    const colon = decoded.indexOf(":");
    if (colon === -1) return undefined;
    const username = decoded.slice(0, colon);
    const password = decoded.slice(colon + 1);
    return base64Json({ username, password, serveraddress });
  }
  return undefined;
}

function base64Json(value: unknown): string {
  return Buffer.from(JSON.stringify(value)).toString("base64");
}

/** Docker Hub is stored under a few hostnames; treat them as one. */
function normaliseHost(host: string): string {
  const bare = host.replace(/^https?:\/\//, "").replace(/\/.*$/, "");
  return DOCKER_HUB_ALIASES.has(bare) ? DEFAULT_REGISTRY : bare;
}

function homeDir(): string {
  return Bun.env.HOME ?? Bun.env.USERPROFILE ?? "";
}

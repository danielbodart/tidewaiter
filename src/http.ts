/**
 * The uniform interface: everything that talks HTTP is this one function type.
 *
 * The Docker Engine API and the image registry are both just handlers, so both
 * can be swapped for an in-memory implementation with no server, no port and no
 * socket. That is what makes the daemon testable end to end - the fake Docker
 * and fake registry in the tests are objects built on this seam, not processes.
 *
 * It is also why nothing below this line knows whether it is talking over a
 * unix socket or TCP.
 *
 * Lifted from portical's http.ts, which is where this shape was worked out.
 */
export type Handler = (request: Request) => Promise<Response>;

/** Talks to a real server over TCP. Used for the registry. */
export const http: Handler = (request) => fetch(request);

/**
 * Talks to a real server over a unix socket.
 *
 * Bun's fetch speaks unix sockets natively, so reaching the Docker Engine API
 * needs no client library and no `docker` binary in the image.
 */
// Bun supports `timeout` on its fetch extension at runtime, but @types/bun
// does not declare it yet, so teach the global type about it. Used below to
// switch off Bun's hard ~300s fetch timeout for the calls that must outlive it.
// See oh-my-pi#2422 / oven-sh/bun: the timeout cannot be extended with an
// AbortSignal, only disabled with this flag.
declare global {
  interface BunFetchRequestInit {
    timeout?: boolean | number;
  }
}

/**
 * Which Docker calls outlive Bun's ~300s fetch timeout and must disable it.
 *
 * Two of them, both for the same underlying reason - a response that Bun's
 * timer would abort even though nothing is wrong:
 *
 *   /events        long-polls, sitting idle whenever no container starts or
 *                  stops. On a quiet host Bun's timer fired every ~5 minutes,
 *                  read as fatal, and restarted the whole daemon.
 *
 *   /images/create pulls an image. A large image over a slow link can stream
 *                  progress for well past 300s while making steady headway;
 *                  aborting it mid-pull would fail an update that was working.
 *
 * Every other Docker call keeps the default bound so a genuinely hung socket
 * cannot wedge a pass forever.
 */
function outlivesTimeout(request: Request): boolean {
  if (request.url.includes("/events")) return true;
  return request.method === "POST" && request.url.includes("/images/create");
}

export function overUnixSocket(path: string): Handler {
  return (request) =>
    outlivesTimeout(request)
      ? fetch(request, { unix: path, timeout: false })
      : fetch(request, { unix: path });
}

/** Fail a request that takes too long, so a silent server cannot hang a pass. */
export function withTimeout(handler: Handler, milliseconds: number): Handler {
  return async (request) => {
    // The incoming request may already carry a signal - a daemon shutting down
    // aborts its calls that way - so both have to be honoured.
    const signal = request.signal
      ? AbortSignal.any([request.signal, AbortSignal.timeout(milliseconds)])
      : AbortSignal.timeout(milliseconds);
    return handler(new Request(request, { signal }));
  };
}

/** Throw with the server's own words rather than a bare status code. */
export async function ok(response: Response): Promise<Response> {
  if (!response.ok) {
    const body = (await response.text()).trim();
    throw new Error(`${response.status} ${response.statusText}${body === "" ? "" : `: ${body}`}`);
  }
  return response;
}

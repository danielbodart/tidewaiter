# Built on the *build* platform whatever the target, and cross-compiled to the
# target architecture by Bun itself. Emulating an arm64 toolchain under QEMU to
# produce an arm64 image takes minutes; this takes seconds.
FROM --platform=$BUILDPLATFORM oven/bun:1-alpine AS build

WORKDIR /app

COPY package.json bun.lock ./
RUN bun install --frozen-lockfile

COPY tsconfig.json ./
COPY src ./src

# Computed by run.ts from the git history, which is not in the build context -
# so it is passed in rather than worked out here. A build with no --build-arg
# still works and says "development", which is the truth about that build.
ARG VERSION=development

ARG TARGETARCH
RUN target="bun-linux-$([ "$TARGETARCH" = "arm64" ] && echo arm64 || echo x64)-musl" && \
    bun build --compile --minify --define "TIDEWAITER_VERSION=\"$VERSION\"" --target="$target" src/main.ts --outfile tidewaiter

FROM alpine:3

# An ARG does not survive into the next stage, so it is declared again. These
# labels are how `docker inspect` answers what a pulled image actually is,
# which matters when every published tag also exists as :latest.
ARG VERSION=development
LABEL org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.title="tidewaiter" \
      org.opencontainers.image.description="Update Docker containers only when idle, health-gate the swap, roll back on failure" \
      org.opencontainers.image.source="https://github.com/danielbodart/tidewaiter" \
      org.opencontainers.image.licenses="Apache-2.0"

# libstdc++ - Bun's compiled binary links against the C++ runtime.
# ca-certificates - for the outbound HTTPS the registry client makes.
# conntrack-tools - the primary activity detector shells out to `conntrack -L`
#   over netlink; it needs CAP_NET_ADMIN at runtime (granted in the compose
#   file, not baked into the image).
# tini - a real PID 1 that reaps the conntrack subprocesses Tidewaiter spawns
#   each pass and forwards signals for a clean shutdown. Unlike portical, which
#   only reads the socket and spawns nothing, Tidewaiter genuinely needs one.
RUN apk add --no-cache libstdc++ ca-certificates conntrack-tools tini

COPY --from=build /app/tidewaiter /usr/local/bin/tidewaiter

# tini as PID 1, then Tidewaiter. `--` stops tini reading Tidewaiter's own
# flags as its own.
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/tidewaiter"]
CMD ["run"]

# The binary `mise run dist` built for the target architecture, and nothing
# compiled here: the release binaries and every image come from the one compile,
# by the Go mise.toml pins, so they cannot disagree.
#
# Only what an empty image cannot make for itself comes from Alpine, pinned by
# digest so a rebuild cannot change it with no commit to say so; the weekly
# update moves the digest. Its stage runs on the build platform whatever the
# target, since nothing taken from it depends on the architecture.
FROM --platform=$BUILDPLATFORM alpine:3.24@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a AS base

# Nothing but the binary. A static Go binary needs no libc, and the Bun build's
# libstdc++ went with Bun. conntrack-tools went too: Tidewaiter dumps the
# conntrack table over netlink itself, and asks the kernel for sockets over
# sock_diag where it used to run ss. The only subprocesses it still starts are
# nix-env and nix-store, for the systemd source, which never runs in this
# image. And so no tini either: there are no children to reap, and Tidewaiter
# handles SIGTERM itself.
FROM scratch

# Computed by ./run from the git history and passed in, as dist/ was built
# with it. These labels are how `docker inspect` answers what a pulled image
# actually is, which matters when every published tag also exists as :latest.
ARG VERSION=development
LABEL org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.title="tidewaiter" \
      org.opencontainers.image.description="Update Docker containers only when idle, health-gate the swap, roll back on failure" \
      org.opencontainers.image.source="https://github.com/danielbodart/tidewaiter" \
      org.opencontainers.image.licenses="Apache-2.0"

# The registry client speaks HTTPS to every registry on the internet.
COPY --from=base /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

# TARGETVARIANT is "v7" for linux/arm/v7 and empty for the others, so this is
# dist/linux-amd64, dist/linux-arm64 or dist/linux-armv7.
ARG TARGETARCH
ARG TARGETVARIANT
COPY dist/linux-${TARGETARCH}${TARGETVARIANT}/tidewaiter /usr/local/bin/tidewaiter

# Where ~/.docker/config.json is looked for, for a private registry's
# credentials. The Alpine image's root had /root from /etc/passwd; an empty
# image has no /etc/passwd, and Docker would make HOME "/", so a config
# mounted at /root/.docker/config.json for the old image would quietly stop
# being read.
ENV HOME=/root

ENTRYPOINT ["/usr/local/bin/tidewaiter"]
CMD ["run"]

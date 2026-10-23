# Built on the *build* platform whatever the target, and cross-compiled to the
# target architecture by Go itself. Emulating an arm toolchain under QEMU to
# produce an arm image takes minutes; this takes seconds.
#
# Pinned to the Go that mise.toml names, rather than floating on a tag that
# would change the compiler under a rebuild with no commit to say so.
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS build

WORKDIR /src

# The modules (godbus, gokit) are fetched in a layer of their own, which the
# source changing does not invalidate.
COPY go.mod go.sum ./
RUN go mod download

COPY main.go ./
COPY internal ./internal

# Computed by ./run from the git history, which is not in the build context -
# so it is passed in rather than worked out here. A build with no --build-arg
# still works and says "development", which is the truth about that build.
ARG VERSION=development

# TARGETVARIANT is "v7" for linux/arm/v7 and empty for the others; GOARM wants
# the bare number, and ignores it on every architecture but arm.
ARG TARGETARCH
ARG TARGETVARIANT
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH GOARM=${TARGETVARIANT#v} \
    go build -trimpath -ldflags "-s -w -X github.com/danielbodart/tidewaiter/internal/version.Version=$VERSION" \
    -o /out/usr/local/bin/tidewaiter .

# Nothing but the binary. A static Go binary needs no libc, and the Bun build's
# libstdc++ went with Bun. conntrack-tools went too: Tidewaiter dumps the
# conntrack table over netlink itself, and asks the kernel for sockets over
# sock_diag where it used to run ss. The only subprocesses it still starts are
# nix-env and nix-store, for the systemd source, which never runs in this
# image. And so no tini either: there are no children to reap, and Tidewaiter
# handles SIGTERM itself.
FROM scratch

# An ARG does not survive into the next stage, so it is declared again. These
# labels are how `docker inspect` answers what a pulled image actually is,
# which matters when every published tag also exists as :latest.
ARG VERSION=development
LABEL org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.title="tidewaiter" \
      org.opencontainers.image.description="Update Docker containers only when idle, health-gate the swap, roll back on failure" \
      org.opencontainers.image.source="https://github.com/danielbodart/tidewaiter" \
      org.opencontainers.image.licenses="Apache-2.0"

# The registry client speaks HTTPS to every registry on the internet.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/ /

# Where ~/.docker/config.json is looked for, for a private registry's
# credentials. The Alpine image's root had /root from /etc/passwd; an empty
# image has no /etc/passwd, and Docker would make HOME "/", so a config
# mounted at /root/.docker/config.json for the old image would quietly stop
# being read.
ENV HOME=/root

ENTRYPOINT ["/usr/local/bin/tidewaiter"]
CMD ["run"]

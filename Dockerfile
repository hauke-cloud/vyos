# syntax=docker/dockerfile:1

# VyOS as a container image.
#
# VyOS publishes no image of its own. build/rootfs.tar.xz is produced by
# `make rootfs` from a signed nightly ISO with upstream's iso-to-oci script,
# which already strips everything a container cannot use (kernel, firmware,
# podman) and masks the systemd units that fight with a container runtime.

# The failover helper is the only thing in the image that is ours. It is a
# static binary, so it does not care what the VyOS userland looks like.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS failover

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY cmd ./cmd
COPY internal ./internal

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-w -s -X main.version=${VERSION}" \
      -o /out/hcloud-vrrp-failover ./cmd/hcloud-vrrp-failover

FROM scratch

ADD build/rootfs.tar.xz /

COPY --from=failover /out/hcloud-vrrp-failover /usr/local/bin/hcloud-vrrp-failover

# Filled in by the CI action (inpacken-un-af-dor-mit).
ARG VERSION=dev
ARG COMMIT=unknown

LABEL org.opencontainers.image.title="vyos" \
  org.opencontainers.image.description="VyOS rolling release as a container image" \
  org.opencontainers.image.source="https://github.com/hauke-cloud/vyos" \
  org.opencontainers.image.version="${VERSION}" \
  org.opencontainers.image.revision="${COMMIT}" \
  org.opencontainers.image.licenses="GPL-2.0-or-later"

# systemd expects SIGRTMIN+3 for an orderly shutdown; SIGTERM is ignored by
# PID 1 and the runtime would fall back to SIGKILL after its timeout.
STOPSIGNAL SIGRTMIN+3

# Same check upstream and containerlab use to decide a VyOS node is up.
HEALTHCHECK --start-period=120s --interval=10s --timeout=5s --retries=3 \
  CMD systemctl is-system-running

CMD ["/sbin/init"]

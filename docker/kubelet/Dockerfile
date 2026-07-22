# syntax=docker/dockerfile:1
#
# Kubernetes-side Incus client image. It is deliberately not an Incus daemon
# runtime: the node-local Quadlet owns incusd, LXC monitors, lxcfs and storage
# tooling. This image only connects to the host Unix API socket.
#
# Build context must be the repository root:
#   docker build -f docker/kubelet/Dockerfile .

ARG ALPINE_VERSION=3.24

FROM alpine:${ALPINE_VERSION} AS build

RUN apk add --no-cache go

WORKDIR /build/incus
COPY . .

# The CLI has no daemon-side C dependencies. Build it statically so the final
# agent image contains no Incus server, LXC, CRIU, Ceph or storage tooling.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/incus ./cmd/incus \
    && test "$(/out/incus --version)" = "7.2"

FROM alpine:${ALPINE_VERSION}

RUN apk add --no-cache ca-certificates

COPY --from=build /out/incus /usr/bin/incus

RUN test "$(incus --version)" = "7.2"

# syntax=docker/dockerfile:1
#
# Container-only ("novm") Incus image built from this repository's source, so
# the resulting incusd carries the openstack-incus patches (shared Ceph pool
# image prefix, zero-copy shared storage migration, cephext driver).
#
# The image is meant to be driven by the OpenStack Nova compute driver over the
# Incus API, so no web UI is included.
#
# Build context must be the repository root:
#   docker build -f docker/alpine-novm/Dockerfile .

ARG ALPINE_VERSION=edge

FROM alpine:${ALPINE_VERSION} AS incus-build

RUN apk add --no-cache \
      acl-dev \
      cowsql-dev \
      eudev-dev \
      gcc \
      gettext-dev \
      go \
      libcap-dev \
      libuv-dev \
      linux-headers \
      lxc-dev \
      make \
      musl-dev \
      pkgconf \
      raft-dev \
      sqlite-dev

# musl ships gettext as a separate library, hence the explicit -lintl.
ENV CGO_ENABLED=1 \
    CGO_LDFLAGS="-lintl" \
    CGO_LDFLAGS_ALLOW="(-Wl,-wrap,pthread_create)|(-Wl,-z,now)"

WORKDIR /build/incus
COPY . .
RUN --mount=type=cache,target=/root/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -o /out/incusd ./cmd/incusd \
    && go build -trimpath -o /out/incus ./cmd/incus

FROM alpine:${ALPINE_VERSION}

# Newest Ceph packaged by Alpine edge. The ceph/rbd CLI is wire-compatible with
# newer clusters (e.g. a Squid 19 client against a Tentacle 20 cluster) and RBD
# mapping is done by the host kernel anyway; bump this once aports ships ceph20.
ARG CEPH_VERSION=19

RUN apk add --no-cache \
      acl \
      apparmor \
      apparmor-profiles-abstractions \
      apparmor-profiles-tunables \
      apparmor-utils \
      attr \
      btrfs-progs \
      ceph${CEPH_VERSION}-common \
      ceph${CEPH_VERSION}-mount \
      dnsmasq \
      e2fsprogs \
      e2fsprogs-extra \
      incus-feature \
      incus-feature-client \
      iproute2 \
      iptables \
      lvm2 \
      lxcfs \
      nftables \
      rsync \
      shadow-subids \
      squashfs-tools \
      thin-provisioning-tools \
      util-linux-misc \
      xz \
    && printf '%s\n' 'root:1000000:1000000000' > /etc/subuid \
    && printf '%s\n' 'root:1000000:1000000000' > /etc/subgid \
    && mkdir -p /var/lib/incus-lxcfs /var/log/incus

# Replace the packaged binaries with the ones built from this repository, the
# Alpine package is only kept for its runtime dependencies and support files.
COPY --from=incus-build /out/incusd /out/incus /tmp/incus-build/
RUN install -m0755 /tmp/incus-build/incusd "$(command -v incusd)" \
    && install -m0755 /tmp/incus-build/incus "$(command -v incus)" \
    && rm -rf /tmp/incus-build

COPY docker/alpine-novm/start.sh docker/alpine-novm/healthcheck.sh /usr/local/sbin/
RUN chmod 0755 /usr/local/sbin/start.sh /usr/local/sbin/healthcheck.sh

VOLUME ["/var/lib/incus"]
STOPSIGNAL SIGPWR
HEALTHCHECK --interval=30s --timeout=10s --start-period=30s --retries=3 \
  CMD ["/usr/local/sbin/healthcheck.sh"]

ENTRYPOINT ["/usr/local/sbin/start.sh"]

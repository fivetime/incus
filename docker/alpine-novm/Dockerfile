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
    go build -trimpath -ldflags "-s -w" -o /out/incusd ./cmd/incusd \
    && go build -trimpath -ldflags "-s -w" -o /out/incus ./cmd/incus

FROM alpine:${ALPINE_VERSION}

# Newest Ceph packaged by Alpine edge. The ceph/rbd CLI is wire-compatible with
# newer clusters (e.g. a Squid 19 client against a Tentacle 20 cluster) and RBD
# mapping is done by the host kernel anyway; bump this once aports ships ceph20.
ARG CEPH_VERSION=19

# Kept minimal for the Nova/ceph-RBD scenario: container instances only, RBD
# storage only (no btrfs/lvm/cephfs tooling), nftables only (no iptables) and
# no apparmor-utils (incusd only execs apparmor_parser, shipped in apparmor).
# The incusd runtime libraries are pulled in through their soname (so:...)
# instead of installing the Alpine incus package, whose binaries would only
# get shadowed by the ones built from this repository.
RUN apk add --no-cache \
      acl \
      apparmor \
      apparmor-profiles-abstractions \
      apparmor-profiles-tunables \
      attr \
      ceph${CEPH_VERSION}-common \
      dnsmasq \
      e2fsprogs \
      e2fsprogs-extra \
      iproute2 \
      lxcfs \
      nftables \
      rsync \
      shadow-subids \
      util-linux-misc \
      xz \
      so:libacl.so.1 \
      so:libcap.so.2 \
      so:libcowsql.so.0 \
      so:libgcc_s.so.1 \
      so:libintl.so.8 \
      so:liblxc.so.1 \
      so:libraft.so.0 \
      so:libsqlite3.so.0 \
      so:libudev.so.1 \
      so:libuv.so.1 \
    && printf '%s\n' 'root:1000000:1000000000' > /etc/subuid \
    && printf '%s\n' 'root:1000000:1000000000' > /etc/subgid \
    && mkdir -p /var/lib/incus-lxcfs /var/log/incus

COPY --from=incus-build /out/incusd /usr/sbin/incusd
COPY --from=incus-build /out/incus /usr/bin/incus

COPY docker/alpine-novm/start.sh docker/alpine-novm/healthcheck.sh /usr/local/sbin/
RUN chmod 0755 /usr/local/sbin/start.sh /usr/local/sbin/healthcheck.sh

VOLUME ["/var/lib/incus"]
STOPSIGNAL SIGPWR
HEALTHCHECK --interval=30s --timeout=10s --start-period=30s --retries=3 \
  CMD ["/usr/local/sbin/healthcheck.sh"]

ENTRYPOINT ["/usr/local/sbin/start.sh"]

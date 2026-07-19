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

# Pinned to a stable release rather than edge: edge is a rolling repository
# where packages get renamed or dropped without notice (e.g. ceph20 -> ceph19).
# Bump deliberately when moving to a newer Alpine release.
ARG ALPINE_VERSION=3.24

FROM alpine:${ALPINE_VERSION} AS incus-build

RUN apk add --no-cache \
      acl-dev \
      cowsql-dev \
      eudev-dev \
      gcc \
      gettext-dev \
      go \
      libapparmor-dev \
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

# Build the minimal aa-exec replacement (see aa-exec.c). Incus needs aa-exec to
# unpack images under a restricted AppArmor profile, and Alpine only ships it in
# the heavyweight apparmor-utils package (python3/perl/bash), which this image
# omits. Compiled here to reuse the toolchain already present in this stage.
RUN cc -O2 -Wall -Wextra docker/alpine-novm/aa-exec.c -lapparmor -o /out/aa-exec \
    && strip /out/aa-exec

FROM alpine:${ALPINE_VERSION} AS criu-build

ARG CRIU_COMMIT=b7bdc14736b1e8598f2ffbb371f42318835a111f

RUN apk add --no-cache \
      bash \
      build-base \
      coreutils \
      e2fsprogs \
      elfutils-dev \
      git \
      gnutls-dev \
      iproute2 \
      libaio-dev \
      libbsd-dev \
      libcap-dev \
      libnet-dev \
      libnl3-dev \
      lz4-dev \
      nftables-dev \
      perl \
      pkgconfig \
      protobuf-c-compiler \
      protobuf-c-dev \
      protobuf-dev \
      python3 \
      util-linux-dev \
    && git clone --filter=blob:none https://github.com/checkpoint-restore/criu.git /build/criu \
    && git -C /build/criu checkout --detach "${CRIU_COMMIT}" \
    && make -C /build/criu -j"$(nproc)" criu WERROR=0 \
    && /build/criu/criu/criu --version

FROM alpine:${ALPINE_VERSION} AS lxc-build

ARG LXC_COMMIT=e4415af07e3ec291205bc828e2b4e8141f91a499

RUN apk add --no-cache \
      build-base \
      dbus-dev \
      git \
      libapparmor-dev \
      libcap-dev \
      libseccomp-dev \
      linux-headers \
      meson \
      openssl-dev \
      samurai \
      util-linux-dev \
    && git clone --filter=blob:none https://github.com/lxc/lxc.git /build/lxc \
    && git -C /build/lxc checkout --detach "${LXC_COMMIT}"

COPY docker/alpine-novm/lxc-restore-finalize-cgroups.patch /tmp/lxc-restore-finalize-cgroups.patch
RUN git -C /build/lxc apply /tmp/lxc-restore-finalize-cgroups.patch \
    && meson setup /build/lxc/build /build/lxc \
      --prefix=/usr \
      -Dtests=false \
      -Dexamples=false \
      -Dman=false \
      -Dinit-script=openrc \
      -Ddistrosysconfdir=/etc \
      -Dselinux=false \
    && ninja -C /build/lxc/build liblxc.so.1.9.0 \
    && ldd /build/lxc/build/liblxc.so.1.9.0 \
    && ! ldd /build/lxc/build/liblxc.so.1.9.0 | grep -q "not found" \
    && strings /build/lxc/build/liblxc.so.1.9.0 | grep -q "apparmor"

FROM alpine:${ALPINE_VERSION}

# Newest Ceph packaged by Alpine edge. The ceph/rbd CLI is wire-compatible with
# newer clusters (e.g. a Squid 19 client against a Tentacle 20 cluster) and RBD
# mapping is done by the host kernel anyway; bump this once aports ships ceph20.
ARG CEPH_VERSION=19

# Kept minimal for the Nova/ceph-RBD scenario: container instances only, RBD
# storage only (no btrfs/lvm/cephfs tooling) and no apparmor-utils. nftables
# is Incus' firewall backend, while CRIU also requires iptables-restore and
# ip6tables-restore to lock a container network during stateful migration.
# CRIU probes legacy restore binaries when restoring some checkpoints, so both
# backends are installed rather than relying on backend-specific symlinks.
# incusd execs apparmor_parser (from apparmor) and aa-exec;
# the latter only ships in apparmor-utils (python3/perl/bash), so a minimal
# libapparmor-based aa-exec is built in the incus-build stage and copied in
# below. incusd also refuses to start without ip, rsync, setfattr, tar (GNU),
# unsquashfs and xz (see cmd/incusd/main_daemon.go), so those must always stay.
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
      gnutls \
      iproute2 \
      iptables \
      iptables-legacy \
      libbsd \
      libnet \
      libnl3 \
      lxc-libs \
      lxcfs \
      lz4-libs \
      nftables \
      protobuf-c \
      rsync \
      shadow-subids \
      squashfs-tools \
      tar \
      util-linux-misc \
      xz \
      so:libacl.so.1 \
      so:libapparmor.so.1 \
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
    && mkdir -p /etc/criu /var/lib/incus-lxcfs /var/log/incus \
    && printf '%s\n' 'enable-external-masters' > /etc/criu/default.conf

COPY --from=incus-build /out/incusd /usr/sbin/incusd
COPY --from=incus-build /out/incus /usr/bin/incus
COPY --from=incus-build /out/aa-exec /usr/local/bin/aa-exec
COPY --from=criu-build /build/criu/criu/criu /usr/local/sbin/criu
COPY --from=lxc-build /build/lxc/build/liblxc.so.1.9.0 /usr/lib/liblxc.so.1.9.0
RUN test "$(incusd --version)" = "7.2" \
    && criu --version \
    && tar --help 2>&1 | grep -q -- "--no-unquote" \
    && ! ldd /usr/sbin/incusd | grep -q "not found" \
    && ! ldd /usr/lib/liblxc.so.1.9.0 | grep -q "not found" \
    && strings /usr/lib/liblxc.so.1.9.0 | grep -q "apparmor" \
    && ! ldd /usr/local/sbin/criu | grep -q "not found"

COPY docker/alpine-novm/start.sh docker/alpine-novm/healthcheck.sh /usr/local/sbin/
RUN chmod 0755 /usr/local/sbin/start.sh /usr/local/sbin/healthcheck.sh

VOLUME ["/var/lib/incus"]
STOPSIGNAL SIGPWR
HEALTHCHECK --interval=30s --timeout=10s --start-period=30s --retries=3 \
  CMD ["/usr/local/sbin/healthcheck.sh"]

ENTRYPOINT ["/usr/local/sbin/start.sh"]

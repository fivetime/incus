#!/bin/sh
set -eu

fail() {
  echo "FATAL: $*" >&2
  exit 1
}

mount_is_shared() {
  awk -v path="$1" '
    $5 == path {
      for (i = 7; i <= NF && $i != "-"; i++) {
        if ($i ~ /^shared:/)
          found = 1
      }
    }
    END { exit !found }' /proc/self/mountinfo
}

mount_fstype_matches() {
  awk -v path="$1" -v pattern="$2" '
    $5 == path {
      for (i = 6; i <= NF; i++) {
        if ($i == "-") {
          if ($(i + 1) ~ pattern)
            found = 1
          break
        }
      }
    }
    END { exit !found }' /proc/self/mountinfo
}

[ "$(id -u)" -eq 0 ] || fail "LXCFS must run as root"
[ "${INCUS_RUNTIME_ROLE:-}" = "lxcfs" ] || fail "INCUS_RUNTIME_ROLE must be lxcfs"
command -v lxcfs >/dev/null 2>&1 || fail "Required command not found: lxcfs"
command -v mkdir >/dev/null 2>&1 || fail "Required command not found: mkdir"
command -v umount >/dev/null 2>&1 || fail "Required command not found: umount"

[ -c /dev/fuse ] || fail "/dev/fuse is required"
mountpoint -q /var/lib/lxcfs \
  || fail "/var/lib/lxcfs must be a host bind mount"
mount_is_shared /var/lib/lxcfs \
  || fail "/var/lib/lxcfs must use shared mount propagation"
awk '$2 == "/sys/fs/cgroup" && $3 == "cgroup2" { found = 1 } END { exit !found }' /proc/mounts \
  || fail "A cgroup v2 host mount is required"

# A crashed LXCFS leaves an unusable FUSE mount. It cannot be reconnected for
# existing guests, but detaching it lets this service support future starts.
if mount_fstype_matches /var/lib/lxcfs '^fuse(\.lxcfs)?$'; then
  if timeout 5 head -n 1 /var/lib/lxcfs/proc/meminfo >/dev/null 2>&1; then
    fail "A live LXCFS mount already owns /var/lib/lxcfs"
  fi

  umount -l /var/lib/lxcfs \
    || fail "Failed to detach the stale LXCFS mount"
fi

mkdir -p /run/lxcfs
chmod 0700 /run/lxcfs
exec lxcfs \
  -f \
  -p /run/lxcfs/lxcfs.pid \
  --runtime-dir /run/lxcfs \
  --enable-loadavg \
  --enable-cfs \
  /var/lib/lxcfs

#!/bin/sh
set -eu

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

check_lxcfs_mount() {
  mountpoint -q /var/lib/lxcfs
  mount_fstype_matches /var/lib/lxcfs '^fuse(\.lxcfs)?$'
  timeout 5 head -n 1 /var/lib/lxcfs/proc/meminfo >/dev/null
}

case "${INCUS_RUNTIME_ROLE:-incusd}" in
  incusd)
    incus admin waitready --timeout=5 >/dev/null
    incus info | grep -q 'driver: lxc'
    criu --version >/dev/null
    command -v iptables-restore >/dev/null
    command -v ip6tables-restore >/dev/null
    command -v iptables-legacy-restore >/dev/null
    command -v ip6tables-legacy-restore >/dev/null
    mountpoint -q /run/incus
    awk '$5 == "/var/lib/incus" {
           for (i = 7; i <= NF && $i != "-"; i++)
             if ($i ~ /^shared:/) found = 1
         }
         END { exit !found }' /proc/self/mountinfo
    test -d /sys/kernel/security/apparmor
    awk '$2 == "/sys/fs/cgroup" && $3 == "cgroup2" { found = 1 } END { exit !found }' /proc/mounts
    check_lxcfs_mount
    ;;
  lxcfs)
    test -s /run/lxcfs/lxcfs.pid
    kill -0 "$(cat /run/lxcfs/lxcfs.pid)"
    check_lxcfs_mount
    ;;
  *)
    exit 1
    ;;
esac

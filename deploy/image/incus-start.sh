#!/bin/sh
set -eu

fail() {
  echo "FATAL: $*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "Required command not found: $1"
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

[ "$(id -u)" -eq 0 ] || fail "Incus must run as root"

case "${INCUS_RUNTIME_ROLE:-incusd}" in
  incusd) ;;
  lxcfs)
    exec /usr/local/sbin/lxcfs-start.sh
    ;;
  *)
    fail "INCUS_RUNTIME_ROLE must be either incusd or lxcfs"
    ;;
esac

for command_name in cgcreate cgexec nsenter readlink; do
  require_command "$command_name"
done

[ "$(cat /proc/1/comm 2>/dev/null)" = "systemd" ] \
  || fail "incusd requires host PID visibility with the host systemd process at PID 1"

# Kubernetes normally places a privileged Pod in its own cgroup and UTS
# namespaces. Join the host cgroup namespace for monitor survival and the host
# UTS namespace for a stable Incus node name. The Pod keeps its mount namespace.
if [ "$(readlink /proc/self/ns/cgroup)" != "$(readlink /proc/1/ns/cgroup)" ] \
  || [ "$(readlink /proc/self/ns/uts)" != "$(readlink /proc/1/ns/uts)" ]; then
  [ "${INCUS_ENTERED_HOST_NAMESPACES:-0}" != "1" ] \
    || fail "Failed to enter the host cgroup and UTS namespaces"
  exec nsenter --target 1 --cgroup --uts -- \
    env INCUS_ENTERED_HOST_NAMESPACES=1 "$0" "$@"
fi

mkdir -p /run/incus
mountpoint -q /run/incus \
  || fail "/run/incus must be a host bind mount so running instances survive outer container restarts"
awk '$5 == "/var/lib/incus" {
       for (i = 7; i <= NF && $i != "-"; i++)
         if ($i ~ /^shared:/) found = 1
     }
     END { exit !found }' /proc/self/mountinfo \
  || fail "/var/lib/incus must use recursive shared mount propagation for system-container disk mounts"
awk '$2 == "/sys/fs/cgroup" && $3 == "cgroup2" { found = 1 } END { exit !found }' /proc/mounts \
  || fail "A cgroup v2 host mount is required"
[ -w /sys/fs/cgroup ] || fail "/sys/fs/cgroup must expose the writable host cgroup v2 hierarchy"
[ -r /proc/sys/kernel/seccomp/actions_avail ] || fail "Kernel seccomp support is required"
grep -qw allow /proc/sys/kernel/seccomp/actions_avail || fail "Kernel seccomp filtering is unavailable"
[ -r /sys/module/apparmor/parameters/enabled ] || fail "Host AppArmor is required"
[ "$(cat /sys/module/apparmor/parameters/enabled)" = "Y" ] || fail "Host AppArmor is disabled"
[ -d /sys/kernel/security/apparmor ] || fail "AppArmor securityfs is unavailable; mount /sys/kernel/security"
[ -w /sys/kernel/security/apparmor/.load ] || fail "AppArmor policy loading is unavailable"

mountpoint -q /var/lib/lxcfs \
  || fail "/var/lib/lxcfs must be the stable host bind shared with incus-lxcfs"
mount_fstype_matches /var/lib/lxcfs '^fuse(\.lxcfs)?$' \
  || fail "/var/lib/lxcfs is not backed by the incus-lxcfs FUSE mount"
timeout 5 head -n 1 /var/lib/lxcfs/proc/meminfo >/dev/null \
  || fail "The incus-lxcfs FUSE mount is not responding"

CURRENT_PROFILE=$(cat /proc/self/attr/current 2>/dev/null || true)
case "$CURRENT_PROFILE" in
  unconfined*|*"(unconfined)"*) ;;
  *) fail "The outer runtime must use the unconfined AppArmor profile" ;;
esac

for command_name in aa-exec apparmor_parser criu incus incusd ip6tables-legacy-restore ip6tables-restore iptables-legacy-restore iptables-restore newgidmap newuidmap nft; do
  require_command "$command_name"
done

[ -r /etc/criu/default.conf ] || fail "CRIU default configuration is missing"
[ "$(sed -n '/^[[:space:]]*#/d; /^[[:space:]]*$/d; p' /etc/criu/default.conf)" = "enable-external-masters" ] \
  || fail "CRIU must enable external masters for stateful migration of shared mounts"

grep -q '^root:[0-9][0-9]*:[0-9][0-9]*$' /etc/subuid || fail "A root subordinate UID range is required"
grep -q '^root:[0-9][0-9]*:[0-9][0-9]*$' /etc/subgid || fail "A root subordinate GID range is required"

mkdir -p /usr/lib/lxc/rootfs /var/log/incus

CONTROL_CGROUP=${INCUS_CONTROL_CGROUP:-/osh-incus}
case "$CONTROL_CGROUP" in
  /*) ;;
  *)
    fail "INCUS_CONTROL_CGROUP must be a non-root absolute cgroup path"
    ;;
esac
case "$CONTROL_CGROUP" in
  /|*..*|*[!A-Za-z0-9_./-]*)
    fail "INCUS_CONTROL_CGROUP must be a non-root absolute cgroup path"
    ;;
esac

CGROUP_CONTROLLERS=$(tr ' ' ',' < /sys/fs/cgroup/cgroup.controllers)
[ -n "$CGROUP_CONTROLLERS" ] || fail "The host cgroup v2 hierarchy has no controllers"
if [ ! -d "/sys/fs/cgroup${CONTROL_CGROUP}" ]; then
  cgcreate -g "${CGROUP_CONTROLLERS}:${CONTROL_CGROUP}" \
    || [ -d "/sys/fs/cgroup${CONTROL_CGROUP}" ] \
    || fail "Unable to create the host control cgroup ${CONTROL_CGROUP}"
fi

DAEMON_ARGS=""
if [ -n "${INCUS_SOCKET_GID:-}" ]; then
  case "${INCUS_SOCKET_GID}" in
    *[!0-9]*|'') fail "INCUS_SOCKET_GID must be a numeric group ID" ;;
  esac
  addgroup -g "${INCUS_SOCKET_GID}" incus-admin
  DAEMON_ARGS="--group incus-admin"
fi

# --sticky keeps child LXC monitor processes in the stable host cgroup instead
# of moving them back under the Kubernetes Pod cgroup.
# shellcheck disable=SC2086
exec cgexec --sticky -g "${CGROUP_CONTROLLERS}:${CONTROL_CGROUP}" incusd ${DAEMON_ARGS}

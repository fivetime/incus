#!/bin/sh
set -eu

fail() {
  echo "FATAL: $*" >&2
  exit 1
}

for command_name in awk head mountpoint podman systemctl timeout; do
  command -v "$command_name" >/dev/null 2>&1 || fail "Required command not found: $command_name"
done

systemctl is-active --quiet incus-lxcfs.service \
  || fail "incus-lxcfs.service is not active"
if systemctl is-active --quiet lxcfs.service; then
  fail "The distribution lxcfs.service must not run alongside incus-lxcfs.service"
fi

[ "$(podman inspect --format '{{.State.Running}}' incus-lxcfs 2>/dev/null)" = "true" ] \
  || fail "The incus-lxcfs Podman container is not running"
[ "$(podman inspect --format '{{range .Config.Env}}{{println .}}{{end}}' incus-lxcfs | awk -F= '$1 == "INCUS_RUNTIME_ROLE" { print $2 }')" = "lxcfs" ] \
  || fail "The incus-lxcfs container does not use INCUS_RUNTIME_ROLE=lxcfs"

mountpoint -q /var/lib/lxcfs || fail "/var/lib/lxcfs is not mounted on the host"
timeout 5 head -n 1 /var/lib/lxcfs/proc/meminfo >/dev/null \
  || fail "The host LXCFS mount is not responding"
podman exec incus-lxcfs /usr/local/sbin/healthcheck.sh \
  || fail "The incus-lxcfs image health check failed"

printf 'incus-lxcfs PID: %s\n' "$(podman inspect --format '{{.State.Pid}}' incus-lxcfs)"
echo "Incus LXCFS host service is healthy"

#!/bin/sh
# Usage: validate-runtime.sh; it takes no arguments and validates both Quadlets.
set -eu

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

[ "$(id -u)" -eq 0 ] || fail "Run this validator as root"

for command_name in grep head mountpoint podman systemctl timeout; do
  command -v "$command_name" >/dev/null 2>&1 \
    || fail "Required command not found: $command_name"
done

systemctl is-active --quiet incus-lxcfs.service \
  || fail "incus-lxcfs.service is not active"
systemctl is-active --quiet incus-podman.service \
  || fail "incus-podman.service is not active"

systemctl cat incus-podman.service \
  | grep -q '^RuntimeDirectoryPreserve=restart$' \
  || fail "incus-podman.service does not preserve /run/incus across restarts"

for container_name in incus-lxcfs incus; do
  podman container exists "$container_name" \
    || fail "Podman container does not exist: $container_name"
done

podman inspect --format '{{range .Config.Env}}{{println .}}{{end}}' incus-lxcfs \
  | grep -qx 'INCUS_RUNTIME_ROLE=lxcfs' \
  || fail "incus-lxcfs has the wrong runtime role"
podman inspect --format '{{range .Config.Env}}{{println .}}{{end}}' incus \
  | grep -qx 'INCUS_RUNTIME_ROLE=incusd' \
  || fail "incus has the wrong runtime role"

mountpoint -q /var/lib/lxcfs \
  || fail "/var/lib/lxcfs is not mounted on the host"
timeout 5 head -n 1 /var/lib/lxcfs/proc/meminfo >/dev/null \
  || fail "The host LXCFS mount is not responding"

podman exec incus-lxcfs /usr/local/sbin/healthcheck.sh \
  || fail "The LXCFS data-plane health check failed"
podman exec incus /usr/local/sbin/healthcheck.sh \
  || fail "The Incus control-plane health check failed"

test -d /run/incus-podman \
  || fail "The persistent Incus runtime directory is missing"

podman exec incus /usr/bin/incus list \
  --all-projects \
  --format csv \
  -c enp \
  | while IFS=, read -r project_name instance_name instance_pid; do
      [ -n "$instance_pid" ] || continue

      timeout 10 podman exec incus /usr/bin/incus exec \
        --project "$project_name" \
        "$instance_name" \
        -- /bin/sh -c 'IFS= read -r line < /proc/meminfo && test -n "$line"' \
        || fail "Guest LXCFS mount failed: ${project_name}/${instance_name}"
    done

incusd_pid=$(podman inspect --format '{{.State.Pid}}' incus)
lxcfs_pid=$(podman inspect --format '{{.State.Pid}}' incus-lxcfs)
echo "incusd host PID: ${incusd_pid}"
echo "lxcfs host PID: ${lxcfs_pid}"
echo "Incus control plane and LXCFS data plane are healthy"

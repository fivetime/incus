#!/bin/bash
set -euo pipefail

image=${1:?Usage: validate-lxcfs-cpu.sh IMAGE}
runtime=${CONTAINER_RUNTIME:-docker}
mount_dir=$(mktemp -d)
server="lxcfs-cpu-server-$$"
sudo_cmd=()

if [ "$(id -u)" -ne 0 ]; then
  sudo_cmd=(sudo)
fi

cleanup() {
  "${runtime}" rm -f "${server}" >/dev/null 2>&1 || true
  if mountpoint -q "${mount_dir}"; then
    "${sudo_cmd[@]}" umount -l "${mount_dir}" || true
  fi
  rmdir "${mount_dir}" 2>/dev/null || true
}
trap cleanup EXIT

"${sudo_cmd[@]}" mount --bind "${mount_dir}" "${mount_dir}"
"${sudo_cmd[@]}" mount --make-rshared "${mount_dir}"

"${runtime}" run --detach --name "${server}" \
  --privileged \
  --pid=host \
  --cgroupns=host \
  --env INCUS_RUNTIME_ROLE=lxcfs \
  --volume /dev/fuse:/dev/fuse \
  --volume "${mount_dir}:/var/lib/lxcfs:rshared" \
  "${image}" >/dev/null

for _ in {1..10}; do
  if awk -v path="${mount_dir}" \
    '$2 == path && $3 == "fuse.lxcfs" { found = 1 } END { exit !found }' \
    /proc/mounts; then
    break
  fi
  sleep 1
done

awk -v path="${mount_dir}" \
  '$2 == path && $3 == "fuse.lxcfs" { found = 1 } END { exit !found }' \
  /proc/mounts

timeout 30 "${runtime}" run --rm \
  --privileged \
  --cpuset-cpus=0 \
  --volume "${mount_dir}:/lxcfs:ro,rslave" \
  --entrypoint sh \
  "${image}" -ec '
    mount --bind /lxcfs/sys/devices/system/cpu /sys/devices/system/cpu
    test "$(cat /sys/devices/system/cpu/online)" = 0
    test "$(cat /sys/devices/system/cpu/possible)" = 0
    test "$(cat /sys/devices/system/cpu/present)" = 0
    test -z "$(cat /sys/devices/system/cpu/offline)"
    test "$(getconf _NPROCESSORS_ONLN)" = 1
    test "$(getconf _NPROCESSORS_CONF)" = 1
    test "$(nproc)" = 1
    test "$(nproc --all)" = 1
  '

# Incus LXCFS Quadlet

This directory owns only the long-lived host LXCFS data plane. Kubernetes
owns `incusd` through the OpenStack Helm Incus chart. Both use the same `novm`
image, but their image digests and maintenance windows are independent.

## Node prerequisites

Provision the host mount before enabling the unit:

```sh
install -d -m 0711 /var/lib/lxcfs
findmnt -no TARGET,PROPAGATION /var/lib/lxcfs
```

`/var/lib/lxcfs` must be shared or recursively shared. If its containing mount
is private, node provisioning must create a persistent bind mount and mark it
recursively shared. Do not stack an unconditional bind mount in
`ExecStartPre`.

This Podman service must be the only LXCFS owner. Disable and mask a
distribution service before the first start:

```sh
systemctl disable --now lxcfs.service
systemctl mask lxcfs.service
```

Install the Quadlet and validator:

```sh
install -Dm0644 incus-lxcfs.container \
  /etc/containers/systemd/incus-lxcfs.container
install -Dm0755 validate-runtime.sh \
  /usr/local/sbin/incus-lxcfs-validate
systemctl daemon-reload
systemctl enable --now incus-lxcfs.service
/usr/local/sbin/incus-lxcfs-validate
```

Node provisioning must separately create `/var/lib/incus`, `/run/incus`, and
`/var/log/incus`. The Kubernetes `incusd` DaemonSet mounts those host paths;
this Quadlet neither starts `incusd` nor reads the Incus database or socket.

## Lifecycle boundary

Do not restart `incus-lxcfs.service` during an `incusd` rollout. Existing
system containers retain references to the old FUSE superblock and cannot
reattach it to a replacement LXCFS process. Drain or migrate every instance
before an LXCFS image update or restart.

`Restart=always` restores LXCFS for newly started containers after an
unexpected failure, but it cannot repair the `/proc` and `/sys` overlays of
containers that used the failed process. Treat an LXCFS PID change as a node
incident and restart or migrate every affected guest.

The Kubernetes chart owns the independent `incusd` control-plane rollout.
Because LXC monitors, `/run/incus`, `/var/lib/incus`, and LXCFS remain on the
host, replacing only the `incusd` Pod does not stop running system containers.

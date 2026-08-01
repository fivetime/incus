# Incus Quadlet runtime

The runtime image has two roles:

- `incus-lxcfs.service` is the long-lived LXCFS data plane.
- `incus-podman.service` is the replaceable `incusd` control plane.

Both services use the same image so their LXC and LXCFS dependencies remain
compatible, but production deployments must pin each Quadlet to an explicitly
tested image digest. Updating the `incus-podman.container` digest does not require
updating or restarting `incus-lxcfs.container`.

## Node prerequisites

The node operator must provide these host paths before enabling the units:

```sh
install -d -m 0711 /var/lib/lxcfs
install -d -m 0700 /var/lib/incus /var/log/incus
```

`/var/lib/lxcfs` must be on a shared or recursively shared mount. Verify it
with:

```sh
findmnt -no TARGET,PROPAGATION /var/lib/lxcfs
```

If the containing host mount is private, node provisioning must create a
persistent bind mount and mark it recursively shared. Do not add an
unconditional `mount --bind` to an `ExecStartPre`; each restart would stack
another bind mount.

Install the units and validator:

```sh
install -Dm0644 incus-lxcfs.container \
  /etc/containers/systemd/incus-lxcfs.container
install -Dm0644 incus-podman.container \
  /etc/containers/systemd/incus-podman.container
install -Dm0755 validate-runtime.sh \
  /usr/local/sbin/incus-quadlet-validate
systemctl daemon-reload
systemctl enable --now incus-lxcfs.service incus-podman.service
/usr/local/sbin/incus-quadlet-validate
```

`RuntimeDirectoryPreserve=restart` is mandatory on `incus-podman.service`. LXC
monitors refer to files below `/run/incus`, including generated `lxc.conf`
files, after `incusd` has exited.

## One-time migration from the combined service

The old image ran LXCFS as a child of the `incus` container. Existing guests
hold references to that FUSE superblock, so it cannot be transferred to the
new service. Schedule one maintenance window:

1. Disable scheduling and drain or cleanly stop every Nova instance on the
   node. Record the instances that must be restarted.
2. Stop the old `incus-podman.service` and verify that no LXC monitor remains.
3. Provision the shared `/var/lib/lxcfs` host mount.
4. Install both new Quadlets while the old service is stopped.
5. Start `incus-lxcfs.service`, then `incus-podman.service`.
6. Run `incus-quadlet-validate`.
7. Restart the recorded instances and re-enable scheduling.

Do not try to make this first transition by sending SIGTERM to the combined
container. That preserves LXC monitors but kills their only LXCFS process,
leaving guest `/proc` mounts with `Socket not connected`.

## Control-plane updates

`SIGTERM` is the Incus reload signal. It closes `incusd` without stopping
running LXC instances. After changing only the `incus-podman.container` image digest:

```sh
systemctl daemon-reload
systemctl restart incus-podman.service
/usr/local/sbin/incus-quadlet-validate
```

Record guest init PIDs before the restart and verify that they are unchanged
afterwards. Also read `/proc/meminfo` in at least one guest; a successful API
health check alone cannot detect a disconnected LXCFS mount. The supplied
validator reads `/proc/meminfo` through every running system container and
reports both outer service PIDs so repeated rollout checks can compare them.

Never restart `incus-lxcfs.service` as part of an `incusd` rollout. An LXCFS
restart requires the same drain-and-restart maintenance procedure as the
one-time migration because existing FUSE mounts cannot reconnect.

`Restart=always` restarts the LXCFS service after a crash so newly started
containers can work, but it cannot repair the old FUSE superblocks already
held by running guests. Treat any LXCFS restart as a node incident and restart
or migrate every affected guest.

For a full node shutdown, cleanly stop instances through Nova or run an
operator-controlled Incus shutdown before stopping LXCFS. `SIGPWR` is the
Incus full-shutdown signal; the Quadlet deliberately uses `SIGTERM` so a
routine control-plane restart does not stop tenant instances.

# System call interception

Incus supports intercepting some specific system calls from unprivileged
containers. If they're considered to be safe, it executes them with
elevated privileges on the host.

Doing so comes with a performance impact for the syscall in question and
will cause some work for Incus to evaluate the request and if allowed,
process it with elevated privileges.

Enabling of specific system call interception options is done on a
per-container basis through container configuration options.

## Available system calls

### `mknod` / `mknodat`

The `mknod` and `mknodat` system calls can be used to create a variety of special files.

Most commonly inside containers, they may be called to create block or character devices.
Creating such devices isn't allowed in unprivileged containers as this
is a very easy way to escalate privileges by allowing direct write
access to resources like disks or memory.

But there are files which are safe to create. For those, intercepting
this syscall may unblock some specific workloads and allow them to run
inside an unprivileged containers.

The devices which are currently allowed are:

- overlayfs whiteout (char 0:0)
- `/dev/console` (char 5:1)
- `/dev/full` (char 1:7)
- `/dev/null` (char 1:3)
- `/dev/random` (char 1:8)
- `/dev/tty` (char 5:0)
- `/dev/urandom` (char 1:9)
- `/dev/zero` (char 1:5)

All file types other than character devices are currently sent to the
kernel as usual, so enabling this feature doesn't change their behavior
at all.

This can be enabled by setting `security.syscalls.intercept.mknod` to `true`.

### `bpf`

The `bpf` system call is used to manage eBPF programs in the kernel.
Those can be attached to a variety of kernel subsystems.

In general, loading of eBPF programs that are not trusted can be problematic as it
can facilitate timing based attacks.

Incus' eBPF support is currently restricted to programs managing devices
cgroup entries. To enable it, you need to set both
`security.syscalls.intercept.bpf` and
`security.syscalls.intercept.bpf.devices` to true.

### `mount`

The `mount` system call allows for mounting both physical and virtual file systems.
By default, unprivileged containers are restricted by the kernel to just
a handful of virtual and network file systems.

To allow mounting physical file systems, system call interception can be used.
Incus offers a variety of options to handle this.

`security.syscalls.intercept.mount` is used to control the entire
feature and needs to be turned on for any of the other options to work.

`security.syscalls.intercept.mount.allowed` allows specifying a list of
file systems which can be directly mounted in the container. This is the
most dangerous option as it allows the user to feed data that is not trusted at
the kernel. This can easily be used to crash the host system or to
attack it. It should only ever be used in trusted environments.

`security.syscalls.intercept.mount.shift` can be set on top of that so
the resulting mount is shifted to the UID/GID map used by the container.
This is needed to avoid everything showing up as `nobody`/`nogroup` inside
of unprivileged containers.

The much safer alternative to those is
`security.syscalls.intercept.mount.fuse` which can be set to pairs of
file-system name and FUSE handler. When this is set, an attempt at
mounting one of the configured file systems will be transparently
redirected to instead calling the FUSE equivalent of that file system.

As this is all running as the caller, it avoids the entire issue around
the kernel attack surface and so is generally considered to be safe,
though you should keep in mind that any kind of system call interception
makes for an easy way to overload the host system.

A FUSE mount created this way is a plain `fuse` mount, and unmounting it
returns before the FUSE implementation has written its final changes:
the kernel only waits for the `DESTROY` reply of a `fuseblk` mount, which
an unprivileged container cannot create. When the file system lives on a
block device that belongs to one of the instance's own `unix-block`
devices, `security.syscalls.intercept.mount.fuseblk` makes Incus create
the superblock on the host as `fuseblk` instead, hand the `/dev/fuse`
descriptor to the FUSE implementation started inside the instance as
`/dev/fd/N`, map the mount to the instance's ID map and attach it at the
requested target. The FUSE implementation still runs as the caller inside
the instance and still parses the file system in user space; the kernel
only holds the block device. Unmounting then returns after the FUSE
implementation has flushed and acknowledged `DESTROY`, as it does for a
kernel block file system.

The caller must hold `CAP_SYS_ADMIN` in the instance, the source must be
a block device matching one of the instance's `unix-block` devices, and
the resulting mount is always `nodev`. It is `nosuid` only when the caller
asks for it: set-user-ID files and file capabilities apply through the
instance's ID map, as on a FUSE mount the instance creates itself, so
they grant nothing outside the instance. Remounts, bind mounts and
sources that are not such a block device keep using the plain FUSE
mount. The FUSE implementation must accept its mount point as `/dev/fd/N`
(libfuse 3.3 or later) and must negotiate `FUSE_ALLOW_IDMAP` (libfuse
3.19 or later with `default_permissions`); `fuse4fs` does so with
`-o kernel`, and `security.syscalls.intercept.mount.fuseblk.options` is
appended to its options for that purpose (for example `kernel,noexcl`,
since the kernel already holds the device and an exclusive open would
fail).

Incus binds the files LXCFS provides (`meminfo`, `cpuinfo`, `stat` and
the like) over the instance's own `/proc`, but a nested container
runtime mounts a new `proc` file system for each container it starts, and
that one shows the host's memory and CPUs. Software in a nested container
that sizes itself from `/proc/meminfo` or the CPU list then exceeds the
instance's limits. With `security.syscalls.intercept.mount.lxcfs` set to
`true` (in addition to `security.syscalls.intercept.mount`), Incus
creates such a `proc` mount on the caller's behalf, in its user, PID and
mount namespaces and with its root, working directory and credentials,
and binds the LXCFS files over it before answering. LXCFS answers each
read from the reading process' control group, so a nested container sees
the instance's limits, or its own if they are lower. A target of the form
`/proc/self/fd/N` or `/proc/thread-self/fd/N`, as `runc` uses, is
resolved to the caller's descriptor.

A caller in a user namespace nested below the instance's (a rootless
runtime, `unshare -U`) is left to the kernel and gets a plain `proc`, as
is any other `/proc/self` target; so are a caller without `CAP_SYS_ADMIN`
and a mount that the caller's own target or options make fail, which the
kernel then answers with its own error. If the overlay fails, or LXCFS isn't
mounted on the host, the kernel mounts a plain `proc` as well, so that
the container still starts, and a warning is recorded on the instance.
This doesn't hide the host from a determined guest: the caller may
unmount the bound files, and the CPU model, `/proc/zoneinfo` and other
interfaces LXCFS doesn't cover still show the host.

### `ioctl` (`EXT4_IOC_RESIZE_FS`)

`EXT4_IOC_RESIZE_FS` is the ioctl `resize2fs` issues on a mounted file system
to grow it without unmounting. The kernel's ext4 allows it only with
`CAP_SYS_RESOURCE` in the initial user namespace, which an unprivileged
container never has, so a guest could not grow its disks online at all.

With `security.syscalls.intercept.ioctl.resize_fs` set to `true`, only this
one ioctl is intercepted (every other ioctl goes through unchanged). A caller
whose thread has `CAP_SYS_RESOURCE` in the instance's user namespace, which
is what `ns_capable()` would check and what the instance's root has, is let
through to the file system; anyone else gets `EPERM`. That gives a container
what a virtual machine gives its guest: root can grow its own disks. Incus
reads nothing from the caller and performs nothing on its behalf; it only
decides whether the request continues. The decision rests on the thread's
credentials, which cannot change while the thread waits in the syscall, so
continuing the syscall after the check opens no window.

The file system has to support the request: a `fuseblk` mount created
through `security.syscalls.intercept.mount.fuseblk` is served by the FUSE
implementation inside the instance, and on an idmapped mount the kernel does
not tell it who the caller is. Incus therefore adds the `host_authorized_resize`
option to the `fuseblk` helper options when this interception is on, which
tells `fuse4fs` that the callers have been checked, and strips it from
`security.syscalls.intercept.mount.fuseblk.options` when it is off, so that
the configuration alone cannot open the resize to everyone in the instance.

### `sched_setscheduler`

The `sched_setscheduler` system call is used to manage process priority.

Granting this may allow a user to significantly increase the priority of
their processes, potentially taking a lot of system resources.

It also allows access to schedulers like `SCHED_FIFO` which are generally
considered to be flawed and can significantly impact overall system
stability. This is why under normal conditions, only the real root user
(or global `CAP_SYS_NICE`) would allow its use.

### `setxattr`

The `setxattr` system call is used to set extended attributes on files.

The attributes which are handled by this currently are:

- `trusted.overlay.opaque` (overlayfs directory whiteout)

Note that because the mediation must happen on a number of character
strings, there is no easy way at present to only intercept the few
attributes we care about. As we only allow the attributes above, this
may result in breakage for other attributes that would have been
previously allowed by the kernel.

This can be enabled by setting `security.syscalls.intercept.setxattr` to `true`.

### `sysinfo`

The `sysinfo` system call is used by some distributions instead of `/proc/` entries to report on resource usage.

In order to provide resource usage information specific to the container, rather than the whole system, this
syscall interception mode uses cgroup-based resource usage information to fill in the system call response.

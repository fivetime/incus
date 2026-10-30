test_interception() {
    if [ -n "${INCUS_OFFLINE:-}" ]; then
        echo "==> SKIP: External connectivity needed to pull test image"
        export TEST_UNMET_REQUIREMENT="external connectivity needed to pull test image"
        return
    fi

    poolName=$(incus profile device get default root pool)

    incus network create incusbr0
    incus init images:debian/13 c1 -s "${poolName}"
    incus config device add c1 eth0 nic network=incusbr0 name=eth0
    incus start c1
    sleep 10
    incus exec c1 -- apt-get install --no-install-recommends --yes attr e2fsprogs fuse2fs fuse

    echo "==> Testing setxattr interception"
    incus exec c1 -- touch xattr-test
    ! incus exec c1 -- setfattr -n trusted.overlay.opaque -v y xattr-test || false
    incus config set c1 security.syscalls.intercept.setxattr true
    incus restart c1 -f
    incus exec c1 -- setfattr -n trusted.overlay.opaque -v y xattr-test
    [ "$(getfattr --only-values --absolute-names -n trusted.overlay.opaque "${INCUS_DIR}/containers/c1/rootfs/root/xattr-test")" = "y" ]

    echo "==> Testing mknod interception"
    ! incus exec c1 -- mknod mknod-test c 1 3 || false
    incus config set c1 security.syscalls.intercept.mknod true
    incus restart c1 -f

    ## Relative path
    incus exec c1 -- mknod mknod-test c 1 3

    ## Absolute path on tmpfs
    incus exec c1 -- mknod /dev/mknod-test c 1 3

    ## Absolute path on rootfs
    incus exec c1 -- mknod /root/mknod-test1 c 1 3

    echo "==> Testing bpf interception"
    incus config set c1 security.syscalls.intercept.bpf=true security.syscalls.intercept.bpf.devices=true
    incus restart c1 -f

    echo "==> Testing mount interception"
    configure_loop_device loop_file_1 loop_device_1

    # Create a device node in the allowed device source path.
    # shellcheck disable=SC2154
    mknod "${TEST_DIR}/dev/loopdisk" b "$(stat -c '%Hr' "${loop_device_1}")" "$(stat -c '%Lr' "${loop_device_1}")"

    incus config device add c1 loop unix-block source="${TEST_DIR}/dev/loopdisk" path=/dev/sda
    incus exec c1 -- mkfs.ext4 /dev/sda
    ! incus exec c1 -- mount /dev/sda /mnt || false
    incus config set c1 security.syscalls.intercept.mount=true

    incus config set c1 security.syscalls.intercept.mount.allowed=ext4
    incus restart c1 -f
    incus exec c1 -- mount /dev/sda /mnt
    [ "$(incus exec c1 -- stat --format=%u:%g /mnt)" = "65534:65534" ]
    incus exec c1 -- umount /mnt

    incus config set c1 security.syscalls.intercept.mount.shift=true
    incus exec c1 -- mount /dev/sda /mnt
    [ "$(incus exec c1 -- stat --format=%u:%g /mnt)" = "0:0" ]
    incus exec c1 -- umount /mnt

    incus config unset c1 security.syscalls.intercept.mount.allowed
    incus config set c1 security.syscalls.intercept.mount.fuse=ext4=fuse2fs
    incus restart c1 -f

    incus exec c1 -- mount /dev/sda /mnt
    [ "$(incus exec c1 -- stat --format=%u:%g /mnt)" = "0:0" ]
    incus exec c1 -- umount /mnt

    echo "==> Testing EXT4_IOC_RESIZE_FS interception"
    incus config set c1 security.syscalls.intercept.ioctl.resize_fs=true
    incus restart c1 -f
    incus exec c1 -- mount /dev/sda /mnt
    # Issue the ioctl directly: resize2fs doesn't see a fuse2fs mount as mounted,
    # so it never reaches the online path. The script prints the error, or
    # nothing on success; it is pushed as a file because the incus wrapper
    # evaluates its arguments again.
    cat > "${TEST_DIR}/resize-fs.pl" << 'EOF'
use Fcntl;
sysopen(my $fh, "/mnt", O_RDONLY) or die "open: $!\n";
my $blocks = pack("Q", 1 << 20);
ioctl($fh, 0x40086610, $blocks) or print "$!\n";
EOF
    incus file push "${TEST_DIR}/resize-fs.pl" c1/root/resize-fs.pl
    # The instance's root reaches the file system, which (fuse2fs) doesn't implement the ioctl.
    out="$(incus exec c1 -- perl /root/resize-fs.pl)"
    [ "${out}" != "Operation not permitted" ] || false
    # A root in a nested user namespace has the capability only there: refused by Incus.
    if incus exec c1 -- unshare -U -r true 2>/dev/null; then
        [ "$(incus exec c1 -- unshare -U -r perl /root/resize-fs.pl)" = "Operation not permitted" ]
    fi
    incus exec c1 -- umount /mnt
    rm "${TEST_DIR}/resize-fs.pl"

    echo "==> Testing fuseblk superblock flags and remount"
    # fuse2fs doesn't know host_authorized_resize, so that interception is off here.
    incus config set c1 security.syscalls.intercept.ioctl.resize_fs=false security.syscalls.intercept.mount.fuseblk=true
    incus restart c1 -f
    # Only where the image's FUSE implementation can serve a fuseblk mount (fd mode, idmapping).
    if incus exec c1 -- mount -o sync /dev/sda /mnt 2>/dev/null; then
        incus exec c1 -- grep -q "^/dev/sda /mnt fuseblk rw,sync,nodev,relatime," /proc/self/mounts
        # The superblock is the host's, so its remount is done by Incus: flags only, nodev kept.
        incus exec c1 -- mount -o remount,async,noatime /mnt
        incus exec c1 -- grep -q "^/dev/sda /mnt fuseblk rw,nodev,noatime," /proc/self/mounts
        incus exec c1 -- mount -o remount,sync /mnt
        incus exec c1 -- grep -q "^/dev/sda /mnt fuseblk rw,sync,nodev,relatime," /proc/self/mounts
        # A bind mount in another mount namespace shares the superblock and sees its flags.
        cat > "${TEST_DIR}/bind-flags.sh" << 'EOF'
mount --bind /mnt /opt && grep "^/dev/sda /opt fuseblk " /proc/self/mounts | cut -d' ' -f4 | cut -d, -f1-2
EOF
        incus file push "${TEST_DIR}/bind-flags.sh" c1/root/bind-flags.sh
        [ "$(incus exec c1 -- unshare -m sh /root/bind-flags.sh)" = "rw,sync" ]
        incus exec c1 -- mount -o remount,async /mnt
        [ "$(incus exec c1 -- unshare -m sh /root/bind-flags.sh)" = "rw,nodev" ]
        # Not the root of the mount: refused.
        ! incus exec c1 -- mount -o remount,sync /mnt/lost+found || false
        # A root in a nested user namespace has the capability only there: refused.
        if incus exec c1 -- unshare -U -r true 2>/dev/null; then
            ! incus exec c1 -- unshare -U -r mount -o remount,sync /mnt || false
            incus exec c1 -- grep -q "^/dev/sda /mnt fuseblk rw,nodev,relatime," /proc/self/mounts
        fi
        incus exec c1 -- umount /mnt
        rm "${TEST_DIR}/bind-flags.sh"
    else
        echo "==> SKIP: the image's FUSE implementation can't serve a fuseblk mount"
    fi
    incus config unset c1 security.syscalls.intercept.mount.fuseblk

    echo "==> Testing LXCFS overlay on proc mounts"
    incus config set c1 limits.memory=512MiB security.syscalls.intercept.mount.lxcfs=true
    incus restart c1 -f
    # Only meaningful where LXCFS covers the instance's own /proc.
    if incus exec c1 -- grep -q " /proc/meminfo fuse.lxcfs " /proc/self/mounts; then
        [ "$(incus exec c1 -- unshare -p -f --mount-proc grep MemTotal /proc/meminfo)" = "$(incus exec c1 -- grep MemTotal /proc/meminfo)" ]
        incus exec c1 -- unshare -p -f --mount-proc grep -q " /proc/meminfo fuse.lxcfs " /proc/self/mounts

        # A nested user namespace is left to the kernel.
        if incus exec c1 -- unshare -U -r true 2>/dev/null; then
            ! incus exec c1 -- unshare -U -r -p -f --mount-proc grep -q " /proc/meminfo fuse.lxcfs " /proc/self/mounts || false
        fi

        incus config set c1 security.syscalls.intercept.mount.lxcfs=false
        ! incus exec c1 -- unshare -p -f --mount-proc grep -q " /proc/meminfo fuse.lxcfs " /proc/self/mounts || false
    fi

    incus delete -f c1
    rm "${TEST_DIR}/dev/loopdisk"
    # shellcheck disable=SC2154
    deconfigure_loop_device "${loop_file_1}" "${loop_device_1}"
    incus network delete incusbr0
}

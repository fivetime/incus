package main

/*
#ifndef _GNU_SOURCE
#define _GNU_SOURCE 1
#endif
#include <fcntl.h>
#include <libgen.h>
#include <limits.h>
#include <linux/nsfs.h>
#include <sched.h>
#include <signal.h>
#include <stdbool.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/capability.h>
#include <sys/fsuid.h>
#include <sys/ioctl.h>
#include <sys/prctl.h>
#include <sys/stat.h>
#include <sys/sysmacros.h>
#include <sys/types.h>
#include <sys/vfs.h>
#include <sys/wait.h>
#include <sys/xattr.h>
#include <time.h>
#include <unistd.h>

#ifndef STATX_MNT_ID
#define STATX_MNT_ID 0x00001000U
#endif

#ifndef STATX_ATTR_MOUNT_ROOT
#define STATX_ATTR_MOUNT_ROOT 0x00002000
#endif

#ifndef AT_STATX_DONT_SYNC
#define AT_STATX_DONT_SYNC 0x4000
#endif

#include "../../shared/cgo/macro.h"
#include "../../shared/cgo/memory_utils.h"
#include "../../shared/cgo/mount_utils.h"
#include "../../shared/cgo/syscall_numbers.h"
#include "../../shared/cgo/syscall_wrappers.h"

extern char* advance_arg(bool required);
extern void attach_userns_fd(int ns_fd);
extern int pidfd_nsfd(int pidfd, pid_t pid);
extern int preserve_ns(pid_t pid, int ns_fd, const char *ns);
extern bool change_namespaces(int pidfd, int nsfd, unsigned int flags);
extern int mount_detach_idmap(const char *path, int fd_userns);
extern int mnt_attributes_new(unsigned int old_flags, unsigned int *new_flags);

static bool chdirchroot_in_mntns(int cwd_fd, int root_fd)
{
	ssize_t len;
	char buf[PATH_MAX];

	if (fchdir(cwd_fd))
		return false;

	len = readlinkat(root_fd, "", buf, sizeof(buf));
	if (len < 0 || len >= sizeof(buf))
		return false;
	buf[len] = '\0';

	if (chroot(buf))
		return false;

	return true;
}

static bool acquire_basic_creds(pid_t pid, int pidfd, int ns_fd,
				int *rootfd, int *cwdfd)
{
	__do_close int cwd_fd = -EBADF, root_fd = -EBADF;
	char buf[256];

	snprintf(buf, sizeof(buf), "/proc/%d/root", pid);
	root_fd = open(buf, O_PATH | O_RDONLY | O_CLOEXEC | O_NOFOLLOW);
	if (root_fd < 0)
		return false;

	snprintf(buf, sizeof(buf), "/proc/%d/cwd", pid);
	cwd_fd = open(buf, O_PATH | O_RDONLY | O_CLOEXEC);
	if (cwd_fd < 0)
		return false;

	if (!change_namespaces(pidfd, ns_fd, CLONE_NEWNS))
		return false;

	if (!chdirchroot_in_mntns(cwd_fd, root_fd))
		return false;

	if (rootfd)
		*rootfd = move_fd(root_fd);

	if (cwdfd)
		*cwdfd = move_fd(cwd_fd);
	return true;
}

static bool reacquire_basic_creds(int pidfd, int ns_fd,
				  int root_fd, int cwd_fd)
{
	if (!change_namespaces(pidfd, ns_fd, CLONE_NEWNS))
		return false;

	if (!chdirchroot_in_mntns(cwd_fd, root_fd))
		return false;

	return true;
}

static bool acquire_final_creds(pid_t pid, uid_t uid, gid_t gid, uid_t fsuid, gid_t fsgid)
{
	int ret;
	cap_t caps;

	caps = cap_get_pid(pid);
	if (!caps) {
		fprintf(stderr, "%d", ENOANO);
		return false;
	}

	ret = prctl(PR_SET_KEEPCAPS, 1);
	if (ret) {
		fprintf(stderr, "%d", ENOANO);
		return false;
	}

	ret = setegid(gid);
	if (ret) {
		fprintf(stderr, "%d", ENOANO);
		return false;
	}

	setfsgid(fsgid);
	if (setfsgid(-1) != fsgid) {
		fprintf(stderr, "%d", ENOANO);
		return false;
	}


	ret = seteuid(uid);
	if (ret) {
		fprintf(stderr, "%d", ENOANO);
		return false;
	}

	setfsuid(fsuid);
	if (setfsuid(-1) != fsuid) {
		fprintf(stderr, "%d", ENOANO);
		return false;
	}

	ret = cap_set_proc(caps);
	if (ret) {
		fprintf(stderr, "%d", ENOANO);
		return false;
	}

	return true;
}

// Expects command line to be in the form:
// <PID> <root-uid> <root-gid> <path> <mode> <dev>
static void mknod_emulate(void)
{
	__do_close int target_dir_fd = -EBADF, pidfd = -EBADF, ns_fd = -EBADF;
	char *target = NULL, *target_dir = NULL, *target_base = NULL;
	int ret;
	char path[PATH_MAX];
	char base_path[PATH_MAX];
	mode_t mode;
	dev_t dev;
	pid_t pid;
	uid_t fsuid, uid;
	gid_t fsgid, gid;
	struct statfs sfs;

	pid = atoi(advance_arg(true));
	pidfd = atoi(advance_arg(true));
	ns_fd = pidfd_nsfd(pidfd, pid);
	if (ns_fd < 0)
		_exit(EXIT_FAILURE);
	target = advance_arg(true);
	mode = atoi(advance_arg(true));
	dev = atoi(advance_arg(true));
	uid = atoi(advance_arg(true));
	gid = atoi(advance_arg(true));
	fsuid = atoi(advance_arg(true));
	fsgid = atoi(advance_arg(true));

	if (!acquire_basic_creds(pid, pidfd, ns_fd, NULL, NULL)) {
		fprintf(stderr, "%d", ENOANO);
		_exit(EXIT_FAILURE);
	}

	ret = snprintf(path, sizeof(path), "%s", target);
	if (ret < 0 || (size_t)ret >= sizeof(path)) {
		fprintf(stderr, "%d", ENAMETOOLONG);
		_exit(EXIT_FAILURE);
	}

	target_dir = dirname(path);
	target_dir_fd = open(target_dir, O_PATH | O_RDONLY | O_CLOEXEC | O_DIRECTORY);
	if (target_dir_fd < 0) {
		fprintf(stderr, "%d", ENOANO);
		_exit(EXIT_FAILURE);
	}

	if (!acquire_final_creds(pid, uid, gid, fsuid, fsgid)) {
		fprintf(stderr, "%d", ENOANO);
		_exit(EXIT_FAILURE);
	}

	ret = fstatfs(target_dir_fd, &sfs);
	if (ret) {
		fprintf(stderr, "%d", ENOANO);
		_exit(EXIT_FAILURE);
	}

	if (sfs.f_flags & MS_NODEV) {
		fprintf(stderr, "%d", EPERM);
		_exit(EXIT_FAILURE);
	}

	// The target path may be relative to the process' cwd, so create the
	// node by name relative to the already opened parent directory.
	ret = snprintf(base_path, sizeof(base_path), "%s", target);
	if (ret < 0 || (size_t)ret >= sizeof(base_path)) {
		fprintf(stderr, "%d", ENAMETOOLONG);
		_exit(EXIT_FAILURE);
	}

	target_base = basename(base_path);
	ret = mknodat(target_dir_fd, target_base, mode, dev);
	if (ret) {
		if (errno == EPERM)
			fprintf(stderr, "%d", ENOMEDIUM);
		else
			fprintf(stderr, "%d", errno);

		_exit(EXIT_FAILURE);
	}
}

const static int ns_flags[] = { CLONE_NEWUSER, CLONE_NEWPID, CLONE_NEWUTS, CLONE_NEWIPC, CLONE_NEWNET, CLONE_NEWCGROUP };

static bool change_creds(int pidfd, int ns_fd, cap_t caps, uid_t nsuid, gid_t nsgid, uid_t nsfsuid, gid_t nsfsgid)
{
	if (prctl(PR_SET_KEEPCAPS, 1))
		return false;

	for (int i = 0; ARRAY_SIZE(ns_flags); i++) {
		if (!change_namespaces(pidfd, ns_fd, ns_flags[i]))
			return false;
	}

	if (setegid(nsgid))
		return false;

	setfsgid(nsfsgid);

	if (seteuid(nsuid))
		return false;

	setfsuid(nsfsuid);

	if (cap_set_proc(caps))
		return false;

	return true;
}

static void setxattr_emulate(void)
{
	__do_close int ns_fd = -EBADF, pidfd = -EBADF, target_fd = -EBADF;
	int flags = 0;
	char *name, *target;
	uid_t nsfsuid, nsuid;
	gid_t nsfsgid, nsgid;
	pid_t pid = 0;
	cap_t caps;
	cap_flag_value_t flag;
	int whiteout;
	void *data;
	size_t size;

	pid = atoi(advance_arg(true));
	pidfd = atoi(advance_arg(true));
	ns_fd = pidfd_nsfd(pidfd, pid);
	if (ns_fd < 0)
		_exit(EXIT_FAILURE);
	nsuid = atoi(advance_arg(true));
	nsgid = atoi(advance_arg(true));
	nsfsuid = atoi(advance_arg(true));
	nsfsgid = atoi(advance_arg(true));
	name = advance_arg(true);
	target = advance_arg(true);
	flags = atoi(advance_arg(true));
	whiteout = atoi(advance_arg(true));
	size = atoi(advance_arg(true));
	data = advance_arg(true);

	if (!acquire_basic_creds(pid, pidfd, ns_fd, NULL, NULL)) {
		fprintf(stderr, "%d", ENOANO);
		_exit(EXIT_FAILURE);
	}

	target_fd = open(target, O_RDONLY | O_CLOEXEC);
	if (target_fd < 0) {
		fprintf(stderr, "%d", errno);
		_exit(EXIT_FAILURE);
	}

	caps = cap_get_pid(pid);
	if (!caps) {
		fprintf(stderr, "%d", ENOANO);
		_exit(EXIT_FAILURE);
	}

	if (whiteout == 1) {
		if (cap_get_flag(caps,  CAP_SYS_ADMIN, CAP_EFFECTIVE, &flag) != 0) {
			fprintf(stderr, "%d", EPERM);
			_exit(EXIT_FAILURE);
		}

		if (flag == CAP_CLEAR) {
			fprintf(stderr, "%d", EPERM);
			_exit(EXIT_FAILURE);
		}
	}

	if (whiteout == 1) {
		if (fsetxattr(target_fd, "trusted.overlay.opaque", "y", 1, flags)) {
			fprintf(stderr, "%d", errno);
			_exit(EXIT_FAILURE);
		}
	} else {
		if (!change_creds(pidfd, ns_fd, caps, nsuid, nsgid, nsfsuid, nsfsgid)) {
			fprintf(stderr, "%d", EFAULT);
			_exit(EXIT_FAILURE);
		}

		if (fsetxattr(target_fd, name, data, size, flags)) {
			fprintf(stderr, "%d", errno);
			_exit(EXIT_FAILURE);
		}
	}
}

// How long the FUSE server of a fuseblk mount may take to answer INIT.
#define FUSEBLK_INIT_TIMEOUT 120

// Exit codes of the child waiting for INIT.
#define FUSEBLK_PROBE_OK 0
#define FUSEBLK_PROBE_FAILED 1
#define FUSEBLK_PROBE_WRONG_DEVICE 2

static bool userns_is_child(int ns_fd);

// fuseblk_set_superblock_flags passes the caller's superblock flags to the
// file system context, as mount(2) does for a superblock the caller may
// create or reconfigure itself: the kernel takes them from the context, not
// from the mount attributes. Each is passed by its own name, so nothing but
// these can reach the context. Creating sets the ones asked for; a remount
// replaces the set, so the others are cleared, except dirsync, which a
// remount can't change.
static void fuseblk_set_superblock_flags(int fs_fd, unsigned long flags, bool reconfigure)
{
	const struct {
		unsigned long flag;
		const char *set;
		const char *clear;
		bool remount;
	} sb_flags[] = {
		{ MS_RDONLY,      "ro",       "rw",         true },
		{ MS_SYNCHRONOUS, "sync",     "async",      true },
		{ MS_LAZYTIME,    "lazytime", "nolazytime", true },
		{ MS_DIRSYNC,     "dirsync",  NULL,         false },
	};
	const char *opt;
	size_t i;
	int ret;

	for (i = 0; i < sizeof(sb_flags) / sizeof(sb_flags[0]); i++) {
		if (reconfigure && !sb_flags[i].remount)
			continue;

		if (flags & sb_flags[i].flag)
			opt = sb_flags[i].set;
		else if (reconfigure)
			opt = sb_flags[i].clear;
		else
			continue;

		ret = incus_fsconfig(fs_fd, FSCONFIG_SET_FLAG, opt, NULL, 0);
		if (ret < 0)
			die("error: failed to set fuseblk superblock flag %s", opt);
	}
}

// fuseblk_create_tree creates the fuseblk superblock for the caller's block
// device on the host, starts the FUSE server inside the container on it,
// waits for the server to answer INIT and returns the detached mount idmapped
// to the container's user namespace. It must run as host root before joining
// any of the container's namespaces: a fuseblk superblock can't be created
// from a user namespace, and /proc/<pid>/root and /dev/fuse must be the host's.
//
// The kernel resolves the "source" of a fuseblk mount when the superblock is
// created, against the calling process' root, and shows that string as the
// mount's source. So that the guest sees the path it asked for (and umount,
// mount -a and monitoring find the mount by it), the superblock is created
// while chrooted to the caller's root rather than from the host through
// /proc/self/fd/<fd>. Chrooting keeps this process in the host's mount
// namespace, so /proc and /dev stay the host's, and resolves the path against
// the same root as the checks before it, even for a caller that is itself
// chrooted within the container; joining the container's mount namespace
// would resolve it against that namespace's root instead. The path is only
// used this way when it holds no symlink; otherwise /proc/self/fd/<fd> is
// kept as the source.
//
// The guest can still replace the node between the checks and the creation.
// It can't create block devices and only sees its own, so it can at most
// swap in another of its own devices, and the device the superblock was
// created on is checked once the server answers INIT, before the mount is
// idmapped or attached: the superblock's s_dev is the block device's number
// and is filled into st_dev by the kernel, not by the server.
//
// This runs in the single-threaded forksyscall process, so chrooting doesn't
// affect any other thread.
static int fuseblk_create_tree(pid_t pid, int pidfd, int ns_fd, int fd_userns,
			       uid_t uid, gid_t gid, uid_t fsuid, gid_t fsgid,
			       const char *helper, const char *source, const char *opts,
			       unsigned long flags, unsigned int dev_major, unsigned int dev_minor)
{
	__do_close int root_fd = -EBADF, dev_fd = -EBADF, fuse_fd = -EBADF, fs_fd = -EBADF,
		       src_fd = -EBADF, host_root_fd = -EBADF, host_cwd_fd = -EBADF;
	bool guest_source = false;
	int fd_tree, ret, status;
	unsigned int attrs = 0;
	char buf[PATH_MAX];
	struct stat st;
	struct timespec deadline, now;
	cap_t caps;
	cap_flag_value_t flag;
	pid_t pid_helper, pid_probe;
	struct lxc_mount_attr attr = {
	    .attr_set		= MOUNT_ATTR_IDMAP,
	};
	struct incus_open_how how = {
	    .flags		= O_PATH | O_CLOEXEC | O_NOFOLLOW,
	    .resolve		= RESOLVE_IN_ROOT | RESOLVE_NO_MAGICLINKS,
	};
	struct incus_open_how how_nosymlinks = {
	    .flags		= O_PATH | O_CLOEXEC | O_NOFOLLOW,
	    .resolve		= RESOLVE_IN_ROOT | RESOLVE_NO_SYMLINKS,
	};

	caps = cap_get_pid(pid);
	if (!caps)
		die("error: failed to get caller capabilities");

	if (cap_get_flag(caps, CAP_SYS_ADMIN, CAP_EFFECTIVE, &flag) != 0 || flag != CAP_SET)
		die_errno(EPERM, "error: caller lacks CAP_SYS_ADMIN");

	cap_free(caps);

	// Resolve the source as the caller would, inside the container's root.
	// The device is only checked here, so a node swapped in after the
	// caller's arguments were checked can't block the open or be used.
	snprintf(buf, sizeof(buf), "/proc/%d/root", pid);
	root_fd = open(buf, O_PATH | O_DIRECTORY | O_CLOEXEC);
	if (root_fd < 0)
		die("error: failed to open container root");

	dev_fd = incus_openat2(root_fd, source, &how, sizeof(how));
	if (dev_fd < 0)
		die("error: failed to open fuseblk source");

	if (fstat(dev_fd, &st))
		die("error: failed to stat fuseblk source");

	if (!S_ISBLK(st.st_mode) || major(st.st_rdev) != dev_major || minor(st.st_rdev) != dev_minor)
		die_errno(EPERM, "error: fuseblk source is not the expected block device");

	// Only give the kernel the guest's path when it resolves without
	// symlinks to the same device.
	src_fd = incus_openat2(root_fd, source, &how_nosymlinks, sizeof(how_nosymlinks));
	if (src_fd >= 0 && fstat(src_fd, &st) == 0 && S_ISBLK(st.st_mode) &&
	    major(st.st_rdev) == dev_major && minor(st.st_rdev) == dev_minor)
		guest_source = true;

	fuse_fd = open("/dev/fuse", O_RDWR | O_CLOEXEC);
	if (fuse_fd < 0)
		die("error: failed to open /dev/fuse");

	fs_fd = incus_fsopen("fuseblk", FSOPEN_CLOEXEC);
	if (fs_fd < 0)
		die("error: failed to create fuseblk mount: fsopen");

	snprintf(buf, sizeof(buf), "%d", fuse_fd);
	ret = incus_fsconfig(fs_fd, FSCONFIG_SET_STRING, "fd", buf, 0);
	if (ret < 0)
		die("error: failed to create fuseblk mount: fsconfig fd");

	ret = incus_fsconfig(fs_fd, FSCONFIG_SET_STRING, "rootmode", "40000", 0);
	if (ret < 0)
		die("error: failed to create fuseblk mount: fsconfig rootmode");

	ret = incus_fsconfig(fs_fd, FSCONFIG_SET_STRING, "user_id", "0", 0);
	if (ret < 0)
		die("error: failed to create fuseblk mount: fsconfig user_id");

	ret = incus_fsconfig(fs_fd, FSCONFIG_SET_STRING, "group_id", "0", 0);
	if (ret < 0)
		die("error: failed to create fuseblk mount: fsconfig group_id");

	ret = incus_fsconfig(fs_fd, FSCONFIG_SET_STRING, "blksize", "4096", 0);
	if (ret < 0)
		die("error: failed to create fuseblk mount: fsconfig blksize");

	ret = incus_fsconfig(fs_fd, FSCONFIG_SET_FLAG, "allow_other", NULL, 0);
	if (ret < 0)
		die("error: failed to create fuseblk mount: fsconfig allow_other");

	ret = incus_fsconfig(fs_fd, FSCONFIG_SET_FLAG, "default_permissions", NULL, 0);
	if (ret < 0)
		die("error: failed to create fuseblk mount: fsconfig default_permissions");

	// ro, sync and the like live on the superblock, not on the mount
	// attributes set below, and the caller's mount call named them.
	fuseblk_set_superblock_flags(fs_fd, flags, false);

	if (guest_source) {
		host_root_fd = open("/", O_PATH | O_DIRECTORY | O_CLOEXEC);
		if (host_root_fd < 0)
			die("error: failed to open host root");

		host_cwd_fd = open(".", O_PATH | O_DIRECTORY | O_CLOEXEC);
		if (host_cwd_fd < 0)
			die("error: failed to open current directory");

		if (fchdir(root_fd) || chroot("."))
			die("error: failed to change to container root");

		// Nothing but the source and the creation may happen while
		// chrooted to the caller's root.
		ret = incus_fsconfig(fs_fd, FSCONFIG_SET_STRING, "source", source, 0);
		if (ret < 0)
			die("error: failed to create fuseblk mount: fsconfig source");

		ret = incus_fsconfig(fs_fd, FSCONFIG_CMD_CREATE, NULL, NULL, 0);
		if (ret < 0)
			die("error: failed to create fuseblk mount: fsconfig create");

		if (fchdir(host_root_fd) || chroot(".") || fchdir(host_cwd_fd))
			die("error: failed to change back to host root");
	} else {
		snprintf(buf, sizeof(buf), "/proc/self/fd/%d", dev_fd);
		ret = incus_fsconfig(fs_fd, FSCONFIG_SET_STRING, "source", buf, 0);
		if (ret < 0)
			die("error: failed to create fuseblk mount: fsconfig source");

		ret = incus_fsconfig(fs_fd, FSCONFIG_CMD_CREATE, NULL, NULL, 0);
		if (ret < 0)
			die("error: failed to create fuseblk mount: fsconfig create");
	}

	// Flags without a mount attribute are dropped: remounts, bind and move
	// mounts never get here.
	//
	// The superblock belongs to the host's user namespace, so the kernel
	// doesn't add the nodev it gives a mount created in the instance and
	// any device node the guest writes would open: always force it. The
	// caller's nosuid is kept as asked. Set-user-ID and file capabilities
	// only apply through the instance's ID map attached below, which the
	// mount never goes without, so they stay within the instance's own
	// user namespace as on any FUSE mount it creates itself.
	mnt_attributes_new(flags, &attrs);
	attrs |= MOUNT_ATTR_NODEV;
	fd_tree = incus_fsmount(fs_fd, FSMOUNT_CLOEXEC, attrs);
	if (fd_tree < 0)
		die("error: failed to create fuseblk mount: fsmount");

	pid_helper = fork();
	if (pid_helper < 0)
		die("error: failed to fork FUSE helper");

	if (pid_helper == 0) {
		ret = fcntl(fuse_fd, F_GETFD);
		if (ret < 0 || fcntl(fuse_fd, F_SETFD, ret & ~FD_CLOEXEC) < 0)
			die("error: failed to pass /dev/fuse to the FUSE helper");

		attach_userns_fd(ns_fd);

		// Attach to pid namespace so that the fuse daemon belongs to
		// the correct pid namespace and dies with the container.
		if (!change_namespaces(pidfd, ns_fd, CLONE_NEWPID))
			die("error: failed to change to target pid namespace");

		if (!acquire_basic_creds(pid, pidfd, ns_fd, NULL, NULL))
			die("error: failed to acquire basic creds");

		if (!acquire_final_creds(pid, uid, gid, fsuid, fsgid))
			die("error: failed to acquire final creds");

		snprintf(buf, sizeof(buf), "/dev/fd/%d", fuse_fd);
		if (strcmp(opts, "") == 0)
			execlp(helper, helper, source, buf, (char *) NULL);
		else
			execlp(helper, helper, source, buf, "-o", opts, (char *) NULL);
		die("error: failed to execute FUSE helper %s", helper);
	}

	close_prot_errno_disarm(fuse_fd);

	// Idmapping a FUSE mount needs the server's INIT reply, which a request
	// waits for. Wait in a child: once interrupted, that wait only answers
	// fatal signals, so a timer can't bring the caller back. The child also
	// checks the device the superblock was created on, as any later stat
	// waits on the guest's server as well.
	pid_probe = fork();
	if (pid_probe < 0)
		die("error: failed to fork FUSE INIT probe");

	if (pid_probe == 0) {
		if (fstatat(fd_tree, "", &st, AT_EMPTY_PATH))
			_exit(FUSEBLK_PROBE_FAILED);

		if (st.st_dev != makedev(dev_major, dev_minor))
			_exit(FUSEBLK_PROBE_WRONG_DEVICE);

		_exit(FUSEBLK_PROBE_OK);
	}

	clock_gettime(CLOCK_MONOTONIC, &deadline);
	deadline.tv_sec += FUSEBLK_INIT_TIMEOUT;
	for (;;) {
		if (pid_helper > 0) {
			ret = waitpid(pid_helper, &status, WNOHANG);
			if (ret == pid_helper) {
				pid_helper = -1;
				if (!WIFEXITED(status) || WEXITSTATUS(status)) {
					kill(pid_probe, SIGKILL);
					die_errno(ECHILD, "error: FUSE helper failed: status=%d", status);
				}
			}
		}

		ret = waitpid(pid_probe, &status, WNOHANG);
		if (ret == pid_probe) {
			if (WIFEXITED(status) && WEXITSTATUS(status) == FUSEBLK_PROBE_WRONG_DEVICE)
				die_errno(EPERM, "error: fuseblk mount was created on another device");

			if (!WIFEXITED(status) || WEXITSTATUS(status) != FUSEBLK_PROBE_OK)
				die_errno(ENOTCONN, "error: FUSE helper did not answer INIT: status=%d", status);

			break;
		}

		if (ret < 0)
			die("error: failed to wait for FUSE INIT probe");

		clock_gettime(CLOCK_MONOTONIC, &now);
		if (now.tv_sec > deadline.tv_sec ||
		    (now.tv_sec == deadline.tv_sec && now.tv_nsec >= deadline.tv_nsec)) {
			// Destroying the superblock makes the helper exit.
			kill(pid_probe, SIGKILL);
			waitpid(pid_probe, NULL, 0);
			close(fd_tree);
			die_errno(ETIMEDOUT, "error: FUSE helper did not answer INIT in %ds", FUSEBLK_INIT_TIMEOUT);
		}

		usleep(10000);
	}

	attr.userns_fd = fd_userns;
	ret = incus_mount_setattr(fd_tree, "", AT_EMPTY_PATH, &attr, sizeof(attr));
	if (ret < 0)
		die("error: failed to create detached idmapped mount");

	return fd_tree;
}

// fuseblk_remount reconfigures a fuseblk superblock Incus created for the
// caller's block device, as the caller's remount would if the superblock
// were its own, and gives the mount the attributes the remount names. The
// superblock belongs to the host's user namespace, so this runs as host root
// in the host's namespaces. The target is resolved only inside the caller's
// root and must be the root of a mount in the caller's mount namespace whose
// superblock is fuseblk on exactly the caller's device; that is read from
// the caller's mountinfo rather than asked of its FUSE server, which may not
// answer. Only the superblock flags a remount may change and the mount
// attributes with a mount_setattr equivalent are applied, each by name: the
// remount's data string is not, as the kernel ignores it on a FUSE remount.
// nodev stays forced and the ID map is left alone.
static void fuseblk_remount(pid_t pid, int ns_fd, const char *target, unsigned long flags,
			    unsigned int dev_major, unsigned int dev_minor)
{
	__do_close int root_fd = -EBADF, target_fd = -EBADF, fs_fd = -EBADF;
	__do_free char *line = NULL;
	struct incus_open_how how = {
	    .flags		= O_PATH | O_CLOEXEC | O_DIRECTORY,
	    .resolve		= RESOLVE_IN_ROOT | RESOLVE_NO_MAGICLINKS,
	};
	struct lxc_mount_attr attr = {};
	struct statx stx;
	unsigned int attrs = 0, maj = 0, min = 0;
	FILE *mountinfo;
	size_t len = 0;
	bool found = false;
	int ret, id, parent;
	char buf[PATH_MAX], fstype[64];
	cap_t caps;
	cap_flag_value_t flag;

	// The caller must administer the instance itself: a root in a user
	// namespace nested below it holds its capabilities only there.
	if (!userns_is_child(ns_fd))
		die_errno(EPERM, "error: caller is in a nested user namespace");

	caps = cap_get_pid(pid);
	if (!caps)
		die("error: failed to get caller capabilities");

	if (cap_get_flag(caps, CAP_SYS_ADMIN, CAP_EFFECTIVE, &flag) != 0 || flag != CAP_SET)
		die_errno(EPERM, "error: caller lacks CAP_SYS_ADMIN");

	cap_free(caps);

	snprintf(buf, sizeof(buf), "/proc/%d/root", pid);
	root_fd = open(buf, O_PATH | O_DIRECTORY | O_CLOEXEC);
	if (root_fd < 0)
		die("error: failed to open container root");

	target_fd = incus_openat2(root_fd, target, &how, sizeof(how));
	if (target_fd < 0)
		die("error: failed to open remount target");

	// Cached attributes are enough: the mount ID and the mount-root
	// attribute come from the VFS, not from the server.
	if (statx(target_fd, "", AT_EMPTY_PATH | AT_STATX_DONT_SYNC, STATX_TYPE | STATX_MNT_ID, &stx))
		die("error: failed to stat remount target");

	if (!(stx.stx_attributes_mask & STATX_ATTR_MOUNT_ROOT) || !(stx.stx_attributes & STATX_ATTR_MOUNT_ROOT))
		die_errno(EINVAL, "error: remount target is not the root of a mount");

	if (!(stx.stx_mask & STATX_MNT_ID))
		die_errno(EINVAL, "error: remount target has no mount ID");

	snprintf(buf, sizeof(buf), "/proc/%d/mountinfo", pid);
	mountinfo = fopen(buf, "re");
	if (!mountinfo)
		die("error: failed to open caller mountinfo");

	while (getline(&line, &len, mountinfo) > 0) {
		char *sep;

		if (sscanf(line, "%d %d %u:%u", &id, &parent, &maj, &min) != 4 || (__u64)id != stx.stx_mnt_id)
			continue;

		sep = strstr(line, " - ");
		if (!sep || sscanf(sep + 3, "%63s", fstype) != 1)
			die_errno(EINVAL, "error: unreadable mountinfo entry for the remount target");

		found = true;
		break;
	}

	fclose(mountinfo);

	if (!found)
		die_errno(EINVAL, "error: remount target is not a mount of the caller");

	if (strcmp(fstype, "fuseblk") != 0 || maj != dev_major || min != dev_minor)
		die_errno(EPERM, "error: remount target is not a fuseblk mount of the caller's device");

	fs_fd = incus_fspick(target_fd, "", FSPICK_EMPTY_PATH | FSPICK_CLOEXEC | FSPICK_NO_AUTOMOUNT);
	if (fs_fd < 0)
		die("error: failed to pick the fuseblk superblock");

	fuseblk_set_superblock_flags(fs_fd, flags, true);

	ret = incus_fsconfig(fs_fd, FSCONFIG_CMD_RECONFIGURE, NULL, NULL, 0);
	if (ret < 0)
		die("error: failed to reconfigure fuseblk superblock");

	// The mount's own attributes follow the remount as mount(2) applies
	// them once the superblock is done: the ones it names are set and
	// the others cleared, except nodev, which stays set, as at creation.
	mnt_attributes_new(flags, &attrs);
	attr.attr_set = attrs | MOUNT_ATTR_NODEV;
	attr.attr_clr = MOUNT_ATTR_RDONLY | MOUNT_ATTR_NOSUID | MOUNT_ATTR_NODEV |
			MOUNT_ATTR_NOEXEC | MOUNT_ATTR__ATIME | MOUNT_ATTR_NODIRATIME;
	ret = incus_mount_setattr(target_fd, "", AT_EMPTY_PATH, &attr, sizeof(attr));
	if (ret < 0)
		die("error: failed to set fuseblk mount attributes");
}

// attach_detached_mount attaches a detached mount created in the host's mount
// namespace to the target path in the container, as the caller.
static void attach_detached_mount(int fd_tree, int fd_mntns, pid_t pid, int pidfd, int ns_fd,
				  int root_fd, int cwd_fd, uid_t nsuid, gid_t nsgid,
				  uid_t nsfsuid, gid_t nsfsgid, const char *target)
{
	int ret;

	ret = setns(fd_mntns, CLONE_NEWNS);
	if (ret)
		die("error: failed to attach to old mount namespace");

	attach_userns_fd(ns_fd);

	if (!change_namespaces(pidfd, ns_fd, CLONE_NEWUSER))
		die("error: failed to change to target user namespace");

	if (!reacquire_basic_creds(pidfd, ns_fd, root_fd, cwd_fd))
		die("error: failed to acquire basic creds");

	if (!acquire_final_creds(pid, nsuid, nsgid, nsfsuid, nsfsgid))
		die("error: failed to acquire final creds");

	ret = incus_move_mount(fd_tree, "", -EBADF, target, MOVE_MOUNT_F_EMPTY_PATH);
	if (ret)
		die("error: failed to attach detached mount");
}

// set_final_creds takes on the caller's IDs and the capabilities read from it
// earlier, like acquire_final_creds for a caller whose PID can't be resolved
// any more.
static bool set_final_creds(cap_t caps, uid_t uid, gid_t gid, uid_t fsuid, gid_t fsgid)
{
	if (prctl(PR_SET_KEEPCAPS, 1))
		return false;

	if (setegid(gid))
		return false;

	setfsgid(fsgid);
	if (setfsgid(-1) != fsgid)
		return false;

	if (seteuid(uid))
		return false;

	setfsuid(fsuid);
	if (setfsuid(-1) != fsuid)
		return false;

	return cap_set_proc(caps) == 0;
}

// fsconfig_set_options passes comma-separated mount options to a file system
// context, as mount(2) would pass its data argument.
static bool fsconfig_set_options(int fs_fd, const char *data)
{
	__do_free char *buf = NULL;
	char *cur, *tok, *val;
	int ret;

	buf = strdup(data);
	if (!buf)
		return false;

	for (cur = buf; (tok = strsep(&cur, ",")); ) {
		if (*tok == '\0')
			continue;

		val = strchr(tok, '=');
		if (val)
			*val++ = '\0';

		if (val && *val != '\0')
			ret = incus_fsconfig(fs_fd, FSCONFIG_SET_STRING, tok, val, 0);
		else
			ret = incus_fsconfig(fs_fd, FSCONFIG_SET_FLAG, tok, NULL, 0);
		if (ret < 0)
			return false;
	}

	return true;
}

// Exit code of a proc_lxcfs_mount that left the mount to the kernel.
#define PROC_LXCFS_SKIPPED 3

// userns_is_child reports whether the user namespace in ns_fd is a direct
// child of this process' user namespace, as the instance's own is.
static bool userns_is_child(int ns_fd)
{
	__do_close int fd_userns = -EBADF, fd_parent = -EBADF, fd_self = -EBADF;
	struct stat st_parent, st_self;

	fd_userns = openat(ns_fd, "user", O_RDONLY | O_CLOEXEC);
	if (fd_userns < 0)
		return false;

	fd_parent = ioctl(fd_userns, NS_GET_PARENT);
	if (fd_parent < 0)
		return false;

	fd_self = open("/proc/self/ns/user", O_RDONLY | O_CLOEXEC);
	if (fd_self < 0)
		return false;

	if (fstat(fd_parent, &st_parent) || fstat(fd_self, &st_self))
		return false;

	return st_parent.st_dev == st_self.st_dev && st_parent.st_ino == st_self.st_ino;
}

// The LXCFS files bound over a proc mounted with
// security.syscalls.intercept.mount.lxcfs, the same ones LXC binds over the
// instance's own /proc.
static const char *proc_lxcfs_files[] = {
	"cpuinfo", "diskstats", "loadavg", "meminfo", "pressure",
	"slabinfo", "stat", "swaps", "uptime",
};

// proc_lxcfs_mount mounts a new proc file system for the caller and binds the
// LXCFS files over it. LXCFS answers each read from the reading process'
// cgroup, so a nested container sees the instance's limits (or its own, if
// lower) instead of the host's memory and CPUs.
//
// The files are cloned while still in the host's mount namespace, where LXCFS
// is mounted. proc shows the PID namespace of the task that mounts it, so the
// mount is made from a child forked after joining the caller's user and PID
// namespaces, with the caller's root, working directory and credentials: the
// kernel applies to it the same checks it applies to the caller's own mount.
static void proc_lxcfs_mount(pid_t pid, int pidfd, int ns_fd)
{
	__do_close int lxcfs_fd = -EBADF, root_fd = -EBADF, cwd_fd = -EBADF, target_fd = -EBADF;
	int fds[ARRAY_SIZE(proc_lxcfs_files)];
	char buf[PATH_MAX];
	const char *source, *target, *lxcfs_dir, *data;
	unsigned long flags;
	unsigned int attrs = 0;
	uid_t nsuid, nsfsuid;
	gid_t nsgid, nsfsgid;
	cap_t caps;
	cap_flag_value_t flag;
	pid_t pid_mount;
	int ret, status;

	nsuid = atoi(advance_arg(true));
	nsgid = atoi(advance_arg(true));
	nsfsuid = atoi(advance_arg(true));
	nsfsgid = atoi(advance_arg(true));
	source = advance_arg(true);
	target = advance_arg(true);
	flags = strtoul(advance_arg(true), NULL, 10);
	lxcfs_dir = advance_arg(true);
	data = advance_arg(false);
	if (data && *data == '\0')
		data = NULL;

	// The IDs are the caller's in the instance's user namespace. A caller in
	// a user namespace nested below it (a rootless runtime, unshare -U) is
	// left to the kernel.
	if (!userns_is_child(ns_fd))
		_exit(PROC_LXCFS_SKIPPED);

	// Read before joining the caller's PID namespace, where its host PID means nothing.
	caps = cap_get_pid(pid);
	if (!caps)
		die("error: failed to get caller capabilities");

	// The kernel refuses such a caller itself; that's no failure to record.
	if (cap_get_flag(caps, CAP_SYS_ADMIN, CAP_EFFECTIVE, &flag) != 0 || flag != CAP_SET)
		_exit(PROC_LXCFS_SKIPPED);

	// Runtimes such as runc mount on /proc/self/fd/N or /proc/thread-self/fd/N,
	// which name the caller's descriptors, not this process'. Open exactly
	// that descriptor through the host's /proc (the caller's thread ID is
	// what the notification carries) and mount on it instead. No other
	// /proc/self path is resolved here, as host root; those are left to the
	// kernel.
	if (strncmp(target, "/proc/self/", 11) == 0 || strncmp(target, "/proc/thread-self/", 18) == 0) {
		const char *fd_str = strchr(target + 6, '/') + 1;
		char *end = NULL;
		long fd_nr;

		if (strncmp(fd_str, "fd/", 3) != 0)
			_exit(PROC_LXCFS_SKIPPED);

		errno = 0;
		fd_nr = strtol(fd_str + 3, &end, 10);
		if (errno || end == fd_str + 3 || *end != '\0' || fd_nr < 0 || fd_nr > INT_MAX)
			_exit(PROC_LXCFS_SKIPPED);

		snprintf(buf, sizeof(buf), "/proc/%d/fd/%ld", pid, fd_nr);
		target_fd = open(buf, O_PATH | O_DIRECTORY | O_CLOEXEC);
		if (target_fd < 0)
			_exit(PROC_LXCFS_SKIPPED);
	}

	// The magic number old callers put in the high bits carries no flags.
	if ((flags & MS_MGC_MSK) == MS_MGC_VAL)
		flags &= ~MS_MGC_MSK;

	if (mnt_attributes_new(flags & ~(MS_SILENT | MS_REC), &attrs))
		_exit(PROC_LXCFS_SKIPPED);

	lxcfs_fd = open(lxcfs_dir, O_PATH | O_DIRECTORY | O_CLOEXEC);
	if (lxcfs_fd < 0)
		die("error: failed to open %s", lxcfs_dir);

	for (int i = 0; i < ARRAY_SIZE(proc_lxcfs_files); i++) {
		fds[i] = incus_open_tree(lxcfs_fd, proc_lxcfs_files[i],
					 OPEN_TREE_CLONE | OPEN_TREE_CLOEXEC | AT_SYMLINK_NOFOLLOW);
		if (fds[i] < 0 && errno != ENOENT)
			die("error: failed to clone %s/%s", lxcfs_dir, proc_lxcfs_files[i]);
	}

	// Through the host's /proc, before joining the caller's namespaces.
	snprintf(buf, sizeof(buf), "/proc/%d/root", pid);
	root_fd = open(buf, O_PATH | O_RDONLY | O_CLOEXEC | O_NOFOLLOW);
	if (root_fd < 0)
		die("error: failed to open caller's root");

	snprintf(buf, sizeof(buf), "/proc/%d/cwd", pid);
	cwd_fd = open(buf, O_PATH | O_RDONLY | O_CLOEXEC);
	if (cwd_fd < 0)
		die("error: failed to open caller's working directory");

	// Not traceable from the instance once in its namespaces.
	if (prctl(PR_SET_DUMPABLE, 0))
		die("error: failed to make the mount process undumpable");

	attach_userns_fd(ns_fd);

	if (!change_namespaces(pidfd, ns_fd, CLONE_NEWPID))
		die("error: failed to change to caller's PID namespace");

	pid_mount = fork();
	if (pid_mount < 0)
		die("error: failed to fork");

	if (pid_mount == 0) {
		__do_close int fs_fd = -EBADF, mnt_fd = -EBADF;

		if (!reacquire_basic_creds(pidfd, ns_fd, root_fd, cwd_fd))
			die("error: failed to acquire basic creds");

		if (!set_final_creds(caps, nsuid, nsgid, nsfsuid, nsfsgid))
			die("error: failed to acquire final creds");

		// This process is now visible in the caller's PID namespace and
		// holds descriptors opened as host root; keep the instance from
		// tracing it. Changing the IDs above reset the flag, so set it here.
		if (prctl(PR_SET_DUMPABLE, 0))
			die("error: failed to make the mount process undumpable");

		// The new file system takes the PID and user namespaces of the
		// process creating it, i.e. the caller's.
		fs_fd = incus_fsopen("proc", FSOPEN_CLOEXEC);
		if (fs_fd < 0)
			die("error: failed to create proc file system: fsopen");

		// Failures that follow from the caller's own arguments (source,
		// options, target) are left to the kernel, which answers the
		// caller's mount with its own error; they aren't for the operator.
		ret = incus_fsconfig(fs_fd, FSCONFIG_SET_STRING, "source", source, 0);
		if (ret < 0)
			_exit(PROC_LXCFS_SKIPPED);

		if (data && !fsconfig_set_options(fs_fd, data))
			_exit(PROC_LXCFS_SKIPPED);

		ret = incus_fsconfig(fs_fd, FSCONFIG_CMD_CREATE, NULL, NULL, 0);
		if (ret < 0)
			_exit(PROC_LXCFS_SKIPPED);

		mnt_fd = incus_fsmount(fs_fd, FSMOUNT_CLOEXEC, attrs);
		if (mnt_fd < 0)
			die("error: failed to create proc mount");

		if (target_fd >= 0)
			ret = incus_move_mount(mnt_fd, "", target_fd, "",
					       MOVE_MOUNT_F_EMPTY_PATH | MOVE_MOUNT_T_EMPTY_PATH);
		else
			ret = incus_move_mount(mnt_fd, "", AT_FDCWD, target, MOVE_MOUNT_F_EMPTY_PATH);
		if (ret)
			_exit(PROC_LXCFS_SKIPPED);

		// mnt_fd keeps naming the root of the now attached mount.
		for (int i = 0; i < ARRAY_SIZE(proc_lxcfs_files); i++) {
			if (fds[i] < 0)
				continue;

			// A proc mounted with subset=pid has no such file to cover.
			ret = incus_move_mount(fds[i], "", mnt_fd, proc_lxcfs_files[i],
					       MOVE_MOUNT_F_EMPTY_PATH);
			if (ret && errno != ENOENT)
				die("error: failed to bind LXCFS %s", proc_lxcfs_files[i]);
		}

		_exit(EXIT_SUCCESS);
	}

	ret = waitpid(pid_mount, &status, 0);
	if (ret == pid_mount && WIFEXITED(status) && WEXITSTATUS(status) == PROC_LXCFS_SKIPPED)
		_exit(PROC_LXCFS_SKIPPED);

	if (ret != pid_mount || !WIFEXITED(status) || WEXITSTATUS(status))
		_exit(EXIT_FAILURE);
}

static void mount_emulate(void)
{
	__do_close int fd_mntns = -EBADF, fd_userns = -EBADF, pidfd = -EBADF,
		       ns_fd = -EBADF, root_fd = -EBADF, cwd_fd = -EBADF;
	char *source = NULL, *shift = NULL, *target = NULL, *fstype = NULL;
	char *helper = NULL, *opts = NULL;
	bool use_fuse, use_fuseblk;
	unsigned int dev_major = 0, dev_minor = 0;
	int fd_tree = -EBADF;
	uid_t nsuid = -1, uid = -1, nsfsuid = -1, fsuid = -1;
	gid_t nsgid = -1, gid = -1, nsfsgid = -1, fsgid = -1;
	int ret;
	pid_t pid = -1;
	unsigned long flags = 0;
	const void *data;

	pid = atoi(advance_arg(true));
	pidfd = atoi(advance_arg(true));
	ns_fd = pidfd_nsfd(pidfd, pid);
	if (ns_fd < 0) {
		fprintf(stderr, "mount: failed to resolve namespace fd: %d", errno);
		_exit(EXIT_FAILURE);
	}

	ret = atoi(advance_arg(true));
	if (ret == 3) {
		proc_lxcfs_mount(pid, pidfd, ns_fd);
		return;
	}

	if (ret == 4) {
		target = advance_arg(true);
		flags = strtoul(advance_arg(true), NULL, 10);
		dev_major = strtoul(advance_arg(true), NULL, 10);
		dev_minor = strtoul(advance_arg(true), NULL, 10);
		fuseblk_remount(pid, ns_fd, target, flags, dev_major, dev_minor);
		return;
	}

	use_fuse = (ret == 1);
	use_fuseblk = (ret == 2);
	if (!use_fuse && !use_fuseblk) {
		source = advance_arg(true);
		target = advance_arg(true);
		fstype = advance_arg(true);
		flags = atoi(advance_arg(true));
		shift = advance_arg(true);
	}

	uid = atoi(advance_arg(true));
	gid = atoi(advance_arg(true));
	fsuid = atoi(advance_arg(true));
	fsgid = atoi(advance_arg(true));
	if (use_fuseblk) {
		helper = advance_arg(true);
		source = advance_arg(true);
		target = advance_arg(true);
		opts = advance_arg(true);
		flags = atoi(advance_arg(true));
		dev_major = strtoul(advance_arg(true), NULL, 10);
		dev_minor = strtoul(advance_arg(true), NULL, 10);
	} else if (!use_fuse) {
		nsuid = atoi(advance_arg(true));
		nsgid = atoi(advance_arg(true));
		nsfsuid = atoi(advance_arg(true));
		nsfsgid = atoi(advance_arg(true));
		data = advance_arg(false);
	}

	fd_userns = preserve_ns(-ESRCH, ns_fd, "user");
	if (fd_userns < 0) {
		fprintf(stderr, "mount: failed to preserve user namespace: %d", errno);
		_exit(EXIT_FAILURE);
	}

	fd_mntns = preserve_ns(getpid(), ns_fd, "mnt");
	if (fd_mntns < 0) {
		fprintf(stderr, "mount: failed to preserve mount namespace: %d", errno);
		_exit(EXIT_FAILURE);
	}

	if (use_fuseblk)
		fd_tree = fuseblk_create_tree(pid, pidfd, ns_fd, fd_userns,
					      uid, gid, fsuid, fsgid, helper, source, opts,
					      flags, dev_major, dev_minor);

	if (use_fuse) {
		attach_userns_fd(ns_fd);

		// Attach to pid namespace so that if we spawn a fuse daemon
		// it'll belong to the correct pid namespace and dies with the
		// container.
		change_namespaces(pidfd, ns_fd, CLONE_NEWPID);
	}

	if (!acquire_basic_creds(pid, pidfd, ns_fd, &root_fd, &cwd_fd)) {
		fprintf(stderr, "mount: failed to acquire basic credentials: %d", errno);
		_exit(EXIT_FAILURE);
	}

	// The fuseblk IDs are the caller's namespace IDs, taken on once the
	// mount is attached from within the container's user namespace.
	if (!use_fuseblk && !acquire_final_creds(pid, uid, gid, fsuid, fsgid)) {
		fprintf(stderr, "mount: failed to acquire final credentials: %d", errno);
		_exit(EXIT_FAILURE);
	}

	if (use_fuseblk) {
		attach_detached_mount(fd_tree, fd_mntns, pid, pidfd, ns_fd, root_fd, cwd_fd,
				      uid, gid, fsuid, fsgid, target);
	} else if (use_fuse) {
		int status;
		pid_t pid_fuse;

		pid_fuse = fork();
		if (pid_fuse < 0) {
			fprintf(stderr, "mount: failed to fork FUSE helper: %d", errno);
			_exit(EXIT_FAILURE);
		}

		if (pid_fuse == 0) {
			const char *fuse_source, *fuse_target, *fuse_opts;

			fuse_source = advance_arg(true);
			fuse_target = advance_arg(true);
			fuse_opts = advance_arg(true);

			if (strcmp(fuse_opts, "") == 0)
				execlp("mount.fuse", "mount.fuse", fuse_source, fuse_target, (char *) NULL);
			else
				execlp("mount.fuse", "mount.fuse", fuse_source, fuse_target, "-o", fuse_opts, (char *) NULL);
			fprintf(stderr, "mount: failed to execute mount.fuse: %d", errno);
			_exit(EXIT_FAILURE);
		}

		ret = waitpid(pid_fuse, &status, 0);
		if ((ret != pid_fuse) || !WIFEXITED(status) || WEXITSTATUS(status)) {
			fprintf(stderr, "mount: FUSE helper failed: status=%d errno=%d", status, errno);
			_exit(EXIT_FAILURE);
		}
	} else if (strcmp(shift, "idmapped") == 0) {
		int fs_fd = -EBADF;

		struct lxc_mount_attr attr = {
		    .attr_set		= MOUNT_ATTR_IDMAP,
		};

		fs_fd = incus_fsopen(fstype, FSOPEN_CLOEXEC);
		if (fs_fd < 0)
			die("error: failed to create detached idmapped mount: fsopen");

		ret = incus_fsconfig(fs_fd, FSCONFIG_SET_STRING, "source", source, 0);
		if (ret < 0)
			die("error: failed to create detached idmapped mount: fsconfig");

		if (data && strcmp((const char *)data, "") != 0) {
			char *buf, *cur, *tok;

			buf = strdup((const char *)data);
			if (!buf)
				die("error: failed to allocate memory for mount data");

			for (cur = buf; (tok = strsep(&cur, ",")); ) {
				char *val;

				if (*tok == '\0')
					continue;

				val = strchr(tok, '=');
				if (val) {
					*val = '\0';
					val++;
				}

				if (val && *val != '\0') {
					ret = incus_fsconfig(fs_fd, FSCONFIG_SET_STRING, tok, val, 0);
				} else {
					ret = incus_fsconfig(fs_fd, FSCONFIG_SET_FLAG, tok, NULL, 0);
				}

				if (ret < 0)
					die("error: failed to create detached idmapped mount: fsconfig");
			}

			free(buf);
		}

		ret = incus_fsconfig(fs_fd, FSCONFIG_CMD_CREATE, NULL, NULL, 0);
		if (ret < 0)
			die("error: failed to create detached idmapped mount: fsconfig");

		fd_tree = incus_fsmount(fs_fd, FSMOUNT_CLOEXEC, flags);
		if (fd_tree < 0)
			die("error: failed to create detached idmapped mount: fsmount");

		attr.userns_fd = fd_userns;
		ret = incus_mount_setattr(fd_tree, "", AT_EMPTY_PATH, &attr, sizeof(attr));
		if (ret < 0)
			die("error: failed to create detached idmapped mount");

		attach_detached_mount(fd_tree, fd_mntns, pid, pidfd, ns_fd, root_fd, cwd_fd,
				      nsuid, nsgid, nsfsuid, nsfsgid, target);
	} else {
		if (mount(source, target, fstype, flags, data) < 0)
			_exit(EXIT_FAILURE);
	}
}

static bool incus_cap_is_set(cap_t caps, cap_value_t cap, cap_flag_t flag)
{
	int ret;
	cap_flag_value_t flagval;

	ret = cap_get_flag(caps, cap, flag, &flagval);
	if (ret < 0)
		return false;

	return flagval == CAP_SET;
}

static void sched_setscheduler_emulate(void)
{
	__do_close int pidfd = -EBADF, ns_fd = -EBADF;
	pid_t pid_caller = -ESRCH, pid_target = -ESRCH;
	int policy = -1, sched_priority = -1;
	int switch_pidns = 0;
	struct sched_param param = {};
	cap_t caps;

	pid_caller = atoi(advance_arg(true));

	pidfd = atoi(advance_arg(true));
	ns_fd = pidfd_nsfd(pidfd, pid_caller);
	if (ns_fd < 0)
		_exit(EXIT_FAILURE);

	switch_pidns = atoi(advance_arg(true));

	pid_target = atoi(advance_arg(true));
	if (pid_target < 0)
		_exit(EXIT_FAILURE);

	policy = atoi(advance_arg(true));
	if (policy < 0)
		_exit(EXIT_FAILURE);

	sched_priority = atoi(advance_arg(true));
	if (sched_priority < 0)
		_exit(EXIT_FAILURE);

	param.sched_priority = sched_priority;

	caps = cap_get_pid(pid_caller);
	if (!caps)
		_exit(EXIT_FAILURE);

	if (!incus_cap_is_set(caps, CAP_SYS_NICE, CAP_EFFECTIVE))
		_exit(EXIT_FAILURE);

	if (switch_pidns && !change_namespaces(pidfd, ns_fd, CLONE_NEWPID))
		_exit(EXIT_FAILURE);

	if (sched_setscheduler(pid_target, policy, &param))
		_exit(EXIT_FAILURE);
}

void forksyscall(void)
{
	char *syscall = NULL;

	// Check that we're root
	if (geteuid() != 0)
		_exit(EXIT_FAILURE);

	// Get the subcommand
	syscall = advance_arg(false);
	if (syscall == NULL ||
	    (strcmp(syscall, "--help") == 0 ||
	     strcmp(syscall, "--version") == 0 || strcmp(syscall, "-h") == 0))
		_exit(EXIT_SUCCESS);

	if (strcmp(syscall, "mknod") == 0)
		mknod_emulate();
	else if (strcmp(syscall, "sched_setscheduler") == 0)
		sched_setscheduler_emulate();
	else if (strcmp(syscall, "setxattr") == 0)
		setxattr_emulate();
	else if (strcmp(syscall, "mount") == 0)
		mount_emulate();
	else
		_exit(EXIT_FAILURE);

	_exit(EXIT_SUCCESS);
}
*/
import "C"

import (
	"errors"

	"github.com/spf13/cobra"

	// Used by cgo.
	_ "github.com/lxc/incus/v7/shared/cgo"
)

type cmdForksyscall struct {
	global *cmdGlobal
}

func (c *cmdForksyscall) command() *cobra.Command {
	// Main subcommand
	cmd := &cobra.Command{}
	cmd.Use = "forksyscall <syscall> <PID> <PidFd> [...]"
	cmd.Short = "Perform syscall operations"
	cmd.Long = `Description:
  Perform syscall operations

  This set of internal commands is used for all seccomp-based container syscall
  operations.
`
	cmd.RunE = c.run
	cmd.Hidden = true

	return cmd
}

func (c *cmdForksyscall) run(_ *cobra.Command, _ []string) error {
	return errors.New("This command should have been intercepted in cgo")
}

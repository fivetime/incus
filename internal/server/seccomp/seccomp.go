//go:build linux && cgo

package seccomp

/*
#ifndef _GNU_SOURCE
#define _GNU_SOURCE 1
#endif
#include <elf.h>
#include <errno.h>
#include <fcntl.h>
#include <linux/seccomp.h>
#include <linux/types.h>
#include <linux/kdev_t.h>
#include <stdbool.h>
#include <stdio.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mount.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/sysmacros.h>
#include <sys/types.h>
#include <unistd.h>

#include "../../../shared/cgo/incus_bpf.h"
#include "../../../shared/cgo/incus_seccomp.h"
#include "../../../shared/cgo/memory_utils.h"
#include "../../../shared/cgo/process_utils.h"
#include "../../../shared/cgo/syscall_wrappers.h"

struct seccomp_notif_sizes expected_sizes;

struct seccomp_notify_proxy_msg {
	uint64_t __reserved;
	pid_t monitor_pid;
	pid_t init_pid;
	struct seccomp_notif_sizes sizes;
	uint64_t cookie_len;
	// followed by: seccomp_notif, seccomp_notif_resp, cookie
};

#define INCUS_SCHED_PARAM_SIZE (sizeof(struct sched_param))
#define SECCOMP_PROXY_MSG_SIZE (sizeof(struct seccomp_notify_proxy_msg))
#define SECCOMP_NOTIFY_SIZE (sizeof(struct seccomp_notif))
#define SECCOMP_RESPONSE_SIZE (sizeof(struct seccomp_notif_resp))
#define SECCOMP_MSG_SIZE_MIN (SECCOMP_PROXY_MSG_SIZE + SECCOMP_NOTIFY_SIZE + SECCOMP_RESPONSE_SIZE)
#define SECCOMP_COOKIE_SIZE (64 * sizeof(char))
#define SECCOMP_MSG_SIZE_MAX (SECCOMP_MSG_SIZE_MIN + SECCOMP_COOKIE_SIZE)

static int seccomp_notify_get_sizes(struct seccomp_notif_sizes *sizes)
{
	if (syscall(SYS_seccomp, SECCOMP_GET_NOTIF_SIZES, 0, sizes) != 0)
		return -1;

	if (sizes->seccomp_notif != sizeof(struct seccomp_notif) ||
	    sizes->seccomp_notif_resp != sizeof(struct seccomp_notif_resp) ||
	    sizes->seccomp_data != sizeof(struct seccomp_data))
		return -1;

	return 0;
}

static int device_allowed(dev_t dev, mode_t mode)
{
	switch (mode & S_IFMT) {
	case S_IFCHR:
		if (dev == makedev(0, 0)) // whiteout
			return 0;
		else if (dev == makedev(5, 1)) // /dev/console
			return 0;
		else if (dev == makedev(1, 7)) // /dev/full
			return 0;
		else if (dev == makedev(1, 3)) // /dev/null
			return 0;
		else if (dev == makedev(1, 8)) // /dev/random
			return 0;
		else if (dev == makedev(5, 0)) // /dev/tty
			return 0;
		else if (dev == makedev(1, 9)) // /dev/urandom
			return 0;
		else if (dev == makedev(1, 5)) // /dev/zero
			return 0;
	}

	return -EPERM;
}

#include <linux/audit.h>

struct incus_seccomp_data_arch {
	int arch;
	int nr_mknod;
	int nr_mknodat;
	int nr_setxattr;
	int nr_mount;
	int nr_bpf;
	int nr_sched_setscheduler;
	int nr_sysinfo;
	int nr_ioctl;
};

#define INCUS_SECCOMP_NOTIFY_MKNOD    0
#define INCUS_SECCOMP_NOTIFY_MKNODAT  1
#define INCUS_SECCOMP_NOTIFY_SETXATTR 2
#define INCUS_SECCOMP_NOTIFY_MOUNT 3
#define INCUS_SECCOMP_NOTIFY_BPF 4
#define INCUS_SECCOMP_NOTIFY_SCHED_SETSCHEDULER 5
#define INCUS_SECCOMP_NOTIFY_SYSINFO 6
#define INCUS_SECCOMP_NOTIFY_IOCTL 7

// ordered by likelihood of usage...
static const struct incus_seccomp_data_arch seccomp_notify_syscall_table[] = {
	{ -1, INCUS_SECCOMP_NOTIFY_MKNOD, INCUS_SECCOMP_NOTIFY_MKNODAT, INCUS_SECCOMP_NOTIFY_SETXATTR, INCUS_SECCOMP_NOTIFY_MOUNT, INCUS_SECCOMP_NOTIFY_BPF, INCUS_SECCOMP_NOTIFY_SCHED_SETSCHEDULER, INCUS_SECCOMP_NOTIFY_SYSINFO, INCUS_SECCOMP_NOTIFY_IOCTL},
#ifdef AUDIT_ARCH_X86_64
	{ AUDIT_ARCH_X86_64,      133, 259, 188, 165, 321, 144, 99, 16 },
#endif
#ifdef AUDIT_ARCH_I386
	{ AUDIT_ARCH_I386,         14, 297, 226,  21, 357, 156, 116, 54 },
#endif
#ifdef AUDIT_ARCH_AARCH64
	{ AUDIT_ARCH_AARCH64,      -1,  33,   5,  40, 280, 119, 179, 29 },
#endif
#ifdef AUDIT_ARCH_ARM
	{ AUDIT_ARCH_ARM,          14, 324, 226,  21, 386, 156, 116, 54 },
#endif
#ifdef AUDIT_ARCH_ARMEB
	{ AUDIT_ARCH_ARMEB,        14, 324, 226,  21, 386, 156, 116, 54 },
#endif
#ifdef AUDIT_ARCH_S390
	{ AUDIT_ARCH_S390,         14, 290, 224,  21, 351, 156, 116, 54 },
#endif
#ifdef AUDIT_ARCH_S390X
	{ AUDIT_ARCH_S390X,        14, 290, 224,  21, 351, 156, 116, 54 },
#endif
#ifdef AUDIT_ARCH_PPC
	{ AUDIT_ARCH_PPC,          14, 288, 209,  21, 361, 156, 116, 54 },
#endif
#ifdef AUDIT_ARCH_PPC64
	{ AUDIT_ARCH_PPC64,        14, 288, 209,  21, 361, 156, 116, 54 },
#endif
#ifdef AUDIT_ARCH_PPC64LE
	{ AUDIT_ARCH_PPC64LE,      14, 288, 209,  21, 361, 156, 116, 54 },
#endif
#ifdef AUDIT_ARCH_RISCV64
	{ AUDIT_ARCH_RISCV64,      -1,  33,   5,  40, 280, 119, 179, 29 },
#endif
#ifdef AUDIT_ARCH_SPARC
	{ AUDIT_ARCH_SPARC,        14, 286, 169, 167, 349, 243, 214, 54 },
#endif
#ifdef AUDIT_ARCH_SPARC64
	{ AUDIT_ARCH_SPARC64,      14, 286, 169, 167, 349, 243, 214, 54 },
#endif
#ifdef AUDIT_ARCH_MIPS
	{ AUDIT_ARCH_MIPS,         14, 290, 224,  21,  -1, 141, 4116, 4054 },
#endif
#ifdef AUDIT_ARCH_MIPSEL
	{ AUDIT_ARCH_MIPSEL,       14, 290, 224,  21,  -1, 141, 4116, 4054 },
#endif
#ifdef AUDIT_ARCH_MIPS64
	{ AUDIT_ARCH_MIPS64,      131, 249, 180, 160,  -1, 141, 5097, 5054 },
#endif
#ifdef AUDIT_ARCH_MIPS64N32
	{ AUDIT_ARCH_MIPS64N32,   131, 253, 180, 160,  -1, 141, 4116, 6054 },
#endif
#ifdef AUDIT_ARCH_MIPSEL64
	{ AUDIT_ARCH_MIPSEL64,    131, 249, 180, 160,  -1, 141, 5097, 5054 },
#endif
#ifdef AUDIT_ARCH_MIPSEL64N32
	{ AUDIT_ARCH_MIPSEL64N32, 131, 253, 180, 160,  -1, 141, 4116, 6054 },
#endif
#ifdef AUDIT_ARCH_LOONGARCH64
	{ AUDIT_ARCH_LOONGARCH64, -1,  33,   5,  40, 280, 119, 179, 29 },
#endif
};

static int seccomp_notify_get_syscall(struct seccomp_notif *req,
				      struct seccomp_notif_resp *resp)
{
	resp->id = req->id;
	resp->flags = req->flags;
	resp->val = 0;
	resp->error = 0;

	for (size_t i = 0; i < (sizeof(seccomp_notify_syscall_table) /
				sizeof(seccomp_notify_syscall_table[0]));
	     i++) {
		const struct incus_seccomp_data_arch *entry = &seccomp_notify_syscall_table[i];

		if (entry->arch != req->data.arch)
			continue;

		if (entry->nr_mknod == req->data.nr)
			return INCUS_SECCOMP_NOTIFY_MKNOD;

		if (entry->nr_mknodat == req->data.nr)
			return INCUS_SECCOMP_NOTIFY_MKNODAT;

		if (entry->nr_setxattr == req->data.nr)
			return INCUS_SECCOMP_NOTIFY_SETXATTR;

		if (entry->nr_mount == req->data.nr)
			return INCUS_SECCOMP_NOTIFY_MOUNT;

		if (entry->nr_bpf == req->data.nr)
			return INCUS_SECCOMP_NOTIFY_BPF;

		if (entry->nr_sched_setscheduler == req->data.nr)
			return INCUS_SECCOMP_NOTIFY_SCHED_SETSCHEDULER;

		if (entry->nr_sysinfo == req->data.nr)
			return INCUS_SECCOMP_NOTIFY_SYSINFO;

		if (entry->nr_ioctl == req->data.nr)
			return INCUS_SECCOMP_NOTIFY_IOCTL;

		break;
	}

	errno = EINVAL;
	return -EINVAL;
}

// Whether the task that sent the notification is still waiting for the answer.
static int seccomp_notify_id_valid(int notify_fd, struct seccomp_notif *req)
{
	return ioctl(notify_fd, SECCOMP_IOCTL_NOTIF_ID_VALID, &req->id);
}

static void seccomp_notify_update_response(struct seccomp_notif_resp *resp,
					   int new_neg_errno, uint32_t flags)
{
	resp->error = new_neg_errno;
	resp->flags |= flags;
}

static void prepare_seccomp_iovec(struct iovec *iov,
				  struct seccomp_notify_proxy_msg *msg,
				  struct seccomp_notif *notif,
				  struct seccomp_notif_resp *resp, char *cookie)
{
	iov[0].iov_base = msg;
	iov[0].iov_len = SECCOMP_PROXY_MSG_SIZE;

	iov[1].iov_base = notif;
	iov[1].iov_len = SECCOMP_NOTIFY_SIZE;

	iov[2].iov_base = resp;
	iov[2].iov_len = SECCOMP_RESPONSE_SIZE;

	iov[3].iov_base = cookie;
	iov[3].iov_len = SECCOMP_COOKIE_SIZE;
}

// We use the BPF_DEVCG_DEV_CHAR macro as a cheap way to detect whether the kernel has
// the correct headers available to be compiled for bpf support. Since cgo doesn't have
// a good way of letting us probe for structs or enums the alternative would be to vendor
// bpf.h similar to what we do for seccomp itself. But that's annoying since bpf.h is quite
// large. So users that want bpf interception support should make sure to have the relevant
// header available at build time.
static inline int pidfd_getfd(int pidfd, int fd, int flags)
{
	return syscall(__NR_pidfd_getfd, pidfd, fd, flags);
}

#define ptr_to_u64(p) ((__u64)((uintptr_t)(p)))

static inline int bpf(int cmd, union bpf_attr *attr, size_t size)
{
	return syscall(__NR_bpf, cmd, attr, size);
}

static int handle_bpf_syscall(pid_t pid_target, int notify_fd, int mem_fd,
			      int tgid, struct seccomp_notify_proxy_msg *msg,
			      struct seccomp_notif *req, struct seccomp_notif_resp *resp,
			      int *bpf_cmd, int *bpf_prog_type, int *bpf_attach_type,
			      unsigned int flags)
{
	__do_close int pidfd = -EBADF, bpf_target_fd = -EBADF, bpf_attach_fd = -EBADF,
		       bpf_prog_fd = -EBADF;
	__do_free struct bpf_insn *insn = NULL;
	char log_buf[4096] = {};
	char license[128] = {};
	size_t insn_size = 0;
	union bpf_attr attr = {}, new_attr = {};
	unsigned int attr_len = sizeof(attr);
	struct seccomp_notif_addfd addfd = {};
	int ret;
	int cmd;

	*bpf_cmd		= -EINVAL;
	*bpf_prog_type		= -EINVAL;
	*bpf_attach_type	= -EINVAL;

	if (attr_len < req->data.args[2])
		return -EFBIG;
	attr_len = req->data.args[2];

	*bpf_cmd = req->data.args[0];
	switch (req->data.args[0]) {
	case BPF_PROG_LOAD:
		cmd = BPF_PROG_LOAD;
		break;
	case BPF_PROG_ATTACH:
		cmd = BPF_PROG_ATTACH;
		break;
	case BPF_PROG_DETACH:
		cmd = BPF_PROG_DETACH;
		break;
	default:
		return -EINVAL;
	}

	ret = pread(mem_fd, &attr, attr_len, req->data.args[1]);
	if (ret < 0)
		return -errno;

	*bpf_prog_type = attr.prog_type;

	if (flags & PIDFD_THREAD)
		pidfd = incus_pidfd_open(pid_target, PIDFD_THREAD);
	else
		pidfd = incus_pidfd_open(tgid, 0);
	if (pidfd < 0)
		return -errno;

	if (ioctl(notify_fd, SECCOMP_IOCTL_NOTIF_ID_VALID, &req->id))
		return -errno;

	switch (cmd) {
	case BPF_PROG_LOAD:
		if (attr.prog_type != BPF_PROG_TYPE_CGROUP_DEVICE)
			return -EINVAL;

		// bpf is currently limited to 1 million instructions. Don't
		// allow the container to allocate more than that.
		if (attr.insn_cnt > 1000000)
			return -EINVAL;

		insn_size = sizeof(struct bpf_insn) * attr.insn_cnt;

		insn = malloc(insn_size);
		if (!insn)
			return -ENOMEM;

		ret = pread(mem_fd, insn, insn_size, attr.insns);
		if (ret < 0)
			return -errno;
		if (ret != insn_size)
			return -EIO;

		memcpy(&new_attr, &attr, sizeof(attr));

		if (attr.log_size > sizeof(log_buf))
			new_attr.log_size = sizeof(log_buf);

		if (new_attr.log_size > 0)
			new_attr.log_buf = ptr_to_u64(log_buf);

		if (attr.license && pread(mem_fd, license, sizeof(license), attr.license) < 0)
			return -errno;

		new_attr.insns		= ptr_to_u64(insn);
		new_attr.license	= ptr_to_u64(license);
		bpf_prog_fd = bpf(cmd, &new_attr, sizeof(new_attr));
		if (bpf_prog_fd < 0) {
			int saved_errno = errno;

			if ((new_attr.log_size) > 0 && (pwrite(mem_fd, log_buf, new_attr.log_size,
							       attr.log_buf) != new_attr.log_size))
				errno = saved_errno;
			return -errno;
		}

		addfd.srcfd	= bpf_prog_fd;
		addfd.id	= req->id;
		addfd.flags	= 0;
		ret = ioctl(notify_fd, SECCOMP_IOCTL_NOTIF_ADDFD, &addfd);
		if (ret < 0)
			return -errno;

		resp->val = ret;
		ret = 0;
		break;
	case BPF_PROG_ATTACH:
		if (attr.attach_type != BPF_CGROUP_DEVICE)
			return -EINVAL;

		*bpf_attach_type = attr.attach_type;

		bpf_target_fd = pidfd_getfd(pidfd, attr.target_fd, 0);
		if (bpf_target_fd < 0)
			return -errno;

		bpf_attach_fd = pidfd_getfd(pidfd, attr.attach_bpf_fd, 0);
		if (bpf_attach_fd < 0)
			return -errno;

		if (!(flags & PIDFD_THREAD) && tgid != pid_target) {
			// Make sure that the file descriptor table is shared
			// so we can be sure that we're talking about the same
			// open files.
			if (!filetable_shared(tgid, pid_target))
				return -EINVAL;

			// Make sure that the threadgroup leader hasn't been
			// recycled in the meantime so we're not accidentally
			// looking at a different threadgroup with the same
			// open files.
			if (!process_still_alive(pidfd))
				return -EINVAL;
		}

		attr.target_fd		= bpf_target_fd;
		attr.attach_bpf_fd	= bpf_attach_fd;
		ret = bpf(cmd, &attr, attr_len);
		break;
	case BPF_PROG_DETACH:
		if (attr.attach_type != BPF_CGROUP_DEVICE)
			return -EINVAL;

		*bpf_attach_type = attr.attach_type;

		bpf_target_fd = pidfd_getfd(pidfd, attr.target_fd, 0);
		if (bpf_target_fd < 0)
			return -errno;

		bpf_attach_fd = pidfd_getfd(pidfd, attr.attach_bpf_fd, 0);
		if (bpf_attach_fd < 0)
			return -errno;

		if (!(flags & PIDFD_THREAD) && tgid != pid_target) {
			// Make sure that the file descriptor table is shared
			// so we can be sure that we're talking about the same
			// open files.
			if (!filetable_shared(tgid, pid_target))
				return -EINVAL;

			// Make sure that the threadgroup leader hasn't been
			// recycled in the meantime so we're not accidentally
			// looking at a different threadgroup with the same
			// open files.
			if (!process_still_alive(pidfd))
				return -EINVAL;
		}

		attr.target_fd		= bpf_target_fd;
		attr.attach_bpf_fd	= bpf_attach_fd;
		ret = bpf(cmd, &attr, attr_len);
		break;
	}

	return ret;
}

#ifndef MS_LAZYTIME
#define MS_LAZYTIME (1<<25)
#endif
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/lxc/incus/v7/internal/linux"
	"github.com/lxc/incus/v7/internal/netutils"
	"github.com/lxc/incus/v7/internal/server/cgroup"
	"github.com/lxc/incus/v7/internal/server/db"
	"github.com/lxc/incus/v7/internal/server/db/cluster"
	"github.com/lxc/incus/v7/internal/server/db/warningtype"
	deviceConfig "github.com/lxc/incus/v7/internal/server/device/config"
	"github.com/lxc/incus/v7/internal/server/project"
	"github.com/lxc/incus/v7/internal/server/state"
	localUtil "github.com/lxc/incus/v7/internal/server/util"
	internalUtil "github.com/lxc/incus/v7/internal/util"
	"github.com/lxc/incus/v7/shared/api"
	_ "github.com/lxc/incus/v7/shared/cgo" // Used by cgo
	"github.com/lxc/incus/v7/shared/idmap"
	"github.com/lxc/incus/v7/shared/logger"
	"github.com/lxc/incus/v7/shared/osarch"
	"github.com/lxc/incus/v7/shared/subprocess"
	"github.com/lxc/incus/v7/shared/util"
)

const (
	incusSeccompNotifyMknod             = C.INCUS_SECCOMP_NOTIFY_MKNOD
	incusSeccompNotifyMknodat           = C.INCUS_SECCOMP_NOTIFY_MKNODAT
	incusSeccompNotifySetxattr          = C.INCUS_SECCOMP_NOTIFY_SETXATTR
	incusSeccompNotifyMount             = C.INCUS_SECCOMP_NOTIFY_MOUNT
	incusSeccompNotifyBpf               = C.INCUS_SECCOMP_NOTIFY_BPF
	incusSeccompNotifySchedSetscheduler = C.INCUS_SECCOMP_NOTIFY_SCHED_SETSCHEDULER
	incusSeccompNotifySysinfo           = C.INCUS_SECCOMP_NOTIFY_SYSINFO
	incusSeccompNotifyIoctl             = C.INCUS_SECCOMP_NOTIFY_IOCTL
)

// ext4IocResizeFs is EXT4_IOC_RESIZE_FS, _IOW('f', 16, __u64), the only ioctl we intercept.
const ext4IocResizeFs = 0x40086610

// capSysResource is CAP_SYS_RESOURCE, which the kernel requires for EXT4_IOC_RESIZE_FS.
const capSysResource = 24

const seccompHeader = `2
`

const defaultSeccompPolicy = `reject_force_umount  # comment this to allow umount -f;  not recommended
[all]
kexec_load errno 38
open_by_handle_at errno 38
init_module errno 38
finit_module errno 38
delete_module errno 38
`

//	8 == SECCOMP_FILTER_FLAG_NEW_LISTENER
//
// 2146435072 == SECCOMP_RET_TRACE .
const seccompNotifyDisallow = `seccomp errno 22 [1,2146435072,SCMP_CMP_MASKED_EQ,2146435072]
seccomp errno 22 [1,8,SCMP_CMP_MASKED_EQ,8]
`

const seccompNotifyMknod = `mknod notify [1,8192,SCMP_CMP_MASKED_EQ,61440]
mknod notify [1,24576,SCMP_CMP_MASKED_EQ,61440]
mknodat notify [2,8192,SCMP_CMP_MASKED_EQ,61440]
mknodat notify [2,24576,SCMP_CMP_MASKED_EQ,61440]
`

const seccompNotifySetxattr = `setxattr notify [3,1,SCMP_CMP_EQ]
`

const seccompNotifySchedSetscheduler = `sched_setscheduler notify
`

const seccompNotifySysinfo = `sysinfo notify
`

const seccompBlockNewMountAPI = `fsopen errno 38
fsconfig errno 38
`

// We don't want to filter any of the following flag combinations since they do
// not cause the creation of a new superblock:
//
// MS_REMOUNT
// MS_BIND
// MS_MOVE
// MS_UNBINDABLE
// MS_PRIVATE
// MS_SLAVE
// MS_SHARED
// MS_KERNMOUNT
// MS_I_VERSION
//
// So define the following mask of allowed flags:
//
// long unsigned int mask = MS_MGC_VAL | MS_RDONLY | MS_NOSUID | MS_NODEV |
//                          MS_NOEXEC | MS_SYNCHRONOUS | MS_MANDLOCK |
//                          MS_DIRSYNC | MS_NOATIME | MS_NODIRATIME | MS_REC |
//                          MS_VERBOSE | MS_SILENT | MS_POSIXACL | MS_RELATIME |
//                          MS_STRICTATIME | MS_LAZYTIME;
//
// Now we inverse the flag:
//
// inverse_mask ~= mask;
//
// Seccomp will now only intercept these flags if they do not contain any of
// the allowed flags, i.e. we only intercept combinations were a new superblock
// is created.

const seccompNotifyMount = `mount notify [3,0,SCMP_CMP_MASKED_EQ,18446744070422410016]
`

// 5 == BPF_PROG_LOAD
// 8 == BPF_PROG_ATTACH
// 9 == BPF_PROG_DETACH .
// Only EXT4_IOC_RESIZE_FS is notified, every other ioctl goes straight through.
const seccompNotifyIoctlResizeFs = `ioctl notify [1,1074292240,SCMP_CMP_EQ]
`

const seccompNotifyBpf = `bpf notify [0,5,SCMP_CMP_EQ]
bpf notify [0,8,SCMP_CMP_EQ]
bpf notify [0,9,SCMP_CMP_EQ]
`

const compatBlockingPolicy = `[%s]
compat_sys_rt_sigaction errno 38
stub_x32_rt_sigreturn errno 38
compat_sys_ioctl errno 38
compat_sys_readv errno 38
compat_sys_writev errno 38
compat_sys_recvfrom errno 38
compat_sys_sendmsg errno 38
compat_sys_recvmsg errno 38
stub_x32_execve errno 38
compat_sys_ptrace errno 38
compat_sys_rt_sigpending errno 38
compat_sys_rt_sigtimedwait errno 38
compat_sys_rt_sigqueueinfo errno 38
compat_sys_sigaltstack errno 38
compat_sys_timer_create errno 38
compat_sys_mq_notify errno 38
compat_sys_kexec_load errno 38
compat_sys_waitid errno 38
compat_sys_set_robust_list errno 38
compat_sys_get_robust_list errno 38
compat_sys_vmsplice errno 38
compat_sys_move_pages errno 38
compat_sys_preadv64 errno 38
compat_sys_pwritev64 errno 38
compat_sys_rt_tgsigqueueinfo errno 38
compat_sys_recvmmsg errno 38
compat_sys_sendmmsg errno 38
compat_sys_process_vm_readv errno 38
compat_sys_process_vm_writev errno 38
compat_sys_setsockopt errno 38
compat_sys_getsockopt errno 38
compat_sys_io_setup errno 38
compat_sys_io_submit errno 38
stub_x32_execveat errno 38
`

// Instance is a seccomp specific instance interface.
// This is used rather than instance.Instance to avoid import loops.
type Instance interface {
	ID() int
	Name() string
	Project() api.Project
	ExpandedConfig() map[string]string
	ExpandedDevices() deviceConfig.Devices
	IsPrivileged() bool
	Architecture() int
	RootfsPath() string
	CGroup() (*cgroup.CGroup, error)
	CurrentIdmap() (*idmap.Set, error)
	DiskIdmap() (*idmap.Set, error)
	IdmappedStorage(path string, fstype string) idmap.StorageType
	InsertSeccompUnixDevice(prefix string, m deviceConfig.Device, pid int) error
}

var seccompPath = internalUtil.VarPath("security", "seccomp")

// ProfilePath returns the seccomp path for the instance.
func ProfilePath(c Instance) string {
	return filepath.Join(seccompPath, project.Instance(c.Project().Name, c.Name()))
}

// InstanceNeedsPolicy returns whether the instance needs a policy or not.
func InstanceNeedsPolicy(c Instance) bool {
	config := c.ExpandedConfig()

	// Check for text keys
	keys := []string{
		"raw.seccomp",
		"security.syscalls.allow",
		"security.syscalls.deny",
		"security.syscalls.whitelist",
		"security.syscalls.blacklist",
	}

	for _, k := range keys {
		_, hasKey := config[k]
		if hasKey {
			return true
		}
	}

	// Check for boolean keys that default to false
	keys = []string{
		"security.syscalls.deny_compat",
		"security.syscalls.blacklist_compat",
		"security.syscalls.intercept.mknod",
		"security.syscalls.intercept.sched_setscheduler",
		"security.syscalls.intercept.setxattr",
		"security.syscalls.intercept.sysinfo",
		"security.syscalls.intercept.mount",
		"security.syscalls.intercept.bpf",
		"security.syscalls.intercept.ioctl.resize_fs",
	}

	for _, k := range keys {
		if util.IsTrue(config[k]) {
			return true
		}
	}

	// Check for boolean keys that default to true
	value, ok := config["security.syscalls.deny_default"]
	if !ok {
		value, ok = config["security.syscalls.blacklist_default"]
	}

	if !ok || util.IsTrue(value) {
		return true
	}

	return false
}

// InstanceNeedsIntercept returns whether instance needs intercept.
func InstanceNeedsIntercept(s *state.State, c Instance) (bool, error) {
	// No need if privileged
	if c.IsPrivileged() {
		return false, nil
	}

	// If nested, assume the host handles it
	if s.OS.RunningInUserNS {
		return false, nil
	}

	config := c.ExpandedConfig()

	keys := []string{
		"security.syscalls.intercept.mknod",
		"security.syscalls.intercept.sched_setscheduler",
		"security.syscalls.intercept.setxattr",
		"security.syscalls.intercept.sysinfo",
		"security.syscalls.intercept.mount",
		"security.syscalls.intercept.bpf",
		"security.syscalls.intercept.ioctl.resize_fs",
	}

	for _, key := range keys {
		if util.IsFalseOrEmpty(config[key]) {
			continue
		}

		return true, nil
	}

	return false, nil
}

// MakePidFd prepares a pidfd to inherit for the init process of the container.
func MakePidFd(pid int) (int, *os.File, error) {
	pidFdFile, err := linux.PidFdOpen(pid, 0)
	if err != nil {
		// Non-leader threads require PIDFD_THREAD (EINVAL before Linux 6.16, ENOENT since).
		if !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOENT) {
			return -1, nil, err
		}

		pidFdFile, err = linux.PidFdOpen(pid, C.PIDFD_THREAD)
		if err != nil {
			return -1, nil, err
		}
	}

	return 3, pidFdFile, nil
}

func seccompGetPolicyContent(s *state.State, c Instance) (string, error) {
	config := c.ExpandedConfig()

	// Full policy override
	raw := config["raw.seccomp"]
	if raw != "" {
		return raw, nil
	}

	// Policy header
	policy := seccompHeader
	allowlist := config["security.syscalls.allow"]
	if allowlist == "" {
		allowlist = config["security.syscalls.whitelist"]
	}

	if allowlist != "" {
		policy += "allowlist\n[all]\n"
		policy += allowlist
	} else {
		policy += "denylist\n[all]\n"

		defaultFlag, ok := config["security.syscalls.deny_default"]
		if !ok {
			defaultFlag, ok = config["security.syscalls.blacklist_default"]
		}

		if !ok || util.IsTrue(defaultFlag) {
			policy += defaultSeccompPolicy
		}
	}

	// Syscall interception
	ok, err := InstanceNeedsIntercept(s, c)
	if err != nil {
		return "", err
	}

	if ok {
		// Prevent the container from overriding our syscall
		// supervision.
		policy += seccompNotifyDisallow

		if util.IsTrue(config["security.syscalls.intercept.mknod"]) {
			policy += seccompNotifyMknod
		}

		if util.IsTrue(config["security.syscalls.intercept.sched_setscheduler"]) {
			policy += seccompNotifySchedSetscheduler
		}

		if util.IsTrue(config["security.syscalls.intercept.setxattr"]) {
			policy += seccompNotifySetxattr
		}

		if util.IsTrue(config["security.syscalls.intercept.sysinfo"]) {
			policy += seccompNotifySysinfo
		}

		if util.IsTrue(config["security.syscalls.intercept.mount"]) {
			policy += seccompNotifyMount
			// We block a subset of the new mount api as we can't easily intercept those mounts.
			// Specifically, we're blocking fsopen and fsconfig which is
			// enough to get /sbin/mount to fallback to the old mount API.
			policy += seccompBlockNewMountAPI
		}

		if util.IsTrue(config["security.syscalls.intercept.bpf"]) {
			policy += seccompNotifyBpf
		}

		if util.IsTrue(config["security.syscalls.intercept.ioctl.resize_fs"]) {
			policy += seccompNotifyIoctlResizeFs
		}
	}

	if allowlist != "" {
		return policy, nil
	}

	// Additional deny entries
	compat, ok := config["security.syscalls.deny_compat"]
	if !ok {
		compat = config["security.syscalls.blacklist_compat"]
	}

	if util.IsTrue(compat) {
		arch, err := osarch.ArchitectureName(c.Architecture())
		if err != nil {
			return "", err
		}

		policy += fmt.Sprintf(compatBlockingPolicy, arch)
	}

	denylist, ok := config["security.syscalls.deny"]
	if !ok {
		denylist = config["security.syscalls.blacklist"]
	}

	if denylist != "" {
		policy += denylist
	}

	return policy, nil
}

// CreateProfile creates a seccomp profile.
func CreateProfile(s *state.State, c Instance) error {
	/* Unlike apparmor, there is no way to "cache" profiles, and profiles
	 * are automatically unloaded when a task dies. Thus, we don't need to
	 * unload them when a container stops, and we don't have to worry about
	 * the mtime on the file for any compiler purpose, so let's just write
	 * out the profile.
	 */
	if !InstanceNeedsPolicy(c) {
		return nil
	}

	profile, err := seccompGetPolicyContent(s, c)
	if err != nil {
		return err
	}

	err = os.MkdirAll(seccompPath, 0o700)
	if err != nil {
		return err
	}

	return os.WriteFile(ProfilePath(c), []byte(profile), 0o600)
}

// DeleteProfile removes a seccomp profile.
func DeleteProfile(c Instance) {
	/* similar to AppArmor, if we've never started this container, the
	 * delete can fail and that's ok.
	 */
	_ = os.Remove(ProfilePath(c))
}

// Server defines a seccomp server.
type Server struct {
	s    *state.State
	path string
	l    net.Listener
}

// Iovec defines an iovec to move data between kernel and userspace.
type Iovec struct {
	ucred    *unix.Ucred
	memFd    int
	procFd   int
	notifyFd int
	msg      *C.struct_seccomp_notify_proxy_msg
	req      *C.struct_seccomp_notif
	resp     *C.struct_seccomp_notif_resp
	cookie   *C.char
	iov      *C.struct_iovec
}

// NewSeccompIovec creates a new seccomp iovec.
func NewSeccompIovec(ucred *unix.Ucred) *Iovec {
	msgPtr := C.malloc(C.sizeof_struct_seccomp_notify_proxy_msg)
	msg := (*C.struct_seccomp_notify_proxy_msg)(msgPtr)
	C.memset(msgPtr, 0, C.sizeof_struct_seccomp_notify_proxy_msg)

	regPtr := C.malloc(C.sizeof_struct_seccomp_notif)
	req := (*C.struct_seccomp_notif)(regPtr)
	C.memset(regPtr, 0, C.sizeof_struct_seccomp_notif)

	respPtr := C.malloc(C.sizeof_struct_seccomp_notif_resp)
	resp := (*C.struct_seccomp_notif_resp)(respPtr)
	C.memset(respPtr, 0, C.sizeof_struct_seccomp_notif_resp)

	cookiePtr := C.malloc(64 * C.sizeof_char)
	cookie := (*C.char)(cookiePtr)
	C.memset(cookiePtr, 0, 64*C.sizeof_char)

	iovUnsafePtr := C.malloc(4 * C.sizeof_struct_iovec)
	iov := (*C.struct_iovec)(iovUnsafePtr)
	C.memset(iovUnsafePtr, 0, 4*C.sizeof_struct_iovec)

	C.prepare_seccomp_iovec(iov, msg, req, resp, cookie)

	return &Iovec{
		memFd:    -1,
		procFd:   -1,
		notifyFd: -1,
		msg:      msg,
		req:      req,
		resp:     resp,
		cookie:   cookie,
		iov:      iov,
		ucred:    ucred,
	}
}

// PutSeccompIovec puts a seccomp iovec.
func (siov *Iovec) PutSeccompIovec() {
	if siov.memFd >= 0 {
		_ = unix.Close(siov.memFd)
	}

	if siov.procFd >= 0 {
		_ = unix.Close(siov.procFd)
	}

	if siov.notifyFd >= 0 {
		_ = unix.Close(siov.notifyFd)
	}

	C.free(unsafe.Pointer(siov.msg))
	C.free(unsafe.Pointer(siov.req))
	C.free(unsafe.Pointer(siov.resp))
	C.free(unsafe.Pointer(siov.cookie))
	C.free(unsafe.Pointer(siov.iov))
}

// ReceiveSeccompIovec receives a seccomp iovec.
func (siov *Iovec) ReceiveSeccompIovec(fd int) (uint64, error) {
	bytes, fds, err := netutils.AbstractUnixReceiveFdData(fd, 3, netutils.UnixFdsAcceptLess, unsafe.Pointer(siov.iov), 4)
	if err != nil {
		return 0, err
	}

	siov.procFd = int(fds[0])
	siov.memFd = int(fds[1])
	siov.notifyFd = int(fds[2])
	logger.Debugf("Syscall handler received fds %d(/proc/<pid>), %d(/proc/<pid>/mem), and %d([seccomp notify])", siov.procFd, siov.memFd, siov.notifyFd)

	return bytes, nil
}

// IsValidSeccompIovec checks whether a seccomp iovec is valid.
func (siov *Iovec) IsValidSeccompIovec(size uint64) bool {
	if size < uint64(C.SECCOMP_MSG_SIZE_MIN) {
		logger.Warnf("Disconnected from seccomp socket after incomplete receive")
		return false
	}

	if siov.msg.__reserved != 0 {
		logger.Warnf("Disconnected from seccomp socket after client sent non-zero reserved field: pid=%v",
			siov.ucred.Pid)
		return false
	}

	if siov.msg.sizes.seccomp_notif != C.expected_sizes.seccomp_notif {
		logger.Warnf("Disconnected from seccomp socket since client uses different seccomp_notif sizes: %d != %d, pid=%v",
			siov.msg.sizes.seccomp_notif, C.expected_sizes.seccomp_notif, siov.ucred.Pid)
		return false
	}

	if siov.msg.sizes.seccomp_notif_resp != C.expected_sizes.seccomp_notif_resp {
		logger.Warnf("Disconnected from seccomp socket since client uses different seccomp_notif_resp sizes: %d != %d, pid=%v",
			siov.msg.sizes.seccomp_notif_resp, C.expected_sizes.seccomp_notif_resp, siov.ucred.Pid)
		return false
	}

	if siov.msg.sizes.seccomp_data != C.expected_sizes.seccomp_data {
		logger.Warnf("Disconnected from seccomp socket since client uses different seccomp_data sizes: %d != %d, pid=%v",
			siov.msg.sizes.seccomp_data, C.expected_sizes.seccomp_data, siov.ucred.Pid)
		return false
	}

	return true
}

// SendSeccompIovec sends seccomp iovec.
func (siov *Iovec) SendSeccompIovec(fd int, errno int, flags uint32) error {
	C.seccomp_notify_update_response(siov.resp, C.int(errno), C.uint32_t(flags))

	msghdr := C.struct_msghdr{}
	msghdr.msg_iov = siov.iov
	msghdr.msg_iovlen = 4 - 1 // without cookie
retry:
	bytes, err := C.sendmsg(C.int(fd), &msghdr, C.MSG_NOSIGNAL)
	if bytes < 0 {
		if err == unix.EINTR {
			logger.Debugf("Caught EINTR, retrying...")
			goto retry
		}

		logger.Debugf("Disconnected from seccomp socket after failed write for process %v: %s", siov.ucred.Pid, err)
		return fmt.Errorf("Failed to send response to seccomp client %v", siov.ucred.Pid)
	}

	if uint64(bytes) != uint64(C.SECCOMP_MSG_SIZE_MIN) {
		logger.Debugf("Disconnected from seccomp socket after short write: pid=%v", siov.ucred.Pid)
		return fmt.Errorf("Failed to send full response to seccomp client %v", siov.ucred.Pid)
	}

	logger.Debugf("Send seccomp notification for id(%d)", siov.resp.id)
	return nil
}

// NewSeccompServer creates a new seccomp server.
func NewSeccompServer(s *state.State, path string, findPID func(pid int32, s *state.State) (Instance, error)) (*Server, error) {
	ret := C.seccomp_notify_get_sizes(&C.expected_sizes)
	if ret < 0 {
		return nil, errors.New("Failed to query kernel for seccomp notifier sizes")
	}

	// Cleanup existing sockets
	if util.PathExists(path) {
		err := os.Remove(path)
		if err != nil {
			return nil, err
		}
	}

	// Bind new socket
	l, err := net.Listen("unixpacket", path)
	if err != nil {
		return nil, err
	}

	// Restrict access
	err = os.Chmod(path, 0o700)
	if err != nil {
		return nil, err
	}

	// Start the server
	server := Server{
		s:    s,
		path: path,
		l:    l,
	}

	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}

			go func() {
				ucred, err := linux.GetUcred(c.(*net.UnixConn))
				if err != nil {
					logger.Errorf("Unable to get ucred from seccomp socket client: %v", err)
					return
				}

				logger.Debugf("Connected to seccomp socket: pid=%v", ucred.Pid)

				unixFile, err := c.(*net.UnixConn).File()
				if err != nil {
					logger.Debugf("Failed to turn unix socket client into file")
					return
				}

				for {
					siov := NewSeccompIovec(ucred)
					bytes, err := siov.ReceiveSeccompIovec(int(unixFile.Fd()))
					if err != nil {
						logger.Debugf("Disconnected from seccomp socket after failed receive: pid=%v, err=%s", ucred.Pid, err)
						_ = c.Close()
						return
					}

					if siov.IsValidSeccompIovec(bytes) {
						go func() { _ = server.HandleValid(int(unixFile.Fd()), siov, findPID) }()
					} else {
						go server.HandleInvalid(int(unixFile.Fd()), siov)
					}
				}
			}()
		}
	}()

	return &server, nil
}

// TaskIDs returns the task IDs for a process.
func TaskIDs(pid int) (int64, int64, int64, int64, error) {
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return -1, -1, -1, -1, err
	}

	reUID, err := regexp.Compile(`^Uid:\s+([0-9]+)\s+([0-9]+)\s+([0-9]+)\s+([0-9]+)`)
	if err != nil {
		return -1, -1, -1, -1, err
	}

	reGID, err := regexp.Compile(`^Gid:\s+([0-9]+)\s+([0-9]+)\s+([0-9]+)\s+([0-9]+)`)
	if err != nil {
		return -1, -1, -1, -1, err
	}

	var UID int64 = -1
	var GID int64 = -1
	var fsUID int64 = -1
	var fsGID int64 = -1
	UIDFound := false
	GIDFound := false
	for line := range strings.SplitSeq(string(status), "\n") {
		if UIDFound && GIDFound {
			break
		}

		if !UIDFound {
			m := reUID.FindStringSubmatch(line)
			if len(m) > 2 {
				// effective uid
				result, err := strconv.ParseInt(m[2], 10, 64)
				if err != nil {
					return -1, -1, -1, -1, err
				}

				UID = result
				UIDFound = true
			}

			if len(m) > 4 {
				// fsuid
				result, err := strconv.ParseInt(m[4], 10, 64)
				if err != nil {
					return -1, -1, -1, -1, err
				}

				fsUID = result
			}

			continue
		}

		if !GIDFound {
			m := reGID.FindStringSubmatch(line)
			if len(m) > 2 {
				// effective gid
				result, err := strconv.ParseInt(m[2], 10, 64)
				if err != nil {
					return -1, -1, -1, -1, err
				}

				GID = result
				GIDFound = true
			}

			if len(m) > 4 {
				// fsgid
				result, err := strconv.ParseInt(m[4], 10, 64)
				if err != nil {
					return -1, -1, -1, -1, err
				}

				fsGID = result
			}

			continue
		}
	}

	return UID, GID, fsUID, fsGID, nil
}

// FindTGID returns the task group leader ID from /proc/<pid> fd.
func FindTGID(procFd int) (int, error) {
	var statusFile *os.File
	fd, err := unix.Openat(procFd, "status", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}

	statusFile = os.NewFile(uintptr(fd), "/proc/<pid>/status")
	status, err := io.ReadAll(statusFile)
	_ = statusFile.Close()
	if err != nil {
		return -1, err
	}

	reTGID, err := regexp.Compile(`^Tgid:\s+([0-9]+)`)
	if err != nil {
		return -1, err
	}

	for line := range strings.SplitSeq(string(status), "\n") {
		m := reTGID.FindStringSubmatch(line)
		if len(m) > 1 {
			result, err := strconv.ParseUint(m[1], 10, 32)
			if err != nil {
				return -1, err
			}

			return int(result), nil
		}
	}

	return -1, nil
}

// CallForkmknod executes fork mknod.
func CallForkmknod(c Instance, dev deviceConfig.Device, requestPID int, s *state.State) int {
	uid, gid, fsuid, fsgid, err := TaskIDs(requestPID)
	if err != nil {
		return int(-C.EPERM)
	}

	pidFdNr, pidFd, err := MakePidFd(requestPID)
	if err != nil {
		return int(-C.EPERM)
	}

	defer logger.WarnOnError(pidFd.Close, "Failed to close pidfd")

	_, stderr, err := subprocess.RunCommandSplit(
		context.TODO(),
		nil,
		[]*os.File{pidFd},
		localUtil.GetExecPath(),
		"forksyscall",
		"mknod",
		dev["pid"],
		fmt.Sprintf("%d", pidFdNr),
		dev["path"],
		dev["mode_t"],
		dev["dev_t"],
		fmt.Sprintf("%d", uid),
		fmt.Sprintf("%d", gid),
		fmt.Sprintf("%d", fsuid),
		fmt.Sprintf("%d", fsgid),
	)
	if err != nil {
		errno, err := strconv.Atoi(stderr)
		if err != nil || errno == C.ENOANO {
			return int(-C.EPERM)
		}

		return -errno
	}

	return 0
}

// HandleInvalid sends a placeholder message to LXC. LXC will notice the short write
// and send a default message to the kernel thereby avoiding a 30s block.
func (srv *Server) HandleInvalid(fd int, siov *Iovec) {
	msghdr := C.struct_msghdr{}
	C.sendmsg(C.int(fd), &msghdr, C.MSG_NOSIGNAL)
	siov.PutSeccompIovec()
}

// MknodArgs arguments for mknod.
type MknodArgs struct {
	cMode C.mode_t
	cDev  C.dev_t
	cPid  C.pid_t
	path  string
}

func (srv *Server) doDeviceSyscall(c Instance, args *MknodArgs, siov *Iovec) int {
	dev := deviceConfig.Device{}
	dev["type"] = "unix-char"
	dev["mode"] = fmt.Sprintf("%#o", args.cMode)
	dev["major"] = fmt.Sprintf("%d", unix.Major(uint64(args.cDev)))
	dev["minor"] = fmt.Sprintf("%d", unix.Minor(uint64(args.cDev)))
	dev["pid"] = fmt.Sprintf("%d", args.cPid)
	dev["path"] = args.path
	dev["mode_t"] = fmt.Sprintf("%d", args.cMode)
	dev["dev_t"] = fmt.Sprintf("%d", args.cDev)

	// /dev is typically mounted by real root, so even though we can
	// mknod stuff in it, access will ultimately be denied.
	//
	// Instead go straight to the fallback mechanism of using a bind-mount.
	if !strings.HasPrefix(dev["path"], "/dev") {
		errno := CallForkmknod(c, dev, int(args.cPid), srv.s)
		if errno != int(-C.ENOMEDIUM) {
			return errno
		}
	}

	err := c.InsertSeccompUnixDevice(fmt.Sprintf("forkmknod.unix.%d", int(args.cPid)), dev, int(args.cPid))
	if err != nil {
		return int(-C.EPERM)
	}

	return 0
}

// HandleMknodSyscall handles a mknod syscall.
func (srv *Server) HandleMknodSyscall(c Instance, siov *Iovec) int {
	ctx := logger.Ctx{
		"container":             c.Name(),
		"project":               c.Project().Name,
		"syscall_number":        siov.req.data.nr,
		"audit_architecture":    siov.req.data.arch,
		"seccomp_notify_id":     siov.req.id,
		"seccomp_notify_flags":  siov.req.flags,
		"seccomp_notify_pid":    siov.req.pid,
		"seccomp_notify_fd":     siov.notifyFd,
		"seccomp_notify_mem_fd": siov.memFd,
	}

	defer logger.Debug("Handling mknod syscall", ctx)

	if C.device_allowed(C.dev_t(siov.req.data.args[2]), C.mode_t(siov.req.data.args[1])) < 0 {
		ctx["err"] = "Device not allowed"
		ctx["syscall_continue"] = "true"
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
		return 0
	}

	cPathBuf := [unix.PathMax]C.char{}
	_, err := C.pread(C.int(siov.memFd), unsafe.Pointer(&cPathBuf[0]), C.size_t(unix.PathMax), C.off_t(siov.req.data.args[0]))
	if err != nil {
		ctx["err"] = fmt.Sprintf("Failed to read memory for mknod syscall: %s", err)
		ctx["syscall_continue"] = "true"
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
		return 0
	}

	args := MknodArgs{
		cMode: C.mode_t(siov.req.data.args[1]),
		cDev:  C.dev_t(siov.req.data.args[2]),
		cPid:  C.pid_t(siov.req.pid),
		path:  C.GoString(&cPathBuf[0]),
	}

	ctx["syscall_args"] = &args

	return srv.doDeviceSyscall(c, &args, siov)
}

// HandleMknodatSyscall handles a mknodat syscall.
func (srv *Server) HandleMknodatSyscall(c Instance, siov *Iovec) int {
	ctx := logger.Ctx{
		"container":             c.Name(),
		"project":               c.Project().Name,
		"syscall_number":        siov.req.data.nr,
		"audit_architecture":    siov.req.data.arch,
		"seccomp_notify_id":     siov.req.id,
		"seccomp_notify_flags":  siov.req.flags,
		"seccomp_notify_pid":    siov.req.pid,
		"seccomp_notify_fd":     siov.notifyFd,
		"seccomp_notify_mem_fd": siov.memFd,
	}

	defer logger.Debug("Handling mknodat syscall", ctx)

	// Make sure to handle 64bit kernel, 32bit container/userspace, 64bit daemon.
	if int32(siov.req.data.args[0]) != int32(C.AT_FDCWD) {
		ctx["err"] = "Non AT_FDCWD mknodat calls are not allowed"
		logger.Debug("bla", ctx)
		ctx["syscall_continue"] = "true"
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
		return 0
	}

	siov.resp.error = C.device_allowed(C.dev_t(siov.req.data.args[3]), C.mode_t(siov.req.data.args[2]))
	if siov.resp.error != 0 {
		ctx["err"] = "Device not allowed"
		ctx["syscall_continue"] = "true"
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
		return 0
	}

	cPathBuf := [unix.PathMax]C.char{}
	_, err := C.pread(C.int(siov.memFd), unsafe.Pointer(&cPathBuf[0]), C.size_t(unix.PathMax), C.off_t(siov.req.data.args[1]))
	if err != nil {
		ctx["err"] = "Failed to read memory for mknodat syscall: %s"
		ctx["syscall_continue"] = "true"
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
		return 0
	}

	args := MknodArgs{
		cMode: C.mode_t(siov.req.data.args[2]),
		cDev:  C.dev_t(siov.req.data.args[3]),
		cPid:  C.pid_t(siov.req.pid),
		path:  C.GoString(&cPathBuf[0]),
	}

	ctx["syscall_args"] = &args

	return srv.doDeviceSyscall(c, &args, siov)
}

// SetxattrArgs arguments for setxattr.
type SetxattrArgs struct {
	nsuid   int64
	nsgid   int64
	nsfsuid int64
	nsfsgid int64
	size    int
	pid     int
	path    string
	name    string
	value   []byte
	flags   C.int
}

// HandleSetxattrSyscall handles setxattr syscalls.
func (srv *Server) HandleSetxattrSyscall(c Instance, siov *Iovec) int {
	ctx := logger.Ctx{
		"container":             c.Name(),
		"project":               c.Project().Name,
		"syscall_number":        siov.req.data.nr,
		"audit_architecture":    siov.req.data.arch,
		"seccomp_notify_id":     siov.req.id,
		"seccomp_notify_flags":  siov.req.flags,
		"seccomp_notify_pid":    siov.req.pid,
		"seccomp_notify_fd":     siov.notifyFd,
		"seccomp_notify_mem_fd": siov.memFd,
	}

	defer logger.Debug("Handling setxattr syscall", ctx)

	args := SetxattrArgs{}

	args.pid = int(siov.req.pid)

	pidFdNr, pidFd, err := MakePidFd(args.pid)
	if err != nil {
		ctx["err"] = fmt.Sprintf("Failed to open pidfd: %s", err)
		ctx["syscall_continue"] = "true"
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
		return 0
	}

	defer logger.WarnOnError(pidFd.Close, "Failed to close pidfd")

	uid, gid, fsuid, fsgid, err := TaskIDs(args.pid)
	if err != nil {
		ctx["syscall_continue"] = "true"
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
		return 0
	}

	idmapset, err := c.CurrentIdmap()
	if err != nil {
		ctx["syscall_continue"] = "true"
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
		return 0
	}

	args.nsuid, args.nsgid = idmapset.ShiftFromNS(uid, gid)
	args.nsfsuid, args.nsfsgid = idmapset.ShiftFromNS(fsuid, fsgid)

	// const char *path
	cBuf := [unix.PathMax]C.char{}
	_, err = C.pread(C.int(siov.memFd), unsafe.Pointer(&cBuf[0]), C.size_t(unix.PathMax), C.off_t(siov.req.data.args[0]))
	if err != nil {
		ctx["err"] = fmt.Sprintf("Failed to read memory for setxattr syscall: %s", err)
		ctx["syscall_continue"] = "true"
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
		return 0
	}

	args.path = C.GoString(&cBuf[0])

	// const char *name
	_, err = C.pread(C.int(siov.memFd), unsafe.Pointer(&cBuf[0]), C.size_t(unix.PathMax), C.off_t(siov.req.data.args[1]))
	if err != nil {
		ctx["err"] = fmt.Sprintf("Failed to read memory for setxattr syscall: %s", err)
		ctx["syscall_continue"] = "true"
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
		return 0
	}

	args.name = C.GoString(&cBuf[0])

	// size_t size
	args.size = int(siov.req.data.args[3])

	// int flags
	args.flags = C.int(siov.req.data.args[4])

	buf := make([]byte, args.size)
	_, err = C.pread(C.int(siov.memFd), unsafe.Pointer(&buf[0]), C.size_t(args.size), C.off_t(siov.req.data.args[2]))
	if err != nil {
		ctx["err"] = fmt.Sprintf("Failed to read memory for setxattr syscall: %s", err)
		ctx["syscall_continue"] = "true"
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
		return 0
	}

	args.value = buf

	whiteout := 0
	if string(args.name) != "trusted.overlay.opaque" || string(args.value) != "y" {
		ctx["syscall_continue"] = "true"
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
		return 0
	}

	whiteout = 1

	_, stderr, err := subprocess.RunCommandSplit(
		context.TODO(),
		nil,
		[]*os.File{pidFd},
		localUtil.GetExecPath(),
		"forksyscall",
		"setxattr",
		fmt.Sprintf("%d", args.pid),
		fmt.Sprintf("%d", pidFdNr),
		fmt.Sprintf("%d", args.nsuid),
		fmt.Sprintf("%d", args.nsgid),
		fmt.Sprintf("%d", args.nsfsuid),
		fmt.Sprintf("%d", args.nsfsgid),
		args.name,
		args.path,
		fmt.Sprintf("%d", args.flags),
		fmt.Sprintf("%d", whiteout),
		fmt.Sprintf("%d", args.size),
		string(args.value),
	)
	if err != nil {
		errno, err := strconv.Atoi(stderr)
		if err != nil || errno == C.ENOANO {
			return int(-C.EPERM)
		}

		return -errno
	}

	return 0
}

// SchedSetschedulerArgs arguments for setxattr.
type SchedSetschedulerArgs struct {
	switchPidns   int
	pidCaller     int
	pidTarget     int
	policy        C.int
	schedPriority C.int
	nsuid         int64
	nsgid         int64
}

// HandleSchedSetschedulerSyscall handles sched_setscheduler syscalls.
func (srv *Server) HandleSchedSetschedulerSyscall(c Instance, siov *Iovec) int {
	ctx := logger.Ctx{
		"container":             c.Name(),
		"project":               c.Project().Name,
		"syscall_number":        siov.req.data.nr,
		"audit_architecture":    siov.req.data.arch,
		"seccomp_notify_id":     siov.req.id,
		"seccomp_notify_flags":  siov.req.flags,
		"seccomp_notify_pid":    siov.req.pid,
		"seccomp_notify_fd":     siov.notifyFd,
		"seccomp_notify_mem_fd": siov.memFd,
	}

	defer logger.Debug("Handling sched_setscheduler syscall", ctx)

	args := SchedSetschedulerArgs{}
	args.pidCaller = int(siov.req.pid)

	pidFdNr, pidFd, err := MakePidFd(args.pidCaller)
	if err != nil {
		ctx["err"] = fmt.Sprintf("Failed to open pidfd: %s", err)
		ctx["syscall_continue"] = "true"
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
		return 0
	}

	defer logger.WarnOnError(pidFd.Close, "Failed to close pidfd")

	uid, gid, _, _, err := TaskIDs(args.pidCaller)
	if err != nil {
		ctx["syscall_continue"] = "true"
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
		return 0
	}

	idmapset, err := c.CurrentIdmap()
	if err != nil {
		ctx["syscall_continue"] = "true"
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
		return 0
	}

	// Only care about userns root for now.
	args.nsuid, args.nsgid = idmapset.ShiftFromNS(uid, gid)
	if args.nsuid != 0 || args.nsgid != 0 {
		ctx["syscall_continue"] = "true"
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
		return 0
	}

	// The target pid is only valid in the container's pid namespace as
	// we're taking it from the raw system call arguments.
	args.pidTarget = int(siov.req.data.args[0])
	if args.pidTarget < 0 {
		ctx["syscall_continue"] = "true"
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
		return 0
	}

	// If the caller passed zero they want to change their own attributes.
	if args.pidTarget == 0 {
		// This pid is relative to our pid namespace so we need to
		// inform forksyscall to not switch pid namespaces when
		// emulating the system call.
		args.pidTarget = args.pidCaller
		args.switchPidns = 0
	} else {
		// The pid is relative to the container's pid namespace.
		args.switchPidns = 1
	}

	// error out if policy < 0
	if args.policy < 0 {
		ctx["syscall_continue"] = "true"
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
		return 0
	}

	// int policy
	args.policy = C.int(siov.req.data.args[1])

	schedParamArgs := C.struct_sched_param{}
	_, err = C.pread(C.int(siov.memFd), unsafe.Pointer(&schedParamArgs), C.INCUS_SCHED_PARAM_SIZE, C.off_t(siov.req.data.args[2]))
	if err != nil {
		ctx["syscall_continue"] = "true"
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
		return 0
	}

	args.schedPriority = schedParamArgs.sched_priority

	_, stderr, err := subprocess.RunCommandSplit(
		context.TODO(),
		nil,
		[]*os.File{pidFd},
		localUtil.GetExecPath(),
		"forksyscall",
		"sched_setscheduler",
		fmt.Sprintf("%d", args.pidCaller),
		fmt.Sprintf("%d", pidFdNr),
		fmt.Sprintf("%d", args.switchPidns),
		fmt.Sprintf("%d", args.pidTarget),
		fmt.Sprintf("%d", args.policy),
		fmt.Sprintf("%d", args.schedPriority),
	)
	if err != nil {
		errno, err := strconv.Atoi(stderr)
		if err != nil || errno == C.ENOANO {
			return int(-C.EPERM)
		}

		return -errno
	}

	return 0
}

// taskCapEff returns the effective capability set of a task from /proc.
func taskCapEff(tid int) (uint64, error) {
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", tid))
	if err != nil {
		return 0, err
	}

	for line := range strings.SplitSeq(string(status), "\n") {
		value, ok := strings.CutPrefix(line, "CapEff:")
		if !ok {
			continue
		}

		return strconv.ParseUint(strings.TrimSpace(value), 16, 64)
	}

	return 0, fmt.Errorf("No CapEff in /proc/%d/status", tid)
}

// taskUserNS identifies the user namespace of a task by the (device, inode) of its /proc/<pid>/ns/user.
func taskUserNS(pid int) (uint64, uint64, error) {
	var st unix.Stat_t

	err := unix.Stat(fmt.Sprintf("/proc/%d/ns/user", pid), &st)
	if err != nil {
		return 0, 0, err
	}

	return uint64(st.Dev), uint64(st.Ino), nil
}

// HandleIoctlSyscall handles the ioctl syscalls we intercept, which is only
// EXT4_IOC_RESIZE_FS: it lets the request through to the file system, i.e.
// to the fuse4fs serving the instance's fuseblk mount, when the caller may
// resize, and refuses it with EPERM otherwise.
//
// The kernel's ext4 asks for CAP_SYS_RESOURCE with capable(), in the initial
// user namespace, which nobody in an unprivileged container has; taken
// literally that would refuse the instance's root as well and make online
// resizing useless. What the check means is "the administrator of this file
// system", and in a container that is CAP_SYS_RESOURCE in the container's own
// user namespace, which is what ns_capable() would test: the thread has the
// capability in its effective set and lives in the instance's user namespace
// itself. The second condition matters: a root inside a nested container
// (a child user namespace, as nesting instances create) has the bit set too,
// but holds the capability only in its own namespace, not in the instance's.
// That matches what a guest expects from a VM, where its root resizes its
// own disks and a container inside the guest does not.
//
// The decision is made on the thread's credentials, not on anything the
// request points to, so the syscall continuing in the kernel after the check
// leaves no window: the thread sits in the syscall until we answer and its
// capabilities and namespace can't change meanwhile. The credentials are
// read first and the notification validated after, so that a thread that
// died and had its id reused is refused rather than judged by its successor.
func (srv *Server) HandleIoctlSyscall(c Instance, siov *Iovec) int {
	ctx := logger.Ctx{
		"container":             c.Name(),
		"project":               c.Project().Name,
		"syscall_number":        siov.req.data.nr,
		"audit_architecture":    siov.req.data.arch,
		"seccomp_notify_id":     siov.req.id,
		"seccomp_notify_flags":  siov.req.flags,
		"seccomp_notify_pid":    siov.req.pid,
		"seccomp_notify_fd":     siov.notifyFd,
		"seccomp_notify_mem_fd": siov.memFd,
	}

	defer logger.Debug("Handling ioctl syscall", ctx)

	ctx["ioctl_cmd"] = fmt.Sprintf("%#x", uint32(siov.req.data.args[1]))

	// The policy only notifies EXT4_IOC_RESIZE_FS; anything else isn't ours to judge.
	if uint32(siov.req.data.args[1]) != ext4IocResizeFs || !util.IsTrue(c.ExpandedConfig()["security.syscalls.intercept.ioctl.resize_fs"]) {
		ctx["syscall_continue"] = "true"
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
		return 0
	}

	caps, err := taskCapEff(int(siov.req.pid))
	if err != nil {
		ctx["err"] = err
		logger.Warn("Refusing EXT4_IOC_RESIZE_FS: cannot read the caller's capabilities", ctx)
		return int(-C.EPERM)
	}

	callerDev, callerIno, err := taskUserNS(int(siov.req.pid))
	if err != nil {
		ctx["err"] = err
		logger.Warn("Refusing EXT4_IOC_RESIZE_FS: cannot read the caller's user namespace", ctx)
		return int(-C.EPERM)
	}

	initDev, initIno, err := taskUserNS(int(siov.msg.init_pid))
	if err != nil {
		ctx["err"] = err
		logger.Warn("Refusing EXT4_IOC_RESIZE_FS: cannot read the instance's user namespace", ctx)
		return int(-C.EPERM)
	}

	// Everything above was read from /proc by pid; only now do we know it was about the caller.
	if siov.notifyFd < 0 || C.seccomp_notify_id_valid(C.int(siov.notifyFd), siov.req) != 0 {
		ctx["err"] = "the seccomp notification is no longer valid, or no notify fd was received"
		logger.Warn("Refusing EXT4_IOC_RESIZE_FS: cannot confirm the caller", ctx)
		return int(-C.EPERM)
	}

	if callerDev != initDev || callerIno != initIno {
		logger.Info("Refusing EXT4_IOC_RESIZE_FS from a nested user namespace", ctx)
		return int(-C.EPERM)
	}

	ctx["caller_cap_eff"] = fmt.Sprintf("%#x", caps)
	if caps&(1<<capSysResource) == 0 {
		logger.Info("Refusing EXT4_IOC_RESIZE_FS from a caller without CAP_SYS_RESOURCE", ctx)
		return int(-C.EPERM)
	}

	ctx["syscall_continue"] = "true"
	C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
	return 0
}

// HandleSysinfoSyscall handles sysinfo syscalls.
func (srv *Server) HandleSysinfoSyscall(c Instance, siov *Iovec) int {
	l := logger.AddContext(logger.Ctx{
		"container":             c.Name(),
		"project":               c.Project().Name,
		"syscall_number":        siov.req.data.nr,
		"audit_architecture":    siov.req.data.arch,
		"seccomp_notify_id":     siov.req.id,
		"seccomp_notify_flags":  siov.req.flags,
		"seccomp_notify_pid":    siov.req.pid,
		"seccomp_notify_fd":     siov.notifyFd,
		"seccomp_notify_mem_fd": siov.memFd,
	})

	defer l.Debug("Handling sysinfo syscall")

	// Pre-fill sysinfo struct with metrics from host system.
	info := unix.Sysinfo_t{}
	err := unix.Sysinfo(&info)
	if err != nil {
		l.Warn("Failed getting sysinfo", logger.Ctx{"err": err})
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))

		return 0
	}

	instMetrics := Sysinfo{} // Architecture independent place to hold instance metrics.

	// Handle i386 on x86_64.
	instMetrics.Unit = 1
	if c.Architecture() == osarch.ARCH_64BIT_INTEL_X86 && siov.req.data.arch == C.AUDIT_ARCH_I386 {
		instMetrics.Unit = 4096
	}

	cg, err := cgroup.NewFileReadWriter(int(siov.msg.init_pid))
	if err != nil {
		l.Warn("Failed loading cgroup", logger.Ctx{"err": err, "pid": siov.msg.init_pid})
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))

		return 0
	}

	// Get instance uptime.
	pidStat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", siov.msg.init_pid))
	if err != nil {
		l.Warn("Failed getting init process info", logger.Ctx{"err": err, "pid": siov.msg.init_pid})
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))

		return 0
	}

	fields := strings.Fields(string(pidStat))
	tickValue, err := strconv.ParseInt(fields[21], 10, 64)
	if err != nil {
		l.Warn("Failed parsing init process info", logger.Ctx{"err": err, "pid": siov.msg.init_pid})
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))

		return 0
	}

	age := float64(tickValue / 100)
	if age > 0 {
		instMetrics.Uptime = int64(time.Since(srv.s.OS.BootTime).Seconds() - age)
	}

	// Get instance process count.
	pids, err := cg.GetProcessesUsage()
	if err != nil {
		l.Warn("Failed getting process count", logger.Ctx{"err": err})
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))

		return 0
	}

	instMetrics.Procs = uint16(pids)

	// Get instance memory stats.
	memStats, err := cg.GetMemoryStats()
	if err != nil {
		l.Warn("Failed getting memory stats", logger.Ctx{"err": err})
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))

		return 0
	}

	for k, v := range memStats {
		switch k {
		case "shmem":
			instMetrics.Sharedram = v
		case "cache":
			instMetrics.Bufferram = v
		}
	}

	// Get instance memory limit.
	memoryLimit, err := cg.GetEffectiveMemoryLimit()
	if err != nil {
		l.Warn("Failed getting effective memory limit", logger.Ctx{"err": err})
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))

		return 0
	}

	// Get instance memory usage.
	memoryUsage, err := cg.GetMemoryUsage()
	if err != nil {
		l.Warn("Failed to get memory usage", logger.Ctx{"err": err})
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))

		return 0
	}

	instMetrics.Totalram = uint64(memoryLimit)
	instMetrics.Freeram = instMetrics.Totalram - uint64(memoryUsage) - instMetrics.Bufferram

	// Get instance swap info.
	if cgroup.Supports(cgroup.MemorySwap) {
		swapLimit, err := cg.GetMemorySwapLimit()
		if err != nil {
			l.Warn("Failed getting swap limit", logger.Ctx{"err": err})
			C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))

			return 0
		}

		swapUsage, err := cg.GetMemorySwapUsage()
		if err != nil {
			l.Warn("Failed getting swap usage", logger.Ctx{"err": err})
			C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))

			return 0
		}

		instMetrics.Totalswap = uint64(swapLimit)
		instMetrics.Freeswap = instMetrics.Totalswap - uint64(swapUsage)
	}

	// Write instance metrics to native sysinfo struct.
	var b []byte
	if c.Architecture() == osarch.ARCH_64BIT_INTEL_X86 && siov.req.data.arch == C.AUDIT_ARCH_I386 {
		ret := instMetrics.ToNative32(info)

		const sz = int(unsafe.Sizeof(ret))
		b = (*(*[sz]byte)(unsafe.Pointer(&ret)))[:]
	} else {
		instMetrics.ToNative(&info)

		const sz = int(unsafe.Sizeof(info))
		b = (*(*[sz]byte)(unsafe.Pointer(&info)))[:]
	}

	// Write sysinfo response into buffer.
	_, err = unix.Pwrite(siov.memFd, b, int64(siov.req.data.args[0]))
	if err != nil {
		l.Warn("Failed writing sysinfo", logger.Ctx{"err": err})
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))

		return 0
	}

	return 0
}

// MountArgs arguments for mount.
type MountArgs struct {
	source    string
	target    string
	fstype    string
	flags     int
	data      string
	pid       int
	idmapType idmap.StorageType
	uid       int64
	gid       int64
	fsuid     int64
	fsgid     int64
	nsuid     int64
	nsgid     int64
	nsfsuid   int64
	nsfsgid   int64
}

const knownFlags C.ulong = C.MS_BIND | C.MS_LAZYTIME | C.MS_MANDLOCK |
	C.MS_NOATIME | C.MS_NODEV | C.MS_NODIRATIME |
	C.MS_NOEXEC | C.MS_NOSUID | C.MS_REMOUNT |
	C.MS_RDONLY | C.MS_STRICTATIME |
	C.MS_SYNCHRONOUS | C.MS_BIND
const knownFlagsRecursive C.ulong = knownFlags | C.MS_REC

var mountFlagsToOptMap = map[C.ulong]string{
	C.MS_BIND:            "bind",
	C.ulong(0):           "defaults",
	C.MS_LAZYTIME:        "lazytime",
	C.MS_MANDLOCK:        "mand",
	C.MS_NOATIME:         "noatime",
	C.MS_NODEV:           "nodev",
	C.MS_NODIRATIME:      "nodiratime",
	C.MS_NOEXEC:          "noexec",
	C.MS_NOSUID:          "nosuid",
	C.MS_REMOUNT:         "remount",
	C.MS_RDONLY:          "ro",
	C.MS_STRICTATIME:     "strictatime",
	C.MS_SYNCHRONOUS:     "sync",
	C.MS_REC | C.MS_BIND: "rbind",
}

func mountFlagsToOpts(flags C.ulong) string {
	var currentBit C.ulong
	opts := ""
	var msRec C.ulong = (flags & C.MS_REC)

	flags = (flags &^ C.MS_REC)
	for currentBit < (4*8 - 1) {
		var flag C.ulong = (1 << currentBit)

		currentBit++

		if (flags & flag) == 0 {
			continue
		}

		if (flag == C.MS_BIND) && msRec == C.MS_REC {
			flag |= msRec
		}

		optOrArg := mountFlagsToOptMap[flag]

		if optOrArg == "" {
			continue
		}

		if opts == "" {
			opts = optOrArg
		} else {
			opts = fmt.Sprintf("%s,%s", opts, optOrArg)
		}
	}

	return opts
}

// mountHandleHugetlbfsArgs adds user namespace root uid and gid to the
// hugetlbfs mount options to make it usable in unprivileged containers.
func (srv *Server) mountHandleHugetlbfsArgs(c Instance, args *MountArgs, nsuid int64, nsgid int64) error {
	if args.fstype != "hugetlbfs" {
		return nil
	}

	if args.data == "" {
		args.data = fmt.Sprintf("uid=%d,gid=%d", nsuid, nsgid)
		return nil
	}

	uidOpt := int64(-1)
	gidOpt := int64(-1)

	idmapset, err := c.CurrentIdmap()
	if err != nil {
		return err
	}

	optStrings := strings.Split(args.data, ",")
	for i, optString := range optStrings {
		if strings.HasPrefix(optString, "uid=") {
			uidFields := strings.Split(optString, "=")
			if len(uidFields) > 1 {
				n, err := strconv.ParseInt(uidFields[1], 10, 64)
				if err != nil {
					// If the user specified garbage, let the kernel tell em what's what.
					return nil
				}

				uidOpt, _ = idmapset.ShiftIntoNS(n, 0)
				if uidOpt < 0 {
					// If the user specified garbage, let the kernel tell em what's what.
					return nil
				}

				optStrings[i] = fmt.Sprintf("uid=%d", uidOpt)
			}
		} else if strings.HasPrefix(optString, "gid=") {
			gidFields := strings.Split(optString, "=")
			if len(gidFields) > 1 {
				n, err := strconv.ParseInt(gidFields[1], 10, 64)
				if err != nil {
					// If the user specified garbage, let the kernel tell em what's what.
					return nil
				}

				gidOpt, _ = idmapset.ShiftIntoNS(n, 0)
				if gidOpt < 0 {
					// If the user specified garbage, let the kernel tell em what's what.
					return nil
				}

				optStrings[i] = fmt.Sprintf("gid=%d", gidOpt)
			}
		}
	}

	if uidOpt == -1 {
		optStrings = append(optStrings, fmt.Sprintf("uid=%d", nsuid))
	}

	if gidOpt == -1 {
		optStrings = append(optStrings, fmt.Sprintf("gid=%d", nsgid))
	}

	args.data = strings.Join(optStrings, ",")
	args.idmapType = idmap.StorageTypeNone
	return nil
}

// HandleMountSyscall handles mount syscalls.
func (srv *Server) HandleMountSyscall(c Instance, siov *Iovec) int {
	ctx := logger.Ctx{
		"container":             c.Name(),
		"project":               c.Project().Name,
		"syscall_number":        siov.req.data.nr,
		"audit_architecture":    siov.req.data.arch,
		"seccomp_notify_id":     siov.req.id,
		"seccomp_notify_flags":  siov.req.flags,
		"seccomp_notify_pid":    siov.req.pid,
		"seccomp_notify_fd":     siov.notifyFd,
		"seccomp_notify_mem_fd": siov.memFd,
	}

	defer logger.Debug("Handling mount syscall", ctx)

	args := MountArgs{
		pid: int(siov.req.pid),
	}

	pidFdNr, pidFd, err := MakePidFd(args.pid)
	if err != nil {
		ctx["err"] = fmt.Sprintf("Failed to open pidfd: %s", err)
		ctx["syscall_continue"] = "true"
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
		return 0
	}

	defer logger.WarnOnError(pidFd.Close, "Failed to close pidfd")

	mntSource := [unix.PathMax]C.char{}
	mntTarget := [unix.PathMax]C.char{}
	mntFs := [unix.PathMax]C.char{}
	mntData := [unix.PathMax]C.char{}

	// const char *source
	if siov.req.data.args[0] != 0 {
		_, err := C.pread(C.int(siov.memFd), unsafe.Pointer(&mntSource[0]), C.size_t(unix.PathMax), C.off_t(siov.req.data.args[0]))
		if err != nil {
			ctx["err"] = fmt.Sprintf("Failed to read source path for of mount syscall: %s", err)
			ctx["syscall_continue"] = "true"
			C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
			return 0
		}
	}
	args.source = C.GoString(&mntSource[0])
	ctx["source"] = args.source

	// const char *target
	if siov.req.data.args[1] != 0 {
		_, err := C.pread(C.int(siov.memFd), unsafe.Pointer(&mntTarget[0]), C.size_t(unix.PathMax), C.off_t(siov.req.data.args[1]))
		if err != nil {
			ctx["err"] = fmt.Sprintf("Failed to read target path for of mount syscall: %s", err)
			ctx["syscall_continue"] = "true"
			C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
			return 0
		}
	}
	args.target = C.GoString(&mntTarget[0])
	ctx["target"] = args.target

	// const char *filesystemtype
	if siov.req.data.args[1] != 0 {
		_, err := C.pread(C.int(siov.memFd), unsafe.Pointer(&mntFs[0]), C.size_t(unix.PathMax), C.off_t(siov.req.data.args[2]))
		if err != nil {
			ctx["err"] = fmt.Sprintf("Failed to read fstype for of mount syscall: %s", err)
			ctx["syscall_continue"] = "true"
			C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
			return 0
		}
	}
	args.fstype = C.GoString(&mntFs[0])
	ctx["fstype"] = args.fstype

	// idmap shift
	fullSrcPath := filepath.Join(fmt.Sprintf("/proc/%d/root/", args.pid), args.source)
	if util.PathExists(fullSrcPath) {
		args.idmapType = srv.MountSyscallShift(c, fullSrcPath, args.fstype)
	} else {
		args.idmapType = srv.MountSyscallShift(c, args.source, args.fstype)
	}

	// unsigned long mountflags
	args.flags = int(siov.req.data.args[3])

	// const void *data
	if siov.req.data.args[4] != 0 {
		_, err := C.pread(C.int(siov.memFd), unsafe.Pointer(&mntData[0]), C.size_t(unix.PathMax), C.off_t(siov.req.data.args[4]))
		if err != nil {
			ctx["err"] = fmt.Sprintf("Failed to read mount data for of mount syscall: %s", err)
			ctx["syscall_continue"] = "true"
			C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
			return 0
		}
	}
	args.data = C.GoString(&mntData[0])
	ctx["data"] = args.data

	err = linux.PidfdSendSignal(int(pidFd.Fd()), 0, 0)
	if err != nil {
		ctx["err"] = fmt.Sprintf("Failed to send signal to target process for of mount syscall: %s", err)
		ctx["syscall_continue"] = "true"
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
		return 0
	}

	// A remount of a fuseblk mount Incus created for the instance is done by the host, which
	// owns the superblock: the kernel refuses it to the guest. As for the resize ioctl, the
	// caller must administer the instance itself, not a user namespace nested below it.
	if major, minor, ok := srv.mountSyscallFuseblkRemount(c, &args); ok {
		ctx["fuseblk_device"] = fmt.Sprintf("%d:%d", major, minor)

		callerDev, callerIno, err := taskUserNS(args.pid)
		initDev, initIno, initErr := taskUserNS(int(siov.msg.init_pid))
		if err == nil {
			err = initErr
		}

		// The namespaces were read from /proc by pid; confirm they were the caller's.
		if err == nil && (siov.notifyFd < 0 || C.seccomp_notify_id_valid(C.int(siov.notifyFd), siov.req) != 0) {
			err = errors.New("the seccomp notification is no longer valid, or no notify fd was received")
		}

		if err == nil && (callerDev != initDev || callerIno != initIno) {
			err = errors.New("the caller is in a nested user namespace")
		}

		var stderr string
		if err == nil {
			_, stderr, err = subprocess.RunCommandSplit(
				context.TODO(),
				nil,
				[]*os.File{pidFd},
				localUtil.GetExecPath(),
				"forksyscall",
				"mount",
				fmt.Sprintf("%d", args.pid),
				fmt.Sprintf("%d", pidFdNr),
				fmt.Sprintf("%d", 4),
				args.target,
				fmt.Sprintf("%d", args.flags),
				fmt.Sprintf("%d", major),
				fmt.Sprintf("%d", minor),
			)
		}

		if err != nil {
			// The kernel then refuses the remount itself, so record why here.
			ctx["err"] = err
			ctx["fuseblk_stderr"] = strings.TrimSpace(stderr)
			ctx["syscall_continue"] = "true"
			logger.Warn("Failed to reconfigure fuseblk mount for syscall interception", ctx)
			C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
			return 0
		}

		return 0
	}

	fuseBinary := ""
	lxcfsDir, lxcfsWanted := srv.mountSyscallLXCFS(c, &args)
	if lxcfsWanted && lxcfsDir == "" {
		ctx["syscall_continue"] = "true"
		logger.Warn("No LXCFS to overlay on intercepted proc mount", ctx)
		go srv.warnProcLXCFS(c, fmt.Sprintf("Mounted a plain proc on %q: LXCFS isn't mounted on the host", args.target))
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
		return 0
	}

	if lxcfsDir == "" {
		var ok bool

		ok, fuseBinary = srv.MountSyscallValid(c, &args)
		if !ok {
			ctx["syscall_continue"] = "true"
			C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
			return 0
		}
	}

	idmapset, err := c.CurrentIdmap()
	if err != nil {
		ctx["syscall_continue"] = "true"
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
		return 0
	}

	args.uid, args.gid, args.fsuid, args.fsgid, err = TaskIDs(args.pid)
	if err != nil {
		ctx["syscall_continue"] = "true"
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
		return 0
	}

	ctx["host_uid"] = args.uid
	ctx["host_gid"] = args.gid
	ctx["host_fsuid"] = args.fsuid
	ctx["host_fsgid"] = args.fsgid

	args.nsuid, args.nsgid = idmapset.ShiftFromNS(args.uid, args.gid)
	args.nsfsuid, args.nsfsgid = idmapset.ShiftFromNS(args.fsuid, args.fsgid)
	ctx["ns_uid"] = args.nsuid
	ctx["ns_gid"] = args.nsgid
	ctx["ns_fsuid"] = args.nsfsuid
	ctx["ns_fsgid"] = args.nsfsgid

	err = srv.mountHandleHugetlbfsArgs(c, &args, args.uid, args.gid)
	if err != nil {
		ctx["syscall_continue"] = "true"
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
		return 0
	}

	if lxcfsDir != "" {
		ctx["lxcfs_dir"] = lxcfsDir
		var stderr string
		_, stderr, err = subprocess.RunCommandSplit(
			context.TODO(),
			nil,
			[]*os.File{pidFd},
			localUtil.GetExecPath(),
			"forksyscall",
			"mount",
			fmt.Sprintf("%d", args.pid),
			fmt.Sprintf("%d", pidFdNr),
			fmt.Sprintf("%d", 3),
			fmt.Sprintf("%d", args.nsuid),
			fmt.Sprintf("%d", args.nsgid),
			fmt.Sprintf("%d", args.nsfsuid),
			fmt.Sprintf("%d", args.nsfsgid),
			args.source,
			args.target,
			fmt.Sprintf("%d", args.flags),
			lxcfsDir,
			args.data,
		)
		if err != nil {
			// The kernel then mounts a plain proc, as without the option.
			// Mounts left to it on purpose (see procLXCFSSkipped) aren't
			// failures; anything else is one the operator must be able to see.
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitCode() == procLXCFSSkipped {
				ctx["lxcfs_skipped"] = "true"
			} else {
				ctx["lxcfs_stderr"] = strings.TrimSpace(stderr)
				logger.Warn("Failed to overlay LXCFS on intercepted proc mount", ctx)
				go srv.warnProcLXCFS(c, fmt.Sprintf("Mounted a plain proc on %q: %s", args.target, strings.TrimSpace(stderr)))
			}
		}
	} else if fuseBinary != "" {
		// Record ignored flags for debugging purposes
		flags := C.ulong(args.flags)
		ctx["fuse_ignored_flags"] = fmt.Sprintf("%x", (flags &^ (knownFlagsRecursive | C.MS_MGC_MSK)))

		addOpts := mountFlagsToOpts(flags)

		fuseSource := fmt.Sprintf("%s#%s", fuseBinary, args.source)
		fuseOpts := ""
		if args.data != "" && addOpts != "" {
			fuseOpts = fmt.Sprintf("%s,%s", args.data, addOpts)
		} else if args.data != "" {
			fuseOpts = args.data
		} else if addOpts != "" {
			fuseOpts = addOpts
		}

		// The guest doesn't get to authorize its own resizes.
		fuseOpts = stripMountOption(fuseOpts, fuseblkHostAuthorizedResize)

		ctx["fuse_source"] = fuseSource
		ctx["fuse_target"] = args.target
		ctx["fuse_opts"] = fuseOpts

		major, minor, fuseblk := srv.mountSyscallFuseblkDevice(c, &args)
		if fuseblk {
			fuseblkOpts := mountFuseblkOpts(fuseOpts, c.ExpandedConfig()["security.syscalls.intercept.mount.fuseblk.options"], util.IsTrue(c.ExpandedConfig()["security.syscalls.intercept.ioctl.resize_fs"]))
			ctx["fuseblk_device"] = fmt.Sprintf("%d:%d", major, minor)
			ctx["fuseblk_opts"] = fuseblkOpts
			var stderr string
			_, stderr, err = subprocess.RunCommandSplit(
				context.TODO(),
				nil,
				[]*os.File{pidFd},
				localUtil.GetExecPath(),
				"forksyscall",
				"mount",
				fmt.Sprintf("%d", args.pid),
				fmt.Sprintf("%d", pidFdNr),
				fmt.Sprintf("%d", 2),
				fmt.Sprintf("%d", args.nsuid),
				fmt.Sprintf("%d", args.nsgid),
				fmt.Sprintf("%d", args.nsfsuid),
				fmt.Sprintf("%d", args.nsfsgid),
				fuseBinary,
				args.source,
				args.target,
				fuseblkOpts,
				fmt.Sprintf("%d", args.flags),
				fmt.Sprintf("%d", major),
				fmt.Sprintf("%d", minor),
			)
			if err != nil {
				// A refused fuseblk mount leaves the guest with the kernel's own
				// answer to its mount call, so record why here.
				ctx["fuseblk_stderr"] = strings.TrimSpace(stderr)
				logger.Warn("Failed to create fuseblk mount for syscall interception", ctx)
			}
		} else {
			_, _, err = subprocess.RunCommandSplit(
				context.TODO(),
				nil,
				[]*os.File{pidFd},
				localUtil.GetExecPath(),
				"forksyscall",
				"mount",
				fmt.Sprintf("%d", args.pid),
				fmt.Sprintf("%d", pidFdNr),
				fmt.Sprintf("%d", 1),
				fmt.Sprintf("%d", args.nsuid),
				fmt.Sprintf("%d", args.nsgid),
				fmt.Sprintf("%d", args.nsfsuid),
				fmt.Sprintf("%d", args.nsfsgid),
				fuseSource,
				args.target,
				fuseOpts,
			)
		}
	} else {
		_, _, err = subprocess.RunCommandSplit(
			context.TODO(),
			nil,
			[]*os.File{pidFd},
			localUtil.GetExecPath(),
			"forksyscall",
			"mount",
			fmt.Sprintf("%d", args.pid),
			fmt.Sprintf("%d", pidFdNr),
			fmt.Sprintf("%d", 0),
			args.source,
			args.target,
			args.fstype,
			fmt.Sprintf("%d", args.flags),
			string(args.idmapType),
			fmt.Sprintf("%d", args.uid),
			fmt.Sprintf("%d", args.gid),
			fmt.Sprintf("%d", args.fsuid),
			fmt.Sprintf("%d", args.fsgid),
			fmt.Sprintf("%d", args.nsuid),
			fmt.Sprintf("%d", args.nsgid),
			fmt.Sprintf("%d", args.nsfsuid),
			fmt.Sprintf("%d", args.nsfsgid),
			args.data,
		)
	}

	if err != nil {
		ctx["err"] = err
		ctx["syscall_continue"] = "true"
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
		return 0
	}

	return 0
}

// HandleBpfSyscall handles mount syscalls.
func (srv *Server) HandleBpfSyscall(c Instance, siov *Iovec) int {
	ctx := logger.Ctx{
		"container":             c.Name(),
		"project":               c.Project().Name,
		"syscall_number":        siov.req.data.nr,
		"audit_architecture":    siov.req.data.arch,
		"seccomp_notify_id":     siov.req.id,
		"seccomp_notify_flags":  siov.req.flags,
		"seccomp_notify_pid":    siov.req.pid,
		"seccomp_notify_fd":     siov.notifyFd,
		"seccomp_notify_mem_fd": siov.memFd,
	}

	defer logger.Debug("Handling bpf syscall", ctx)
	var bpfCmd, bpfProgType, bpfAttachType, tgid C.int
	var flags C.uint

	if util.IsFalseOrEmpty(c.ExpandedConfig()["security.syscalls.intercept.bpf.devices"]) {
		ctx["syscall_continue"] = "true"
		ctx["syscall_handler_reason"] = "No bpf policy specified"
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
		return 0
	}

	flags |= C.PIDFD_THREAD
	tgid = -1

	// Locking to a thread shouldn't be necessary but it still makes me
	// queezy that Go could just wander off to somewhere.
	runtime.LockOSThread()
	ret := C.handle_bpf_syscall(
		C.pid_t(siov.req.pid),
		C.int(siov.notifyFd),
		C.int(siov.memFd),
		C.int(tgid),
		siov.msg,
		siov.req,
		siov.resp,
		&bpfCmd,
		&bpfProgType,
		&bpfAttachType, flags,
	)
	runtime.UnlockOSThread()
	ctx["bpf_cmd"] = fmt.Sprintf("%d", bpfCmd)
	ctx["bpf_prog_type"] = fmt.Sprintf("%d", bpfProgType)
	ctx["bpf_attach_type"] = fmt.Sprintf("%d", bpfAttachType)
	if ret < 0 {
		ctx["syscall_continue"] = "true"
		ctx["syscall_handler_error"] = fmt.Sprintf("%s - Failed to handle bpf syscall", unix.Errno(-ret))
		C.seccomp_notify_update_response(siov.resp, 0, C.uint32_t(seccompUserNotifFlagContinue))
		return 0
	}

	return 0
}

func (srv *Server) handleSyscall(c Instance, siov *Iovec) int {
	switch int(C.seccomp_notify_get_syscall(siov.req, siov.resp)) {
	case incusSeccompNotifyMknod:
		return srv.HandleMknodSyscall(c, siov)
	case incusSeccompNotifyMknodat:
		return srv.HandleMknodatSyscall(c, siov)
	case incusSeccompNotifySetxattr:
		return srv.HandleSetxattrSyscall(c, siov)
	case incusSeccompNotifyMount:
		return srv.HandleMountSyscall(c, siov)
	case incusSeccompNotifyBpf:
		return srv.HandleBpfSyscall(c, siov)
	case incusSeccompNotifySchedSetscheduler:
		return srv.HandleSchedSetschedulerSyscall(c, siov)
	case incusSeccompNotifyIoctl:
		return srv.HandleIoctlSyscall(c, siov)
	case incusSeccompNotifySysinfo:
		return srv.HandleSysinfoSyscall(c, siov)
	}

	return int(-C.EINVAL)
}

const seccompUserNotifFlagContinue uint32 = 0x00000001

// HandleValid handles a valid seccomp notifier message.
func (srv *Server) HandleValid(fd int, siov *Iovec, findPID func(pid int32, s *state.State) (Instance, error)) error {
	defer siov.PutSeccompIovec()

	c, err := findPID(int32(siov.msg.monitor_pid), srv.s)
	if err != nil {
		_ = siov.SendSeccompIovec(fd, 0, seccompUserNotifFlagContinue)

		logger.Errorf("Failed to find container for monitor %d", siov.msg.monitor_pid)
		return err
	}

	errno := srv.handleSyscall(c, siov)

	err = siov.SendSeccompIovec(fd, errno, 0)
	if err != nil {
		return err
	}

	return nil
}

// Stop stops a seccomp server.
func (srv *Server) Stop() error {
	_ = os.Remove(srv.path)
	return srv.l.Close()
}

// MountSyscallFilter creates a mount syscall filter from the config.
func MountSyscallFilter(config map[string]string) []string {
	fs := []string{}

	if util.IsFalseOrEmpty(config["security.syscalls.intercept.mount"]) {
		return fs
	}

	fsAllowed := strings.Split(config["security.syscalls.intercept.mount.allowed"], ",")
	if len(fsAllowed) > 0 && fsAllowed[0] != "" {
		fs = append(fs, fsAllowed...)
	}

	return fs
}

// SyscallInterceptMountFilter creates a new mount syscall interception filter.
func SyscallInterceptMountFilter(config map[string]string) (map[string]string, error) {
	if util.IsFalseOrEmpty(config["security.syscalls.intercept.mount"]) {
		return map[string]string{}, nil
	}

	fsMap := map[string]string{}
	fsFused := strings.Split(config["security.syscalls.intercept.mount.fuse"], ",")
	if len(fsFused) > 0 && fsFused[0] != "" {
		for _, ent := range fsFused {
			fsfuse := strings.Split(ent, "=")
			if len(fsfuse) != 2 {
				return map[string]string{}, fmt.Errorf("security.syscalls.intercept.mount.fuse is not of the form 'filesystem=fuse-binary': %s", ent)
			}

			// fsfuse[0] == filesystems that are ok to mount
			// fsfuse[1] == fuse binary to use to mount filesystemstype
			fsMap[fsfuse[0]] = fsfuse[1]
		}
	}

	fsAllowed := strings.Split(config["security.syscalls.intercept.mount.allowed"], ",")
	if len(fsAllowed) > 0 && fsAllowed[0] != "" {
		for _, allowedfs := range fsAllowed {
			if fsMap[allowedfs] != "" {
				return map[string]string{}, fmt.Errorf("Filesystem %s cannot appear in security.syscalls.intercept.mount.allowed and security.syscalls.intercept.mount.fuse", allowedfs)
			}

			fsMap[allowedfs] = ""
		}
	}

	return fsMap, nil
}

// MountSyscallValid checks whether this is a mount syscall we intercept.
func (srv *Server) MountSyscallValid(c Instance, args *MountArgs) (bool, string) {
	fsMap, err := SyscallInterceptMountFilter(c.ExpandedConfig())
	if err != nil {
		return false, ""
	}

	fuse, ok := fsMap[args.fstype]
	if ok {
		return true, fuse
	}

	return false, ""
}

// lxcfsProcDirs are where the system LXCFS may be mounted, as the LXC driver looks for it.
var lxcfsProcDirs = []string{"/var/lib/lxcfs/proc", "/var/lib/incus-lxcfs/proc"}

// procLXCFSSkipped is the forksyscall exit code for a proc mount it deliberately left to the
// kernel: the caller lacks CAP_SYS_ADMIN, is in a user namespace nested below the instance's,
// names a /proc/self target other than one of its descriptors, or passed a target or options
// that make the mount fail, which the kernel then reports to it.
const procLXCFSSkipped = 3

// mountSyscallLXCFS reports whether this mount is a new proc file system the instance wants
// overlaid with LXCFS and, if so, the LXCFS proc directory to use (empty if none is mounted).
func (srv *Server) mountSyscallLXCFS(c Instance, args *MountArgs) (string, bool) {
	if args.fstype != "proc" || !util.IsTrue(c.ExpandedConfig()["security.syscalls.intercept.mount.lxcfs"]) {
		return "", false
	}

	if args.flags&(C.MS_REMOUNT|C.MS_BIND|C.MS_MOVE) != 0 {
		return "", false
	}

	for _, dir := range lxcfsProcDirs {
		if util.PathExists(filepath.Join(dir, "meminfo")) {
			return dir, true
		}
	}

	return "", true
}

// warnProcLXCFS records a warning on the instance for a proc mount that was left without the
// LXCFS overlay, so that software in it sizing itself from the host's resources can be traced.
func (srv *Server) warnProcLXCFS(c Instance, message string) {
	if srv.s == nil || srv.s.DB.Cluster == nil || c.ID() <= 0 {
		return
	}

	err := srv.s.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		return tx.UpsertWarning(ctx, srv.s.ServerName, c.Project().Name, cluster.TypeInstance, c.ID(), warningtype.ProcLXCFSOverlayFailed, message)
	})
	if err != nil {
		logger.Warn("Failed to record LXCFS overlay warning", logger.Ctx{"project": c.Project().Name, "instance": c.Name(), "err": err})
	}
}

// fuseblkHostAuthorizedResize is the fuse4fs option that lets an online resize request
// through although the kernel withholds the caller's uid on an idmapped mount: it tells
// fuse4fs that the host checks the callers itself.
const fuseblkHostAuthorizedResize = "host_authorized_resize"

// stripMountOption removes every entry named key (bare or key=value) from a comma-separated option list.
func stripMountOption(opts string, key string) string {
	kept := []string{}
	for opt := range strings.SplitSeq(opts, ",") {
		if opt == "" {
			continue
		}

		name, _, _ := strings.Cut(opt, "=")
		if name == key {
			continue
		}

		kept = append(kept, opt)
	}

	return strings.Join(kept, ",")
}

// mountFuseblkOpts appends the configured fuseblk helper options to the options derived
// from the guest's mount call. The host_authorized_resize option is Incus' to give: it is
// added when the EXT4_IOC_RESIZE_FS interception checks the callers and dropped from both
// the guest's and the configured options otherwise, so that neither the guest nor the
// configuration alone can open the resize to everyone in the instance.
func mountFuseblkOpts(fuseOpts string, extraOpts string, authorizeResize bool) string {
	opts := []string{}
	fuseOpts = stripMountOption(fuseOpts, fuseblkHostAuthorizedResize)
	if fuseOpts != "" {
		opts = append(opts, fuseOpts)
	}

	extraOpts = stripMountOption(extraOpts, fuseblkHostAuthorizedResize)
	if extraOpts != "" {
		opts = append(opts, extraOpts)
	}

	if authorizeResize {
		opts = append(opts, fuseblkHostAuthorizedResize)
	}

	return strings.Join(opts, ",")
}

// unixBlockDeviceNumbers returns the host major and minor numbers of a unix-block device entry.
func unixBlockDeviceNumbers(dev deviceConfig.Device) (uint32, uint32, bool) {
	if dev["major"] != "" && dev["minor"] != "" {
		major, err := strconv.ParseUint(dev["major"], 10, 32)
		if err != nil {
			return 0, 0, false
		}

		minor, err := strconv.ParseUint(dev["minor"], 10, 32)
		if err != nil {
			return 0, 0, false
		}

		return uint32(major), uint32(minor), true
	}

	source := dev["source"]
	if source == "" {
		source = dev["path"]
	}

	var st unix.Stat_t
	err := unix.Stat(source, &st)
	if err != nil || st.Mode&unix.S_IFMT != unix.S_IFBLK {
		return 0, 0, false
	}

	return unix.Major(uint64(st.Rdev)), unix.Minor(uint64(st.Rdev)), true
}

// mountSyscallFuseblkDevice reports whether a FUSE-redirected mount should be created as a host-side
// fuseblk superblock: the instance must enable it, the call must create a new mount from an absolute
// path, and that path must be a block device whose numbers match one of the instance's own unix-block
// devices. It returns the device numbers the host side must find again when it opens the source.
func (srv *Server) mountSyscallFuseblkDevice(c Instance, args *MountArgs) (uint32, uint32, bool) {
	if !util.IsTrue(c.ExpandedConfig()["security.syscalls.intercept.mount.fuseblk"]) {
		return 0, 0, false
	}

	if args.flags&(C.MS_REMOUNT|C.MS_BIND|C.MS_MOVE) != 0 {
		return 0, 0, false
	}

	if !filepath.IsAbs(args.source) {
		return 0, 0, false
	}

	var st unix.Stat_t
	err := unix.Lstat(filepath.Join(fmt.Sprintf("/proc/%d/root/", args.pid), args.source), &st)
	if err != nil || st.Mode&unix.S_IFMT != unix.S_IFBLK {
		return 0, 0, false
	}

	major := unix.Major(uint64(st.Rdev))
	minor := unix.Minor(uint64(st.Rdev))
	if !instanceUnixBlockDevice(c, major, minor) {
		return 0, 0, false
	}

	return major, minor, true
}

// instanceUnixBlockDevice reports whether the device numbers are those of one of the instance's
// own unix-block devices.
func instanceUnixBlockDevice(c Instance, major uint32, minor uint32) bool {
	for _, dev := range c.ExpandedDevices() {
		if dev["type"] != "unix-block" {
			continue
		}

		devMajor, devMinor, ok := unixBlockDeviceNumbers(dev)
		if ok && devMajor == major && devMinor == minor {
			return true
		}
	}

	return false
}

// fuseblkRemountAllowed reports whether a mount call is a remount Incus performs on a fuseblk
// superblock it created: a plain remount, neither a bind nor a move, and without mandatory
// locking, which FUSE refuses.
func fuseblkRemountAllowed(flags int) bool {
	if flags&C.MS_REMOUNT == 0 || flags&(C.MS_BIND|C.MS_MOVE) != 0 {
		return false
	}

	return flags&C.MS_MANDLOCK == 0
}

// mountSyscallFuseblkRemount reports whether a mount call remounts the root of a fuseblk mount
// Incus created for one of the instance's own unix-block devices: the instance must enable
// fuseblk mounts, the call must be a plain remount of an absolute path, and the directory there
// must be on such a device. It returns the device numbers the host side must find again on the
// mount it picks.
func (srv *Server) mountSyscallFuseblkRemount(c Instance, args *MountArgs) (uint32, uint32, bool) {
	if !util.IsTrue(c.ExpandedConfig()["security.syscalls.intercept.mount.fuseblk"]) {
		return 0, 0, false
	}

	if !fuseblkRemountAllowed(args.flags) || !filepath.IsAbs(args.target) {
		return 0, 0, false
	}

	// Cached attributes are enough for the device, and don't ask the instance's FUSE server.
	var stx unix.Statx_t
	err := unix.Statx(unix.AT_FDCWD, filepath.Join(fmt.Sprintf("/proc/%d/root/", args.pid), args.target), unix.AT_STATX_DONT_SYNC, unix.STATX_TYPE, &stx)
	if err != nil || stx.Mode&unix.S_IFMT != unix.S_IFDIR {
		return 0, 0, false
	}

	if !instanceUnixBlockDevice(c, stx.Dev_major, stx.Dev_minor) {
		return 0, 0, false
	}

	return stx.Dev_major, stx.Dev_minor, true
}

// MountSyscallShift checks whether this mount syscall needs shifting.
func (srv *Server) MountSyscallShift(c Instance, path string, fsType string) idmap.StorageType {
	if util.IsTrue(c.ExpandedConfig()["security.syscalls.intercept.mount.shift"]) {
		diskIdmap, err := c.DiskIdmap()
		if err != nil {
			return idmap.StorageTypeNone
		}

		if diskIdmap == nil {
			return c.IdmappedStorage(path, fsType)
		}
	}

	return idmap.StorageTypeNone
}

var pageSize = 4096

func init() {
	tmp := unix.Getpagesize()
	if tmp > 0 {
		pageSize = tmp
	}
}

//go:build linux && cgo

package cephownership

/*
#cgo LDFLAGS: -ldl
#include <dlfcn.h>
#include <errno.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

// The librados C ABI is loaded at runtime; non-Ceph daemons need no Ceph libraries.
static int ownership_cas(const char *cluster_name, const char *user, const char *config,
                        const char *fsid, int64_t pool_id, const char *object,
                        const char *previous, const char *next) {
    void *lib = dlopen("librados.so.2", RTLD_NOW | RTLD_LOCAL);
    if (!lib) return -ENOSYS;

    int (*create)(void **, const char *, const char *, uint64_t) = dlsym(lib, "rados_create2");
    int (*read_conf)(void *, const char *) = dlsym(lib, "rados_conf_read_file");
    int (*connect_cluster)(void *) = dlsym(lib, "rados_connect");
    int (*get_fsid)(void *, char *, size_t) = dlsym(lib, "rados_cluster_fsid");
    int (*create_io)(void *, int64_t, void **) = dlsym(lib, "rados_ioctx_create2");
    void (*destroy_io)(void *) = dlsym(lib, "rados_ioctx_destroy");
    void (*shutdown_cluster)(void *) = dlsym(lib, "rados_shutdown");
    void *(*create_op)(void) = dlsym(lib, "rados_create_write_op");
    void (*release_op)(void *) = dlsym(lib, "rados_release_write_op");
    void (*assert_exists)(void *) = dlsym(lib, "rados_write_op_assert_exists");
    void (*compare)(void *, const char *, uint8_t, const char *, size_t, int *) = dlsym(lib, "rados_write_op_omap_cmp");
    void (*set)(void *, const char * const *, const char * const *, const size_t *, size_t) = dlsym(lib, "rados_write_op_omap_set");
    int (*operate)(void *, void *, const char *, time_t *, int) = dlsym(lib, "rados_write_op_operate");
    if (!create || !read_conf || !connect_cluster || !get_fsid || !create_io || !destroy_io ||
        !shutdown_cluster || !create_op || !release_op || !assert_exists || !compare || !set || !operate) {
        dlclose(lib);
        return -ENOSYS;
    }

    void *cluster = NULL;
    void *io = NULL;
    int result = create(&cluster, cluster_name, user, 0);
    if (result < 0) goto done;
    result = read_conf(cluster, *config ? config : NULL);
    if (result < 0) goto done;
    result = connect_cluster(cluster);
    if (result < 0) goto done;
    char actual_fsid[37] = {0};
    result = get_fsid(cluster, actual_fsid, sizeof(actual_fsid));
    if (result < 0) goto done;
    if (strcmp(actual_fsid, fsid) != 0) {
        result = -EXDEV;
        goto done;
    }
    result = create_io(cluster, pool_id, &io);
    if (result < 0) goto done;

    // RBD format-2 user metadata uses raw values under the metadata_ OMAP prefix.
    const char *key = "metadata_incus.openstack.materialization_ownership";
    for (int retry = 0; retry < 2; retry++) {
        const char *expected = retry == 0 ? previous : next;
        void *op = create_op();
        if (!op) {
            result = -ENOMEM;
            break;
        }
        int compare_result = 0;
        assert_exists(op);
        compare(op, key, 1, expected, strlen(expected), &compare_result);
        const char *keys[] = {key};
        const char *values[] = {next};
        size_t lengths[] = {strlen(next)};
        set(op, keys, values, lengths, 1);
        result = operate(op, io, object, NULL, 0);
        release_op(op);
        if (result >= 0 || result != -ECANCELED) break;
    }

done:
    if (io) destroy_io(io);
    if (cluster) shutdown_cluster(cluster);
    dlclose(lib);
    return result;
}
*/
import "C"

import (
	"fmt"
	"syscall"
	"unsafe"
)

// Transfer replaces an exact marker atomically, or accepts a replay of the same replacement.
// The caller must already hold the external fenced takeover grant; this does not fence a host.
func Transfer(binding Binding, previous string, next string) error {
	err := binding.validate(previous, next)
	if err != nil {
		return err
	}

	values := []string{binding.Cluster, "client." + binding.User, binding.ConfigFile, binding.FSID, "rbd_header." + binding.ImageID, previous, next}
	args := make([]*C.char, len(values))
	for i, value := range values {
		args[i] = C.CString(value)
	}

	defer func() {
		for _, arg := range args {
			C.free(unsafe.Pointer(arg))
		}
	}()

	result := C.ownership_cas(args[0], args[1], args[2], args[3], C.int64_t(binding.PoolID), args[4], args[5], args[6])
	if result < 0 {
		return fmt.Errorf("Transfer immutable RBD ownership: %w", syscall.Errno(-result))
	}

	return nil
}

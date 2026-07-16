package storage

import (
	"github.com/lxc/incus/v7/internal/server/storage/drivers"
)

// PoolSharedIdentity returns the identity of the remote shared storage backing the
// given pool (currently the Ceph cluster fsid and OSD pool name), or empty strings
// when the pool is not backed by shared remote storage or the identity cannot be
// determined.
//
// Two independent servers reporting the same identity see the exact same volumes,
// which allows a migration between them to skip the data transfer and hand the
// volume over in place.
func PoolSharedIdentity(pool Pool) (string, string) {
	if pool.Driver().Info().Name != "ceph" {
		return "", ""
	}

	config := pool.ToAPI().Config

	clusterName := config["ceph.cluster_name"]
	if clusterName == "" {
		clusterName = drivers.CephDefaultCluster
	}

	userName := config["ceph.user.name"]
	if userName == "" {
		userName = drivers.CephDefaultUser
	}

	fsid, err := drivers.CephFsid(clusterName, userName)
	if err != nil {
		return "", ""
	}

	return fsid, config["ceph.osd.pool_name"]
}

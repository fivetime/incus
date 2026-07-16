package storage

import (
	"context"
	"fmt"

	"github.com/lxc/incus/v7/internal/server/db"
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
	driverName := pool.Driver().Info().Name
	if driverName != "ceph" && driverName != "cephext" {
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

// checkExternalVolumeClaimUnique returns an error when another volume in the
// pool already claims the same externally managed image. Two claims of the
// same image on one server would allow its filesystem to be used twice
// concurrently, corrupting it. Cross-server exclusion is the external owner's
// responsibility (e.g. Cinder attachment tracking).
func checkExternalVolumeClaimUnique(pool Pool, projectName string, volumeName string, volumeConfig map[string]string) error {
	imageName := volumeConfig["ceph.rbd.image_name"]
	if imageName == "" {
		return nil
	}

	p, ok := pool.(*backend)
	if !ok {
		return nil
	}

	var volumes []*db.StorageVolume
	err := p.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		var err error
		volumes, err = tx.GetStoragePoolVolumes(ctx, pool.ID(), false)
		return err
	})
	if err != nil {
		return fmt.Errorf("Failed checking for existing claims of RBD image %q: %w", imageName, err)
	}

	for _, vol := range volumes {
		if vol.Config["ceph.rbd.image_name"] != imageName {
			continue
		}

		if vol.Project == projectName && vol.Name == volumeName {
			continue // The volume being (re-)created itself.
		}

		return fmt.Errorf("RBD image %q is already claimed by volume %q in project %q", imageName, vol.Name, vol.Project)
	}

	return nil
}

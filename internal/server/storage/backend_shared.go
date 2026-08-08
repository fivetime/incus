package storage

import (
	"context"
	"crypto/sha256"
	"fmt"

	internalInstance "github.com/lxc/incus/v7/internal/instance"
	"github.com/lxc/incus/v7/internal/server/db"
	"github.com/lxc/incus/v7/internal/server/storage/drivers"
	"github.com/lxc/incus/v7/shared/api"
)

// externalRBDClaimIdentity identifies an externally-owned RBD image independently
// of the Incus pool name used to reach it.
//
// Ceph cluster aliases and client names are local connection details. The FSID,
// OSD pool and image name identify the physical resource that must have only one
// local cephext claim.
type externalRBDClaimIdentity struct {
	ClusterFSID string
	OSDPool     string
	ImageName   string
}

func externalRBDClaimIdentityFromConfig(config map[string]string, imageName string, fsidLookup func(string, string) (string, error)) (externalRBDClaimIdentity, error) {
	if imageName == "" {
		return externalRBDClaimIdentity{}, fmt.Errorf("Cannot identify an external RBD claim without an image name")
	}

	clusterName := config["ceph.cluster_name"]
	if clusterName == "" {
		clusterName = drivers.CephDefaultCluster
	}

	userName := config["ceph.user.name"]
	if userName == "" {
		userName = drivers.CephDefaultUser
	}

	osdPoolName := config["ceph.osd.pool_name"]
	if osdPoolName == "" {
		osdPoolName = config["source"]
	}

	if osdPoolName == "" {
		return externalRBDClaimIdentity{}, fmt.Errorf("Cannot identify an external RBD claim without an OSD pool")
	}

	fsid, err := fsidLookup(clusterName, userName)
	if err != nil {
		return externalRBDClaimIdentity{}, fmt.Errorf("Get Ceph FSID for external RBD claim: %w", err)
	}

	if fsid == "" {
		return externalRBDClaimIdentity{}, fmt.Errorf("Ceph cluster %q returned an empty FSID", clusterName)
	}

	return externalRBDClaimIdentity{
		ClusterFSID: fsid,
		OSDPool:     osdPoolName,
		ImageName:   imageName,
	}, nil
}

func externalRBDClaimLockName(identity externalRBDClaimIdentity) string {
	// Lock names are internal only. Hashing prevents pool names or Ceph FSIDs
	// containing separators from changing the lock namespace.
	digest := sha256.Sum256([]byte(identity.ClusterFSID + "\x00" + identity.OSDPool + "\x00" + identity.ImageName))
	return fmt.Sprintf("ExternalVolumeClaim_%x", digest[:])
}

func externalRBDClaimIdentityForPool(pool Pool, imageName string) (externalRBDClaimIdentity, error) {
	return externalRBDClaimIdentityFromConfig(pool.ToAPI().Config, imageName, drivers.CephFsid)
}

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

func instanceStorageVolumeShouldDelete(volExists bool, config map[string]string) bool {
	return volExists && !internalInstance.StorageDeleteProtected(config)
}

func instanceStorageVolumeShouldNormalizeRootfs(driverName string, volExists bool, config map[string]string) bool {
	return driverName == "cephext" && instanceStorageVolumeShouldDelete(volExists, config)
}

func instanceStorageVolumeConfigForDelete(driverName string, config map[string]string) map[string]string {
	if driverName == "cephext" {
		return config
	}

	return nil
}

// checkExternalVolumeClaimUnique returns an error when another volume in a
// cephext pool backed by the same physical Ceph image already claims it. Two
// claims of the same image on one server would allow its filesystem to be used
// twice concurrently, corrupting it. Cross-server exclusion is the external
// owner's responsibility (e.g. Cinder attachment tracking).
func checkExternalVolumeClaimUnique(pool Pool, identity externalRBDClaimIdentity, projectName string, volumeName string, volumeConfig map[string]string) error {
	imageName := volumeConfig["ceph.rbd.image_name"]
	if imageName == "" {
		return nil
	}

	p, ok := pool.(*backend)
	if !ok {
		return nil
	}

	type claimCandidate struct {
		pool    api.StoragePool
		volumes []*db.StorageVolume
	}

	var candidates []claimCandidate
	err := p.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		pools, _, err := tx.GetStoragePools(ctx, nil)
		if err != nil {
			return err
		}

		for poolID, candidatePool := range pools {
			if candidatePool.Driver != "cephext" {
				continue
			}

			volumes, err := tx.GetStoragePoolVolumes(ctx, poolID, false)
			if err != nil {
				return err
			}

			candidates = append(candidates, claimCandidate{pool: candidatePool, volumes: volumes})
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("Failed checking for existing claims of RBD image %q: %w", imageName, err)
	}

	for _, candidate := range candidates {
		hasImageClaim := false
		for _, vol := range candidate.volumes {
			if vol.Config["ceph.rbd.image_name"] == imageName {
				hasImageClaim = true
				break
			}
		}
		if !hasImageClaim {
			continue
		}

		candidateIdentity := identity
		if candidate.pool.Name != pool.Name() {
			candidateIdentity, err = externalRBDClaimIdentityFromConfig(candidate.pool.Config, imageName, drivers.CephFsid)
			if err != nil {
				return fmt.Errorf("Identify cephext pool %q while checking claim uniqueness: %w", candidate.pool.Name, err)
			}
		}

		if candidateIdentity != identity {
			continue
		}

		for _, vol := range candidate.volumes {
			if vol.Config["ceph.rbd.image_name"] != imageName {
				continue
			}

			if candidate.pool.Name == pool.Name() && vol.Project == projectName && vol.Name == volumeName {
				continue // The volume being (re-)created itself.
			}

			return fmt.Errorf("RBD image %q is already claimed by volume %q in project %q through cephext pool %q", imageName, vol.Name, vol.Project, candidate.pool.Name)
		}
	}

	return nil
}

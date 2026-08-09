//go:build linux && cgo && !agent

package storagematerializationattempt

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/lxc/incus/v7/internal/server/db"
)

const materializationOwnershipVersion = 1

// OwnershipMarker returns the canonical storage-object ownership marker for a
// newly materialized rootfs generation.
func OwnershipMarker(attempt *db.StorageMaterializationAttempt, storageIdentity string) (string, error) {
	if attempt == nil {
		return "", errors.New("Storage materialization attempt is required")
	}

	if attempt.CleanupDisposition != CleanupDelete || !attempt.BaselineClean {
		return "", errors.New("Only a clean-baseline delete attempt can own a newly materialized storage object")
	}

	if storageIdentity == "" {
		return "", errors.New("Materialization ownership requires an immutable storage identity")
	}

	bound := *attempt
	bound.StorageIdentity = storageIdentity
	err := validateBinding(&bound)
	if err != nil {
		return "", err
	}

	canonical := struct {
		Version            int    `json:"version"`
		Token              string `json:"token"`
		AllocationID       string `json:"allocation_id"`
		ComputeID          string `json:"compute_id"`
		Owner              string `json:"owner"`
		Project            string `json:"project"`
		InstanceName       string `json:"instance_name"`
		IDMapBase          int64  `json:"idmap_base"`
		IDMapSize          int64  `json:"idmap_size"`
		StorageDriver      string `json:"storage_driver"`
		StoragePool        string `json:"storage_pool"`
		StorageVolume      string `json:"storage_volume"`
		RBDImage           string `json:"rbd_image"`
		StorageIdentity    string `json:"storage_identity"`
		BaselineClean      bool   `json:"baseline_clean"`
		CleanupDisposition string `json:"cleanup_disposition"`
	}{
		Version: materializationOwnershipVersion,
		Token:   bound.Token, AllocationID: bound.AllocationID,
		ComputeID: bound.ComputeID, Owner: bound.Owner,
		Project: bound.Project, InstanceName: bound.InstanceName,
		IDMapBase: bound.IDMapBase, IDMapSize: bound.IDMapSize,
		StorageDriver: bound.StorageDriver, StoragePool: bound.StoragePool,
		StorageVolume: bound.StorageVolume, RBDImage: bound.RBDImage,
		StorageIdentity: storageIdentity, BaselineClean: bound.BaselineClean,
		CleanupDisposition: bound.CleanupDisposition,
	}

	data, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}

	digest := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", digest), nil
}

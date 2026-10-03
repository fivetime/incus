package drivers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/lxc/incus/v7/internal/server/locking"
	"github.com/lxc/incus/v7/internal/server/storage/cephownership"
	"github.com/lxc/incus/v7/shared/subprocess"
)

// TransferVolumeMaterializationOwnership requires the caller's fenced, exclusive takeover grant.
func (d *ceph) TransferVolumeMaterializationOwnership(vol Volume, expectedIdentity string, clusterFSID string, previous string, next string) error {
	expected, err := parseCanonicalRBDVolumeIdentity(expectedIdentity)
	if err != nil {
		return err
	}

	if vol.contentType != ContentTypeFS || vol.volType != VolumeTypeContainer {
		return errors.New("Fenced RBD takeover supports container filesystem roots only")
	}

	if expected.PoolID > uint64(^uint64(0)>>1) {
		return errors.New("RBD pool ID exceeds librados bounds")
	}

	// Preserve the flatten-before-mount lock order used by identity-bound deletion.
	flattenUnlock, err := locking.Lock(context.TODO(), d.flattenLockName(vol))
	if err != nil {
		return err
	}

	defer flattenUnlock()
	unlock, err := vol.MountLock()
	if err != nil {
		return err
	}

	defer unlock()
	identity, err := d.GetVolumeIdentity(vol)
	if err != nil {
		return err
	}

	if identity != expectedIdentity {
		return errors.New("RBD name no longer identifies the takeover root")
	}

	err = cephownership.Transfer(cephownership.Binding{
		Cluster: d.config["ceph.cluster_name"],
		User:    d.config["ceph.user.name"],
		FSID:    clusterFSID,
		PoolID:  int64(expected.PoolID),
		ImageID: expected.ID,
	}, previous, next)
	if err != nil {
		return err
	}

	identity, err = d.GetVolumeIdentity(vol)
	if err != nil {
		return err
	}

	if identity != expectedIdentity {
		return errors.New("RBD name changed during takeover; original identity remains protected")
	}

	return nil
}

const cephMaterializationOwnershipKey = "incus.openstack.materialization_ownership"

// GetStorageClusterIdentity returns the configured Ceph cluster's canonical FSID.
func (d *ceph) GetStorageClusterIdentity() (string, error) {
	out, err := subprocess.RunCommand("ceph", "--cluster", d.config["ceph.cluster_name"], "--id", d.config["ceph.user.name"], "fsid")
	if err != nil {
		return "", err
	}

	fsid := strings.TrimSpace(out)
	parsed, err := uuid.Parse(fsid)
	if err != nil || parsed == uuid.Nil || parsed.String() != fsid {
		return "", errors.New("Ceph returned an invalid cluster FSID")
	}

	return fsid, nil
}

// GetVolumeMaterializationOwnership returns the ownership marker stored on an
// RBD image. An absent marker is returned as an empty string.
func (d *ceph) GetVolumeMaterializationOwnership(vol Volume) (string, error) {
	out, err := subprocess.RunCommand(
		"rbd",
		"--id", d.config["ceph.user.name"],
		"--cluster", d.config["ceph.cluster_name"],
		"--pool", d.config["ceph.osd.pool_name"],
		"image-meta", "list",
		d.getRBDVolumeName(vol, "", false),
		"--format", "json",
	)
	if err != nil {
		return "", fmt.Errorf("List RBD image metadata: %w", err)
	}

	return parseCephMaterializationOwnership(out)
}

func parseCephMaterializationOwnership(data string) (string, error) {
	// rbd image-meta list prints no output at all (rather than "{}") for an
	// image without any metadata keys, which is the normal state of a volume
	// cloned from a pristine image cache.
	if strings.TrimSpace(data) == "" {
		return "", nil
	}

	metadata := map[string]string{}
	err := json.Unmarshal([]byte(data), &metadata)
	if err != nil {
		return "", fmt.Errorf("Invalid RBD image metadata: %w", err)
	}

	return metadata[cephMaterializationOwnershipKey], nil
}

// SetVolumeMaterializationOwnership persists ownership evidence on an RBD
// image. A newly cloned image can inherit metadata from its parent, so the
// create path replaces stale evidence and verifies the new value immediately.
// Crash recovery never calls this method; it only accepts an exact marker.
func (d *ceph) SetVolumeMaterializationOwnership(vol Volume, ownership string) error {
	if ownership == "" {
		return errors.New("RBD materialization ownership cannot be empty")
	}

	existing, err := d.GetVolumeMaterializationOwnership(vol)
	if err != nil {
		return err
	}

	if existing == ownership {
		return nil
	}

	_, err = subprocess.RunCommand(
		"rbd",
		"--id", d.config["ceph.user.name"],
		"--cluster", d.config["ceph.cluster_name"],
		"--pool", d.config["ceph.osd.pool_name"],
		"image-meta", "set",
		d.getRBDVolumeName(vol, "", false),
		cephMaterializationOwnershipKey,
		ownership,
	)
	if err != nil {
		return fmt.Errorf("Set RBD materialization ownership: %w", err)
	}

	persisted, err := d.GetVolumeMaterializationOwnership(vol)
	if err != nil {
		return err
	}

	if persisted != ownership {
		return errors.New("RBD materialization ownership verification failed")
	}

	return nil
}

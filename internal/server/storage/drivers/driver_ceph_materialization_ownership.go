package drivers

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lxc/incus/v7/shared/subprocess"
)

const cephMaterializationOwnershipKey = "incus.openstack.materialization_ownership"

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
	if err := json.Unmarshal([]byte(data), &metadata); err != nil {
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

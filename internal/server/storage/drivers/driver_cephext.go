package drivers

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/lxc/incus/v7/internal/instancewriter"
	"github.com/lxc/incus/v7/internal/server/backup"
	localMigration "github.com/lxc/incus/v7/internal/server/migration"
	"github.com/lxc/incus/v7/internal/server/operations"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/revert"
	"github.com/lxc/incus/v7/shared/units"
	"github.com/lxc/incus/v7/shared/validate"
)

// cephext is a variant of the ceph driver for volumes whose RBD images are
// managed by an external system (e.g. OpenStack Cinder). Each volume maps to a
// pre-existing image in the configured OSD pool through the ceph.rbd.image_name
// volume configuration key: creating a volume claims the image in place and
// deleting a volume merely releases it. All operations that would alter the
// image's data or its snapshots outside of the external owner's control
// (snapshots, copies, backups, resizing) are refused.
type cephext struct {
	ceph
}

// Info returns info about the driver and its environment.
func (d *cephext) Info() Info {
	info := d.ceph.Info()
	info.Name = "cephext"
	info.OptimizedImages = false
	info.VolumeTypes = []VolumeType{VolumeTypeCustom, VolumeTypeContainer}

	return info
}

// Create checks that the externally managed OSD pool exists.
func (d *cephext) Create() error {
	// The OSD pool is owned by the external system, so it must be specified
	// explicitly and must already exist. No placeholder volume is created as the
	// pool is expected to be shared.
	if d.config["source"] == "" {
		return errors.New(`The cephext driver requires "source" to be set to an existing OSD pool`)
	}

	if d.config["ceph.osd.pool_name"] != "" && d.config["source"] != d.config["ceph.osd.pool_name"] {
		return errors.New(`The "source" and "ceph.osd.pool_name" property must not differ for Ceph OSD storage pools`)
	}

	d.config["ceph.osd.pool_name"] = d.config["source"]

	poolExists, err := d.osdPoolExists()
	if err != nil {
		return fmt.Errorf("Failed checking the existence of the ceph %q osd pool: %w", d.config["ceph.osd.pool_name"], err)
	}

	if !poolExists {
		return fmt.Errorf("The ceph %q osd pool doesn't exist", d.config["ceph.osd.pool_name"])
	}

	// The pool is never owned by this driver, so it must never be deleted.
	d.config["volatile.pool.pristine"] = "false"

	return nil
}

// Mount mounts the storage pool.
func (d *cephext) Mount() (bool, error) {
	// The pool has no placeholder volume, nothing to check.
	return true, nil
}

// commonVolumeRules returns validation rules which are common for all volume types.
func (d *cephext) commonVolumeRules() map[string]func(value string) error {
	rules := d.ceph.commonVolumeRules()

	// gendoc:generate(entity=storage_volume_cephext, group=common, key=ceph.rbd.image_name)
	//
	// ---
	//  type: string
	//  condition: -
	//  default: -
	//  shortdesc: Name of the externally managed RBD image backing the volume
	rules["ceph.rbd.image_name"] = validate.IsAny

	return rules
}

// ValidateVolume validates the supplied volume config.
func (d *cephext) ValidateVolume(vol Volume, removeUnknownKeys bool) error {
	return d.validateVolume(vol, d.commonVolumeRules(), removeUnknownKeys)
}

// CreateVolume claims the pre-existing externally managed RBD image the volume
// refers to. No image is ever created and no filler is run as the image content
// is managed by the external owner.
func (d *cephext) CreateVolume(vol Volume, filler *VolumeFiller, op *operations.Operation) error {
	if vol.config["ceph.rbd.image_name"] == "" {
		return errors.New(`Volumes require the "ceph.rbd.image_name" configuration key to refer to an existing RBD image`)
	}

	volExists, err := d.HasVolume(vol)
	if err != nil {
		return err
	}

	if !volExists {
		return fmt.Errorf("RBD image %q not found in the %q osd pool", vol.config["ceph.rbd.image_name"], d.config["ceph.osd.pool_name"])
	}

	return vol.EnsureMountPath(false)
}

// DeleteVolume releases the externally managed RBD image, leaving it untouched.
func (d *cephext) DeleteVolume(vol Volume, op *operations.Operation) error {
	// Unmount and unmap.
	_, err := d.UnmountVolume(vol, false, op)
	if err != nil {
		return err
	}

	err = os.Remove(vol.MountPath())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("Failed to remove '%s': %w", vol.MountPath(), err)
	}

	return nil
}

// CreateVolumeFromBackup is not supported on externally managed volumes.
func (d *cephext) CreateVolumeFromBackup(vol Volume, srcBackup backup.Info, srcData io.ReadSeeker, basePrefix string, op *operations.Operation) (VolumePostHook, revert.Hook, error) {
	return nil, nil, ErrNotSupported
}

// CreateVolumeFromCopy is not supported on externally managed volumes.
func (d *cephext) CreateVolumeFromCopy(vol Volume, srcVol Volume, copySnapshots bool, allowInconsistent bool, op *operations.Operation) error {
	return ErrNotSupported
}

// CreateVolumeFromMigration claims the volume in place when the shared storage
// handover was negotiated, any other form of migration is refused.
func (d *cephext) CreateVolumeFromMigration(vol Volume, conn io.ReadWriteCloser, volTargetArgs localMigration.VolumeTargetArgs, preFiller *VolumeFiller, op *operations.Operation) error {
	if !volTargetArgs.SharedStorage && volTargetArgs.ClusterMoveSourceName == "" {
		return ErrNotSupported
	}

	return d.ceph.CreateVolumeFromMigration(vol, conn, volTargetArgs, preFiller, op)
}

// MigrateVolume sends nothing when the shared storage handover was negotiated,
// any other form of migration is refused.
func (d *cephext) MigrateVolume(vol Volume, conn io.ReadWriteCloser, volSrcArgs *localMigration.VolumeSourceArgs, op *operations.Operation) error {
	if !volSrcArgs.SharedStorage && !volSrcArgs.ClusterMove {
		return ErrNotSupported
	}

	return d.ceph.MigrateVolume(vol, conn, volSrcArgs, op)
}

// RefreshVolume is not supported on externally managed volumes.
func (d *cephext) RefreshVolume(vol Volume, srcVol Volume, srcSnapshots []Volume, allowInconsistent bool, op *operations.Operation) error {
	return ErrNotSupported
}

// RenameVolume is not supported as the RBD image name is fixed by its external owner.
func (d *cephext) RenameVolume(vol Volume, newVolName string, op *operations.Operation) error {
	return ErrNotSupported
}

// SetVolumeQuota refuses any size change, the image size is managed externally.
func (d *cephext) SetVolumeQuota(vol Volume, size string, allowUnsafeResize bool, op *operations.Operation) error {
	sizeBytes, err := units.ParseByteSizeString(size)
	if err != nil {
		return err
	}

	if sizeBytes <= 0 {
		return nil
	}

	return ErrNotSupported
}

// BackupVolume is not supported on externally managed volumes.
func (d *cephext) BackupVolume(vol Volume, writer instancewriter.InstanceWriter, basePrefix string, optimized bool, snapshots []string, op *operations.Operation) error {
	return ErrNotSupported
}

// CreateVolumeSnapshot is not supported, snapshots belong to the external owner.
func (d *cephext) CreateVolumeSnapshot(snapVol Volume, op *operations.Operation) error {
	return ErrNotSupported
}

// DeleteVolumeSnapshot is not supported, snapshots belong to the external owner.
func (d *cephext) DeleteVolumeSnapshot(snapVol Volume, op *operations.Operation) error {
	return ErrNotSupported
}

// MountVolumeSnapshot is not supported, snapshots belong to the external owner.
func (d *cephext) MountVolumeSnapshot(snapVol Volume, op *operations.Operation) error {
	return ErrNotSupported
}

// UnmountVolumeSnapshot is not supported, snapshots belong to the external owner.
func (d *cephext) UnmountVolumeSnapshot(snapVol Volume, op *operations.Operation) (bool, error) {
	return false, ErrNotSupported
}

// RenameVolumeSnapshot is not supported, snapshots belong to the external owner.
func (d *cephext) RenameVolumeSnapshot(snapVol Volume, newSnapshotName string, op *operations.Operation) error {
	return ErrNotSupported
}

// RestoreVolume is not supported, snapshots belong to the external owner.
func (d *cephext) RestoreVolume(vol Volume, snapshotName string, op *operations.Operation) error {
	return ErrNotSupported
}

// VolumeSnapshots returns no snapshots, any snapshot on the RBD image belongs to
// the external owner and must stay invisible to Incus.
func (d *cephext) VolumeSnapshots(vol Volume, op *operations.Operation) ([]string, error) {
	return nil, nil
}

// GetResources returns the pool resource usage information.
func (d *cephext) GetResources() (*api.ResourcesStoragePool, error) {
	return d.ceph.GetResources()
}

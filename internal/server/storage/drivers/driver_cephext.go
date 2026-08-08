package drivers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/lxc/incus/v7/internal/instancewriter"
	"github.com/lxc/incus/v7/internal/linux"
	"github.com/lxc/incus/v7/internal/server/backup"
	localMigration "github.com/lxc/incus/v7/internal/server/migration"
	"github.com/lxc/incus/v7/internal/server/operations"
	"github.com/lxc/incus/v7/internal/server/rootfsidmap"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/revert"
	"github.com/lxc/incus/v7/shared/subprocess"
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
	rules["ceph.rbd.image_name"] = validate.Optional(func(value string) error {
		// A plain image name only: snapshot ("@") and pool/namespace ("/")
		// references or any other special characters must be rejected as the
		// name ends up in rbd command lines and maps to exactly one image in
		// the pool.
		if len(value) > 255 {
			return errors.New("RBD image name is too long")
		}

		for _, r := range value {
			if !isCephRBDNameCharacter(r) {
				return fmt.Errorf("Invalid character %q in RBD image name, only alphanumeric characters and \"-\", \".\", \"_\" are allowed", r)
			}
		}

		return nil
	})

	// gendoc:generate(entity=storage_volume_cephext, group=common, key=ceph.rbd.idmap_provenance)
	//
	// ---
	//  type: string
	//  condition: -
	//  default: -
	//  shortdesc: Trusted one-time offline repair assertion that an external root is namespace-owned
	rules["ceph.rbd.idmap_provenance"] = validate.Optional(validate.IsOneOf("normalized"))

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

	// Refuse image-backed creation loudly rather than silently returning a
	// volume without the image's content: the claimed image must already
	// contain a prepared filesystem. Fillers without a fingerprint (e.g. the
	// empty rootfs structure filler) are safe to skip as the claimed image
	// provides that content.
	if filler != nil && filler.Fingerprint != "" {
		return errors.New("Volumes on cephext pools cannot be created from an image, they claim an externally prepared RBD image instead")
	}

	volExists, err := d.HasVolume(vol)
	if err != nil {
		return err
	}

	if !volExists {
		return fmt.Errorf("RBD image %q not found in the %q osd pool", vol.config["ceph.rbd.image_name"], d.config["ceph.osd.pool_name"])
	}

	// Refuse to claim an image that is already mapped on this host, it is in
	// use through another claim or by something else entirely.
	_, _, err = d.getRBDMappedDevPath(vol, false)
	if err == nil {
		return fmt.Errorf("RBD image %q is already mapped on this host", vol.config["ceph.rbd.image_name"])
	}

	err = vol.EnsureMountPath(false)
	if err != nil {
		return err
	}

	// Verify the claimed image is actually usable before accepting it, so that
	// a wrong image fails the claim rather than the instance's first start: it
	// must carry a mountable filesystem and, for container volumes, the
	// expected top-level rootfs directory.
	if vol.contentType == ContentTypeFS {
		err = vol.MountTask(func(mountPath string, op *operations.Operation) error {
			if vol.volType == VolumeTypeContainer {
				rootfsInfo, err := os.Lstat(filepath.Join(mountPath, "rootfs"))
				if err != nil || !rootfsInfo.IsDir() {
					return fmt.Errorf("RBD image %q does not contain a top-level rootfs directory", vol.config["ceph.rbd.image_name"])
				}

				// Only an orchestrator that created this root from a normalized image may
				// assert the initial state. Existing markers always remain authoritative.
				if vol.config["ceph.rbd.idmap_provenance"] == "normalized" {
					err = rootfsidmap.SeedNormalizedRootfsIDMapProvenance(mountPath)
					if err != nil {
						return fmt.Errorf("Seed rootfs ID map provenance: %w", err)
					}
				}

				err = rootfsidmap.ValidateRootfsIDMapProvenance(mountPath)
				if err != nil {
					return fmt.Errorf("Validate rootfs ID map provenance: %w", err)
				}
			}

			return nil
		}, op)
		if err != nil {
			return err
		}
	}

	return nil
}

// cephextExistingMountMatchesTarget verifies that a mount has the same backing
// RBD device as the exact immutable image identity resolved from the configured
// Ceph cluster. A mountpoint alone is not sufficient: it could be a stale mount
// or a different image mounted over the volume's path.
func cephextExistingMountMatchesTarget(mountInfo []string, mappings []cephRBDMapping) bool {
	if len(mappings) != 1 {
		return false
	}

	separator := -1
	for i, field := range mountInfo {
		if field == "-" {
			separator = i
			break
		}
	}

	// mountinfo fields following "-" are filesystem type, mount source and
	// super options. Require the source to be the exact RBD device we found.
	if separator < 0 || len(mountInfo) <= separator+2 {
		return false
	}

	return filepath.Clean(mountInfo[separator+2]) == filepath.Clean(mappings[0].DevicePath)
}

func (d *cephext) existingMountMatchesTarget(vol Volume) (bool, error) {
	imageName := d.getRBDVolumeName(vol, "", false)
	expected, err := d.getRBDVolumeIdentity(imageName)
	if err != nil {
		return false, fmt.Errorf("Read configured RBD image identity: %w", err)
	}

	mappings, err := findRBDMappingsByIdentity("/sys/devices/rbd", expected)
	if err != nil {
		return false, fmt.Errorf("Inspect configured RBD image mappings: %w", err)
	}

	mountInfo, err := linux.GetMountinfo(vol.MountPath())
	if err != nil {
		return false, fmt.Errorf("Read existing volume mount source: %w", err)
	}

	return cephextExistingMountMatchesTarget(mountInfo, mappings), nil
}

// MountVolume refuses to bring an externally managed image into use when it is
// already in use elsewhere, then mounts it as usual.
func (d *cephext) MountVolume(vol Volume, op *operations.Operation) error {
	// The same image being used through two volumes at once, from this or any
	// other server, would corrupt its filesystem. Any RBD watcher means the
	// image is currently mapped somewhere; the only acceptable case is this
	// volume's own existing mount (mount reference counting). This check is
	// advisory (a concurrent claim can still race it), the authoritative
	// exclusion is expected from the external owner's attachment tracking.
	out, err := subprocess.RunCommand(
		"rbd",
		"--id", d.config["ceph.user.name"],
		"--cluster", d.config["ceph.cluster_name"],
		"--pool", d.config["ceph.osd.pool_name"],
		"status",
		d.getRBDVolumeName(vol, "", false),
		"--format", "json")
	if err != nil {
		return fmt.Errorf("Failed checking RBD image status: %w", err)
	}

	var status struct {
		Watchers []struct {
			Address string `json:"address"`
		} `json:"watchers"`
	}

	err = json.Unmarshal([]byte(out), &status)
	if err != nil {
		return fmt.Errorf("Failed parsing RBD image status: %w", err)
	}

	mountPointExists := vol.contentType == ContentTypeFS && linux.IsMountPoint(vol.MountPath())
	if mountPointExists {
		ownMount, err := d.existingMountMatchesTarget(vol)
		if err != nil {
			return err
		}

		// A single watcher is expected for this local mapping. More watchers
		// mean that a second client is using the image, even if the local mount
		// happens to be correct.
		if !ownMount || len(status.Watchers) != 1 {
			return fmt.Errorf("Existing mount at %q does not exclusively use RBD image %q", vol.MountPath(), vol.config["ceph.rbd.image_name"])
		}
	} else if len(status.Watchers) > 0 {
		return fmt.Errorf("RBD image %q is already in use (%d watcher(s))", vol.config["ceph.rbd.image_name"], len(status.Watchers))
	}

	return d.ceph.MountVolume(vol, op)
}

// DeleteVolume releases the externally managed RBD image, leaving it untouched.
func (d *cephext) DeleteVolume(vol Volume, op *operations.Operation) error {
	return d.deleteVolumeRelease(vol, false, op)
}

// DeleteVolumeWithIdentity releases the externally managed RBD image only if its immutable identity still matches.
func (d *cephext) DeleteVolumeWithIdentity(vol Volume, expectedStorageIdentity string, op *operations.Operation) error {
	if expectedStorageIdentity == "" {
		return errors.New("Cannot release external Ceph volume without an immutable storage identity")
	}

	return d.deleteVolumeWithIdentity(vol, expectedStorageIdentity, op)
}

func (d *cephext) deleteVolumeWithIdentity(vol Volume, expectedStorageIdentity string, op *operations.Operation) error {
	unlock, err := vol.MountLock()
	if err != nil {
		return err
	}

	defer unlock()

	return runVolumeIdentityBoundAction(vol, expectedStorageIdentity, d.GetVolumeIdentity, func() error {
		return d.deleteVolumeRelease(vol, true, op)
	})
}

func (d *cephext) deleteVolumeRelease(vol Volume, volumeLocked bool, op *operations.Operation) error {
	// Unmount and unmap. UnmountVolume takes the volume lock itself, so a
	// caller that already holds it needs the inner unmountVolume promoted
	// from the embedded ceph driver instead.
	unmountVolume := d.UnmountVolume
	if volumeLocked {
		unmountVolume = d.unmountVolume
	}

	_, err := unmountVolume(vol, false, op)
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

// SetVolumeQuota never touches the RBD image itself as its size is managed by
// the external owner. When the external owner has grown the image (e.g. a
// Cinder volume extend), a quota request up to the image's actual size grows
// the contained filesystem to fill the device; anything larger is refused as
// the image must be extended through its owner first.
func (d *cephext) SetVolumeQuota(vol Volume, size string, allowUnsafeResize bool, op *operations.Operation) error {
	sizeBytes, err := units.ParseByteSizeString(size)
	if err != nil {
		return err
	}

	if sizeBytes <= 0 {
		return nil
	}

	// Activate the volume to read the device's actual size.
	ourMap, devPath, err := d.getRBDMappedDevPath(vol, true)
	if err != nil {
		return err
	}

	if ourMap {
		defer func() { _ = d.rbdUnmapVolume(vol, true) }()
	}

	actualSizeBytes, err := BlockDiskSizeBytes(devPath)
	if err != nil {
		return fmt.Errorf("Error getting current size: %w", err)
	}

	if sizeBytes > actualSizeBytes {
		return fmt.Errorf("Volume size can only be grown through the image's external owner: %w", ErrNotSupported)
	}

	if sizeBytes < actualSizeBytes {
		// The externally managed device size is authoritative and its
		// filesystem is only ever grown to fill it, so a smaller request is
		// refused rather than recording a size that doesn't match reality or
		// silently growing beyond what was asked for.
		return fmt.Errorf("Volume size must match the externally managed device size of %d bytes: %w", actualSizeBytes, ErrCannotBeShrunk)
	}

	// The requested size matches the device, grow the contained filesystem to
	// fill it (idempotent, a no-op when it already does).
	if vol.contentType == ContentTypeFS {
		err = growFileSystem(vol.ConfigBlockFilesystem(), devPath, vol)
		if err != nil {
			return err
		}
	}

	return nil
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

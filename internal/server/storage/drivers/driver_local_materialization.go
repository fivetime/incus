package drivers

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	"github.com/lxc/incus/v7/internal/server/operations"
	"github.com/lxc/incus/v7/shared/subprocess"
)

const (
	dirMaterializationIdentityXattr = "trusted.incus.materialization_identity"
	materializationOwnershipXattr   = "trusted.incus.materialization_ownership"
	lvmMaterializationTagPrefix     = "incus.materialization."
	zfsMaterializationOwnershipProp = "incus:materialization_ownership"
)

func validateMaterializationVolume(vol Volume) error {
	if vol.Type() != VolumeTypeContainer || vol.ContentType() != ContentTypeFS || vol.IsSnapshot() {
		return errors.New("Materialization identity protection requires a container filesystem volume")
	}

	return nil
}

func validateMaterializationOwnership(ownership string) (string, error) {
	digest, found := strings.CutPrefix(ownership, "sha256:")
	if !found || len(digest) != sha256.Size*2 || strings.ToLower(digest) != digest {
		return "", errors.New("Invalid materialization ownership digest")
	}

	_, err := hex.DecodeString(digest)
	if err != nil {
		return "", errors.New("Invalid materialization ownership digest")
	}

	return digest, nil
}

func getMaterializationXattr(path string, name string) (string, error) {
	size, err := unix.Getxattr(path, name, nil)
	if errors.Is(err, unix.ENODATA) {
		return "", nil
	}

	if err != nil {
		return "", err
	}

	value := make([]byte, size)
	size, err = unix.Getxattr(path, name, value)
	if err != nil {
		return "", err
	}

	return string(value[:size]), nil
}

func setMaterializationXattr(path string, name string, value string) error {
	if value == "" {
		return errors.New("Materialization xattr value cannot be empty")
	}

	err := unix.Setxattr(path, name, []byte(value), 0)
	if err != nil {
		return err
	}

	file, err := os.Open(path)
	if err != nil {
		return err
	}

	syncErr := file.Sync()
	closeErr := file.Close()
	return errors.Join(syncErr, closeErr)
}

func getXattrMaterializationOwnership(vol Volume) (string, error) {
	return getMaterializationXattr(vol.MountPath(), materializationOwnershipXattr)
}

func setXattrMaterializationOwnership(vol Volume, ownership string) error {
	_, err := validateMaterializationOwnership(ownership)
	if err != nil {
		return err
	}

	err = setMaterializationXattr(vol.MountPath(), materializationOwnershipXattr, ownership)
	if err != nil {
		return err
	}

	persisted, err := getXattrMaterializationOwnership(vol)
	if err != nil {
		return err
	}

	if persisted != ownership {
		return errors.New("Materialization ownership xattr verification failed")
	}

	return nil
}

// InitializeVolumeIdentity replaces any identity inherited by a copied directory volume.
func (d *dir) InitializeVolumeIdentity(vol Volume) error {
	err := validateMaterializationVolume(vol)
	if err != nil {
		return err
	}

	unlock, err := vol.MountLock()
	if err != nil {
		return err
	}

	defer unlock()

	identity := uuid.NewString()
	err = setMaterializationXattr(vol.MountPath(), dirMaterializationIdentityXattr, identity)
	if err != nil {
		return fmt.Errorf("Set directory volume identity: %w", err)
	}

	persisted, err := getMaterializationXattr(vol.MountPath(), dirMaterializationIdentityXattr)
	if err != nil {
		return fmt.Errorf("Read directory volume identity: %w", err)
	}

	if persisted != identity {
		return errors.New("Directory volume identity verification failed")
	}

	return nil
}

// GetVolumeIdentity returns the trusted random identity assigned after directory materialization.
func (d *dir) GetVolumeIdentity(vol Volume) (string, error) {
	err := validateMaterializationVolume(vol)
	if err != nil {
		return "", err
	}

	value, err := getMaterializationXattr(vol.MountPath(), dirMaterializationIdentityXattr)
	if err != nil {
		return "", fmt.Errorf("Read directory volume identity: %w", err)
	}

	identity, err := uuid.Parse(value)
	if err != nil || identity.String() != value {
		return "", errors.New("Directory volume has no valid immutable identity")
	}

	return "dir:" + value, nil
}

// GetVolumeMaterializationOwnership returns directory ownership evidence.
func (d *dir) GetVolumeMaterializationOwnership(vol Volume) (string, error) {
	err := validateMaterializationVolume(vol)
	if err != nil {
		return "", err
	}

	return getXattrMaterializationOwnership(vol)
}

// SetVolumeMaterializationOwnership persists directory ownership evidence.
func (d *dir) SetVolumeMaterializationOwnership(vol Volume, ownership string) error {
	err := validateMaterializationVolume(vol)
	if err != nil {
		return err
	}

	unlock, err := vol.MountLock()
	if err != nil {
		return err
	}

	defer unlock()

	return setXattrMaterializationOwnership(vol, ownership)
}

func parseLVMVolumeIdentity(output string) (string, error) {
	value := strings.TrimSpace(output)
	if value == "" || len(value) > 128 || strings.IndexFunc(value, unicode.IsSpace) >= 0 {
		return "", errors.New("Invalid LVM logical volume UUID")
	}

	for _, char := range value {
		if !unicode.IsLetter(char) && !unicode.IsDigit(char) && char != '-' {
			return "", errors.New("Invalid LVM logical volume UUID")
		}
	}

	return "lvm:" + value, nil
}

// GetVolumeIdentity returns the LVM logical volume UUID.
func (d *lvm) GetVolumeIdentity(vol Volume) (string, error) {
	err := validateMaterializationVolume(vol)
	if err != nil {
		return "", err
	}

	volPath := d.lvmPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, vol.name)
	output, err := subprocess.RunCommand("lvs", "--noheadings", "--readonly", "-o", "lv_uuid", volPath)
	if err != nil {
		return "", fmt.Errorf("Read LVM logical volume UUID: %w", err)
	}

	return parseLVMVolumeIdentity(output)
}

func parseLVMMaterializationTags(output string) ([]string, error) {
	tags := []string{}
	for _, tag := range strings.Split(strings.TrimSpace(output), ",") {
		tag = strings.TrimSpace(tag)
		if !strings.HasPrefix(tag, lvmMaterializationTagPrefix) {
			continue
		}

		digest := strings.TrimPrefix(tag, lvmMaterializationTagPrefix)
		_, err := validateMaterializationOwnership("sha256:" + digest)
		if err != nil {
			return nil, fmt.Errorf("Invalid LVM materialization ownership tag %q", tag)
		}

		tags = append(tags, tag)
	}

	return tags, nil
}

func (d *lvm) getLVMMaterializationTags(vol Volume) ([]string, error) {
	volPath := d.lvmPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, vol.name)
	output, err := subprocess.RunCommand("lvs", "--noheadings", "--readonly", "-o", "lv_tags", volPath)
	if err != nil {
		return nil, fmt.Errorf("Read LVM materialization ownership tags: %w", err)
	}

	return parseLVMMaterializationTags(output)
}

// GetVolumeMaterializationOwnership returns the single ownership tag stored on an LVM volume.
func (d *lvm) GetVolumeMaterializationOwnership(vol Volume) (string, error) {
	err := validateMaterializationVolume(vol)
	if err != nil {
		return "", err
	}

	tags, err := d.getLVMMaterializationTags(vol)
	if err != nil {
		return "", err
	}

	if len(tags) == 0 {
		return "", nil
	}

	if len(tags) != 1 {
		return "", errors.New("LVM volume has conflicting materialization ownership tags")
	}

	return "sha256:" + strings.TrimPrefix(tags[0], lvmMaterializationTagPrefix), nil
}

// SetVolumeMaterializationOwnership persists ownership as an LVM tag.
func (d *lvm) SetVolumeMaterializationOwnership(vol Volume, ownership string) error {
	err := validateMaterializationVolume(vol)
	if err != nil {
		return err
	}

	digest, err := validateMaterializationOwnership(ownership)
	if err != nil {
		return err
	}

	unlock, err := vol.MountLock()
	if err != nil {
		return err
	}

	defer unlock()

	tags, err := d.getLVMMaterializationTags(vol)
	if err != nil {
		return err
	}

	desired := lvmMaterializationTagPrefix + digest
	volPath := d.lvmPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, vol.name)
	found := false
	for _, tag := range tags {
		found = found || tag == desired
	}

	if !found {
		_, err = subprocess.RunCommand("lvchange", "--addtag", desired, volPath)
		if err != nil {
			return fmt.Errorf("Add LVM materialization ownership tag: %w", err)
		}
	}

	for _, tag := range tags {
		if tag == desired {
			continue
		}

		_, err = subprocess.RunCommand("lvchange", "--deltag", tag, volPath)
		if err != nil {
			return fmt.Errorf("Remove stale LVM materialization ownership tag: %w", err)
		}
	}

	persisted, err := d.GetVolumeMaterializationOwnership(vol)
	if err != nil {
		return err
	}

	if persisted != ownership {
		return errors.New("LVM materialization ownership verification failed")
	}

	return nil
}

func parseZFSVolumeIdentity(output string) (string, error) {
	value := strings.TrimSpace(output)
	guid, err := strconv.ParseUint(value, 10, 64)
	if err != nil || guid == 0 || strconv.FormatUint(guid, 10) != value {
		return "", errors.New("Invalid ZFS dataset GUID")
	}

	return "zfs:" + value, nil
}

func validateZFSIdentityBoundDeletion(expectedStorageIdentity string, dependentClones int) error {
	if expectedStorageIdentity != "" && dependentClones > 0 {
		return errors.New("Cannot identity-delete a materialized ZFS volume with dependent clones")
	}

	return nil
}

// GetVolumeIdentity returns the immutable ZFS dataset GUID.
func (d *zfs) GetVolumeIdentity(vol Volume) (string, error) {
	err := validateMaterializationVolume(vol)
	if err != nil {
		return "", err
	}

	output, err := subprocess.RunCommand("zfs", "get", "-H", "-p", "-o", "value", "guid", d.dataset(vol, false))
	if err != nil {
		return "", fmt.Errorf("Read ZFS dataset GUID: %w", err)
	}

	return parseZFSVolumeIdentity(output)
}

func parseZFSMaterializationOwnership(output string) (string, error) {
	output = strings.TrimSpace(output)
	if output == "" {
		return "", nil
	}

	fields := strings.SplitN(output, "\t", 2)
	if len(fields) != 2 {
		return "", errors.New("Invalid ZFS materialization ownership property output")
	}

	if fields[0] == "-" && fields[1] == "-" {
		return "", nil
	}

	if fields[1] != "local" {
		return "", nil
	}

	_, err := validateMaterializationOwnership(fields[0])
	if err != nil {
		return "", err
	}

	return fields[0], nil
}

// GetVolumeMaterializationOwnership returns a locally-set ZFS ownership property.
func (d *zfs) GetVolumeMaterializationOwnership(vol Volume) (string, error) {
	err := validateMaterializationVolume(vol)
	if err != nil {
		return "", err
	}

	output, err := subprocess.RunCommand("zfs", "get", "-H", "-p", "-o", "value,source", zfsMaterializationOwnershipProp, d.dataset(vol, false))
	if err != nil {
		return "", fmt.Errorf("Read ZFS materialization ownership property: %w", err)
	}

	return parseZFSMaterializationOwnership(output)
}

// SetVolumeMaterializationOwnership persists a local ZFS ownership property.
func (d *zfs) SetVolumeMaterializationOwnership(vol Volume, ownership string) error {
	err := validateMaterializationVolume(vol)
	if err != nil {
		return err
	}

	_, err = validateMaterializationOwnership(ownership)
	if err != nil {
		return err
	}

	unlock, err := vol.MountLock()
	if err != nil {
		return err
	}

	defer unlock()

	err = d.setDatasetProperties(d.dataset(vol, false), zfsMaterializationOwnershipProp+"="+ownership)
	if err != nil {
		return fmt.Errorf("Set ZFS materialization ownership property: %w", err)
	}

	persisted, err := d.GetVolumeMaterializationOwnership(vol)
	if err != nil {
		return err
	}

	if persisted != ownership {
		return errors.New("ZFS materialization ownership verification failed")
	}

	return nil
}

func parseBtrfsVolumeIdentity(output string) (string, error) {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "UUID:") {
			continue
		}

		value := strings.TrimSpace(strings.TrimPrefix(line, "UUID:"))
		identity, err := uuid.Parse(value)
		if err != nil {
			return "", errors.New("Invalid Btrfs subvolume UUID")
		}

		return "btrfs:" + identity.String(), nil
	}

	return "", errors.New("Btrfs subvolume has no UUID")
}

// GetVolumeIdentity returns the Btrfs subvolume UUID.
func (d *btrfs) GetVolumeIdentity(vol Volume) (string, error) {
	err := validateMaterializationVolume(vol)
	if err != nil {
		return "", err
	}

	output, err := subprocess.RunCommand("btrfs", "subvolume", "show", vol.MountPath())
	if err != nil {
		return "", fmt.Errorf("Read Btrfs subvolume UUID: %w", err)
	}

	return parseBtrfsVolumeIdentity(output)
}

// GetVolumeMaterializationOwnership returns Btrfs ownership evidence.
func (d *btrfs) GetVolumeMaterializationOwnership(vol Volume) (string, error) {
	err := validateMaterializationVolume(vol)
	if err != nil {
		return "", err
	}

	return getXattrMaterializationOwnership(vol)
}

// SetVolumeMaterializationOwnership persists Btrfs ownership evidence.
func (d *btrfs) SetVolumeMaterializationOwnership(vol Volume, ownership string) error {
	err := validateMaterializationVolume(vol)
	if err != nil {
		return err
	}

	unlock, err := vol.MountLock()
	if err != nil {
		return err
	}

	defer unlock()

	return setXattrMaterializationOwnership(vol, ownership)
}

func deleteMaterializedVolumeWithIdentity(vol Volume, expectedStorageIdentity string, action func() error) error {
	err := validateMaterializationVolume(vol)
	if err != nil {
		return err
	}

	if expectedStorageIdentity == "" {
		return errors.New("Cannot delete materialized volume without an immutable storage identity")
	}

	unlock, err := vol.MountLock()
	if err != nil {
		return err
	}

	defer unlock()

	return action()
}

// DeleteVolumeWithIdentity deletes a directory volume under its lock after identity verification.
func (d *dir) DeleteVolumeWithIdentity(vol Volume, expectedStorageIdentity string, op *operations.Operation) error {
	return deleteMaterializedVolumeWithIdentity(vol, expectedStorageIdentity, func() error {
		return d.deleteVolume(vol, expectedStorageIdentity, op)
	})
}

// DeleteVolumeWithIdentity deletes an LVM volume under its lock after identity verification.
func (d *lvm) DeleteVolumeWithIdentity(vol Volume, expectedStorageIdentity string, op *operations.Operation) error {
	return deleteMaterializedVolumeWithIdentity(vol, expectedStorageIdentity, func() error {
		return d.deleteVolume(vol, expectedStorageIdentity, true, op)
	})
}

// DeleteVolumeWithIdentity deletes a ZFS volume under its lock after identity verification.
func (d *zfs) DeleteVolumeWithIdentity(vol Volume, expectedStorageIdentity string, op *operations.Operation) error {
	return deleteMaterializedVolumeWithIdentity(vol, expectedStorageIdentity, func() error {
		return d.deleteVolume(vol, expectedStorageIdentity, op)
	})
}

// DeleteVolumeWithIdentity deletes a Btrfs volume under its lock after identity verification.
func (d *btrfs) DeleteVolumeWithIdentity(vol Volume, expectedStorageIdentity string, op *operations.Operation) error {
	return deleteMaterializedVolumeWithIdentity(vol, expectedStorageIdentity, func() error {
		return d.deleteVolume(vol, expectedStorageIdentity, op)
	})
}

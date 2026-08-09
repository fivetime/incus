package drivers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/lxc/incus/v7/internal/linux"
	"github.com/lxc/incus/v7/shared/logger"
	"github.com/lxc/incus/v7/shared/subprocess"
	"github.com/lxc/incus/v7/shared/util"
)

const (
	cephIdentityReleaseTombstonePrefix  = "incus_identity_release_"
	cephIdentityReleaseQuarantinePrefix = "incus_identity_quarantine_"
	cephIdentityReleaseMaxTransitions   = 32
)

var errCephIdentityImageNotFound = errors.New("Ceph identity-bound image not found")

type cephRBDPoolIdentity struct {
	PoolID uint64
}

type cephRBDVolumeIdentity struct {
	PoolID          uint64 `json:"pool_id"`
	ID              string `json:"id"`
	BlockNamePrefix string `json:"block_name_prefix"`
}

// isLowercaseHexString reports whether every character is a lowercase hex digit.
// RBD image IDs are hex *strings*, not hex-encoded bytes: Ceph builds them by
// concatenating an instance ID and a random value formatted with std::hex,
// which does not zero-pad, so a legitimate ID is frequently odd in length.
// Decoding one as bytes rejects roughly half of all real images.
func isLowercaseHexString(value string) bool {
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}

	return value != ""
}

func (i cephRBDVolumeIdentity) validate() error {
	if i.ID == "" || i.BlockNamePrefix == "" {
		return errors.New("RBD image information has an incomplete immutable identity")
	}

	if !isLowercaseHexString(i.ID) {
		return fmt.Errorf("Invalid RBD image ID %q: expected canonical lowercase hexadecimal", i.ID)
	}

	if i.BlockNamePrefix != "rbd_data."+i.ID {
		return errors.New("RBD block name prefix does not match the immutable image ID")
	}

	return nil
}

func (i cephRBDVolumeIdentity) canonical() (string, error) {
	err := i.validate()
	if err != nil {
		return "", err
	}

	data, err := json.Marshal(i)
	if err != nil {
		return "", err
	}

	return string(data), nil
}

func parseCanonicalRBDVolumeIdentity(value string) (cephRBDVolumeIdentity, error) {
	identity := cephRBDVolumeIdentity{}
	if value == "" {
		return identity, errors.New("Cannot use an empty RBD storage identity")
	}

	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&identity)
	if err != nil {
		return identity, fmt.Errorf("Invalid RBD storage identity: %w", err)
	}

	var trailing any
	err = decoder.Decode(&trailing)
	if !errors.Is(err, io.EOF) {
		return identity, errors.New("Invalid trailing data in RBD storage identity")
	}

	canonical, err := identity.canonical()
	if err != nil {
		return identity, err
	}

	if canonical != value {
		return identity, errors.New("RBD storage identity is not in canonical form")
	}

	return identity, nil
}

func parseRBDVolumeIdentity(data string, poolID uint64) (string, error) {
	info := struct {
		ID              string `json:"id"`
		BlockNamePrefix string `json:"block_name_prefix"`
	}{}
	err := json.Unmarshal([]byte(data), &info)
	if err != nil {
		return "", fmt.Errorf("Invalid RBD image information: %w", err)
	}

	identity := cephRBDVolumeIdentity{
		PoolID:          poolID,
		ID:              info.ID,
		BlockNamePrefix: info.BlockNamePrefix,
	}

	return identity.canonical()
}

func parseRBDPoolIdentity(data string, expectedPoolName string) (cephRBDPoolIdentity, error) {
	result := struct {
		Pool   string  `json:"pool"`
		PoolID *uint64 `json:"pool_id"`
	}{}
	err := json.Unmarshal([]byte(data), &result)
	if err != nil {
		return cephRBDPoolIdentity{}, fmt.Errorf("Invalid Ceph pool mapping: %w", err)
	}

	if result.Pool != expectedPoolName || result.PoolID == nil {
		return cephRBDPoolIdentity{}, errors.New("Ceph pool mapping does not identify the configured pool")
	}

	return cephRBDPoolIdentity{PoolID: *result.PoolID}, nil
}

// parseRadosDFPoolIdentity reads the pool id out of "rados df" output.
//
// The pool must appear exactly once by name and carry a numeric id.
// Anything else is reported as unusable so the caller falls back to the
// authoritative mapping rather than guessing.
func parseRadosDFPoolIdentity(data string, expectedPoolName string) (cephRBDPoolIdentity, error) {
	result := struct {
		Pools []struct {
			Name string  `json:"name"`
			ID   *uint64 `json:"id"`
		} `json:"pools"`
	}{}
	err := json.Unmarshal([]byte(data), &result)
	if err != nil {
		return cephRBDPoolIdentity{}, fmt.Errorf("Invalid Ceph pool usage report: %w", err)
	}

	found := cephRBDPoolIdentity{}
	matches := 0
	for _, pool := range result.Pools {
		if pool.Name != expectedPoolName {
			continue
		}

		if pool.ID == nil {
			return cephRBDPoolIdentity{}, errors.New("Ceph pool usage report has no pool id")
		}

		found = cephRBDPoolIdentity{PoolID: *pool.ID}
		matches++
	}

	if matches != 1 {
		return cephRBDPoolIdentity{}, errors.New("Ceph pool usage report does not identify the configured pool")
	}

	return found, nil
}

// getRBDPoolIdentity returns the immutable numeric id of the configured pool.
//
// This is on the hot path of every exact identity read, and an instance
// delete performs dozens of them. "rados df" is the C++ client and answers
// roughly five times faster than the Python "ceph" CLI for the same value
// (measured in the daemon container: 94ms against 470ms), so it is tried
// first. "ceph osd map" remains the authority: any transport failure,
// permission gap, or output this cannot read falls back to it rather than
// letting a cheaper source decide identity.
func (d *ceph) getRBDPoolIdentity() (cephRBDPoolIdentity, error) {
	poolName := d.config["ceph.osd.pool_name"]

	out, err := subprocess.RunCommand(
		"rados",
		"--id", d.config["ceph.user.name"],
		"--cluster", d.config["ceph.cluster_name"],
		"df",
		"--format", "json",
	)
	if err == nil {
		identity, parseErr := parseRadosDFPoolIdentity(out, poolName)
		if parseErr == nil {
			return identity, nil
		}
	}

	out, err = subprocess.RunCommand(
		"ceph",
		"--name", fmt.Sprintf("client.%s", d.config["ceph.user.name"]),
		"--cluster", d.config["ceph.cluster_name"],
		"osd", "map", poolName, "rbd_directory",
		"--format", "json",
	)
	if err != nil {
		return cephRBDPoolIdentity{}, err
	}

	return parseRBDPoolIdentity(out, poolName)
}

func (d *ceph) getRBDVolumeIdentity(imageName string) (cephRBDVolumeIdentity, error) {
	poolBefore, err := d.getRBDPoolIdentity()
	if err != nil {
		return cephRBDVolumeIdentity{}, err
	}

	out, err := subprocess.RunCommand(
		"rbd",
		"--id", d.config["ceph.user.name"],
		"--cluster", d.config["ceph.cluster_name"],
		"--pool", d.config["ceph.osd.pool_name"],
		"info", imageName,
		"--format", "json",
	)
	if err != nil {
		if isRBDNotFoundExitError(err) {
			return cephRBDVolumeIdentity{}, errCephIdentityImageNotFound
		}

		return cephRBDVolumeIdentity{}, err
	}

	poolAfter, err := d.getRBDPoolIdentity()
	if err != nil {
		return cephRBDVolumeIdentity{}, err
	}

	if poolBefore != poolAfter {
		return cephRBDVolumeIdentity{}, errors.New("Ceph pool identity changed while reading RBD image identity")
	}

	canonical, err := parseRBDVolumeIdentity(out, poolBefore.PoolID)
	if err != nil {
		return cephRBDVolumeIdentity{}, err
	}

	return parseCanonicalRBDVolumeIdentity(canonical)
}

// HasVolumeIdentity reports whether the exact RBD image remains in a protocol-controlled location.
func (d *ceph) HasVolumeIdentity(vol Volume, expectedStorageIdentity string) (bool, error) {
	expected, err := parseCanonicalRBDVolumeIdentity(expectedStorageIdentity)
	if err != nil {
		return false, err
	}

	poolIdentity, err := d.getRBDPoolIdentity()
	if err != nil {
		return false, err
	}

	if poolIdentity.PoolID != expected.PoolID {
		return false, nil
	}

	store := cephIdentityReleaseAdapter{driver: d}
	for _, name := range []string{
		d.getRBDVolumeName(vol, "", false),
		cephIdentityName(cephIdentityReleaseTombstonePrefix, expected),
		cephIdentityName(cephIdentityReleaseQuarantinePrefix, expected),
	} {
		identity, exists, err := imageIdentityOrMissing(store, name)
		if err != nil {
			return false, err
		}

		if exists && identityMatches(identity, expected) {
			return true, nil
		}
	}

	trash, err := store.TrashEntries()
	if err != nil {
		return false, err
	}

	_, found := findTrashIdentity(trash, expected.ID)
	return found, nil
}

type cephRBDMapping struct {
	DevicePath string
	PoolID     uint64
	ImageID    string
}

// isDetachedRBDSysfsReadError reports whether reading an RBD device attribute
// failed because the device disappeared. A concurrent unmap can remove a
// device between listing /sys/devices/rbd and reading its attributes: the
// directory read then fails with ENOENT, while an attribute read on a
// half-torn-down device fails with ENODEV. Either way the device is not the
// one being looked for and the scan must skip it, not abort.
func isDetachedRBDSysfsReadError(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, unix.ENODEV)
}

func findRBDMappingsByIdentity(sysfsRoot string, expected cephRBDVolumeIdentity) ([]cephRBDMapping, error) {
	entries, err := os.ReadDir(sysfsRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	mappings := []cephRBDMapping{}
	for _, entry := range entries {
		index, err := strconv.ParseUint(entry.Name(), 10, 64)
		if err != nil {
			continue
		}

		deviceDir := filepath.Join(sysfsRoot, entry.Name())
		poolIDValue, err := os.ReadFile(filepath.Join(deviceDir, "pool_id"))
		if isDetachedRBDSysfsReadError(err) {
			continue
		}

		if err != nil {
			return nil, err
		}

		poolID, err := strconv.ParseUint(strings.TrimSpace(string(poolIDValue)), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("Invalid RBD mapping pool ID for device %q: %w", entry.Name(), err)
		}

		imageIDValue, err := os.ReadFile(filepath.Join(deviceDir, "image_id"))
		if isDetachedRBDSysfsReadError(err) {
			continue
		}

		if err != nil {
			return nil, err
		}

		imageID := strings.TrimSpace(string(imageIDValue))
		if poolID != expected.PoolID || imageID != expected.ID {
			continue
		}

		mappings = append(mappings, cephRBDMapping{
			DevicePath: fmt.Sprintf("/dev/rbd%d", index),
			PoolID:     poolID,
			ImageID:    imageID,
		})
	}

	return mappings, nil
}

func (d *ceph) releaseCephVolumeLocalState(vol Volume, expectedStorageIdentity string) error {
	identity, err := parseCanonicalRBDVolumeIdentity(expectedStorageIdentity)
	if err != nil {
		return err
	}

	unlock, err := vol.MountLock()
	if err != nil {
		return err
	}

	defer unlock()

	return d.releaseCephVolumeLocalStateLocked(vol, identity, false)
}

// releaseCephVolumeLocalStateLocked releases host-local state for an identity-verified volume.
// With unmount=false any mount reference or mountpoint aborts the release (conservative path for
// callers that have not proven exclusive teardown rights). With unmount=true the caller has proven
// the claim is failed or abandoned (no database record, or an authorized identity-bound delete), so
// leftover mounts and in-memory mount references are the stale state being released: a legitimate
// user always holds the volume's database record, and a failed migration receive can leak both the
// mountpoint and its reference count, which would otherwise deadlock fencing forever.
func (d *ceph) releaseCephVolumeLocalStateLocked(vol Volume, identity cephRBDVolumeIdentity, unmount bool) error {
	if vol.MountInUse() {
		if !unmount {
			return fmt.Errorf("Cannot release Ceph volume local state while references remain: %w", ErrInUse)
		}

		// The caller holds the mount lock and has proven the claim failed or
		// abandoned, so remaining references were leaked (e.g. by a failed
		// migration receive). Leaving them would poison the next claim of the
		// same volume name: its own release would see phantom references and
		// refuse, failing that later migration on the source side.
		stale := vol.MountRefCountReset()
		d.logger.Warn("Cleared stale mount references during forced volume local state release", logger.Ctx{"volume": vol.Name(), "references": stale})
	}

	if vol.contentType == ContentTypeFS && linux.IsMountPoint(vol.MountPath()) {
		if !unmount {
			return fmt.Errorf("Cannot release Ceph volume local state while it remains mounted: %w", ErrInUse)
		}

		_, err := forceUnmount(vol.MountPath())
		if err != nil {
			return err
		}
	}

	for pass := 0; pass < cephIdentityReleaseMaxTransitions; pass++ {
		mappings, err := findRBDMappingsByIdentity("/sys/devices/rbd", identity)
		if err != nil {
			return fmt.Errorf("Inspect exact RBD mappings: %w", err)
		}

		if len(mappings) == 0 {
			return nil
		}

		for _, mapping := range mappings {
			err = d.unmapRBDDevice(mapping.DevicePath)
			if err != nil {
				return fmt.Errorf("Release exact RBD mapping %q: %w", mapping.DevicePath, err)
			}
		}
	}

	return errors.New("Exact RBD mappings remain after repeated release attempts")
}

func (d *ceph) unmapRBDDevice(devicePath string) error {
	for attempt := 0; attempt < 10; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), cephRBDUnmapTimeout)
		_, err := subprocess.RunCommandContext(
			ctx,
			"rbd",
			"--id", d.config["ceph.user.name"],
			"--cluster", d.config["ceph.cluster_name"],
			"device", "unmap", devicePath,
		)
		timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
		cancel()

		if timedOut {
			d.logger.Warn("RBD device unmap timed out, forcing detached device removal", logger.Ctx{"dev": devicePath})
			forceCtx, forceCancel := context.WithTimeout(context.Background(), cephRBDUnmapTimeout)
			_, forceErr := subprocess.RunCommandContext(
				forceCtx,
				"rbd",
				"--id", d.config["ceph.user.name"],
				"--cluster", d.config["ceph.cluster_name"],
				"device", "unmap", "--options", "force", devicePath,
			)
			forceCancel()
			return forceErr
		}

		if err == nil || commandExitCodeIs(err, 22) {
			return nil
		}

		if !commandExitCodeIs(err, 16) {
			return err
		}

		time.Sleep(time.Second)
	}

	return fmt.Errorf("RBD device %q remained busy", devicePath)
}

func commandExitCodeIs(err error, expected int) bool {
	var runError subprocess.RunError
	if !errors.As(err, &runError) {
		return false
	}

	var exitError *exec.ExitError
	return errors.As(runError.Unwrap(), &exitError) && exitError.ExitCode() == expected
}

type cephRBDTrashEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func parseRBDTrashEntries(data string) ([]cephRBDTrashEntry, error) {
	entries := []cephRBDTrashEntry{}
	err := json.Unmarshal([]byte(data), &entries)
	if err != nil {
		return nil, fmt.Errorf("Invalid RBD trash listing: %w", err)
	}

	seenIDs := map[string]struct{}{}
	for _, entry := range entries {
		if entry.ID == "" || entry.Name == "" {
			return nil, errors.New("RBD trash listing contains an incomplete entry")
		}

		if !isLowercaseHexString(entry.ID) {
			return nil, errors.New("RBD trash listing contains a non-canonical image ID")
		}

		_, found := seenIDs[entry.ID]
		if found {
			return nil, errors.New("RBD trash listing contains a duplicate image ID")
		}

		seenIDs[entry.ID] = struct{}{}
	}

	return entries, nil
}

type cephIdentityReleaseStore interface {
	PoolIdentity() (uint64, error)
	ImageIdentity(name string) (cephRBDVolumeIdentity, error)
	RenameImage(oldName string, newName string) error
	TrashEntries() ([]cephRBDTrashEntry, error)
	TrashMove(name string) error
	TrashRestore(imageID string, name string) error
	TrashRemove(imageID string) error
	SnapshotNames(name string) ([]string, error)
	SnapshotChildren(name string, snapshotName string) ([]string, error)
	SnapshotUnprotect(name string, snapshotName string) error
	SnapshotPurge(name string) error
}

type cephIdentityReleaseAdapter struct {
	driver *ceph
}

// PoolIdentity returns the pool's own id, which scopes every image id the release
// algorithm compares.
func (a cephIdentityReleaseAdapter) PoolIdentity() (uint64, error) {
	identity, err := a.driver.getRBDPoolIdentity()
	return identity.PoolID, err
}

// ImageIdentity resolves a name to the identity currently behind it, so the caller can tell
// its own image from another one that reused the name.
func (a cephIdentityReleaseAdapter) ImageIdentity(name string) (cephRBDVolumeIdentity, error) {
	return a.driver.getRBDVolumeIdentity(name)
}

// RenameImage moves an image between names without touching its data or its id.
func (a cephIdentityReleaseAdapter) RenameImage(oldName string, newName string) error {
	_, err := subprocess.RunCommand("rbd", "--id", a.driver.config["ceph.user.name"], "--cluster", a.driver.config["ceph.cluster_name"], "mv", fmt.Sprintf("%s/%s", a.driver.config["ceph.osd.pool_name"], oldName), fmt.Sprintf("%s/%s", a.driver.config["ceph.osd.pool_name"], newName))
	if isRBDNotFoundExitError(err) {
		return errCephIdentityImageNotFound
	}

	return err
}

// TrashEntries lists the pool's trash so a caller can recognise an image another daemon
// already moved out of the way.
func (a cephIdentityReleaseAdapter) TrashEntries() ([]cephRBDTrashEntry, error) {
	out, err := subprocess.RunCommand("rbd", "--id", a.driver.config["ceph.user.name"], "--cluster", a.driver.config["ceph.cluster_name"], "--pool", a.driver.config["ceph.osd.pool_name"], "trash", "ls", "--format", "json")
	if err != nil {
		return nil, err
	}

	return parseRBDTrashEntries(out)
}

// TrashMove sends a named image to the trash, where it keeps its id.
func (a cephIdentityReleaseAdapter) TrashMove(name string) error {
	_, err := subprocess.RunCommand("rbd", "--id", a.driver.config["ceph.user.name"], "--cluster", a.driver.config["ceph.cluster_name"], "--pool", a.driver.config["ceph.osd.pool_name"], "trash", "mv", name)
	if isRBDNotFoundExitError(err) {
		return errCephIdentityImageNotFound
	}

	return err
}

// TrashRestore brings an image back from the trash under the given name, addressing it by id
// because its former name may already be taken.
func (a cephIdentityReleaseAdapter) TrashRestore(imageID string, name string) error {
	_, err := subprocess.RunCommand("rbd", "--id", a.driver.config["ceph.user.name"], "--cluster", a.driver.config["ceph.cluster_name"], "--pool", a.driver.config["ceph.osd.pool_name"], "trash", "restore", imageID, "--image", name)
	if isRBDNotFoundExitError(err) {
		return errCephIdentityImageNotFound
	}

	return err
}

// TrashRemove permanently deletes a trashed image by id.
func (a cephIdentityReleaseAdapter) TrashRemove(imageID string) error {
	_, err := subprocess.RunCommand("rbd", "--id", a.driver.config["ceph.user.name"], "--cluster", a.driver.config["ceph.cluster_name"], "--pool", a.driver.config["ceph.osd.pool_name"], "trash", "rm", imageID)
	if isRBDNotFoundExitError(err) {
		return errCephIdentityImageNotFound
	}

	return err
}

// SnapshotNames lists an image's snapshots, which must all be gone before it can be removed.
func (a cephIdentityReleaseAdapter) SnapshotNames(name string) ([]string, error) {
	out, err := subprocess.RunCommand("rbd", "--id", a.driver.config["ceph.user.name"], "--cluster", a.driver.config["ceph.cluster_name"], "--pool", a.driver.config["ceph.osd.pool_name"], "snap", "ls", name, "--format", "json")
	if isRBDNotFoundExitError(err) {
		return nil, errCephIdentityImageNotFound
	}

	if err != nil {
		return nil, err
	}

	entries := []struct {
		Name string `json:"name"`
	}{}
	err = json.Unmarshal([]byte(out), &entries)
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Name == "" {
			return nil, errors.New("RBD snapshot listing contains an unnamed snapshot")
		}

		names = append(names, entry.Name)
	}

	return names, nil
}

// SnapshotChildren lists the clones of a snapshot; a snapshot with children cannot be purged.
func (a cephIdentityReleaseAdapter) SnapshotChildren(name string, snapshotName string) ([]string, error) {
	out, err := subprocess.RunCommand("rbd", "--id", a.driver.config["ceph.user.name"], "--cluster", a.driver.config["ceph.cluster_name"], "--pool", a.driver.config["ceph.osd.pool_name"], "children", "--image", name, "--snap", snapshotName)
	if isRBDNotFoundExitError(err) {
		return nil, errCephIdentityImageNotFound
	}

	if err != nil {
		return nil, err
	}

	return strings.Fields(strings.TrimSpace(out)), nil
}

// SnapshotUnprotect clears the protection that blocks removing a snapshot.
func (a cephIdentityReleaseAdapter) SnapshotUnprotect(name string, snapshotName string) error {
	_, err := subprocess.RunCommand("rbd", "--id", a.driver.config["ceph.user.name"], "--cluster", a.driver.config["ceph.cluster_name"], "--pool", a.driver.config["ceph.osd.pool_name"], "snap", "unprotect", fmt.Sprintf("%s@%s", name, snapshotName))
	if commandExitCodeIs(err, 22) {
		return nil
	}

	if isRBDNotFoundExitError(err) {
		return errCephIdentityImageNotFound
	}

	return err
}

// SnapshotPurge removes every snapshot of an image.
func (a cephIdentityReleaseAdapter) SnapshotPurge(name string) error {
	_, err := subprocess.RunCommand("rbd", "--id", a.driver.config["ceph.user.name"], "--cluster", a.driver.config["ceph.cluster_name"], "--pool", a.driver.config["ceph.osd.pool_name"], "snap", "purge", name)
	if isRBDNotFoundExitError(err) {
		return errCephIdentityImageNotFound
	}

	return err
}

func cephIdentityName(prefix string, identity cephRBDVolumeIdentity) string {
	canonical, _ := identity.canonical()
	digest := sha256.Sum256([]byte(canonical))
	return prefix + hex.EncodeToString(digest[:])
}

func identityMatches(actual cephRBDVolumeIdentity, expected cephRBDVolumeIdentity) bool {
	return actual == expected
}

func verifyRBDIdentityReleasePool(store cephIdentityReleaseStore, expectedPoolID uint64) error {
	poolID, err := store.PoolIdentity()
	if err != nil {
		return err
	}

	if poolID != expectedPoolID {
		return errors.New("Ceph pool identity changed during exact RBD release")
	}

	return nil
}

func imageIdentityOrMissing(store cephIdentityReleaseStore, name string) (cephRBDVolumeIdentity, bool, error) {
	identity, err := store.ImageIdentity(name)
	if errors.Is(err, errCephIdentityImageNotFound) {
		return cephRBDVolumeIdentity{}, false, nil
	}

	return identity, err == nil, err
}

func findTrashIdentity(entries []cephRBDTrashEntry, imageID string) (cephRBDTrashEntry, bool) {
	for _, entry := range entries {
		if entry.ID == imageID {
			return entry, true
		}
	}

	return cephRBDTrashEntry{}, false
}

func quarantineNamedIdentity(store cephIdentityReleaseStore, sourceName string, actual cephRBDVolumeIdentity) error {
	quarantineName := cephIdentityName(cephIdentityReleaseQuarantinePrefix, actual)
	_, exists, err := imageIdentityOrMissing(store, quarantineName)
	if err != nil {
		return err
	}

	if exists {
		return fmt.Errorf("Cannot quarantine unexpected RBD image at reserved name %q because its quarantine name is occupied", sourceName)
	}

	err = store.RenameImage(sourceName, quarantineName)
	if err != nil {
		return err
	}

	quarantined, exists, err := imageIdentityOrMissing(store, quarantineName)
	if err != nil {
		return err
	}

	if !exists || !identityMatches(quarantined, actual) {
		return errors.New("Unexpected RBD image was not preserved under its quarantine identity")
	}

	return fmt.Errorf("Quarantined unexpected RBD image %q as %q after an identity race", sourceName, quarantineName)
}

func restoreTrashIdentityToQuarantine(store cephIdentityReleaseStore, entry cephRBDTrashEntry, expectedPoolID uint64) error {
	err := verifyRBDIdentityReleasePool(store, expectedPoolID)
	if err != nil {
		return err
	}

	seed := cephRBDVolumeIdentity{PoolID: expectedPoolID, ID: entry.ID, BlockNamePrefix: "rbd_data." + entry.ID}
	quarantineName := cephIdentityName(cephIdentityReleaseQuarantinePrefix, seed)
	_, exists, err := imageIdentityOrMissing(store, quarantineName)
	if err != nil {
		return err
	}

	if exists {
		return errors.New("Cannot restore unexpected trashed RBD image because its quarantine name is occupied")
	}

	err = store.TrashRestore(entry.ID, quarantineName)
	if err != nil {
		return err
	}

	actual, exists, err := imageIdentityOrMissing(store, quarantineName)
	if err != nil {
		return err
	}

	if !exists || actual.PoolID != expectedPoolID || actual.ID != entry.ID {
		return errors.New("Unexpected trashed RBD image was not restored under its quarantine identity")
	}

	return fmt.Errorf("Quarantined unexpected trashed RBD image %q as %q after an identity race", entry.ID, quarantineName)
}

func prepareRBDIdentityTombstone(store cephIdentityReleaseStore, originalName string, expected cephRBDVolumeIdentity) (string, bool, error) {
	tombstoneName := cephIdentityName(cephIdentityReleaseTombstonePrefix, expected)
	quarantineName := cephIdentityName(cephIdentityReleaseQuarantinePrefix, expected)

	for transition := 0; transition < cephIdentityReleaseMaxTransitions; transition++ {
		err := verifyRBDIdentityReleasePool(store, expected.PoolID)
		if err != nil {
			return "", false, err
		}

		quarantineIdentity, quarantineExists, err := imageIdentityOrMissing(store, quarantineName)
		if err != nil {
			return "", false, err
		}

		if quarantineExists && identityMatches(quarantineIdentity, expected) {
			return "", false, errors.New("Expected RBD image exists in quarantine and cannot be deleted automatically")
		}

		tombstoneIdentity, tombstoneExists, err := imageIdentityOrMissing(store, tombstoneName)
		if err != nil {
			return "", false, err
		}

		if tombstoneExists {
			if !identityMatches(tombstoneIdentity, expected) {
				return "", false, quarantineNamedIdentity(store, tombstoneName, tombstoneIdentity)
			}

			return tombstoneName, false, nil
		}

		trashEntries, err := store.TrashEntries()
		if err != nil {
			return "", false, err
		}

		_, found := findTrashIdentity(trashEntries, expected.ID)
		if found {
			err = store.TrashRestore(expected.ID, tombstoneName)
			if err != nil && !errors.Is(err, errCephIdentityImageNotFound) {
				return "", false, err
			}

			continue
		}

		originalIdentity, originalExists, err := imageIdentityOrMissing(store, originalName)
		if err != nil {
			return "", false, err
		}

		if !originalExists || !identityMatches(originalIdentity, expected) {
			// Recheck reserved states after observing the original name, as another daemon may be moving A.
			trashEntries, err = store.TrashEntries()
			if err != nil {
				return "", false, err
			}

			_, found := findTrashIdentity(trashEntries, expected.ID)
			if found {
				continue
			}

			// Only existence matters here: a tombstone that appeared under us
			// sends the loop around again, and the top of the loop is what
			// checks whether the identity is actually ours.
			_, tombstoneExists, err = imageIdentityOrMissing(store, tombstoneName)
			if err != nil {
				return "", false, err
			}

			if tombstoneExists {
				continue
			}

			return "", originalExists, nil
		}

		err = store.RenameImage(originalName, tombstoneName)
		if err != nil && !errors.Is(err, errCephIdentityImageNotFound) {
			return "", false, err
		}
	}

	return "", false, errors.New("RBD identity release did not converge while reserving its tombstone")
}

func inspectRBDIdentityReleasePostcondition(store cephIdentityReleaseStore, originalName string, expected cephRBDVolumeIdentity) (bool, bool, error) {
	err := verifyRBDIdentityReleasePool(store, expected.PoolID)
	if err != nil {
		return false, false, err
	}

	quarantineName := cephIdentityName(cephIdentityReleaseQuarantinePrefix, expected)
	quarantineIdentity, quarantineExists, err := imageIdentityOrMissing(store, quarantineName)
	if err != nil {
		return false, false, err
	}

	if quarantineExists && identityMatches(quarantineIdentity, expected) {
		return false, false, errors.New("Expected RBD image remains in quarantine")
	}

	tombstoneName := cephIdentityName(cephIdentityReleaseTombstonePrefix, expected)
	tombstoneIdentity, tombstoneExists, err := imageIdentityOrMissing(store, tombstoneName)
	if err != nil {
		return false, false, err
	}

	if tombstoneExists {
		if identityMatches(tombstoneIdentity, expected) {
			return true, false, nil
		}

		return false, false, quarantineNamedIdentity(store, tombstoneName, tombstoneIdentity)
	}

	trash, err := store.TrashEntries()
	if err != nil {
		return false, false, err
	}

	_, found := findTrashIdentity(trash, expected.ID)
	if found {
		return true, false, nil
	}

	originalIdentity, originalExists, err := imageIdentityOrMissing(store, originalName)
	if err != nil {
		return false, false, err
	}

	if !originalExists {
		return false, false, nil
	}

	if identityMatches(originalIdentity, expected) {
		return true, false, nil
	}

	return false, true, nil
}

func purgeRBDIdentitySnapshots(store cephIdentityReleaseStore, tombstoneName string, expected cephRBDVolumeIdentity) error {
	err := verifyRBDIdentityReleasePool(store, expected.PoolID)
	if err != nil {
		return err
	}

	snapshots, err := store.SnapshotNames(tombstoneName)
	if err != nil {
		return err
	}

	for _, snapshotName := range snapshots {
		children, err := store.SnapshotChildren(tombstoneName, snapshotName)
		if err != nil {
			return err
		}

		if len(children) > 0 {
			return fmt.Errorf("Cannot release exact RBD image while snapshot %q has dependent clones", snapshotName)
		}
	}

	for _, snapshotName := range snapshots {
		identity, exists, err := imageIdentityOrMissing(store, tombstoneName)
		if err != nil {
			return err
		}

		if !exists || !identityMatches(identity, expected) {
			return errors.New("RBD tombstone identity changed before snapshot unprotect")
		}

		err = store.SnapshotUnprotect(tombstoneName, snapshotName)
		if err != nil {
			return err
		}

		identity, exists, err = imageIdentityOrMissing(store, tombstoneName)
		if err != nil {
			return err
		}

		if !exists || !identityMatches(identity, expected) {
			return errors.New("RBD tombstone identity changed after snapshot unprotect")
		}
	}

	if len(snapshots) == 0 {
		return nil
	}

	identity, exists, err := imageIdentityOrMissing(store, tombstoneName)
	if err != nil {
		return err
	}

	if !exists || !identityMatches(identity, expected) {
		return errors.New("RBD tombstone identity changed before snapshot purge")
	}

	err = store.SnapshotPurge(tombstoneName)
	if err != nil {
		return err
	}

	identity, exists, err = imageIdentityOrMissing(store, tombstoneName)
	if err != nil {
		return err
	}

	if !exists || !identityMatches(identity, expected) {
		return errors.New("RBD tombstone identity changed after snapshot purge")
	}

	return nil
}

func trashAndRemoveRBDIdentity(store cephIdentityReleaseStore, tombstoneName string, expected cephRBDVolumeIdentity) error {
	err := verifyRBDIdentityReleasePool(store, expected.PoolID)
	if err != nil {
		return err
	}

	before, err := store.TrashEntries()
	if err != nil {
		return err
	}

	beforeIDs := map[string]struct{}{}
	for _, entry := range before {
		beforeIDs[entry.ID] = struct{}{}
	}

	identity, exists, err := imageIdentityOrMissing(store, tombstoneName)
	if err != nil {
		return err
	}

	if !exists || !identityMatches(identity, expected) {
		return errors.New("RBD tombstone identity changed before trash transition")
	}

	err = store.TrashMove(tombstoneName)
	if err != nil && !errors.Is(err, errCephIdentityImageNotFound) {
		return err
	}

	err = verifyRBDIdentityReleasePool(store, expected.PoolID)
	if err != nil {
		return err
	}

	after, err := store.TrashEntries()
	if err != nil {
		return err
	}

	_, found := findTrashIdentity(after, expected.ID)
	if !found {
		for _, entry := range after {
			_, existed := beforeIDs[entry.ID]
			if !existed && entry.Name == tombstoneName {
				return restoreTrashIdentityToQuarantine(store, entry, expected.PoolID)
			}
		}

		// A concurrent exact-ID releaser can complete trash removal before this caller observes it.
		return nil
	}

	err = verifyRBDIdentityReleasePool(store, expected.PoolID)
	if err != nil {
		return err
	}

	err = store.TrashRemove(expected.ID)
	if err != nil && !errors.Is(err, errCephIdentityImageNotFound) {
		return err
	}

	err = verifyRBDIdentityReleasePool(store, expected.PoolID)
	if err != nil {
		return err
	}

	after, err = store.TrashEntries()
	if err != nil {
		return err
	}

	_, found = findTrashIdentity(after, expected.ID)
	if found {
		return errors.New("Exact RBD image remains in trash after identity-bound removal")
	}

	return nil
}

func deleteRBDVolumeWithIdentity(store cephIdentityReleaseStore, originalName string, expected cephRBDVolumeIdentity) (bool, error) {
	for transition := 0; transition < cephIdentityReleaseMaxTransitions; transition++ {
		tombstoneName, replacementExists, err := prepareRBDIdentityTombstone(store, originalName, expected)
		if err != nil {
			return false, err
		}

		if tombstoneName == "" {
			expectedExists, currentReplacementExists, err := inspectRBDIdentityReleasePostcondition(store, originalName, expected)
			if err != nil {
				return false, err
			}

			if expectedExists {
				continue
			}

			return replacementExists || currentReplacementExists, nil
		}

		err = purgeRBDIdentitySnapshots(store, tombstoneName, expected)
		if errors.Is(err, errCephIdentityImageNotFound) {
			continue
		}

		if err != nil {
			return false, err
		}

		err = trashAndRemoveRBDIdentity(store, tombstoneName, expected)
		if errors.Is(err, errCephIdentityImageNotFound) {
			continue
		}

		if err != nil {
			return false, err
		}

		expectedExists, replacementExists, err := inspectRBDIdentityReleasePostcondition(store, originalName, expected)
		if err != nil {
			return false, err
		}

		if expectedExists {
			continue
		}

		return replacementExists, nil
	}

	return false, errors.New("RBD identity release did not converge")
}

func (d *ceph) deleteVolumeWithExactIdentity(vol Volume, expectedStorageIdentity string) error {
	expected, err := parseCanonicalRBDVolumeIdentity(expectedStorageIdentity)
	if err != nil {
		return err
	}

	if vol.IsVMBlock() {
		return errors.New("Cannot identity-delete a VM block volume without an identity-bound companion filesystem volume")
	}

	unlock, err := vol.MountLock()
	if err != nil {
		return err
	}

	defer unlock()

	err = d.releaseCephVolumeLocalStateLocked(vol, expected, true)
	if err != nil {
		return err
	}

	replacementExists, err := deleteRBDVolumeWithIdentity(cephIdentityReleaseAdapter{driver: d}, d.getRBDVolumeName(vol, "", false), expected)
	if err != nil {
		return err
	}

	mappings, err := findRBDMappingsByIdentity("/sys/devices/rbd", expected)
	if err != nil {
		return err
	}

	if len(mappings) > 0 {
		return errors.New("Exact RBD mappings remain after storage deletion")
	}

	if replacementExists || vol.contentType != ContentTypeFS || !util.PathExists(vol.MountPath()) {
		return nil
	}

	if vol.MountInUse() || linux.IsMountPoint(vol.MountPath()) {
		return fmt.Errorf("Cannot wipe Ceph volume mount path while it is active: %w", ErrInUse)
	}

	err = wipeDirectory(vol.MountPath())
	if err != nil {
		return err
	}

	err = os.Remove(vol.MountPath())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("Failed to remove %q: %w", vol.MountPath(), err)
	}

	return nil
}

//go:build linux && cgo && !agent

package drivers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lxc/incus/v7/internal/linux"
	"github.com/lxc/incus/v7/internal/server/db"
	"github.com/lxc/incus/v7/internal/server/db/cluster"
	"github.com/lxc/incus/v7/internal/server/instance/operationlock"
	"github.com/lxc/incus/v7/internal/server/project"
	storagePools "github.com/lxc/incus/v7/internal/server/storage"
	storageDrivers "github.com/lxc/incus/v7/internal/server/storage/drivers"
	"github.com/lxc/incus/v7/internal/server/storage/rescue"
	internalUtil "github.com/lxc/incus/v7/internal/util"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/osarch"
	"github.com/lxc/incus/v7/shared/util"
)

const (
	rescueTokenKey     = "volatile.rescue.token"
	rescueImageKey     = "volatile.rescue.image"
	rescuePhaseKey     = "volatile.rescue.phase"
	rescueCompletedKey = "volatile.rescue.completed"
)

// Rescue prepares a temporary root within the already owned root volume.
func (d *lxc) Rescue(token string, fingerprint string) error {
	if !rescue.ValidRequest(token, fingerprint) {
		return errors.New("Rescue requires an exact token and image fingerprint")
	}

	if d.IsRunning() || d.IsSnapshot() || d.IsPrivileged() || d.stateful {
		return errors.New("Rescue requires a stopped, unprivileged, stateless container")
	}

	if d.localConfig[migrationCheckpointKey] != "" {
		return errors.New("Retained migration checkpoint must be resolved before rescue")
	}

	for _, device := range d.expandedDevices {
		path := device["path"]
		if device["type"] == "disk" && path != "/" && (path == "/mnt" || path == "/mnt/root" || strings.HasPrefix(path, "/mnt/root/")) {
			return errors.New("A disk device overlaps the rescue mountpoint")
		}
	}

	op, err := operationlock.CreateWaitGet(d.Project().Name, d.Name(), d.op, operationlock.ActionRestore, nil, false, false)
	if err != nil {
		return err
	}

	defer op.Done(nil)
	d.stopForkfile(false)
	current := d.localConfig[rescueTokenKey]
	if current != "" && (current != token || d.localConfig[rescueImageKey] != fingerprint) {
		return errors.New("Another rescue generation owns this instance")
	}

	if current == "" && d.localConfig[rescueCompletedKey] == token {
		return errors.New("This rescue generation has already been restored")
	}

	imageProject := project.ImageProjectFromRecord(&d.project)
	var image *api.Image
	err = d.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		var err error
		_, image, err = tx.GetImage(ctx, fingerprint, cluster.ImageFilter{Project: &imageProject})
		return err
	})
	if err != nil {
		return err
	}

	architecture, err := osarch.ArchitectureName(d.architecture)
	if err != nil {
		return err
	}

	if image.Fingerprint != fingerprint || image.Type != "container" || image.Architecture != architecture || util.IsTrue(image.Properties["requirements.privileged"]) {
		return errors.New("Rescue image is incompatible with this unprivileged container")
	}

	storageType, err := d.getStorageType()
	if err != nil {
		return err
	}

	if storageType != "ceph" && storageType != "cephext" && storageType != "dir" {
		return errors.New("Rescue requires a Ceph or directory root storage pool")
	}

	_, err = d.mount()
	if err != nil {
		return err
	}

	defer func() { _ = d.unmount() }()
	diskMap, err := d.DiskIdmap()
	if err != nil {
		return err
	}

	nextMap, err := d.NextIdmap()
	if err != nil {
		return err
	}

	if diskMap == nil || !diskMap.Equals(nextMap) {
		return errors.New("Rescue requires a fixed on-disk isolated ID map")
	}

	if current == "" {
		_, _, err = d.handleIdmappedStorage()
		if err != nil {
			return fmt.Errorf("Validate original root ID map before rescue: %w", err)
		}

		err = d.VolatileSet(map[string]string{
			rescueTokenKey: token, rescueImageKey: fingerprint,
			rescuePhaseKey: "preparing", rescueCompletedKey: "",
		})
		if err != nil {
			return err
		}
	}

	if strings.HasPrefix(d.localConfig[rescuePhaseKey], "restoring") {
		return errors.New("This rescue generation is being restored")
	}

	maxMemory, err := linux.DeviceTotalMemory()
	if err != nil {
		maxMemory = 0
	} else {
		maxMemory /= 10
	}

	imageFile := internalUtil.VarPath("images", fingerprint)
	err = rescue.Prepare(d.Path(), token, fingerprint, func(path string) error {
		err := storagePools.ImageUnpackContainer(imageFile, path, storageType != "dir", maxMemory, nil)
		if err != nil {
			return err
		}

		return storageDrivers.RemapRootfsIDMap(filepath.Join(path, "rootfs"), storageType, nil, diskMap)
	})
	if err != nil {
		return fmt.Errorf("Prepare rescue root: %w", err)
	}

	return d.VolatileSet(map[string]string{rescuePhaseKey: "active"})
}

// Unrescue restores the original root without changing storage attachment ownership.
func (d *lxc) Unrescue(token string) error {
	if d.IsRunning() || d.IsSnapshot() {
		return errors.New("Unrescue requires a stopped container")
	}

	op, err := operationlock.CreateWaitGet(d.Project().Name, d.Name(), d.op, operationlock.ActionRestore, nil, false, false)
	if err != nil {
		return err
	}

	defer op.Done(nil)
	return d.restoreRescue(token)
}

func (d *lxc) restoreRescue(token string) error {
	current := d.localConfig[rescueTokenKey]
	if current == "" && token != "" && d.localConfig[rescueCompletedKey] == token {
		return nil
	}

	if current == "" || current != token {
		return errors.New("Unrescue token does not own this instance")
	}

	_, err := d.mount()
	if err != nil {
		return err
	}

	defer func() { _ = d.unmount() }()
	phase := d.localConfig[rescuePhaseKey]
	allowMissing := phase == "preparing" || phase == "restoring-prepare"
	restorePhase := "restoring"
	if allowMissing {
		restorePhase = "restoring-prepare"
	}

	d.stopForkfile(false)
	err = d.VolatileSet(map[string]string{rescuePhaseKey: restorePhase})
	if err != nil {
		return err
	}

	err = rescue.Restore(d.Path(), token, allowMissing)
	if err != nil {
		return err
	}

	return d.VolatileSet(map[string]string{
		rescueTokenKey: "", rescueImageKey: "", rescuePhaseKey: "",
		rescueCompletedKey: token,
	})
}

func (d *lxc) validateRescueRoot() error {
	token := d.localConfig[rescueTokenKey]
	if token == "" {
		return nil
	}

	if d.localConfig[rescuePhaseKey] != "active" {
		return errors.New("An interrupted rescue transaction must be resolved before starting")
	}

	err := rescue.ValidateActive(d.Path(), token)
	if err != nil {
		return err
	}

	diskMap, err := d.DiskIdmap()
	if err != nil {
		return err
	}

	nextMap, err := d.NextIdmap()
	if err != nil {
		return err
	}

	if diskMap == nil || !diskMap.Equals(nextMap) {
		return errors.New("Rescue cannot change the original root ID map")
	}

	return nil
}

func (d *lxc) rescueImagePath() string {
	if d.localConfig[rescueTokenKey] != "" {
		return rescue.ImagePath(d.Path())
	}

	return d.Path()
}

// TemplatesPath selects the temporary image's templates while in rescue mode.
func (d *lxc) TemplatesPath() string {
	return filepath.Join(d.rescueImagePath(), "templates")
}

func (d *lxc) ensureRescueMountpoint() error {
	root, err := os.OpenRoot(d.RootfsPath())
	if err != nil {
		return err
	}

	defer func() { _ = root.Close() }()
	err = root.MkdirAll("mnt/root", 0o755)
	if err != nil {
		return err
	}

	info, err := root.Lstat("mnt/root")
	if err != nil || !info.IsDir() {
		return errors.New("Rescue mountpoint is not a directory")
	}

	return nil
}

// RootfsPath selects a committed temporary rescue root without moving the original.
func (d *lxc) RootfsPath() string {
	if d.localConfig[rescueTokenKey] != "" && d.localConfig[rescuePhaseKey] == "active" {
		return filepath.Join(rescue.ImagePath(d.Path()), "rootfs")
	}

	return d.common.RootfsPath()
}

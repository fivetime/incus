//go:build linux && cgo && !agent

package drivers

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	liblxc "github.com/lxc/go-lxc"

	internalInstance "github.com/lxc/incus/v7/internal/instance"
	"github.com/lxc/incus/v7/internal/server/instance"
	"github.com/lxc/incus/v7/internal/server/instance/operationlock"
	storagePools "github.com/lxc/incus/v7/internal/server/storage"
	storageDrivers "github.com/lxc/incus/v7/internal/server/storage/drivers"
	internalUtil "github.com/lxc/incus/v7/internal/util"
	"github.com/lxc/incus/v7/shared/validate"
)

const (
	migrationCheckpointKey      = "volatile.migration.checkpoint"
	migrationCheckpointStateKey = "volatile.migration.checkpoint.state"
)

func migrationCheckpointPath(instanceUUID string, operationUUID string) (string, error) {
	for _, value := range []string{instanceUUID, operationUUID} {
		err := validate.IsUUID(value)
		if err != nil {
			return "", fmt.Errorf("Invalid migration checkpoint identity: %w", err)
		}
	}

	return internalUtil.VarPath("migration-checkpoints", instanceUUID, operationUUID), nil
}

func (d *lxc) createMigrationCheckpoint() (string, error) {
	if d.LocalConfig()[migrationCheckpointKey] != "" {
		return "", errors.New("A previous migration checkpoint still requires recovery")
	}

	if d.op == nil {
		return "", errors.New("Migration checkpoint requires an operation identity")
	}

	operationUUID := d.op.ID()
	path, err := migrationCheckpointPath(d.LocalConfig()["volatile.uuid"], operationUUID)
	if err != nil {
		return "", err
	}

	err = os.MkdirAll(filepath.Dir(path), 0o700)
	if err != nil {
		return "", err
	}

	err = os.Mkdir(path, 0o700)
	if err != nil {
		return "", err
	}

	err = d.VolatileSet(map[string]string{
		migrationCheckpointKey:      operationUUID,
		migrationCheckpointStateKey: "dumping",
	})
	if err != nil {
		return "", errors.Join(err, os.Remove(path))
	}

	return path, nil
}

func (d *lxc) clearMigrationCheckpoint() error {
	operationUUID := d.LocalConfig()[migrationCheckpointKey]
	if operationUUID == "" {
		return nil
	}

	path, err := migrationCheckpointPath(d.LocalConfig()["volatile.uuid"], operationUUID)
	if err != nil {
		return err
	}

	err = os.RemoveAll(path)
	if err != nil {
		return err
	}

	return d.VolatileSet(map[string]string{migrationCheckpointKey: "", migrationCheckpointStateKey: ""})
}

// RestoreMigrationCheckpoint resumes the exact retained source checkpoint after external fencing.
func (d *lxc) RestoreMigrationCheckpoint(operationUUID string) error {
	if d.LocalConfig()[migrationCheckpointKey] != operationUUID || operationUUID == "" {
		return errors.New("Migration checkpoint operation identity does not match")
	}

	if internalInstance.StorageHandoverInProgress(d.LocalConfig()) || internalInstance.StorageDeleteProtected(d.LocalConfig()) {
		return errors.New("Migration checkpoint source storage ownership has not been restored")
	}

	phase := d.LocalConfig()[migrationCheckpointStateKey]
	if phase != "ready" && phase != "restored" {
		return errors.New("Migration checkpoint dump did not complete")
	}

	// Ordinary starts are blocked while the marker exists, so a running source
	// can only be a restore whose response or marker cleanup was interrupted.
	if d.IsRunning() {
		return d.clearMigrationCheckpoint()
	}

	if phase == "restored" {
		return errors.New("The restored migration source has subsequently stopped")
	}

	path, err := migrationCheckpointPath(d.LocalConfig()["volatile.uuid"], operationUUID)
	if err != nil {
		return err
	}

	_, err = os.Stat(filepath.Join(path, "final", "inventory.img"))
	if err != nil {
		return fmt.Errorf("Migration checkpoint inventory is unavailable: %w", err)
	}

	op, err := operationlock.CreateWaitGet(d.Project().Name, d.Name(), d.op, operationlock.ActionStart, nil, false, false)
	if err != nil {
		return err
	}

	defer func() { op.Done(err) }()
	pool, err := storagePools.LoadByInstance(d.state, d)
	if err != nil {
		return err
	}

	_, err = pool.MountInstance(d, d.op)
	if err != nil {
		return err
	}

	err = d.migrate(&instance.CriuMigrationArgs{
		Cmd: liblxc.MIGRATE_RESTORE, StateDir: path, Function: "migration",
		Stop: false, ActionScript: false, DumpDir: "final",
	})
	releaseErr := pool.UnmountInstance(d, nil)
	if releaseErr != nil && !errors.Is(releaseErr, storageDrivers.ErrInUse) {
		err = errors.Join(err, releaseErr)
	}

	if err != nil {
		return err
	}

	err = d.VolatileSet(map[string]string{migrationCheckpointStateKey: "restored"})
	if err != nil {
		return err
	}

	err = d.clearMigrationCheckpoint()
	return err
}

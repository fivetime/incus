//go:build linux && cgo && !agent

package storage

import (
	"errors"
	"fmt"

	"github.com/lxc/incus/v7/internal/server/db"
	"github.com/lxc/incus/v7/internal/server/storage/drivers"
	"github.com/lxc/incus/v7/internal/server/storagematerializationattempt"
)

func persistStorageMaterializationOwnership(driver drivers.Driver, vol drivers.Volume, attempt *db.StorageMaterializationAttempt, identity string) error {
	if attempt == nil || attempt.CleanupDisposition != storagematerializationattempt.CleanupDelete {
		return nil
	}

	provider, ok := driver.(drivers.VolumeMaterializationOwnershipProvider)
	if !ok {
		if identity != "" {
			return errors.New("Storage driver cannot persist materialization ownership evidence")
		}

		return nil
	}

	marker, err := storagematerializationattempt.OwnershipMarker(attempt, identity)
	if err != nil {
		return fmt.Errorf("Build materialized root storage ownership marker: %w", err)
	}

	if err := provider.SetVolumeMaterializationOwnership(vol, marker); err != nil {
		return fmt.Errorf("Persist materialized root storage ownership marker: %w", err)
	}

	persisted, err := provider.GetVolumeMaterializationOwnership(vol)
	if err != nil {
		return fmt.Errorf("Verify materialized root storage ownership marker: %w", err)
	}

	if persisted != marker {
		return errors.New("Materialized root storage ownership marker changed before commit")
	}

	return nil
}

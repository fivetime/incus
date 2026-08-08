package storage

import (
	"errors"
	"fmt"

	"github.com/lxc/incus/v7/internal/server/storage/drivers"
)

// ReleaseVolumeLocalState releases a volume's host-local state after verifying its immutable identity.
func ReleaseVolumeLocalState(driver drivers.Driver, vol drivers.Volume, expectedStorageIdentity string) error {
	return releaseVolumeLocalState(driver, vol, expectedStorageIdentity)
}

func releaseVolumeLocalState(driver any, vol drivers.Volume, expectedStorageIdentity string) error {
	localStateReleaser, ok := driver.(drivers.VolumeLocalStateReleaser)
	if !ok {
		return errors.New("Storage driver cannot safely release volume local state")
	}

	err := localStateReleaser.ReleaseVolumeLocalState(vol, expectedStorageIdentity)
	if err != nil {
		return err
	}

	err = validateVolumeLocalStateReleased(localStateReleaser, vol, expectedStorageIdentity)
	if err != nil {
		return err
	}

	return nil
}

// ReleaseVolumeLocalStateDetached tears down host-local state that provably belongs to a failed
// or abandoned claim, including leaked mounts and mount references. The caller must have proven
// the volume has no database record (or be deleting it) before invoking this; drivers without
// detached release support fall back to the conservative release.
func ReleaseVolumeLocalStateDetached(driver drivers.Driver, vol drivers.Volume, expectedStorageIdentity string) error {
	detachedReleaser, ok := any(driver).(drivers.VolumeDetachedLocalStateReleaser)
	if !ok {
		return releaseVolumeLocalState(driver, vol, expectedStorageIdentity)
	}

	err := detachedReleaser.ReleaseVolumeDetachedLocalState(vol, expectedStorageIdentity)
	if err != nil {
		return err
	}

	return validateVolumeLocalStateReleased(detachedReleaser, vol, expectedStorageIdentity)
}

// ValidateVolumeLocalStateReleased proves that a volume has no host-local state.
func ValidateVolumeLocalStateReleased(driver drivers.Driver, vol drivers.Volume, expectedStorageIdentity string) error {
	provider, ok := driver.(drivers.VolumeLocalStateProvider)
	if !ok {
		return errors.New("Storage driver cannot prove volume local state")
	}

	return validateVolumeLocalStateReleased(provider, vol, expectedStorageIdentity)
}

func validateVolumeLocalStateReleased(provider drivers.VolumeLocalStateProvider, vol drivers.Volume, expectedStorageIdentity string) error {
	if expectedStorageIdentity == "" {
		return errors.New("Cannot prove volume local state without an immutable storage identity")
	}

	hasLocalState, err := provider.HasVolumeLocalState(vol, expectedStorageIdentity)
	if err != nil {
		return fmt.Errorf("Inspect volume local state: %w", err)
	}

	if hasLocalState {
		return errors.New("Volume still has host-local state")
	}

	return nil
}

func completeStorageReleaseAfterLocalStateProof(provider drivers.VolumeLocalStateProvider, vol drivers.Volume, expectedStorageIdentity string, complete func() error) error {
	err := validateVolumeLocalStateReleased(provider, vol, expectedStorageIdentity)
	if err != nil {
		return err
	}

	return complete()
}

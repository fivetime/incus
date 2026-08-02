package drivers

import (
	"errors"
	"fmt"
	"io"

	internalInstance "github.com/lxc/incus/v7/internal/instance"
	"github.com/lxc/incus/v7/internal/server/instance"
)

const sharedStorageHandoverReadyMarker = "incus-shared-storage-handover-ready-v1"

func sharedStorageHandoverDriverIdentity(driverName string) string {
	if !isCephSharedStorageDriver(driverName) {
		return ""
	}

	return driverName + "+ready-v1"
}

func signalSharedStorageHandoverReady(conn io.Writer) error {
	n, err := io.WriteString(conn, sharedStorageHandoverReadyMarker)
	if err != nil {
		return fmt.Errorf("Send shared storage handover readiness: %w", err)
	}

	if n != len(sharedStorageHandoverReadyMarker) {
		return io.ErrShortWrite
	}

	return nil
}

func releaseAndSignalSharedStorageHandover(release func() error, conn io.Writer) error {
	err := release()
	if err != nil {
		return fmt.Errorf("Release source root before shared storage handover: %w", err)
	}

	return signalSharedStorageHandoverReady(conn)
}

func waitSharedStorageHandoverReady(conn io.Reader) error {
	marker := make([]byte, len(sharedStorageHandoverReadyMarker))
	_, err := io.ReadFull(conn, marker)
	if err != nil {
		return fmt.Errorf("Receive shared storage handover readiness: %w", err)
	}

	if string(marker) != sharedStorageHandoverReadyMarker {
		return errors.New("Invalid shared storage handover readiness marker")
	}

	return nil
}

func claimSharedStorageMigrationTarget(sharedStorage bool, conn io.Reader, guard func() error, claim func() error) error {
	if sharedStorage {
		err := waitSharedStorageHandoverReady(conn)
		if err != nil {
			return err
		}

		err = withMigrationAttemptGuard(guard, "before shared storage claim", nil)
		if err != nil {
			return err
		}
	}

	return claim()
}

func withMigrationAttemptGuard(guard func() error, phase string, action func() error) error {
	if guard != nil {
		err := guard()
		if err != nil {
			return fmt.Errorf("Migration attempt is fenced %s: %w", phase, err)
		}
	}

	if action == nil {
		return nil
	}

	return action()
}

func withSharedStorageMigrationTargetProtection(sharedStorage bool, driverName string, volatileSet func(map[string]string) error, claim func() error) error {
	if sharedStorage && isCephSharedStorageDriver(driverName) {
		changes := map[string]string{
			internalInstance.ConfigVolatileMigrationStorageHandoverRole:     internalInstance.StorageHandoverRoleTarget,
			internalInstance.ConfigVolatileMigrationStorageDeleteProtection: "true",
		}

		err := volatileSet(changes)
		if err != nil {
			return fmt.Errorf("Persist target storage protection: %w", err)
		}
	}

	return claim()
}

func markSharedStorageMigrationTargetReceiveComplete(sharedStorage bool, driverName string, volatileSet func(map[string]string) error) error {
	if !sharedStorage || !isCephSharedStorageDriver(driverName) {
		return nil
	}

	return volatileSet(map[string]string{
		internalInstance.ConfigVolatileMigrationStorageReceiveComplete: "true",
	})
}

func releaseSharedStorageMigrationTargetClaim(driverName string, isRunning func() bool, stop func() error, unmount func() error, volatileSet func(map[string]string) error, deleteInstance func() error) error {
	cleanupError := func(action string, err error) error {
		return errors.Join(instance.ErrMigrationTargetCleanupIncomplete, fmt.Errorf("%s: %w", action, err))
	}

	if !isCephSharedStorageDriver(driverName) {
		return cleanupError("Release shared storage migration target claim", fmt.Errorf("Unsupported storage driver %q", driverName))
	}

	if isRunning() {
		err := stop()
		if err != nil {
			return cleanupError("Stop migration target", err)
		}

		if isRunning() {
			return cleanupError("Stop migration target", errors.New("Instance is still running"))
		}
	}

	err := unmount()
	if err != nil {
		return cleanupError("Unmount migration target storage", err)
	}

	changes := map[string]string{
		internalInstance.ConfigVolatileMigrationStorageHandoverRole:     internalInstance.StorageHandoverRoleTarget,
		internalInstance.ConfigVolatileMigrationStorageDeleteProtection: "true",
	}
	if driverName == "ceph" {
		changes[internalInstance.ConfigVolatileMigrationStorageHandover] = "pending"
	}

	err = volatileSet(changes)
	if err != nil {
		return cleanupError("Protect migration target storage", err)
	}

	err = deleteInstance()
	if err != nil {
		return cleanupError("Delete migration target storage claim", err)
	}

	return nil
}

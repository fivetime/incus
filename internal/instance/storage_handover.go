package instance

import (
	"errors"
	"fmt"

	"github.com/lxc/incus/v7/shared/util"
)

// ErrStorageHandoverInactive indicates that no shared storage handover is in progress.
var ErrStorageHandoverInactive = errors.New("Instance has no active shared storage handover")

// ErrStorageHandoverIncomplete indicates that the recorded evidence cannot prove a transition is safe.
var ErrStorageHandoverIncomplete = errors.New("Shared storage handover state does not prove this transition is safe")

// ErrStorageHandoverUnsupported indicates that a storage driver cannot perform a handover transition.
var ErrStorageHandoverUnsupported = errors.New("Storage handover transition is not supported")

const (
	// ConfigVolatileMigrationStorageHandover records the source side of a shared storage handover.
	ConfigVolatileMigrationStorageHandover = "volatile.migration.storage_handover"

	// ConfigVolatileMigrationStorageHandoverRole records whether this record is the migration source or target.
	ConfigVolatileMigrationStorageHandoverRole = "volatile.migration.storage_handover_role"

	// ConfigVolatileMigrationStorageDeleteProtection preserves shared storage when deleting a local instance record.
	ConfigVolatileMigrationStorageDeleteProtection = "volatile.migration.storage_delete_protection"

	// ConfigVolatileMigrationStorageReceiveComplete proves that the target completed its migration receive.
	ConfigVolatileMigrationStorageReceiveComplete = "volatile.migration.storage_receive_complete"

	// StorageHandoverStateProtected keeps shared storage when the local instance record is deleted.
	StorageHandoverStateProtected = "protected"

	// StorageHandoverStateOwned makes the local instance record authoritative for shared storage deletion.
	StorageHandoverStateOwned = "owned"

	// StorageHandoverStateSourceOwned restores a fenced source after external target cleanup proof.
	StorageHandoverStateSourceOwned = "source-owned"

	// StorageHandoverRoleSource identifies the original migration source record.
	StorageHandoverRoleSource = "source"

	// StorageHandoverRoleTarget identifies the migration target record.
	StorageHandoverRoleTarget = "target"
)

// StorageHandoverConfigChanges returns the volatile changes for a storage handover state transition.
func StorageHandoverConfigChanges(state string) (map[string]string, error) {
	switch state {
	case StorageHandoverStateProtected:
		return map[string]string{
			ConfigVolatileMigrationStorageDeleteProtection: "true",
		}, nil
	case StorageHandoverStateOwned:
		return map[string]string{
			ConfigVolatileMigrationStorageDeleteProtection: "",
			ConfigVolatileMigrationStorageReceiveComplete:  "",
			ConfigVolatileMigrationStorageHandover:         "",
			ConfigVolatileMigrationStorageHandoverRole:     "",
		}, nil
	case StorageHandoverStateSourceOwned:
		return map[string]string{
			ConfigVolatileMigrationStorageDeleteProtection: "",
			ConfigVolatileMigrationStorageReceiveComplete:  "",
			ConfigVolatileMigrationStorageHandover:         "",
			ConfigVolatileMigrationStorageHandoverRole:     "",
		}, nil
	default:
		return nil, fmt.Errorf("Invalid storage handover state %q", state)
	}
}

// StorageHandoverAPIConfigChanges validates a public storage handover transition.
func StorageHandoverAPIConfigChanges(state string, config map[string]string, driverName string) (map[string]string, error) {
	if !StorageHandoverDriverSupported(driverName, state) {
		return nil, fmt.Errorf("%w: state %q with storage driver %q", ErrStorageHandoverUnsupported, state, driverName)
	}

	switch state {
	case StorageHandoverStateProtected:
		marker := config[ConfigVolatileMigrationStorageHandover]
		role := config[ConfigVolatileMigrationStorageHandoverRole]
		sourceEvidence := (marker == "pending" || marker == "committed") &&
			role == StorageHandoverRoleSource &&
			!util.IsTrue(config[ConfigVolatileMigrationStorageReceiveComplete])
		targetEvidence := marker == "" && role == StorageHandoverRoleTarget &&
			util.IsTrue(config[ConfigVolatileMigrationStorageDeleteProtection])
		if sourceEvidence || targetEvidence {
			break
		}

		if marker == "" && role == "" &&
			!util.IsTrue(config[ConfigVolatileMigrationStorageDeleteProtection]) &&
			!util.IsTrue(config[ConfigVolatileMigrationStorageReceiveComplete]) {
			return nil, ErrStorageHandoverInactive
		}

		return nil, ErrStorageHandoverIncomplete
	case StorageHandoverStateOwned:
		marker := config[ConfigVolatileMigrationStorageHandover]
		role := config[ConfigVolatileMigrationStorageHandoverRole]
		deleteProtected := util.IsTrue(config[ConfigVolatileMigrationStorageDeleteProtection])
		receiveComplete := util.IsTrue(config[ConfigVolatileMigrationStorageReceiveComplete])
		targetComplete := false
		if marker == "" && role == StorageHandoverRoleTarget && receiveComplete {
			switch driverName {
			case "ceph":
				targetComplete = deleteProtected
			case "cephext":
				targetComplete = !deleteProtected
			}
		}

		noHandoverState := marker == "" && role == "" && !deleteProtected && !receiveComplete

		if noHandoverState {
			return map[string]string{}, nil
		}

		if !targetComplete {
			return nil, ErrStorageHandoverIncomplete
		}

	case StorageHandoverStateSourceOwned:
		marker := config[ConfigVolatileMigrationStorageHandover]
		role := config[ConfigVolatileMigrationStorageHandoverRole]
		deleteProtected := util.IsTrue(config[ConfigVolatileMigrationStorageDeleteProtection])
		receiveComplete := util.IsTrue(config[ConfigVolatileMigrationStorageReceiveComplete])

		if marker == "" && role == "" && !deleteProtected && !receiveComplete {
			return map[string]string{}, nil
		}

		if (marker != "pending" && marker != "committed") ||
			role != StorageHandoverRoleSource ||
			receiveComplete {
			return nil, ErrStorageHandoverIncomplete
		}

	default:
		return nil, fmt.Errorf("Invalid storage handover state %q", state)
	}

	return StorageHandoverConfigChanges(state)
}

// StorageDeleteProtected returns whether deleting the local record must preserve its shared storage.
func StorageDeleteProtected(config map[string]string) bool {
	return config[ConfigVolatileMigrationStorageHandover] != "" || util.IsTrue(config[ConfigVolatileMigrationStorageDeleteProtection])
}

// StorageHandoverDriverSupported returns whether the storage driver supports a handover state transition.
func StorageHandoverDriverSupported(driverName string, state string) bool {
	switch state {
	case StorageHandoverStateProtected:
		return driverName == "ceph"
	case StorageHandoverStateOwned, StorageHandoverStateSourceOwned:
		return driverName == "ceph" || driverName == "cephext"
	default:
		return false
	}
}

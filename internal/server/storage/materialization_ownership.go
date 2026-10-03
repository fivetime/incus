//go:build linux && cgo && !agent

package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	internalInstance "github.com/lxc/incus/v7/internal/instance"
	"github.com/lxc/incus/v7/internal/server/db"
	"github.com/lxc/incus/v7/internal/server/instance"
	"github.com/lxc/incus/v7/internal/server/sharedroottakeover"
	"github.com/lxc/incus/v7/internal/server/storage/drivers"
	"github.com/lxc/incus/v7/internal/server/storagematerializationattempt"
)

func (b *backend) validateSharedRootOwnership(inst instance.Instance, vol drivers.Volume) error {
	if b.driver.Info().Name != "ceph" {
		return nil
	}

	config := inst.LocalConfig()
	expectedMarker := config["volatile.shared_root_takeover.ownership"]
	expectedIdentity := config["volatile.shared_root_takeover.storage_identity"]
	expectedFSID := config["volatile.shared_root_takeover.cluster_fsid"]
	if expectedFSID != "" {
		provider, ok := b.driver.(drivers.StorageClusterIdentityProvider)
		if !ok {
			return errors.New("Shared root lacks physical cluster identity verification")
		}

		fsid, err := provider.GetStorageClusterIdentity()
		if err != nil || fsid != expectedFSID {
			return errors.New("Shared root pool points to another physical Ceph cluster")
		}
	}
	token := config["volatile.shared_root_takeover.token"]
	if token != "" && token == config[internalInstance.ConfigOpenStackRootfsMaterializationID] {
		record, err := sharedroottakeover.New(b.state.DB.Node).Get(context.Background(), token)
		if err != nil {
			return err
		}

		switch record.Phase {
		case sharedroottakeover.Committed, sharedroottakeover.Starting, sharedroottakeover.Started, sharedroottakeover.Completed:
		default:
			return errors.New("Original root cannot be mounted or deleted before takeover ownership commits")
		}

		binding := sharedroottakeover.Binding{}
		err = json.Unmarshal([]byte(record.Binding), &binding)
		if err != nil {
			return err
		}

		marker, err := sharedroottakeover.OwnershipMarker(binding)
		if err != nil || marker != expectedMarker || binding.StorageIdentity != expectedIdentity ||
			binding.TargetComputeID != config[internalInstance.ConfigOpenStackComputeID] || binding.Owner != config["user.openstack.uuid"] {
			return errors.New("Local takeover configuration differs from its durable ownership journal")
		}
	}

	if expectedMarker == "" {
		materializationToken := config[internalInstance.ConfigOpenStackRootfsMaterializationID]
		if materializationToken == "" {
			return nil
		}

		attempt, err := storagematerializationattempt.New(b.state.DB.Node).Get(context.Background(), materializationToken)
		if err != nil {
			return err
		}

		if attempt.State != storagematerializationattempt.StateCommitted || attempt.CleanupDisposition != storagematerializationattempt.CleanupDelete {
			return nil
		}

		expectedIdentity = attempt.StorageIdentity
		expectedMarker, err = storagematerializationattempt.OwnershipMarker(attempt, expectedIdentity)
		if err != nil {
			return err
		}
	}

	identityProvider, ok := b.driver.(drivers.VolumeIdentityProvider)
	if !ok {
		return errors.New("Shared root lacks immutable identity verification")
	}

	identity, err := identityProvider.GetVolumeIdentity(vol)
	if err != nil || identity != expectedIdentity {
		return errors.New("Shared root no longer identifies this instance's original image")
	}

	markerProvider, ok := b.driver.(drivers.VolumeMaterializationOwnershipProvider)
	if !ok {
		return errors.New("Shared root lacks ownership verification")
	}

	marker, err := markerProvider.GetVolumeMaterializationOwnership(vol)
	if err != nil || marker != expectedMarker {
		return errors.New("Shared root ownership has moved; stale instance cannot mount or delete the image")
	}

	return nil
}

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

	err = provider.SetVolumeMaterializationOwnership(vol, marker)
	if err != nil {
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

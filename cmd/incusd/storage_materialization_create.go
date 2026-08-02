package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	internalInstance "github.com/lxc/incus/v7/internal/instance"
	"github.com/lxc/incus/v7/internal/server/db"
	serverInstance "github.com/lxc/incus/v7/internal/server/instance"
	"github.com/lxc/incus/v7/internal/server/locking"
	"github.com/lxc/incus/v7/internal/server/operations"
	"github.com/lxc/incus/v7/internal/server/response"
	"github.com/lxc/incus/v7/internal/server/state"
	storagePools "github.com/lxc/incus/v7/internal/server/storage"
	"github.com/lxc/incus/v7/internal/server/storage/drivers"
	"github.com/lxc/incus/v7/internal/server/storagematerializationattempt"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/logger"
)

func storageMaterializationAttemptForRequest(s *state.State, projectName string, req *api.InstancesPost) (*db.StorageMaterializationAttempt, error) {
	if err := validateStorageMaterializationRequest(req); err != nil {
		return nil, err
	}

	config := req.Config
	values := []string{
		config[internalInstance.ConfigOpenStackIDMapAllocationID],
		config[internalInstance.ConfigOpenStackComputeID],
		config[internalInstance.ConfigOpenStackRootfsMaterializationID],
		config["user.openstack.uuid"],
	}
	if values[2] == "" {
		return nil, nil
	}

	base, err := strconv.ParseInt(config["security.idmap.base"], 10, 64)
	if err != nil {
		return nil, err
	}
	size, err := strconv.ParseInt(config["security.idmap.size"], 10, 64)
	if err != nil {
		return nil, err
	}

	token := config[internalInstance.ConfigOpenStackRootfsMaterializationID]
	attempt, err := storagematerializationattempt.New(s.DB.Node).Get(context.Background(), token)
	if err != nil {
		return nil, err
	}
	expected := &db.StorageMaterializationAttempt{Token: token, AllocationID: values[0], ComputeID: values[1], Owner: values[3], Project: projectName, InstanceName: req.Name, IDMapBase: base, IDMapSize: size, StorageDriver: attempt.StorageDriver, StoragePool: attempt.StoragePool, StorageVolume: attempt.StorageVolume, RBDImage: attempt.RBDImage, StorageIdentity: attempt.StorageIdentity, BaselineClean: attempt.BaselineClean, CleanupDisposition: attempt.CleanupDisposition}
	if !storagematerializationattempt.SameBinding(attempt, expected) {
		return nil, storagematerializationattempt.ErrBindingMismatch
	}
	return attempt, nil
}

func validateStorageMaterializationRequest(req *api.InstancesPost) error {
	config := req.Config
	allocationID := config[internalInstance.ConfigOpenStackIDMapAllocationID]
	computeID := config[internalInstance.ConfigOpenStackComputeID]
	token := config[internalInstance.ConfigOpenStackRootfsMaterializationID]
	owner := config["user.openstack.uuid"]
	idmapBase := config["security.idmap.base"]
	idmapSize := config["security.idmap.size"]
	values := []string{
		allocationID,
		computeID,
		token,
		owner,
	}
	protocolConfigured := allocationID != "" || computeID != "" || token != ""
	openStackIDMapConfigured := owner != "" && (idmapBase != "" || idmapSize != "")
	if !protocolConfigured && !openStackIDMapConfigured {
		return nil
	}
	for _, value := range values {
		if value == "" {
			return errors.New("OpenStack rootfs materialization A/H/T/U must be supplied together")
		}
	}
	for _, value := range values {
		if err := validateCanonicalStorageMaterializationUUID(value); err != nil {
			return fmt.Errorf("Invalid rootfs materialization identity: %w", err)
		}
	}

	if req.Source.Refresh || (req.Source.Type != "image" && req.Source.Type != "none" && req.Source.Type != "migration") {
		return fmt.Errorf("Storage materialization attempts do not support source type %q or refresh", req.Source.Type)
	}

	base, err := strconv.ParseInt(idmapBase, 10, 64)
	if err != nil || base < 0 {
		return errors.New("Storage materialization attempts require a fixed security.idmap.base")
	}
	size, err := strconv.ParseInt(idmapSize, 10, 64)
	if err != nil || size <= 0 || size > 1<<32 || base > (1<<32)-size {
		return errors.New("Storage materialization attempts require a valid fixed security.idmap.size")
	}

	return nil
}

func beginStorageMaterializationAttempt(ctx context.Context, s *state.State, projectName string, req *api.InstancesPost, op *operations.Operation) (*db.StorageMaterializationAttempt, error) {
	attempt, err := storageMaterializationAttemptForRequest(s, projectName, req)
	if err != nil || attempt == nil {
		return attempt, err
	}

	return storagematerializationattempt.New(s.DB.Node).Start(ctx, attempt.Token, *attempt, s.StartTime.UnixNano(), op.ID())
}

func commitStorageMaterializationAttempt(ctx context.Context, s *state.State, attempt *db.StorageMaterializationAttempt) error {
	if attempt == nil {
		return nil
	}

	inst, err := serverInstance.LoadByProjectAndName(s, attempt.Project, attempt.InstanceName)
	if err != nil {
		return fmt.Errorf("Load materialized instance before commit: %w", err)
	}

	if err := validateStorageMaterializationInstance(attempt, inst); err != nil {
		return err
	}

	return storagematerializationattempt.New(s.DB.Node).Commit(ctx, attempt.Token)
}

func commitMigrationAndStorageMaterializationAttempts(ctx context.Context, s *state.State, migrationToken string, attempt *db.StorageMaterializationAttempt) error {
	if attempt == nil {
		return errors.New("Storage materialization attempt is required")
	}

	inst, err := serverInstance.LoadByProjectAndName(s, attempt.Project, attempt.InstanceName)
	if err != nil {
		return fmt.Errorf("Load materialized instance before migration commit: %w", err)
	}
	if err := validateStorageMaterializationInstance(attempt, inst); err != nil {
		return err
	}
	if err := validateStorageMaterializationHandoverTarget(attempt, inst.LocalConfig()); err != nil {
		return err
	}

	return storagematerializationattempt.New(s.DB.Node).CommitWithMigration(ctx, migrationToken, attempt.Token)
}

func validateStorageMaterializationHandoverTarget(attempt *db.StorageMaterializationAttempt, config map[string]string) error {
	if attempt == nil || attempt.CleanupDisposition != storagematerializationattempt.CleanupHandover {
		return nil
	}

	if config[internalInstance.ConfigVolatileMigrationStorageHandoverRole] != internalInstance.StorageHandoverRoleTarget ||
		config[internalInstance.ConfigVolatileMigrationStorageReceiveComplete] != "true" ||
		config[internalInstance.ConfigVolatileMigrationStorageDeleteProtection] != "true" {
		return errors.New("Storage handover materialization lacks a completed protected shared migration target")
	}

	return nil
}

func validateStorageMaterializationInstance(attempt *db.StorageMaterializationAttempt, inst serverInstance.Instance) error {
	config := inst.LocalConfig()
	if config[internalInstance.ConfigOpenStackRootfsMaterializationID] != attempt.Token ||
		config[internalInstance.ConfigOpenStackIDMapAllocationID] != attempt.AllocationID ||
		config[internalInstance.ConfigOpenStackComputeID] != attempt.ComputeID ||
		config["user.openstack.uuid"] != attempt.Owner ||
		config["security.idmap.base"] != strconv.FormatInt(attempt.IDMapBase, 10) ||
		config["security.idmap.size"] != strconv.FormatInt(attempt.IDMapSize, 10) {
		return errors.New("Materialized instance no longer matches the registered A/H/T/U and idmap binding")
	}

	return nil
}

func failStorageMaterializationAttempt(s *state.State, attempt *db.StorageMaterializationAttempt, createErr error) error {
	if attempt == nil {
		return createErr
	}
	manager := storagematerializationattempt.New(s.DB.Node)
	latest, err := manager.Abort(context.Background(), attempt.Token)
	if err != nil {
		return errors.Join(createErr, err)
	}
	err = reconcileStorageMaterializationAttempt(context.Background(), s, latest, false)
	if err != nil {
		return errors.Join(createErr, fmt.Errorf("Storage materialization cleanup is incomplete: %w", err))
	}
	return createErr
}

func validateStorageMaterializationReconcileBoundary(s *state.State, attempt *db.StorageMaterializationAttempt, allowUnboundSameDaemon bool) error {
	if allowUnboundSameDaemon {
		if attempt.OperationUUID != "" || attempt.DaemonStart != s.StartTime.UnixNano() {
			return errors.New("Internal unbound materialization reconciliation does not match this daemon setup failure")
		}

		return nil
	}

	if attempt.OperationUUID == "" && attempt.DaemonStart == s.StartTime.UnixNano() {
		return errors.New("Cannot reconcile a same-daemon materialization attempt before its target operation has been identified")
	}

	return nil
}

func reconcileStorageMaterializationAttempt(ctx context.Context, s *state.State, attempt *db.StorageMaterializationAttempt, allowUnboundSameDaemon bool) error {
	if attempt == nil || !attempt.Started {
		return errors.New("Only a started storage materialization attempt can be reconciled")
	}

	unlockInstance, err := locking.Lock(ctx, "storage_materialization_instance_"+attempt.Project+"_"+attempt.InstanceName)
	if err != nil {
		return err
	}
	defer unlockInstance()

	unlockAttempt, err := locking.Lock(ctx, "storage_materialization_reconcile_"+attempt.Token)
	if err != nil {
		return err
	}
	defer unlockAttempt()

	manager := storagematerializationattempt.New(s.DB.Node)
	attempt, err = manager.Get(ctx, attempt.Token)
	if err != nil {
		return err
	}
	if err := validateStorageMaterializationReconcileBoundary(s, attempt, allowUnboundSameDaemon); err != nil {
		return err
	}
	if attempt.State == storagematerializationattempt.StateClean {
		return nil
	}
	if attempt.State != storagematerializationattempt.StateAborted || !attempt.Started || attempt.Finished {
		return errors.New("Only an aborted started materialization attempt can be reconciled")
	}
	deleteBackend, err := storageMaterializationCleanupDeletesBackend(attempt)
	if err != nil {
		return err
	}

	pool, err := storagePools.LoadByName(s, attempt.StoragePool)
	if err != nil {
		return err
	}
	if pool.Driver().Info().Name != attempt.StorageDriver {
		return errors.New("Materialization storage driver changed")
	}

	dbVol, dbErr := storagePools.VolumeDBGet(pool, attempt.Project, attempt.InstanceName, drivers.VolumeTypeContainer)
	if dbErr == nil {
		config := dbVol.Config
		if attempt.RBDImage != "" && config["ceph.rbd.image_name"] != attempt.RBDImage {
			return errors.New("Materialization volume DB claim refers to another RBD image")
		}

		vol := pool.GetVolume(drivers.VolumeTypeContainer, drivers.ContentTypeFS, attempt.StorageVolume, config)
		exists, err := pool.Driver().HasVolume(vol)
		if err != nil {
			return fmt.Errorf("Check materialized volume before cleanup: %w", err)
		}

		attempt, err = bindMaterializationCleanupIdentity(ctx, manager, attempt, pool.Driver(), vol, exists)
		if err != nil {
			return err
		}

		if provider, ok := pool.Driver().(drivers.VolumeIdentityProvider); ok {
			if exists && attempt.StorageIdentity == "" {
				return errors.New("Materialized volume has no recorded immutable identity")
			}

			if exists && (!deleteBackend || attempt.StorageIdentity == "") {
				identity, err := provider.GetVolumeIdentity(vol)
				if err != nil {
					return fmt.Errorf("Read materialized volume identity before cleanup: %w", err)
				}
				if identity != attempt.StorageIdentity {
					return errors.New("Materialized volume identity changed before cleanup")
				}
			}
		}
	} else if !response.IsNotFoundError(dbErr) {
		return dbErr
	}

	inst, err := serverInstance.LoadByProjectAndName(s, attempt.Project, attempt.InstanceName)
	if err == nil {
		if err := validateStorageMaterializationInstance(attempt, inst); err != nil {
			return err
		}
		if inst.IsRunning() {
			return errors.New("Cannot reconcile a running materialization target")
		}
		if storageMaterializationCleanupProtectsBackend(attempt.CleanupDisposition) && !internalInstance.StorageDeleteProtected(inst.LocalConfig()) {
			if err := inst.VolatileSet(map[string]string{internalInstance.ConfigVolatileMigrationStorageDeleteProtection: "true"}); err != nil {
				return fmt.Errorf("Persist retained storage cleanup disposition on instance: %w", err)
			}
		}
		if err := inst.Delete(true, true); err != nil {
			return fmt.Errorf("Delete failed materialization instance: %w", err)
		}
	} else if !response.IsNotFoundError(err) {
		return err
	}

	_, err = serverInstance.LoadByProjectAndName(s, attempt.Project, attempt.InstanceName)
	if err == nil {
		return errors.New("Materialization instance still exists after cleanup")
	}
	if !response.IsNotFoundError(err) {
		return err
	}

	dbVol, dbErr = storagePools.VolumeDBGet(pool, attempt.Project, attempt.InstanceName, drivers.VolumeTypeContainer)
	if dbErr == nil {
		config := dbVol.Config
		if attempt.RBDImage != "" && config["ceph.rbd.image_name"] != attempt.RBDImage {
			return errors.New("Materialization volume DB claim refers to another RBD image")
		}
		vol := pool.GetVolume(drivers.VolumeTypeContainer, drivers.ContentTypeFS, attempt.StorageVolume, config)
		if !deleteBackend {
			if err := storagePools.ReleaseVolumeLocalState(pool.Driver(), vol, attempt.StorageIdentity); err != nil {
				return fmt.Errorf("Release failed materialization storage local state: %w", err)
			}
		} else {
			if attempt.StorageIdentity != "" {
				err := deleteStorageMaterializationVolumeWithIdentity(pool.Driver(), vol, attempt.StorageIdentity)
				if err != nil {
					return fmt.Errorf("Delete failed materialization storage: %w", err)
				}
			}
		}
		if err := storagePools.VolumeDBDelete(pool, attempt.Project, attempt.InstanceName, drivers.VolumeTypeContainer); err != nil {
			return err
		}
	} else if !response.IsNotFoundError(dbErr) {
		return dbErr
	}

	config := map[string]string{}
	if attempt.RBDImage != "" {
		config["ceph.rbd.image_name"] = attempt.RBDImage
	}
	vol := pool.GetVolume(drivers.VolumeTypeContainer, drivers.ContentTypeFS, attempt.StorageVolume, config)
	if !deleteBackend {
		if err := storagePools.ReleaseVolumeLocalState(pool.Driver(), vol, attempt.StorageIdentity); err != nil {
			return fmt.Errorf("Release detached Ceph local claim state: %w", err)
		}

		if err := storagePools.ValidateVolumeLocalStateReleased(pool.Driver(), vol, attempt.StorageIdentity); err != nil {
			return fmt.Errorf("Prove detached Ceph local claim release: %w", err)
		}
	} else {
		exists, err := pool.Driver().HasVolume(vol)
		if err != nil {
			return err
		}

		attempt, err = bindMaterializationCleanupIdentity(ctx, manager, attempt, pool.Driver(), vol, exists)
		if err != nil {
			return err
		}

		if attempt.StorageIdentity != "" {
			if err := deleteStorageMaterializationVolumeWithIdentity(pool.Driver(), vol, attempt.StorageIdentity); err != nil {
				return err
			}
		}
	}

	return manager.FinishClean(ctx, attempt.Token)
}

func deleteStorageMaterializationVolumeWithIdentity(driver drivers.Driver, vol drivers.Volume, expectedStorageIdentity string) error {
	if expectedStorageIdentity == "" {
		return errors.New("Cannot delete materialization storage without an immutable identity")
	}

	deleter, ok := driver.(drivers.VolumeIdentityBoundDeleter)
	if !ok {
		return errors.New("Materialization storage driver cannot perform identity-bound deletion")
	}

	return deleter.DeleteVolumeWithIdentity(vol, expectedStorageIdentity, nil)
}

func bindMaterializationCleanupIdentity(ctx context.Context, manager *storagematerializationattempt.Manager, attempt *db.StorageMaterializationAttempt, driver drivers.Driver, vol drivers.Volume, exists bool) (*db.StorageMaterializationAttempt, error) {
	if attempt == nil {
		return nil, errors.New("Storage materialization attempt is required")
	}
	if !exists || attempt.StorageIdentity != "" {
		return attempt, nil
	}
	if !attempt.BaselineClean || attempt.CleanupDisposition != storagematerializationattempt.CleanupDelete || attempt.StoragePhase != storagematerializationattempt.PhasePending {
		return nil, errors.New("Cannot bind an unproven materialization storage generation during cleanup")
	}

	provider, ok := driver.(drivers.VolumeIdentityProvider)
	if !ok {
		return nil, errors.New("Cannot recover materialized storage without immutable identity support")
	}
	identity, err := provider.GetVolumeIdentity(vol)
	if err != nil {
		return nil, fmt.Errorf("Read newly materialized volume identity during cleanup: %w", err)
	}
	if identity == "" {
		return nil, errors.New("Newly materialized volume has an empty immutable identity")
	}
	markerProvider, ok := driver.(drivers.VolumeMaterializationOwnershipProvider)
	if !ok {
		return nil, errors.New("Cannot recover materialized storage without durable ownership evidence")
	}
	expectedMarker, err := storagematerializationattempt.OwnershipMarker(attempt, identity)
	if err != nil {
		return nil, fmt.Errorf("Build materialized volume ownership marker: %w", err)
	}
	marker, err := markerProvider.GetVolumeMaterializationOwnership(vol)
	if err != nil {
		return nil, fmt.Errorf("Read materialized volume ownership marker: %w", err)
	}
	if marker != expectedMarker {
		return nil, errors.New("Materialized volume lacks exact durable ownership evidence for this attempt")
	}

	err = manager.SetStoragePhase(ctx, attempt.Token, storagematerializationattempt.PhaseMaterialized, identity)
	if err != nil {
		return nil, fmt.Errorf("Persist newly materialized volume identity during cleanup: %w", err)
	}

	attempt, err = manager.Get(ctx, attempt.Token)
	if err != nil {
		return nil, err
	}

	return attempt, nil
}

func storageMaterializationCleanupDeletesBackend(attempt *db.StorageMaterializationAttempt) (bool, error) {
	if attempt == nil {
		return false, errors.New("Storage materialization attempt is required")
	}

	switch attempt.CleanupDisposition {
	case storagematerializationattempt.CleanupDelete:
		if attempt.StorageDriver == "cephext" {
			return false, errors.New("External Ceph materialization cannot delete its backend image")
		}
		return true, nil
	case storagematerializationattempt.CleanupDetach, storagematerializationattempt.CleanupHandover:
		return false, nil
	default:
		return false, errors.New("Storage materialization cleanup disposition is invalid")
	}
}

func storageMaterializationCleanupProtectsBackend(cleanupDisposition string) bool {
	return cleanupDisposition == storagematerializationattempt.CleanupDetach || cleanupDisposition == storagematerializationattempt.CleanupHandover
}

func reconcileStorageMaterializationAttemptsAfterRestart(ctx context.Context, s *state.State) {
	manager := storagematerializationattempt.New(s.DB.Node)
	attempts, err := manager.ListUnfinished(ctx)
	if err != nil {
		logger.Error("Failed loading unfinished storage materialization attempts", logger.Ctx{"err": err})
		return
	}

	for i := range attempts {
		attempt := &attempts[i]
		if attempt.DaemonStart == s.StartTime.UnixNano() {
			continue
		}

		attempt, err := manager.Abort(ctx, attempt.Token)
		if err == nil {
			err = reconcileStorageMaterializationAttempt(ctx, s, attempt, false)
		}
		if err != nil {
			logger.Error("Storage materialization recovery remains uncertain", logger.Ctx{
				"attempt":  attempt.Token,
				"project":  attempt.Project,
				"instance": attempt.InstanceName,
				"pool":     attempt.StoragePool,
				"volume":   attempt.StorageVolume,
				"err":      err,
			})
			continue
		}

		logger.Info("Recovered unfinished storage materialization attempt", logger.Ctx{"attempt": attempt.Token, "project": attempt.Project, "instance": attempt.InstanceName})
	}
}

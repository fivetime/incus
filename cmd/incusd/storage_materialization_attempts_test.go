//go:build linux && cgo && !agent

package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	internalInstance "github.com/lxc/incus/v7/internal/instance"
	"github.com/lxc/incus/v7/internal/server/db"
	"github.com/lxc/incus/v7/internal/server/db/operationtype"
	"github.com/lxc/incus/v7/internal/server/operations"
	"github.com/lxc/incus/v7/internal/server/state"
	"github.com/lxc/incus/v7/internal/server/storage/drivers"
	"github.com/lxc/incus/v7/internal/server/storagematerializationattempt"
	"github.com/lxc/incus/v7/shared/api"
)

type materializationIdentityTestDriver struct {
	drivers.Driver
	identity         string
	ownership        string
	identityChecks   int
	boundDeletes     int
	unboundedDeletes int
}

type materializationUnsupportedTestDriver struct {
	drivers.Driver
}

func (d *materializationIdentityTestDriver) GetVolumeIdentity(_ drivers.Volume) (string, error) {
	return d.identity, nil
}

func (d *materializationIdentityTestDriver) GetVolumeMaterializationOwnership(_ drivers.Volume) (string, error) {
	return d.ownership, nil
}

func (d *materializationIdentityTestDriver) SetVolumeMaterializationOwnership(_ drivers.Volume, ownership string) error {
	d.ownership = ownership
	return nil
}

func (d *materializationIdentityTestDriver) DeleteVolume(_ drivers.Volume, _ *operations.Operation) error {
	d.unboundedDeletes++
	return nil
}

func (d *materializationIdentityTestDriver) DeleteVolumeWithIdentity(_ drivers.Volume, expectedStorageIdentity string, _ *operations.Operation) error {
	d.identityChecks++
	if expectedStorageIdentity == "" || expectedStorageIdentity != d.identity {
		return errors.New("storage identity mismatch")
	}

	d.boundDeletes++
	return nil
}

func materializationConfig() map[string]string {
	return map[string]string{
		internalInstance.ConfigOpenStackIDMapAllocationID:       "11111111-1111-4111-8111-111111111111",
		internalInstance.ConfigOpenStackComputeID:               "22222222-2222-4222-8222-222222222222",
		internalInstance.ConfigOpenStackRootfsMaterializationID: "33333333-3333-4333-8333-333333333333",
		"user.openstack.uuid":                                   "44444444-4444-4444-8444-444444444444",
		"security.idmap.base":                                   "1000000",
		"security.idmap.size":                                   "65536",
	}
}

func TestMaterializationRequestProtocolTrigger(t *testing.T) {
	request := func(config map[string]string) *api.InstancesPost {
		return &api.InstancesPost{InstancePut: api.InstancePut{Config: config}, Source: api.InstanceSource{Type: "image"}}
	}

	require.NoError(t, validateStorageMaterializationRequest(request(nil)))
	require.NoError(t, validateStorageMaterializationRequest(request(map[string]string{
		"user.openstack.uuid": "44444444-4444-4444-8444-444444444444",
	})))
	require.NoError(t, validateStorageMaterializationRequest(request(map[string]string{
		"security.idmap.base": "1000000",
		"security.idmap.size": "65536",
	})))

	for name, config := range map[string]map[string]string{
		"allocation only": {
			internalInstance.ConfigOpenStackIDMapAllocationID: "11111111-1111-4111-8111-111111111111",
		},
		"compute only": {
			internalInstance.ConfigOpenStackComputeID: "22222222-2222-4222-8222-222222222222",
		},
		"materialization only": {
			internalInstance.ConfigOpenStackRootfsMaterializationID: "33333333-3333-4333-8333-333333333333",
		},
		"owner and idmap base only": {
			"user.openstack.uuid": "44444444-4444-4444-8444-444444444444",
			"security.idmap.base": "1000000",
		},
		"owner and complete fixed idmap without protocol identity": {
			"user.openstack.uuid": "44444444-4444-4444-8444-444444444444",
			"security.idmap.base": "1000000",
			"security.idmap.size": "65536",
		},
	} {
		t.Run(name, func(t *testing.T) {
			require.Error(t, validateStorageMaterializationRequest(request(config)))
		})
	}

	missingOwner := materializationConfig()
	delete(missingOwner, "user.openstack.uuid")
	require.Error(t, validateStorageMaterializationRequest(request(missingOwner)))
	missingToken := materializationConfig()
	delete(missingToken, internalInstance.ConfigOpenStackRootfsMaterializationID)
	require.Error(t, validateStorageMaterializationRequest(request(missingToken)))
	require.NoError(t, validateStorageMaterializationRequest(request(materializationConfig())))
}

func TestMaterializationRequestSourceAndCanonicalIdentity(t *testing.T) {
	for _, sourceType := range []string{"image", "none", "migration"} {
		req := api.InstancesPost{InstancePut: api.InstancePut{Config: materializationConfig()}, Source: api.InstanceSource{Type: sourceType}}
		require.NoError(t, validateStorageMaterializationRequest(&req))
	}

	for _, sourceType := range []string{"copy", "backup", ""} {
		req := api.InstancesPost{InstancePut: api.InstancePut{Config: materializationConfig()}, Source: api.InstanceSource{Type: sourceType}}
		require.Error(t, validateStorageMaterializationRequest(&req))
	}

	for _, token := range []string{
		"33333333-3333-4333-8333-333333333333",
		"33333333333343338333333333333333",
		"33333333-3333-4333-8333-33333333333A",
		"urn:uuid:33333333-3333-4333-8333-333333333333",
	} {
		config := materializationConfig()
		config[internalInstance.ConfigOpenStackRootfsMaterializationID] = token
		req := api.InstancesPost{InstancePut: api.InstancePut{Config: config}, Source: api.InstanceSource{Type: "image"}}
		if token == "33333333-3333-4333-8333-333333333333" {
			require.NoError(t, validateStorageMaterializationRequest(&req))
		} else {
			require.Error(t, validateStorageMaterializationRequest(&req))
		}
	}
}

func TestMaterializationHandoverRequestRequiresOrdinaryCeph(t *testing.T) {
	base := int64(1000000)
	size := int64(65536)
	req := api.StorageMaterializationAttemptPut{
		AllocationID: "11111111-1111-4111-8111-111111111111",
		ComputeID:    "22222222-2222-4222-8222-222222222222",
		Owner:        "44444444-4444-4444-8444-444444444444",
		InstanceName: "instance-00000001", IDMapBase: &base, IDMapSize: &size,
		StorageDriver: "ceph", StoragePool: "rootfs", StorageVolume: "nova_instance-00000001",
		CleanupDisposition: storagematerializationattempt.CleanupHandover,
	}

	_, err := storageMaterializationAttemptFromPut("33333333-3333-4333-8333-333333333333", "nova", req)
	require.NoError(t, err)

	req.StorageDriver = "cephext"
	_, err = storageMaterializationAttemptFromPut("33333333-3333-4333-8333-333333333333", "nova", req)
	require.Error(t, err)

	req.StorageDriver = "zfs"
	_, err = storageMaterializationAttemptFromPut("33333333-3333-4333-8333-333333333333", "nova", req)
	require.Error(t, err)
}

func TestMaterializationDestructiveCleanupCapabilitiesFailClosed(t *testing.T) {
	unsupported := &materializationUnsupportedTestDriver{}
	for _, disposition := range []string{
		storagematerializationattempt.CleanupDelete,
		storagematerializationattempt.CleanupHandover,
	} {
		require.Error(t, validateStorageMaterializationCleanupCapabilities(unsupported, disposition))
	}

	require.NoError(t, validateStorageMaterializationCleanupCapabilities(unsupported, storagematerializationattempt.CleanupDetach))

	supported := &materializationIdentityTestDriver{}
	require.NoError(t, validateStorageMaterializationCleanupCapabilities(supported, storagematerializationattempt.CleanupDelete))
	require.NoError(t, validateStorageMaterializationCleanupCapabilities(supported, storagematerializationattempt.CleanupHandover))
}

func TestMaterializationHandoverCommitRequiresCompletedProtectedTarget(t *testing.T) {
	attempt := &db.StorageMaterializationAttempt{CleanupDisposition: storagematerializationattempt.CleanupHandover}
	config := map[string]string{}
	require.Error(t, validateStorageMaterializationHandoverTarget(attempt, config))

	config[internalInstance.ConfigVolatileMigrationStorageHandoverRole] = internalInstance.StorageHandoverRoleTarget
	require.Error(t, validateStorageMaterializationHandoverTarget(attempt, config))

	config[internalInstance.ConfigVolatileMigrationStorageReceiveComplete] = "true"
	require.Error(t, validateStorageMaterializationHandoverTarget(attempt, config))

	config[internalInstance.ConfigVolatileMigrationStorageDeleteProtection] = "true"
	require.NoError(t, validateStorageMaterializationHandoverTarget(attempt, config))

	attempt.CleanupDisposition = storagematerializationattempt.CleanupDelete
	require.NoError(t, validateStorageMaterializationHandoverTarget(attempt, nil))
}

func TestMaterializationSettledRequiresOperationBoundary(t *testing.T) {
	now := time.Now()
	s := &state.State{StartTime: now}
	attempt := &db.StorageMaterializationAttempt{Started: true, DaemonStart: now.UnixNano()}
	require.Error(t, storageMaterializationAttemptOperationFinished(s, attempt))

	attempt.DaemonStart = now.Add(-time.Minute).UnixNano()
	require.NoError(t, storageMaterializationAttemptOperationFinished(s, attempt))

	op, err := operations.OperationCreate(nil, "nova", operations.OperationClassTask, operationtype.InstanceCreate, nil, nil, func(*operations.Operation) error { return nil }, nil, nil, nil)
	require.NoError(t, err)
	attempt.DaemonStart = now.UnixNano()
	attempt.OperationUUID = op.ID()
	require.Error(t, storageMaterializationAttemptOperationFinished(s, attempt))
	require.NoError(t, op.Start())
	require.NoError(t, op.Wait(context.Background()))
	require.NoError(t, storageMaterializationAttemptOperationFinished(s, attempt))
}

func TestMaterializationInternalSetupFailureReconcileBoundary(t *testing.T) {
	now := time.Now()
	s := &state.State{StartTime: now}
	attempt := &db.StorageMaterializationAttempt{Started: true, DaemonStart: now.UnixNano()}

	require.Error(t, validateStorageMaterializationReconcileBoundary(s, attempt, false))
	for _, failurePoint := range []string{"CreateInternal", "newMigrationSink", "OperationCreate"} {
		t.Run(failurePoint, func(t *testing.T) {
			require.NoError(t, validateStorageMaterializationReconcileBoundary(s, attempt, true))
		})
	}

	attempt.DaemonStart = now.Add(-time.Minute).UnixNano()
	require.NoError(t, validateStorageMaterializationReconcileBoundary(s, attempt, false))
	require.Error(t, validateStorageMaterializationReconcileBoundary(s, attempt, true))

	attempt.DaemonStart = now.UnixNano()
	attempt.OperationUUID = "operation-bound"
	require.NoError(t, validateStorageMaterializationReconcileBoundary(s, attempt, false))
	require.Error(t, validateStorageMaterializationReconcileBoundary(s, attempt, true))
}

func TestMaterializationProofIsPersistedNotRecomputedFromState(t *testing.T) {
	attempt := &db.StorageMaterializationAttempt{
		Token: "33333333-3333-4333-8333-333333333333", State: storagematerializationattempt.StateRetired,
		ProofOutcome: storagematerializationattempt.ProofReconciledClean, ProofDigest: "sha256:durable",
	}

	result := storageMaterializationAttemptToAPI(attempt)
	require.NotNil(t, result.Proof)
	require.Equal(t, storagematerializationattempt.ProofReconciledClean, result.Proof.Outcome)
	require.Equal(t, "sha256:durable", result.Proof.Digest)
}

func TestMaterializationProofAcknowledgementRequiresExactGeneration(t *testing.T) {
	attempt := &db.StorageMaterializationAttempt{
		Token: "33333333-3333-4333-8333-333333333333", AllocationID: "11111111-1111-4111-8111-111111111111",
		ComputeID: "22222222-2222-4222-8222-222222222222", Owner: "44444444-4444-4444-8444-444444444444",
		InstanceName: "instance-00000001", IDMapBase: 1000000, IDMapSize: 65536,
		ProofOutcome: storagematerializationattempt.ProofReconciledClean, ProofDigest: "sha256:durable",
	}

	query := url.Values{
		"proof-digest": {attempt.ProofDigest}, "allocation-id": {attempt.AllocationID}, "compute-id": {attempt.ComputeID},
		"owner": {attempt.Owner}, "instance": {attempt.InstanceName}, "idmap-base": {strconv.FormatInt(attempt.IDMapBase, 10)},
		"idmap-size": {strconv.FormatInt(attempt.IDMapSize, 10)},
	}

	req := httptest.NewRequest("DELETE", "/1.0/storage-materialization-attempts/"+attempt.Token+"?"+query.Encode(), nil)
	require.NoError(t, validateStorageMaterializationAcknowledgement(req, attempt))

	query.Set("proof-digest", "sha256:another-generation")
	req = httptest.NewRequest("DELETE", "/1.0/storage-materialization-attempts/"+attempt.Token+"?"+query.Encode(), nil)
	require.ErrorIs(t, validateStorageMaterializationAcknowledgement(req, attempt), storagematerializationattempt.ErrBindingMismatch)

	query.Set("proof-digest", attempt.ProofDigest)
	query.Del("compute-id")
	req = httptest.NewRequest("DELETE", "/1.0/storage-materialization-attempts/"+attempt.Token+"?"+query.Encode(), nil)
	require.Error(t, validateStorageMaterializationAcknowledgement(req, attempt))
}

func TestMaterializationRetainedCleanupCanNeverDeleteBackend(t *testing.T) {
	for _, driver := range []string{"cephext", "ceph", "zfs"} {
		deletes, err := storageMaterializationCleanupDeletesBackend(&db.StorageMaterializationAttempt{
			StorageDriver: driver, CleanupDisposition: storagematerializationattempt.CleanupDetach,
		})
		require.NoError(t, err)
		require.False(t, deletes)
	}

	deletes, err := storageMaterializationCleanupDeletesBackend(&db.StorageMaterializationAttempt{
		StorageDriver: "ceph", CleanupDisposition: storagematerializationattempt.CleanupHandover,
	})
	require.NoError(t, err)
	require.False(t, deletes)
	require.True(t, storageMaterializationCleanupProtectsBackend(storagematerializationattempt.CleanupHandover))
	require.True(t, storageMaterializationCleanupProtectsBackend(storagematerializationattempt.CleanupDetach))
	require.False(t, storageMaterializationCleanupProtectsBackend(storagematerializationattempt.CleanupDelete))

	deletes, err = storageMaterializationCleanupDeletesBackend(&db.StorageMaterializationAttempt{
		StorageDriver: "ceph", CleanupDisposition: storagematerializationattempt.CleanupDelete,
	})
	require.NoError(t, err)
	require.True(t, deletes)

	_, err = storageMaterializationCleanupDeletesBackend(&db.StorageMaterializationAttempt{
		StorageDriver: "cephext", CleanupDisposition: storagematerializationattempt.CleanupDelete,
	})
	require.Error(t, err)
}

func TestMaterializationCleanupDeleteIsIdentityBound(t *testing.T) {
	vol := drivers.NewVolume(nil, "rootfs", drivers.VolumeTypeContainer, drivers.ContentTypeFS, "nova_instance-00000001", nil, nil)
	for _, path := range []string{"volume DB claim cleanup", "backend-only cleanup"} {
		t.Run(path, func(t *testing.T) {
			driver := &materializationIdentityTestDriver{identity: "rbd_data.second-generation"}
			err := deleteStorageMaterializationVolumeWithIdentity(driver, vol, "rbd_data.first-generation")
			require.Error(t, err)
			require.Equal(t, 1, driver.identityChecks)
			require.Zero(t, driver.boundDeletes)
			require.Zero(t, driver.unboundedDeletes)
		})
	}

	driver := &materializationIdentityTestDriver{identity: "rbd_data.first-generation"}
	require.NoError(t, deleteStorageMaterializationVolumeWithIdentity(driver, vol, driver.identity))
	require.Equal(t, 1, driver.identityChecks)
	require.Equal(t, 1, driver.boundDeletes)
	require.Zero(t, driver.unboundedDeletes)

	require.Error(t, deleteStorageMaterializationVolumeWithIdentity(driver, vol, ""))
	require.Equal(t, 1, driver.identityChecks)
}

func TestCleanupBindsIdentityOnlyForCleanDeleteBaseline(t *testing.T) {
	node, cleanup := db.NewTestNode(t)
	defer cleanup()

	ctx := context.Background()
	manager := storagematerializationattempt.New(node)
	attempt := db.StorageMaterializationAttempt{
		Token: "55555555-5555-4555-8555-555555555555", AllocationID: "11111111-1111-4111-8111-111111111111",
		ComputeID: "22222222-2222-4222-8222-222222222222", Owner: "44444444-4444-4444-8444-444444444444",
		Project: "nova", InstanceName: "instance-00000001", IDMapBase: 1000000, IDMapSize: 65536,
		StorageDriver: "ceph", StoragePool: "rootfs", StorageVolume: "nova_instance-00000001",
		BaselineClean: true, CleanupDisposition: storagematerializationattempt.CleanupDelete,
	}

	_, err := manager.Register(ctx, attempt)
	require.NoError(t, err)
	_, err = manager.Start(ctx, attempt.Token, attempt, 42, "")
	require.NoError(t, err)
	attemptPtr, err := manager.Abort(ctx, attempt.Token)
	require.NoError(t, err)

	driver := &materializationIdentityTestDriver{identity: "rbd_data.new-generation"}
	driver.ownership, err = storagematerializationattempt.OwnershipMarker(attemptPtr, driver.identity)
	require.NoError(t, err)
	attemptPtr, err = bindMaterializationCleanupIdentity(ctx, manager, attemptPtr, driver, drivers.Volume{}, true)
	require.NoError(t, err)
	require.Equal(t, "rbd_data.new-generation", attemptPtr.StorageIdentity)
	require.Equal(t, storagematerializationattempt.PhaseMaterialized, attemptPtr.StoragePhase)

	unproven := *attemptPtr
	unproven.StorageIdentity = ""
	unproven.BaselineClean = false
	unproven.StoragePhase = storagematerializationattempt.PhasePending
	_, err = bindMaterializationCleanupIdentity(ctx, nil, &unproven, driver, drivers.Volume{}, true)
	require.Error(t, err)

	second := attempt
	second.Token = "66666666-6666-4666-8666-666666666666"
	second.AllocationID = "77777777-7777-4777-8777-777777777777"
	second.Owner = "88888888-8888-4888-8888-888888888888"
	second.InstanceName = "instance-00000002"
	second.StorageVolume = "nova_instance-00000002"
	_, err = manager.Register(ctx, second)
	require.NoError(t, err)
	_, err = manager.Start(ctx, second.Token, second, 43, "")
	require.NoError(t, err)
	secondPtr, err := manager.Abort(ctx, second.Token)
	require.NoError(t, err)
	_, err = bindMaterializationCleanupIdentity(ctx, manager, secondPtr, driver, drivers.Volume{}, true)
	require.ErrorContains(t, err, "exact durable ownership evidence")
}

// A create can die between making the image and stamping it. The attempt
// registered with a clean baseline, so nothing else could have made the
// volume now sitting under its name, and refusing to adopt it strands both
// the volume and its ID map claim with no way to ever release them.
func TestCleanupAdoptsAnUnstampedVolumeFromItsOwnAttempt(t *testing.T) {
	node, cleanup := db.NewTestNode(t)
	defer cleanup()

	ctx := context.Background()
	manager := storagematerializationattempt.New(node)
	attempt := db.StorageMaterializationAttempt{
		Token: "99999999-9999-4999-8999-999999999999", AllocationID: "11111111-1111-4111-8111-111111111111",
		ComputeID: "22222222-2222-4222-8222-222222222222", Owner: "44444444-4444-4444-8444-444444444444",
		Project: "nova", InstanceName: "instance-00000003", IDMapBase: 1000000, IDMapSize: 65536,
		StorageDriver: "ceph", StoragePool: "rootfs", StorageVolume: "nova_instance-00000003",
		BaselineClean: true, CleanupDisposition: storagematerializationattempt.CleanupDelete,
	}

	_, err := manager.Register(ctx, attempt)
	require.NoError(t, err)
	_, err = manager.Start(ctx, attempt.Token, attempt, 44, "")
	require.NoError(t, err)
	attemptPtr, err := manager.Abort(ctx, attempt.Token)
	require.NoError(t, err)

	// The volume exists but carries no ownership marker at all.
	driver := &materializationIdentityTestDriver{identity: "rbd_data.unstamped", ownership: ""}

	attemptPtr, err = bindMaterializationCleanupIdentity(ctx, manager, attemptPtr, driver, drivers.Volume{}, true)
	require.NoError(t, err)
	require.Equal(t, "rbd_data.unstamped", attemptPtr.StorageIdentity)
	require.Equal(t, storagematerializationattempt.PhaseMaterialized, attemptPtr.StoragePhase)

	// Adoption stamps the volume so a later pass sees exact ownership.
	expected, err := storagematerializationattempt.OwnershipMarker(attemptPtr, driver.identity)
	require.NoError(t, err)
	require.Equal(t, expected, driver.ownership)
}

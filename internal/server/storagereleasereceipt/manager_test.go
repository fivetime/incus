//go:build linux && cgo && !agent

package storagereleasereceipt

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus/v7/internal/server/db"
	"github.com/lxc/incus/v7/internal/server/storagematerializationattempt"
)

func testReceipt() db.StorageReleaseReceipt {
	return db.StorageReleaseReceipt{
		Token:              "06ada2e7-67f9-4c4e-b071-da45f25cfc67",
		Owner:              "11111111-1111-1111-1111-111111111111",
		AllocationID:       "22222222-2222-2222-2222-222222222222",
		ComputeID:          "33333333-3333-3333-3333-333333333333",
		MaterializationID:  "06ada2e7-67f9-4c4e-b071-da45f25cfc67",
		Project:            "nova",
		InstanceName:       "instance-00000001",
		IDMapBase:          1000000,
		IDMapSize:          65536,
		StorageDriver:      "cephext",
		StoragePool:        "cinder-bfv",
		StorageVolume:      "nova_instance-00000001",
		RBDImage:           "volume-11111111-1111-1111-1111-111111111111",
		StorageIdentity:    "rbd_data.1234567890abcdef",
		BaselineClean:      true,
		CleanupDisposition: storagematerializationattempt.CleanupDetach,
		Outcome:            OutcomeNormalized,
	}
}

func TestStorageReleaseReceiptLifecycle(t *testing.T) {
	node, cleanup := db.NewTestNode(t)
	defer cleanup()

	manager := New(node)
	expected := testReceipt()

	pending, err := manager.Begin(context.Background(), expected)
	require.NoError(t, err)
	require.Equal(t, StatePending, pending.State)
	require.NotZero(t, pending.CreatedAt)
	require.Zero(t, pending.CompletedAt)

	repeated, err := manager.Begin(context.Background(), expected)
	require.NoError(t, err)
	require.Equal(t, pending, repeated)

	complete, err := manager.Complete(context.Background(), expected)
	require.NoError(t, err)
	require.Equal(t, StateComplete, complete.State)
	require.NotZero(t, complete.CompletedAt)

	repeated, err = manager.Complete(context.Background(), expected)
	require.NoError(t, err)
	require.Equal(t, complete, repeated)
}

func TestStorageReleaseReceiptRejectsTokenRebinding(t *testing.T) {
	node, cleanup := db.NewTestNode(t)
	defer cleanup()

	manager := New(node)
	expected := testReceipt()
	_, err := manager.Begin(context.Background(), expected)
	require.NoError(t, err)

	rebound := expected
	rebound.RBDImage = "volume-22222222-2222-2222-2222-222222222222"
	_, err = manager.Begin(context.Background(), rebound)
	require.ErrorIs(t, err, ErrBindingMismatch)
	_, err = manager.Complete(context.Background(), rebound)
	require.ErrorIs(t, err, ErrBindingMismatch)
	pending, err := manager.Get(context.Background(), expected.Token)
	require.NoError(t, err)
	require.Equal(t, StatePending, pending.State)

	rebound = expected
	rebound.StorageIdentity = "rbd_data.recreated"
	_, err = manager.Complete(context.Background(), rebound)
	require.ErrorIs(t, err, ErrBindingMismatch)
	pending, err = manager.Get(context.Background(), expected.Token)
	require.NoError(t, err)
	require.Equal(t, StatePending, pending.State)

	rebound = expected
	rebound.MaterializationID = "44444444-4444-4444-4444-444444444444"
	_, err = manager.Begin(context.Background(), rebound)
	require.Error(t, err)

	rebound = expected
	rebound.BaselineClean = false
	_, err = manager.Begin(context.Background(), rebound)
	require.Error(t, err)

	rebound = expected
	rebound.CleanupDisposition = storagematerializationattempt.CleanupDelete
	_, err = manager.Begin(context.Background(), rebound)
	require.Error(t, err)
}

func TestStorageReleaseReceiptDetachedLifecycle(t *testing.T) {
	node, cleanup := db.NewTestNode(t)
	defer cleanup()

	manager := New(node)
	expected := testReceipt()
	expected.Outcome = OutcomeDetached

	pending, err := manager.Begin(context.Background(), expected)
	require.NoError(t, err)
	require.Equal(t, OutcomeDetached, pending.Outcome)
	require.Equal(t, StatePending, pending.State)

	complete, err := manager.Complete(context.Background(), expected)
	require.NoError(t, err)
	require.Equal(t, StateComplete, complete.State)
}

func TestStorageReleaseReceiptSourceDeleteDispositionCanDetach(t *testing.T) {
	node, cleanup := db.NewTestNode(t)
	defer cleanup()

	manager := New(node)
	expected := testReceipt()
	expected.StorageDriver = "ceph"
	expected.StoragePool = "ceph-rootfs"
	expected.RBDImage = ""
	expected.CleanupDisposition = storagematerializationattempt.CleanupDelete
	expected.Outcome = OutcomeDetached

	pending, err := manager.Begin(context.Background(), expected)
	require.NoError(t, err)
	require.Equal(t, storagematerializationattempt.CleanupDelete, pending.CleanupDisposition)
	require.Equal(t, OutcomeDetached, pending.Outcome)

	complete, err := manager.Complete(context.Background(), expected)
	require.NoError(t, err)
	require.Equal(t, StateComplete, complete.State)
	require.Equal(t, storagematerializationattempt.CleanupDelete, complete.CleanupDisposition)
}

func TestStorageReleaseReceiptOutcomeMatrix(t *testing.T) {
	tests := []struct {
		name        string
		driver      string
		disposition string
		outcome     string
		valid       bool
	}{
		{name: "external normalize", driver: "cephext", disposition: storagematerializationattempt.CleanupDetach, outcome: OutcomeNormalized, valid: true},
		{name: "external migration detach", driver: "cephext", disposition: storagematerializationattempt.CleanupDetach, outcome: OutcomeDetached, valid: true},
		{name: "external cannot delete", driver: "cephext", disposition: storagematerializationattempt.CleanupDetach, outcome: OutcomeDeleted},
		{name: "ceph spawn delete", driver: "ceph", disposition: storagematerializationattempt.CleanupDelete, outcome: OutcomeDeleted, valid: true},
		{name: "ceph source migration detach", driver: "ceph", disposition: storagematerializationattempt.CleanupDelete, outcome: OutcomeDetached, valid: true},
		{name: "ceph delete cannot normalize", driver: "ceph", disposition: storagematerializationattempt.CleanupDelete, outcome: OutcomeNormalized},
		{name: "ceph handover delete", driver: "ceph", disposition: storagematerializationattempt.CleanupHandover, outcome: OutcomeDeleted, valid: true},
		{name: "ceph handover migration detach", driver: "ceph", disposition: storagematerializationattempt.CleanupHandover, outcome: OutcomeDetached, valid: true},
		{name: "ceph handover cannot normalize", driver: "ceph", disposition: storagematerializationattempt.CleanupHandover, outcome: OutcomeNormalized},
		{name: "ceph retained detach", driver: "ceph", disposition: storagematerializationattempt.CleanupDetach, outcome: OutcomeDetached, valid: true},
		{name: "ceph retained cannot delete", driver: "ceph", disposition: storagematerializationattempt.CleanupDetach, outcome: OutcomeDeleted},
		{name: "ceph retained cannot normalize", driver: "ceph", disposition: storagematerializationattempt.CleanupDetach, outcome: OutcomeNormalized},
		{name: "local delete", driver: "zfs", disposition: storagematerializationattempt.CleanupDelete, outcome: OutcomeDeleted, valid: true},
		{name: "local cannot detach", driver: "zfs", disposition: storagematerializationattempt.CleanupDelete, outcome: OutcomeDetached},
		{name: "local cannot normalize", driver: "zfs", disposition: storagematerializationattempt.CleanupDelete, outcome: OutcomeNormalized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			receipt := testReceipt()
			receipt.StorageDriver = tt.driver
			receipt.StoragePool = tt.driver + "-pool"
			receipt.CleanupDisposition = tt.disposition
			receipt.Outcome = tt.outcome

			err := validateExpected(receipt)
			if tt.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestStorageReleaseReceiptHandoverFinalDeleteLifecycle(t *testing.T) {
	node, cleanup := db.NewTestNode(t)
	defer cleanup()

	ctx := context.Background()
	manager := New(node)
	expected := testReceipt()
	expected.StorageDriver = "ceph"
	expected.StoragePool = "ceph-rootfs"
	expected.RBDImage = ""
	expected.CleanupDisposition = storagematerializationattempt.CleanupHandover
	expected.Outcome = OutcomeDeleted

	_, err := manager.Begin(ctx, expected)
	require.NoError(t, err)
	_, err = manager.Complete(ctx, expected)
	require.NoError(t, err)

	attempt := materializationForReceipt(expected)
	materializationManager := storagematerializationattempt.New(node)
	_, err = materializationManager.Register(ctx, attempt)
	require.NoError(t, err)
	_, err = materializationManager.Start(ctx, attempt.Token, attempt, 42, "operation-handover")
	require.NoError(t, err)
	require.NoError(t, materializationManager.SetStoragePhase(ctx, attempt.Token, storagematerializationattempt.PhaseMaterialized, attempt.StorageIdentity))
	require.ErrorIs(t, materializationManager.Commit(ctx, attempt.Token), storagematerializationattempt.ErrHandoverRequiresMigrationCommit)

	migrationToken := "55555555-5555-4555-8555-555555555555"
	err = node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		return tx.CreateMigrationAttempt(ctx, db.MigrationAttempt{
			Token: migrationToken, Project: attempt.Project, ResourceType: "instance", ResourceName: attempt.InstanceName,
			State: storagematerializationattempt.StateActive, Started: true, OperationUUID: "operation-handover",
		})
	})
	require.NoError(t, err)
	require.NoError(t, materializationManager.CommitWithMigration(ctx, migrationToken, attempt.Token))
	require.NoError(t, manager.Delete(ctx, expected))

	receipt, err := manager.Get(ctx, expected.Token)
	require.NoError(t, err)
	require.Equal(t, StateRetired, receipt.State)
	retiredAttempt, err := materializationManager.Get(ctx, attempt.Token)
	require.NoError(t, err)
	require.Equal(t, storagematerializationattempt.StateRetired, retiredAttempt.State)
}

func TestStorageReleaseReceiptCannotCompleteWithoutPending(t *testing.T) {
	node, cleanup := db.NewTestNode(t)
	defer cleanup()

	_, err := New(node).Complete(context.Background(), testReceipt())
	require.ErrorIs(t, err, ErrNotFound)
}

func TestStorageReleaseReceiptDeleteRequiresCompleteExactBinding(t *testing.T) {
	node, cleanup := db.NewTestNode(t)
	defer cleanup()

	manager := New(node)
	expected := testReceipt()
	_, err := manager.Begin(context.Background(), expected)
	require.NoError(t, err)

	err = manager.Delete(context.Background(), expected)
	require.ErrorIs(t, err, ErrBindingMismatch)

	_, err = manager.Complete(context.Background(), expected)
	require.NoError(t, err)
	commitReceiptMaterialization(t, node, expected)

	rebound := expected
	rebound.Owner = "22222222-2222-2222-2222-222222222222"
	err = manager.Delete(context.Background(), rebound)
	require.ErrorIs(t, err, ErrBindingMismatch)

	err = manager.Delete(context.Background(), expected)
	require.NoError(t, err)

	retired, err := manager.Get(context.Background(), expected.Token)
	require.NoError(t, err)
	require.Equal(t, StateRetired, retired.State)

	err = manager.Delete(context.Background(), expected)
	require.NoError(t, err)

	_, err = manager.Begin(context.Background(), expected)
	require.ErrorIs(t, err, ErrBindingMismatch)

	rebound = expected
	rebound.AllocationID = "33333333-3333-3333-3333-333333333333"
	_, err = manager.Begin(context.Background(), rebound)
	require.ErrorIs(t, err, ErrBindingMismatch)
}

func TestStorageReleaseReceiptDeleteRequiresMatchingCommittedMaterialization(t *testing.T) {
	node, cleanup := db.NewTestNode(t)
	defer cleanup()

	manager := New(node)
	expected := testReceipt()
	_, err := manager.Begin(context.Background(), expected)
	require.NoError(t, err)
	_, err = manager.Complete(context.Background(), expected)
	require.NoError(t, err)

	err = manager.Delete(context.Background(), expected)
	require.ErrorIs(t, err, ErrBindingMismatch)

	attempt := materializationForReceipt(expected)
	attempt.StorageIdentity = "rbd_data.another-generation"
	materializationManager := storagematerializationattempt.New(node)
	_, err = materializationManager.Register(context.Background(), attempt)
	require.NoError(t, err)
	_, err = materializationManager.Start(context.Background(), attempt.Token, attempt, 42, "operation-one")
	require.NoError(t, err)
	require.NoError(t, materializationManager.SetStoragePhase(context.Background(), attempt.Token, storagematerializationattempt.PhaseMaterialized, attempt.StorageIdentity))
	require.NoError(t, materializationManager.Commit(context.Background(), attempt.Token))

	err = manager.Delete(context.Background(), expected)
	require.ErrorIs(t, err, ErrBindingMismatch)
	receipt, err := manager.Get(context.Background(), expected.Token)
	require.NoError(t, err)
	require.Equal(t, StateComplete, receipt.State)
}

func TestStorageReleaseReceiptDeleteAtomicallyRetiresMaterialization(t *testing.T) {
	node, cleanup := db.NewTestNode(t)
	defer cleanup()

	manager := New(node)
	expected := testReceipt()
	_, err := manager.Begin(context.Background(), expected)
	require.NoError(t, err)
	_, err = manager.Complete(context.Background(), expected)
	require.NoError(t, err)
	commitReceiptMaterialization(t, node, expected)

	require.NoError(t, manager.Delete(context.Background(), expected))
	require.NoError(t, manager.Delete(context.Background(), expected))

	receipt, err := manager.Get(context.Background(), expected.Token)
	require.NoError(t, err)
	require.Equal(t, StateRetired, receipt.State)

	attempt, err := storagematerializationattempt.New(node).Get(context.Background(), expected.MaterializationID)
	require.NoError(t, err)
	require.Equal(t, storagematerializationattempt.StateRetired, attempt.State)
}

func commitReceiptMaterialization(t *testing.T, node *db.Node, receipt db.StorageReleaseReceipt) {
	t.Helper()
	attempt := materializationForReceipt(receipt)
	manager := storagematerializationattempt.New(node)
	_, err := manager.Register(context.Background(), attempt)
	require.NoError(t, err)
	_, err = manager.Start(context.Background(), attempt.Token, attempt, 42, "operation-one")
	require.NoError(t, err)
	require.NoError(t, manager.SetStoragePhase(context.Background(), attempt.Token, storagematerializationattempt.PhaseMaterialized, attempt.StorageIdentity))
	require.NoError(t, manager.Commit(context.Background(), attempt.Token))
}

func materializationForReceipt(receipt db.StorageReleaseReceipt) db.StorageMaterializationAttempt {
	return db.StorageMaterializationAttempt{
		Token:              receipt.MaterializationID,
		AllocationID:       receipt.AllocationID,
		ComputeID:          receipt.ComputeID,
		Owner:              receipt.Owner,
		Project:            receipt.Project,
		InstanceName:       receipt.InstanceName,
		IDMapBase:          receipt.IDMapBase,
		IDMapSize:          receipt.IDMapSize,
		StorageDriver:      receipt.StorageDriver,
		StoragePool:        receipt.StoragePool,
		StorageVolume:      receipt.StorageVolume,
		RBDImage:           receipt.RBDImage,
		StorageIdentity:    receipt.StorageIdentity,
		BaselineClean:      receipt.BaselineClean,
		CleanupDisposition: receipt.CleanupDisposition,
	}
}

//go:build linux && cgo && !agent

package storagematerializationattempt

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus/v7/internal/server/db"
)

func newTestManager(t *testing.T) (*Manager, func()) {
	t.Helper()
	node, cleanup := db.NewTestNode(t)
	return New(node), cleanup
}

func testBinding(token string) db.StorageMaterializationAttempt {
	return db.StorageMaterializationAttempt{
		Token: token, AllocationID: "11111111-1111-4111-8111-111111111111",
		ComputeID: "22222222-2222-4222-8222-222222222222", Owner: "33333333-3333-4333-8333-333333333333",
		Project: "nova", InstanceName: "instance-00000001", IDMapBase: 1000000, IDMapSize: 65536,
		StorageDriver: "ceph", StoragePool: "rootfs", StorageVolume: "nova_instance-00000001",
		BaselineClean: true, CleanupDisposition: CleanupDelete,
	}
}

func TestRegistrationIsIdempotentAndTokenBound(t *testing.T) {
	manager, cleanup := newTestManager(t)
	defer cleanup()
	ctx := context.Background()
	binding := testBinding("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")

	first, err := manager.Register(ctx, binding)
	require.NoError(t, err)
	second, err := manager.Register(ctx, binding)
	require.NoError(t, err)
	require.Equal(t, first, second)

	other := binding
	other.StorageVolume = "nova_other"
	_, err = manager.Register(ctx, other)
	require.ErrorIs(t, err, ErrBindingMismatch)
}

func TestOnlyOneNonRetiredAttemptCanFenceAnInstanceName(t *testing.T) {
	manager, cleanup := newTestManager(t)
	defer cleanup()
	ctx := context.Background()
	first := testBinding("abababab-abab-4bab-8bab-abababababab")
	_, err := manager.Register(ctx, first)
	require.NoError(t, err)

	second := first
	second.Token = "acacacac-acac-4cac-8cac-acacacacacac"
	_, err = manager.Register(ctx, second)
	require.Error(t, err)

	stored, err := manager.GetByInstance(ctx, first.Project, first.InstanceName)
	require.NoError(t, err)
	require.Equal(t, first.Token, stored.Token)
}

func TestStartIsAtomicAndCommitRequiresMaterialization(t *testing.T) {
	manager, cleanup := newTestManager(t)
	defer cleanup()
	ctx := context.Background()
	binding := testBinding("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	_, err := manager.Register(ctx, binding)
	require.NoError(t, err)

	attempt, err := manager.Start(ctx, binding.Token, binding, 42, "operation-one")
	require.NoError(t, err)
	require.True(t, attempt.Started)
	require.Equal(t, PhasePending, attempt.StoragePhase)
	require.Equal(t, "operation-one", attempt.OperationUUID)
	require.Equal(t, int64(42), attempt.DaemonStart)
	require.ErrorIs(t, manager.Commit(ctx, binding.Token), ErrNotMaterialized)

	require.NoError(t, manager.SetStoragePhase(ctx, binding.Token, PhaseMaterialized, "rbd-id-one"))
	require.NoError(t, manager.Commit(ctx, binding.Token))
	attempt, err = manager.Get(ctx, binding.Token)
	require.NoError(t, err)
	require.Equal(t, StateCommitted, attempt.State)
}

func TestStartBeforeOperationAndBindExactlyOnce(t *testing.T) {
	manager, cleanup := newTestManager(t)
	defer cleanup()
	ctx := context.Background()
	binding := testBinding("babababa-baba-4aba-8aba-babababababa")
	_, err := manager.Register(ctx, binding)
	require.NoError(t, err)

	attempt, err := manager.Start(ctx, binding.Token, binding, 42, "")
	require.NoError(t, err)
	require.True(t, attempt.Started)
	require.Empty(t, attempt.OperationUUID)
	require.Equal(t, PhasePending, attempt.StoragePhase)

	require.NoError(t, manager.BindOperation(ctx, binding.Token, 42, "operation-one"))
	require.NoError(t, manager.BindOperation(ctx, binding.Token, 42, "operation-one"))
	require.ErrorIs(t, manager.BindOperation(ctx, binding.Token, 42, "operation-two"), ErrBindingMismatch)

	attempt, err = manager.Get(ctx, binding.Token)
	require.NoError(t, err)
	require.Equal(t, "operation-one", attempt.OperationUUID)
}

func TestBindOperationRejectsAnotherDaemonGeneration(t *testing.T) {
	manager, cleanup := newTestManager(t)
	defer cleanup()
	ctx := context.Background()
	binding := testBinding("bcbababa-baba-4aba-8aba-babababababa")
	_, err := manager.Register(ctx, binding)
	require.NoError(t, err)
	_, err = manager.Start(ctx, binding.Token, binding, 42, "")
	require.NoError(t, err)

	require.Error(t, manager.BindOperation(ctx, binding.Token, 43, "operation-one"))
	attempt, err := manager.Get(ctx, binding.Token)
	require.NoError(t, err)
	require.Empty(t, attempt.OperationUUID)
}

func TestMigrationAndMaterializationCommitAreAtomic(t *testing.T) {
	manager, cleanup := newTestManager(t)
	defer cleanup()
	ctx := context.Background()
	binding := testBinding("bcbcbcbc-bcbc-4cbc-8cbc-bcbcbcbcbcbc")
	_, err := manager.Register(ctx, binding)
	require.NoError(t, err)
	_, err = manager.Start(ctx, binding.Token, binding, 42, "operation-atomic")
	require.NoError(t, err)

	migrationToken := "bdbdbdbd-bdbd-4dbd-8dbd-bdbdbdbdbdbd"
	err = manager.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		return tx.CreateMigrationAttempt(ctx, db.MigrationAttempt{
			Token: migrationToken, Project: binding.Project, ResourceType: "instance", ResourceName: binding.InstanceName,
			State: StateActive, Started: true, OperationUUID: "operation-atomic",
		})
	})
	require.NoError(t, err)

	// The materialization CAS fails, so the migration CAS in the same transaction must roll back.
	err = manager.CommitWithMigration(ctx, migrationToken, binding.Token)
	require.Error(t, err)
	err = manager.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		migration, err := tx.GetMigrationAttempt(ctx, migrationToken)
		require.NoError(t, err)
		require.Equal(t, StateActive, migration.State)
		require.False(t, migration.Finished)
		return nil
	})
	require.NoError(t, err)

	require.NoError(t, manager.SetStoragePhase(ctx, binding.Token, PhaseMaterialized, "rbd-id-atomic"))
	mismatchedMigrationToken := "bfbfbfbf-bfbf-4fbf-8fbf-bfbfbfbfbfbf"
	err = manager.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		return tx.CreateMigrationAttempt(ctx, db.MigrationAttempt{
			Token: mismatchedMigrationToken, Project: binding.Project, ResourceType: "instance", ResourceName: binding.InstanceName,
			State: StateActive, Started: true, OperationUUID: "operation-another-generation",
		})
	})
	require.NoError(t, err)
	require.ErrorIs(t, manager.CommitWithMigration(ctx, mismatchedMigrationToken, binding.Token), ErrBindingMismatch)

	require.NoError(t, manager.CommitWithMigration(ctx, migrationToken, binding.Token))
	err = manager.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		migration, err := tx.GetMigrationAttempt(ctx, migrationToken)
		require.NoError(t, err)
		require.Equal(t, StateCommitted, migration.State)
		require.True(t, migration.Finished)
		return nil
	})
	require.NoError(t, err)
	materialization, err := manager.Get(ctx, binding.Token)
	require.NoError(t, err)
	require.Equal(t, StateCommitted, materialization.State)
}

func TestHandoverRequiresAtomicMigrationCommit(t *testing.T) {
	manager, cleanup := newTestManager(t)
	defer cleanup()

	ctx := context.Background()
	binding := testBinding("c1c1c1c1-c1c1-41c1-81c1-c1c1c1c1c1c1")
	binding.CleanupDisposition = CleanupHandover
	binding.StorageIdentity = "rbd-id-handover"
	_, err := manager.Register(ctx, binding)
	require.NoError(t, err)
	_, err = manager.Start(ctx, binding.Token, binding, 42, "operation-handover")
	require.NoError(t, err)
	require.NoError(t, manager.SetStoragePhase(ctx, binding.Token, PhaseMaterialized, binding.StorageIdentity))
	require.ErrorIs(t, manager.Commit(ctx, binding.Token), ErrHandoverRequiresMigrationCommit)

	migrationToken := "c2c2c2c2-c2c2-42c2-82c2-c2c2c2c2c2c2"
	err = manager.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		return tx.CreateMigrationAttempt(ctx, db.MigrationAttempt{
			Token: migrationToken, Project: binding.Project, ResourceType: "instance", ResourceName: binding.InstanceName,
			State: StateActive, Started: true, OperationUUID: "operation-handover",
		})
	})
	require.NoError(t, err)
	require.NoError(t, manager.CommitWithMigration(ctx, migrationToken, binding.Token))

	materialization, err := manager.Get(ctx, binding.Token)
	require.NoError(t, err)
	require.Equal(t, StateCommitted, materialization.State)
	err = manager.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		migration, err := tx.GetMigrationAttempt(ctx, migrationToken)
		require.NoError(t, err)
		require.Equal(t, StateCommitted, migration.State)
		return nil
	})
	require.NoError(t, err)
}

func TestCleanupDispositionIsImmutableAndChangesProof(t *testing.T) {
	deleteBinding := testBinding("bebebebe-bebe-4ebe-8ebe-bebebebebebe")
	detachBinding := deleteBinding
	detachBinding.CleanupDisposition = CleanupDetach
	detachBinding.StorageIdentity = "rbd-id-detached"

	deleteDigest, err := ProofDigest(&deleteBinding, ProofNotMaterialized)
	require.NoError(t, err)
	detachDigest, err := ProofDigest(&detachBinding, ProofNotMaterialized)
	require.NoError(t, err)
	require.NotEqual(t, deleteDigest, detachDigest)
	require.False(t, SameBinding(&deleteBinding, &detachBinding))

	detachBinding.StorageDriver = "cephext"
	require.NoError(t, validateBinding(&detachBinding))
	detachBinding.CleanupDisposition = CleanupDelete
	require.Error(t, validateBinding(&detachBinding))
}

func TestDetachedMaterializationRequiresCephIdentity(t *testing.T) {
	binding := testBinding("dededede-dede-4ede-8ede-dededededede")
	binding.CleanupDisposition = CleanupDetach
	require.Error(t, validateBinding(&binding))

	binding.StorageIdentity = "rbd-id-one"
	binding.StorageDriver = "zfs"
	require.Error(t, validateBinding(&binding))

	binding.StorageDriver = "ceph"
	require.NoError(t, validateBinding(&binding))
}

func TestHandoverMaterializationRequiresOrdinaryCephIdentity(t *testing.T) {
	binding := testBinding("d1d1d1d1-d1d1-41d1-81d1-d1d1d1d1d1d1")
	binding.CleanupDisposition = CleanupHandover
	require.Error(t, validateBinding(&binding))

	binding.StorageIdentity = "rbd-id-handover"
	binding.StorageDriver = "cephext"
	require.Error(t, validateBinding(&binding))

	binding.StorageDriver = "zfs"
	require.Error(t, validateBinding(&binding))

	binding.StorageDriver = "ceph"
	require.NoError(t, validateBinding(&binding))
}

func TestStorageIdentityIsPartOfMaterializationBinding(t *testing.T) {
	first := testBinding("dfdfdfdf-dfdf-4fdf-8fdf-dfdfdfdfdfdf")
	first.StorageIdentity = "rbd-id-one"
	second := first
	second.StorageIdentity = "rbd-id-two"
	require.False(t, SameBinding(&first, &second))
}

func TestPhaseAndIdentityAreMonotonic(t *testing.T) {
	manager, cleanup := newTestManager(t)
	defer cleanup()
	ctx := context.Background()
	binding := testBinding("cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	_, err := manager.Register(ctx, binding)
	require.NoError(t, err)
	_, err = manager.Start(ctx, binding.Token, binding, 1, "operation-one")
	require.NoError(t, err)

	require.NoError(t, manager.SetStoragePhase(ctx, binding.Token, PhaseMaterialized, "rbd-id-one"))
	require.Error(t, manager.SetStoragePhase(ctx, binding.Token, PhasePending, ""))
	require.Error(t, manager.SetStoragePhase(ctx, binding.Token, PhaseMaterialized, "rbd-id-two"))
	require.NoError(t, manager.SetStoragePhase(ctx, binding.Token, PhaseMaterialized, "rbd-id-one"))
}

func TestAbortBeforeStartFencesLatePostAndProofSurvivesRetire(t *testing.T) {
	manager, cleanup := newTestManager(t)
	defer cleanup()
	ctx := context.Background()
	binding := testBinding("dddddddd-dddd-4ddd-8ddd-dddddddddddd")
	_, err := manager.Register(ctx, binding)
	require.NoError(t, err)

	aborted, err := manager.Abort(ctx, binding.Token)
	require.NoError(t, err)
	require.False(t, aborted.Started)
	require.True(t, aborted.Finished)
	require.Equal(t, ProofNotMaterialized, aborted.ProofOutcome)
	require.True(t, strings.HasPrefix(aborted.ProofDigest, "sha256:"))
	digest := aborted.ProofDigest

	_, err = manager.Start(ctx, binding.Token, binding, 1, "late-operation")
	require.ErrorIs(t, err, ErrAborted)
	require.NoError(t, manager.Delete(ctx, binding.Token))
	require.NoError(t, manager.Delete(ctx, binding.Token))
	retired, err := manager.Get(ctx, binding.Token)
	require.NoError(t, err)
	require.Equal(t, StateRetired, retired.State)
	require.Equal(t, digest, retired.ProofDigest)
	_, err = manager.Register(ctx, binding)
	require.ErrorIs(t, err, ErrBindingMismatch)
}

func TestAbortStartRaceLinearizesCleanup(t *testing.T) {
	manager, cleanup := newTestManager(t)
	defer cleanup()
	ctx := context.Background()
	binding := testBinding("eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee")
	_, err := manager.Register(ctx, binding)
	require.NoError(t, err)

	start := make(chan struct{})
	startResult := make(chan error, 1)
	abortResult := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, err := manager.Start(ctx, binding.Token, binding, 1, "operation-race")
		startResult <- err
	}()
	go func() {
		defer wg.Done()
		<-start
		_, err := manager.Abort(ctx, binding.Token)
		abortResult <- err
	}()
	close(start)
	wg.Wait()

	startErr := <-startResult
	abortErr := <-abortResult
	require.NoError(t, abortErr)
	require.True(t, startErr == nil || errors.Is(startErr, ErrAborted), startErr)

	attempt, err := manager.Get(ctx, binding.Token)
	require.NoError(t, err)
	require.Equal(t, StateAborted, attempt.State)
	if startErr == nil {
		require.True(t, attempt.Started)
		require.False(t, attempt.Finished)
		require.Empty(t, attempt.ProofDigest)
	} else {
		require.False(t, attempt.Started)
		require.True(t, attempt.Finished)
		require.Equal(t, ProofNotMaterialized, attempt.ProofOutcome)
		require.NotEmpty(t, attempt.ProofDigest)
	}
}

func TestCleanProofAndRestartEnumeration(t *testing.T) {
	manager, cleanup := newTestManager(t)
	defer cleanup()
	ctx := context.Background()
	binding := testBinding("ffffffff-ffff-4fff-8fff-ffffffffffff")
	_, err := manager.Register(ctx, binding)
	require.NoError(t, err)
	_, err = manager.Start(ctx, binding.Token, binding, 7, "operation-restart")
	require.NoError(t, err)
	require.NoError(t, manager.SetStoragePhase(ctx, binding.Token, PhaseMaterialized, "rbd-id-one"))
	_, err = manager.Abort(ctx, binding.Token)
	require.NoError(t, err)

	unfinished, err := manager.ListUnfinished(ctx)
	require.NoError(t, err)
	require.Len(t, unfinished, 1)
	require.Equal(t, int64(7), unfinished[0].DaemonStart)

	require.NoError(t, manager.FinishClean(ctx, binding.Token))
	clean, err := manager.Get(ctx, binding.Token)
	require.NoError(t, err)
	require.Equal(t, StateClean, clean.State)
	require.Equal(t, ProofReconciledClean, clean.ProofOutcome)
	require.NotEmpty(t, clean.ProofDigest)
	unfinished, err = manager.ListUnfinished(ctx)
	require.NoError(t, err)
	require.Empty(t, unfinished)
}

func TestCanonicalUUIDAndIDMapBounds(t *testing.T) {
	manager, cleanup := newTestManager(t)
	defer cleanup()
	ctx := context.Background()

	for _, token := range []string{
		"AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA",
		"aaaaaaaaaaaa4aaa8aaaaaaaaaaaaaaa",
		"urn:uuid:aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
	} {
		binding := testBinding(token)
		_, err := manager.Register(ctx, binding)
		require.Error(t, err)
	}

	binding := testBinding("12345678-1234-4234-8234-123456789abc")
	binding.IDMapBase = 1 << 32
	_, err := manager.Register(ctx, binding)
	require.Error(t, err)
}

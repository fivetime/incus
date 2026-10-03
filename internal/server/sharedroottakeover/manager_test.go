//go:build linux && cgo && !agent

package sharedroottakeover

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus/v7/internal/server/db"
	"github.com/lxc/incus/v7/internal/server/storagematerializationattempt"
)

func testBinding() Binding {
	configuration := []byte(`{"instance":{"name":"instance-test"}}`)
	digest := sha256.Sum256(configuration)
	return Binding{
		Token:                   "11111111-1111-4111-8111-111111111111",
		Owner:                   "22222222-2222-4222-8222-222222222222",
		AllocationID:            "33333333-3333-4333-8333-333333333333",
		SourceComputeID:         "44444444-4444-4444-8444-444444444444",
		TargetComputeID:         "55555555-5555-4555-8555-555555555555",
		SourceMaterializationID: "66666666-6666-4666-8666-666666666666",
		Project:                 "nova", InstanceName: "instance-test", StoragePool: "rootfs",
		StorageVolume: "nova_instance-test", ClusterFSID: "77777777-7777-4777-8777-777777777777",
		StorageIdentity: `{"pool_id":9,"id":"abc","block_name_prefix":"rbd_data.abc"}`,
		SourceOwnership: "sha256:" + strings.Repeat("a", 64), SourceGeneration: 1,
		IDMapBase: 1000000, IDMapSize: 65536,
		FenceDigest:         "sha256:" + strings.Repeat("b", 64),
		GrantDigest:         "sha256:" + strings.Repeat("c", 64),
		ConfigurationDigest: "sha256:" + hex.EncodeToString(digest[:]), Configuration: configuration,
	}
}

func TestJournalKeepsPendingEffectsAcrossManagerRestart(t *testing.T) {
	node, cleanup := db.NewTestNode(t)
	defer cleanup()
	manager := New(node)
	ctx := context.Background()
	binding := testBinding()
	_, err := manager.Register(ctx, binding)
	require.NoError(t, err)
	_, err = manager.Advance(ctx, binding, Prepared, Claiming)
	require.NoError(t, err)
	require.NoError(t, manager.RecordFailure(ctx, binding.Token, errors.New("response lost after RBD transfer")))

	restarted := New(node)
	record, err := restarted.Register(ctx, binding)
	require.NoError(t, err)
	require.Equal(t, Claiming, record.Phase)
	require.Equal(t, "response lost after RBD transfer", record.LastError)
	_, err = restarted.Advance(ctx, binding, Claiming, Claimed)
	require.NoError(t, err)
	_, err = restarted.Advance(ctx, binding, Claiming, Claimed)
	require.NoError(t, err)
	_, err = restarted.Advance(ctx, binding, Claimed, Starting)
	require.ErrorIs(t, err, ErrPhaseConflict)
	_, err = restarted.Advance(ctx, binding, Committed, Starting)
	require.ErrorIs(t, err, ErrPhaseConflict)
}

func TestCommitAtomicallyCreatesNormalReleaseBinding(t *testing.T) {
	node, cleanup := db.NewTestNode(t)
	defer cleanup()
	manager := New(node)
	ctx := context.Background()
	binding := testBinding()
	_, err := manager.Register(ctx, binding)
	require.NoError(t, err)
	_, err = manager.Commit(ctx, binding)
	require.ErrorIs(t, err, ErrPhaseConflict)
	_, err = storagematerializationattempt.New(node).Get(ctx, binding.Token)
	require.ErrorIs(t, err, storagematerializationattempt.ErrNotFound)
	phases := []string{Prepared, Claiming, Claimed, Importing, Imported}
	for i := 0; i < len(phases)-1; i++ {
		_, err = manager.Advance(ctx, binding, phases[i], phases[i+1])
		require.NoError(t, err)
	}

	_, err = manager.Commit(ctx, binding)
	require.NoError(t, err)
	attempt, err := storagematerializationattempt.New(node).Get(ctx, binding.Token)
	require.NoError(t, err)
	require.Equal(t, binding.StorageIdentity, attempt.StorageIdentity)
	require.Equal(t, binding.TargetComputeID, attempt.ComputeID)
	require.Equal(t, binding.AllocationID, attempt.AllocationID)
	require.Equal(t, binding.IDMapBase, attempt.IDMapBase)
	require.Equal(t, storagematerializationattempt.StateCommitted, attempt.State)
	require.Equal(t, storagematerializationattempt.CleanupHandover, attempt.CleanupDisposition)
	_, err = New(node).Commit(ctx, binding)
	require.NoError(t, err)
	for _, transition := range [][2]string{{Committed, Starting}, {Starting, Started}, {Started, Completed}} {
		_, err = manager.Advance(ctx, binding, transition[0], transition[1])
		require.NoError(t, err)
	}

	require.Error(t, manager.Retire(ctx, binding.Token), "No receipt must never release original-root journal uniqueness")
	other := binding
	other.Token = binding.SourceMaterializationID
	_, err = manager.Register(ctx, other)
	require.Error(t, err)
}

func TestReplayCannotReplaceDiskTargetOrConfiguration(t *testing.T) {
	node, cleanup := db.NewTestNode(t)
	defer cleanup()
	manager := New(node)
	ctx := context.Background()
	binding := testBinding()
	_, err := manager.Register(ctx, binding)
	require.NoError(t, err)
	changes := []func(*Binding){
		func(b *Binding) { b.TargetComputeID = b.Owner },
		func(b *Binding) { b.StorageIdentity = `{"pool_id":9,"id":"def","block_name_prefix":"rbd_data.def"}` },
		func(b *Binding) { b.FenceDigest = b.ConfigurationDigest },
		func(b *Binding) { b.SourceGeneration++ },
		func(b *Binding) { b.SourceOwnership = b.FenceDigest },
	}
	for _, change := range changes {
		other := binding
		change(&other)
		_, err = manager.Register(ctx, other)
		require.ErrorIs(t, err, ErrBindingMismatch)
	}
}

func TestOneLocalTransactionPerOriginalImage(t *testing.T) {
	node, cleanup := db.NewTestNode(t)
	defer cleanup()
	manager := New(node)
	first := testBinding()
	second := first
	second.Token = "88888888-8888-4888-8888-888888888888"
	second.InstanceName = "different-instance"
	var wait sync.WaitGroup
	results := make(chan error, 2)
	for _, binding := range []Binding{first, second} {
		wait.Add(1)
		go func(binding Binding) {
			defer wait.Done()
			_, err := manager.Register(context.Background(), binding)
			results <- err
		}(binding)
	}

	wait.Wait()
	close(results)
	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
		}
	}

	require.Equal(t, 1, succeeded)
}

//go:build linux && cgo && !agent

package storage

import (
	"errors"
	"testing"

	internalInstance "github.com/lxc/incus/v7/internal/instance"
	"github.com/lxc/incus/v7/internal/server/db"
	"github.com/lxc/incus/v7/internal/server/storage/drivers"
	"github.com/lxc/incus/v7/internal/server/storagematerializationattempt"
	"github.com/lxc/incus/v7/internal/server/storagereleasereceipt"
	"github.com/lxc/incus/v7/shared/idmap"
)

type storageReleaseIdentityPresenceTestProvider struct {
	exists bool
	err    error
}

func (p *storageReleaseIdentityPresenceTestProvider) HasVolumeIdentity(drivers.Volume, string) (bool, error) {
	return p.exists, p.err
}

func TestRootfsIDMapMatchesReleaseGeneration(t *testing.T) {
	combined := &idmap.Set{Entries: []idmap.Entry{{
		IsUID: true, IsGID: true, HostID: 1000000, NSID: 0, MapRange: 65536,
	}}}
	if !rootfsIDMapMatchesReleaseGeneration(combined, 1000000, 65536) {
		t.Fatal("Combined fixed UID/GID map did not match")
	}

	split := &idmap.Set{Entries: []idmap.Entry{
		{IsUID: true, HostID: 1000000, NSID: 0, MapRange: 65536},
		{IsGID: true, HostID: 1000000, NSID: 0, MapRange: 65536},
	}}
	if !rootfsIDMapMatchesReleaseGeneration(split, 1000000, 65536) {
		t.Fatal("Split fixed UID/GID map did not match")
	}

	extra := &idmap.Set{Entries: append(split.Entries, idmap.Entry{
		IsUID: true, HostID: 2000000, NSID: 100000, MapRange: 65536,
	})}
	if rootfsIDMapMatchesReleaseGeneration(extra, 1000000, 65536) {
		t.Fatal("Map with an additional range matched a fixed generation")
	}
}

func TestRootfsIDMapEmptyMeansNeverApplied(t *testing.T) {
	// A container that never started has an empty volatile.last_state.idmap:
	// no shifted state exists at any generation, which the release path
	// accepts, while any actually-applied different map stays refused.
	if !rootfsIDMapEmpty(nil) {
		t.Fatal("nil current map must count as never applied")
	}

	if !rootfsIDMapEmpty(&idmap.Set{}) {
		t.Fatal("empty current map must count as never applied")
	}

	applied := &idmap.Set{Entries: []idmap.Entry{{
		IsUID: true, IsGID: true, HostID: 1000000, NSID: 0, MapRange: 65536,
	}}}
	if rootfsIDMapEmpty(applied) {
		t.Fatal("an applied map is not empty")
	}
	// The empty case must not be reachable through the match helper alone.
	if rootfsIDMapMatchesReleaseGeneration(&idmap.Set{}, 1000000, 65536) {
		t.Fatal("empty map must not match a generation outright")
	}
}

func TestCompletedStorageReleaseReceiptMatchesInstance(t *testing.T) {
	const (
		token      = "06ada2e7-67f9-4c4e-b071-da45f25cfc67"
		owner      = "11111111-1111-1111-1111-111111111111"
		allocation = "22222222-2222-2222-2222-222222222222"
		compute    = "33333333-3333-3333-3333-333333333333"
	)

	localConfig := map[string]string{
		internalInstance.ConfigVolatileRootfsIDMapReleaseToken:  token,
		internalInstance.ConfigVolatileRootfsIDMapReleaseOwner:  owner,
		internalInstance.ConfigVolatileRootfsIDMapAllocationID:  allocation,
		internalInstance.ConfigVolatileRootfsIDMapComputeID:     compute,
		internalInstance.ConfigOpenStackIDMapAllocationID:       allocation,
		internalInstance.ConfigOpenStackComputeID:               compute,
		internalInstance.ConfigOpenStackRootfsMaterializationID: token,
		"user.openstack.uuid":                                   owner,
	}

	receipt := &db.StorageReleaseReceipt{
		Token: token, Owner: owner, AllocationID: allocation, ComputeID: compute, MaterializationID: token,
		Project: "nova", InstanceName: "instance-00000001",
		StorageDriver: "cephext", StoragePool: "cinder-bfv",
		RBDImage:        "volume-11111111-1111-1111-1111-111111111111",
		StorageIdentity: "immutable", Outcome: storagereleasereceipt.OutcomeNormalized,
		BaselineClean: true, CleanupDisposition: storagematerializationattempt.CleanupDetach,
		State: storagereleasereceipt.StateComplete,
	}

	if !storageReleaseReceiptCanRecoverInstance(receipt, localConfig, "nova", "instance-00000001", "cinder-bfv") {
		t.Fatal("Complete receipt did not recover a post-VolumeDBDelete retry")
	}

	receipt.State = storagereleasereceipt.StatePending
	if storageReleaseReceiptCanRecoverInstance(receipt, localConfig, "nova", "instance-00000001", "cinder-bfv") {
		t.Fatal("Pending receipt reconstructed a deleted local claim")
	}

	receipt.Outcome = storagereleasereceipt.OutcomeDetached
	if !storageReleaseReceiptCanRecoverInstance(receipt, localConfig, "nova", "instance-00000001", "cinder-bfv") {
		t.Fatal("Pending detached receipt did not recover a post-VolumeDBDelete retry")
	}

	receipt.MaterializationID = "44444444-4444-4444-4444-444444444444"
	if storageReleaseReceiptCanRecoverInstance(receipt, localConfig, "nova", "instance-00000001", "cinder-bfv") {
		t.Fatal("Receipt from another materialization attempt recovered this instance")
	}
}

func TestDeletedCephStorageReleaseRequiresLocalStateProof(t *testing.T) {
	receipt := &db.StorageReleaseReceipt{
		StorageDriver: "ceph",
		Outcome:       storagereleasereceipt.OutcomeDeleted,
	}

	if !storageReleaseReceiptRequiresLocalStateProof(receipt) {
		t.Fatal("Deleted Ceph receipt could reach completion or volume DB deletion without a local mapping proof")
	}

	residualMapping := errors.New("RBD mapping remains")
	checks := 0
	err := validateRequiredStorageReleaseLocalState(receipt, func() error {
		checks++
		return residualMapping
	})
	if !errors.Is(err, residualMapping) || checks != 1 {
		t.Fatalf("Deleted Ceph receipt accepted residual local state: checks=%d err=%v", checks, err)
	}

	receipt.StorageDriver = "dir"
	if storageReleaseReceiptRequiresLocalStateProof(receipt) {
		t.Fatal("Non-block directory storage unexpectedly required a Ceph local mapping proof")
	}

	err = validateRequiredStorageReleaseLocalState(receipt, func() error {
		checks++
		return residualMapping
	})
	if err != nil || checks != 1 {
		t.Fatalf("Directory storage ran a Ceph local-state proof: checks=%d err=%v", checks, err)
	}
}

func TestPendingStorageReleaseRejectsSameNameRecreation(t *testing.T) {
	receipt := db.StorageReleaseReceipt{
		StorageIdentity: "rbd_data.first-generation",
		Outcome:         storagereleasereceipt.OutcomeDetached,
	}

	if err := validatePendingStorageReleaseBinding(true, "rbd_data.second-generation", receipt); err == nil {
		t.Fatal("Same-name replacement reached receipt completion or volume DB deletion")
	}

	if err := validatePendingStorageReleaseBinding(true, receipt.StorageIdentity, receipt); err != nil {
		t.Fatal(err)
	}

	receipt.Outcome = storagereleasereceipt.OutcomeDeleted
	if err := validatePendingStorageReleaseBinding(true, receipt.StorageIdentity, receipt); err == nil {
		t.Fatal("Deleted identity reached receipt completion after reappearing")
	}

	if err := validatePendingStorageReleaseBinding(true, "rbd_data.second-generation", receipt); err != nil {
		t.Fatalf("Same-name replacement blocked exact old-generation completion: %v", err)
	}

	if err := validatePendingStorageReleaseBinding(false, "", receipt); err != nil {
		t.Fatal(err)
	}
}

func TestDeletedStorageReleaseRequiresExactIdentityAbsence(t *testing.T) {
	vol := drivers.NewVolume(nil, "pool", drivers.VolumeTypeContainer, drivers.ContentTypeFS, "instance", nil, nil)
	receipt := db.StorageReleaseReceipt{StorageDriver: "ceph", StorageIdentity: "immutable", Outcome: storagereleasereceipt.OutcomeDeleted}
	provider := &storageReleaseIdentityPresenceTestProvider{exists: true}

	if err := validateDeletedStorageIdentityAbsent(provider, vol, receipt); err == nil {
		t.Fatal("Deleted receipt accepted an exact identity retained in tombstone or trash")
	}

	provider.exists = false
	if err := validateDeletedStorageIdentityAbsent(provider, vol, receipt); err != nil {
		t.Fatal(err)
	}

	if err := validateDeletedStorageIdentityAbsent(struct{}{}, vol, receipt); err == nil {
		t.Fatal("Ceph deletion was accepted without an exact identity presence proof")
	}

	receipt.Outcome = storagereleasereceipt.OutcomeDetached
	if err := validateDeletedStorageIdentityAbsent(struct{}{}, vol, receipt); err != nil {
		t.Fatal(err)
	}
}

func TestRootfsNormalizationRejectsSameNameRecreationBeforeRemap(t *testing.T) {
	diskIDMapReads := 0
	remaps := 0
	volatileWrites := 0
	err := runInstanceStorageIdentityBoundAction("rbd_data.first-generation", func() (string, error) {
		return "rbd_data.second-generation", nil
	}, func() error {
		diskIDMapReads++
		remaps++
		volatileWrites++
		return nil
	})
	if err == nil {
		t.Fatal("Same-name replacement reached rootfs ID map normalization")
	}

	if diskIDMapReads != 0 || remaps != 0 || volatileWrites != 0 {
		t.Fatalf("Identity mismatch reached rootfs normalization: DiskIdmap=%d RemapRootfsIDMap=%d VolatileSet=%d", diskIDMapReads, remaps, volatileWrites)
	}

	err = runInstanceStorageIdentityBoundAction("rbd_data.first-generation", func() (string, error) {
		return "rbd_data.first-generation", nil
	}, func() error {
		diskIDMapReads++
		remaps++
		volatileWrites++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if diskIDMapReads != 1 || remaps != 1 || volatileWrites != 1 {
		t.Fatalf("Matching identity did not run the complete rootfs normalization: DiskIdmap=%d RemapRootfsIDMap=%d VolatileSet=%d", diskIDMapReads, remaps, volatileWrites)
	}
}

func TestStorageReleaseReceiptAllowsMissingVolumeDB(t *testing.T) {
	tests := []struct {
		name    string
		outcome string
		state   string
		allowed bool
	}{
		{name: "deleted complete", outcome: storagereleasereceipt.OutcomeDeleted, state: storagereleasereceipt.StateComplete, allowed: true},
		{name: "deleted pending", outcome: storagereleasereceipt.OutcomeDeleted, state: storagereleasereceipt.StatePending},
		{name: "normalized complete", outcome: storagereleasereceipt.OutcomeNormalized, state: storagereleasereceipt.StateComplete, allowed: true},
		{name: "normalized pending", outcome: storagereleasereceipt.OutcomeNormalized, state: storagereleasereceipt.StatePending},
		{name: "detached complete", outcome: storagereleasereceipt.OutcomeDetached, state: storagereleasereceipt.StateComplete, allowed: true},
		{name: "detached pending crash replay", outcome: storagereleasereceipt.OutcomeDetached, state: storagereleasereceipt.StatePending, allowed: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			receipt := &db.StorageReleaseReceipt{Outcome: tt.outcome, State: tt.state}
			if storageReleaseReceiptAllowsMissingVolumeDB(receipt) != tt.allowed {
				t.Fatalf("Unexpected missing volume DB replay decision for %s/%s", tt.outcome, tt.state)
			}
		})
	}

	if storageReleaseReceiptAllowsMissingVolumeDB(nil) {
		t.Fatal("Missing volume DB was accepted without a receipt")
	}
}

func TestProtectedStorageReleaseUsesDetachedOutcome(t *testing.T) {
	if instanceStorageReleaseOutcome("ceph", true) != storagereleasereceipt.OutcomeDetached {
		t.Fatal("Protected Ceph handover release was not detached")
	}

	if instanceStorageReleaseOutcome("cephext", true) != storagereleasereceipt.OutcomeDetached {
		t.Fatal("Protected cephext handover release was not detached")
	}

	if instanceStorageReleaseOutcome("cephext", false) != storagereleasereceipt.OutcomeNormalized {
		t.Fatal("Final cephext release was not normalized")
	}

	if instanceStorageReleaseOutcome("ceph", false) != storagereleasereceipt.OutcomeDeleted {
		t.Fatal("Final Incus-owned Ceph release was not deleted")
	}
}

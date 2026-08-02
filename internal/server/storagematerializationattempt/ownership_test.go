//go:build linux && cgo && !agent

package storagematerializationattempt

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus/v7/internal/server/db"
)

func TestOwnershipMarkerBindsFullGeneration(t *testing.T) {
	attempt := &db.StorageMaterializationAttempt{
		Token: "33333333-3333-4333-8333-333333333333", AllocationID: "11111111-1111-4111-8111-111111111111",
		ComputeID: "22222222-2222-4222-8222-222222222222", Owner: "44444444-4444-4444-8444-444444444444",
		Project: "nova", InstanceName: "instance-00000001", IDMapBase: 1000000, IDMapSize: 65536,
		StorageDriver: "ceph", StoragePool: "rootfs", StorageVolume: "nova_instance-00000001",
		BaselineClean: true, CleanupDisposition: CleanupDelete,
	}

	marker, err := OwnershipMarker(attempt, "rbd-id-one")
	require.NoError(t, err)
	require.Regexp(t, `^sha256:[0-9a-f]{64}$`, marker)

	changed := *attempt
	changed.Owner = "55555555-5555-4555-8555-555555555555"
	other, err := OwnershipMarker(&changed, "rbd-id-one")
	require.NoError(t, err)
	require.NotEqual(t, marker, other)

	other, err = OwnershipMarker(attempt, "rbd-id-two")
	require.NoError(t, err)
	require.NotEqual(t, marker, other)

	_, err = OwnershipMarker(attempt, "")
	require.Error(t, err)
}

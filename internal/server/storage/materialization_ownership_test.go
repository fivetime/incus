//go:build linux && cgo && !agent

package storage

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus/v7/internal/server/db"
	"github.com/lxc/incus/v7/internal/server/storage/drivers"
	"github.com/lxc/incus/v7/internal/server/storagematerializationattempt"
)

type materializationOwnershipTestDriver struct {
	drivers.Driver
	marker       string
	corruptAfter bool
	setCalls     int
}

func (d *materializationOwnershipTestDriver) GetVolumeMaterializationOwnership(drivers.Volume) (string, error) {
	if d.corruptAfter && d.setCalls > 0 {
		return "another-generation", nil
	}

	return d.marker, nil
}

func (d *materializationOwnershipTestDriver) SetVolumeMaterializationOwnership(_ drivers.Volume, marker string) error {
	d.setCalls++
	d.marker = marker
	return nil
}

func materializationOwnershipTestAttempt() *db.StorageMaterializationAttempt {
	return &db.StorageMaterializationAttempt{
		Token: "33333333-3333-4333-8333-333333333333", AllocationID: "11111111-1111-4111-8111-111111111111",
		ComputeID: "22222222-2222-4222-8222-222222222222", Owner: "44444444-4444-4444-8444-444444444444",
		Project: "nova", InstanceName: "instance-00000001", IDMapBase: 1000000, IDMapSize: 65536,
		StorageDriver: "ceph", StoragePool: "rootfs", StorageVolume: "nova_instance-00000001",
		BaselineClean: true, CleanupDisposition: storagematerializationattempt.CleanupDelete,
	}
}

func TestPersistStorageMaterializationOwnership(t *testing.T) {
	driver := &materializationOwnershipTestDriver{}
	err := persistStorageMaterializationOwnership(
		driver, drivers.Volume{}, materializationOwnershipTestAttempt(), "rbd-id-one")
	require.NoError(t, err)
	require.Equal(t, 1, driver.setCalls)
	require.Regexp(t, `^sha256:[0-9a-f]{64}$`, driver.marker)
}

func TestPersistStorageMaterializationOwnershipRejectsChangedMarker(t *testing.T) {
	driver := &materializationOwnershipTestDriver{corruptAfter: true}
	err := persistStorageMaterializationOwnership(
		driver, drivers.Volume{}, materializationOwnershipTestAttempt(), "rbd-id-one")
	require.ErrorContains(t, err, "changed before commit")
}

func TestPersistStorageMaterializationOwnershipSkipsDetachedVolume(t *testing.T) {
	driver := &materializationOwnershipTestDriver{}
	attempt := materializationOwnershipTestAttempt()
	attempt.CleanupDisposition = storagematerializationattempt.CleanupDetach
	err := persistStorageMaterializationOwnership(
		driver, drivers.Volume{}, attempt, "rbd-id-one")
	require.NoError(t, err)
	require.Zero(t, driver.setCalls)
}

package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	internalInstance "github.com/lxc/incus/v7/internal/instance"
	"github.com/lxc/incus/v7/internal/server/locking"
)

func TestExternalRBDClaimIdentityUsesPhysicalCephIdentity(t *testing.T) {
	lookup := func(clusterName string, userName string) (string, error) {
		switch clusterName {
		case "ceph-a", "ceph-a-alias":
			return "11111111-1111-1111-1111-111111111111", nil
		case "ceph-b":
			return "22222222-2222-2222-2222-222222222222", nil
		default:
			return "", errors.New("unknown cluster")
		}
	}

	imageName := "volume-8231d2e8-e306-40e4-8f42-a9d2475f2e05"
	identity, err := externalRBDClaimIdentityFromConfig(map[string]string{
		"ceph.cluster_name":  "ceph-a",
		"ceph.user.name":     "cinder",
		"ceph.osd.pool_name": "cinder-volumes",
	}, imageName, lookup)
	if err != nil {
		t.Fatal(err)
	}

	alias, err := externalRBDClaimIdentityFromConfig(map[string]string{
		"ceph.cluster_name": "ceph-a-alias",
		"ceph.user.name":    "another-client",
		"source":            "cinder-volumes",
	}, imageName, lookup)
	if err != nil {
		t.Fatal(err)
	}

	if identity != alias {
		t.Fatalf("Pools reaching the same physical RBD image produced different identities: %#v != %#v", identity, alias)
	}

	otherCluster, err := externalRBDClaimIdentityFromConfig(map[string]string{
		"ceph.cluster_name":  "ceph-b",
		"ceph.osd.pool_name": "cinder-volumes",
	}, imageName, lookup)
	if err != nil {
		t.Fatal(err)
	}

	if identity == otherCluster {
		t.Fatal("Different Ceph clusters produced the same physical claim identity")
	}

	otherPool, err := externalRBDClaimIdentityFromConfig(map[string]string{
		"ceph.cluster_name":  "ceph-a",
		"ceph.osd.pool_name": "other-volumes",
	}, imageName, lookup)
	if err != nil {
		t.Fatal(err)
	}

	if identity == otherPool {
		t.Fatal("Different OSD pools produced the same physical claim identity")
	}

	if _, err := externalRBDClaimIdentityFromConfig(map[string]string{"ceph.cluster_name": "ceph-a"}, imageName, lookup); err == nil {
		t.Fatal("Missing OSD pool was accepted")
	}
}

func TestExternalRBDClaimLockSerializesPoolAliases(t *testing.T) {
	identity := externalRBDClaimIdentity{
		ClusterFSID: "11111111-1111-1111-1111-111111111111",
		OSDPool:     "cinder-volumes",
		ImageName:   "volume-8231d2e8-e306-40e4-8f42-a9d2475f2e05",
	}

	lockName := externalRBDClaimLockName(identity)
	aliasLockName := externalRBDClaimLockName(identity)
	if lockName != aliasLockName {
		t.Fatal("Equivalent physical RBD claims did not share a lock name")
	}

	unlock, err := locking.Lock(context.Background(), lockName)
	if err != nil {
		t.Fatal(err)
	}

	acquired := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		unlockAlias, err := locking.Lock(context.Background(), aliasLockName)
		if err != nil {
			result <- err
			return
		}

		close(acquired)
		unlockAlias()
		result <- nil
	}()

	select {
	case <-acquired:
		t.Fatal("Equivalent physical RBD claim lock did not serialize concurrent access")
	case err := <-result:
		t.Fatalf("Concurrent lock attempt returned before release: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	unlock()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}

	case <-time.After(time.Second):
		t.Fatal("Concurrent physical RBD claim lock was not released")
	}
}

func TestInstanceStorageVolumeShouldDelete(t *testing.T) {
	tests := []struct {
		name      string
		volExists bool
		config    map[string]string
		expected  bool
	}{
		{name: "missing volume", volExists: false, config: map[string]string{}, expected: false},
		{name: "owned volume", volExists: true, config: map[string]string{}, expected: true},
		{
			name:      "protected target volume",
			volExists: true,
			config: map[string]string{
				internalInstance.ConfigVolatileMigrationStorageDeleteProtection: "true",
			},
			expected: false,
		},
		{
			name:      "pending source volume",
			volExists: true,
			config: map[string]string{
				internalInstance.ConfigVolatileMigrationStorageHandover:     "pending",
				internalInstance.ConfigVolatileMigrationStorageHandoverRole: internalInstance.StorageHandoverRoleSource,
			},
			expected: false,
		},
		{
			name:      "committed source volume",
			volExists: true,
			config: map[string]string{
				internalInstance.ConfigVolatileMigrationStorageHandover:     "committed",
				internalInstance.ConfigVolatileMigrationStorageHandoverRole: internalInstance.StorageHandoverRoleSource,
			},
			expected: false,
		},
		{
			name:      "malformed handover fails closed",
			volExists: true,
			config: map[string]string{
				internalInstance.ConfigVolatileMigrationStorageHandover: "unexpected",
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := instanceStorageVolumeShouldDelete(tt.volExists, tt.config)
			if actual != tt.expected {
				t.Fatalf("instanceStorageVolumeShouldDelete() = %t, want %t", actual, tt.expected)
			}
		})
	}

	changes, err := internalInstance.StorageHandoverConfigChanges(internalInstance.StorageHandoverStateOwned)
	if err != nil {
		t.Fatalf("Owned transition failed: %v", err)
	}

	config := map[string]string{
		internalInstance.ConfigVolatileMigrationStorageDeleteProtection: "true",
		internalInstance.ConfigVolatileMigrationStorageHandover:         "committed",
		internalInstance.ConfigVolatileMigrationStorageHandoverRole:     internalInstance.StorageHandoverRoleSource,
	}

	for key, value := range changes {
		if value == "" {
			delete(config, key)
		}
	}

	if !instanceStorageVolumeShouldDelete(true, config) {
		t.Fatal("Owned transition did not restore normal volume deletion")
	}
}

func TestInstanceStorageVolumeShouldNormalizeRootfs(t *testing.T) {
	tests := []struct {
		name       string
		driverName string
		volExists  bool
		config     map[string]string
		expected   bool
	}{
		{name: "owned cephext root", driverName: "cephext", volExists: true, config: map[string]string{}, expected: true},
		{name: "missing cephext root", driverName: "cephext", volExists: false, config: map[string]string{}, expected: false},
		{name: "ordinary ceph root", driverName: "ceph", volExists: true, config: map[string]string{}, expected: false},
		{
			name:       "protected cephext migration loser",
			driverName: "cephext",
			volExists:  true,
			config: map[string]string{
				internalInstance.ConfigVolatileMigrationStorageDeleteProtection: "true",
			},
			expected: false,
		},
		{
			name:       "cephext source with pending handover",
			driverName: "cephext",
			volExists:  true,
			config: map[string]string{
				internalInstance.ConfigVolatileMigrationStorageHandover:     "pending",
				internalInstance.ConfigVolatileMigrationStorageHandoverRole: internalInstance.StorageHandoverRoleSource,
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := instanceStorageVolumeShouldNormalizeRootfs(tt.driverName, tt.volExists, tt.config)
			if actual != tt.expected {
				t.Fatalf("instanceStorageVolumeShouldNormalizeRootfs() = %t, want %t", actual, tt.expected)
			}
		})
	}
}

func TestInstanceStorageVolumeConfigForDelete(t *testing.T) {
	config := map[string]string{"ceph.rbd.image_name": "volume-8231d2e8-e306-40e4-8f42-a9d2475f2e05"}
	cephextConfig := instanceStorageVolumeConfigForDelete("cephext", config)
	if cephextConfig["ceph.rbd.image_name"] != config["ceph.rbd.image_name"] {
		t.Fatal("cephext deletion lost the externally managed RBD image name")
	}

	if instanceStorageVolumeConfigForDelete("ceph", config) != nil {
		t.Fatal("Ordinary storage deletion unexpectedly retained driver-specific config")
	}
}

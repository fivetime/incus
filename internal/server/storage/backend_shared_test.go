package storage

import (
	"testing"

	internalInstance "github.com/lxc/incus/v7/internal/instance"
)

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

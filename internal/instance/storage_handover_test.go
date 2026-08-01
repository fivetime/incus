package instance

import (
	"errors"
	"testing"
)

func applyStorageHandoverChanges(config map[string]string, changes map[string]string) {
	for key, value := range changes {
		if value == "" {
			delete(config, key)
			continue
		}

		config[key] = value
	}
}

func TestStorageHandoverConfigChanges(t *testing.T) {
	config := map[string]string{
		ConfigVolatileMigrationStorageHandover:        "committed",
		ConfigVolatileMigrationStorageHandoverRole:    StorageHandoverRoleSource,
		ConfigVolatileMigrationStorageReceiveComplete: "true",
	}

	for i := 0; i < 2; i++ {
		changes, err := StorageHandoverConfigChanges(StorageHandoverStateProtected)
		if err != nil {
			t.Fatalf("Protected transition failed: %v", err)
		}

		applyStorageHandoverChanges(config, changes)
		if !StorageDeleteProtected(config) {
			t.Fatal("Protected transition did not protect storage deletion")
		}
	}

	for i := 0; i < 2; i++ {
		changes, err := StorageHandoverConfigChanges(StorageHandoverStateOwned)
		if err != nil {
			t.Fatalf("Owned transition failed: %v", err)
		}

		applyStorageHandoverChanges(config, changes)
		if StorageDeleteProtected(config) {
			t.Fatal("Owned transition did not restore normal storage deletion")
		}

		_, ok := config[ConfigVolatileMigrationStorageHandover]
		if ok {
			t.Fatal("Owned transition left the handover marker")
		}

		_, ok = config[ConfigVolatileMigrationStorageHandoverRole]
		if ok {
			t.Fatal("Owned transition left the handover role")
		}

		_, ok = config[ConfigVolatileMigrationStorageDeleteProtection]
		if ok {
			t.Fatal("Owned transition left delete protection")
		}

		_, ok = config[ConfigVolatileMigrationStorageReceiveComplete]
		if ok {
			t.Fatal("Owned transition left receive completion proof")
		}
	}
}

func TestStorageHandoverConfigChangesInvalid(t *testing.T) {
	_, err := StorageHandoverConfigChanges("invalid")
	if err == nil {
		t.Fatal("Expected invalid storage handover state to fail")
	}
}

func TestStorageHandoverAPIConfigChanges(t *testing.T) {
	tests := []struct {
		name          string
		state         string
		driverName    string
		config        map[string]string
		expectError   error
		expectChanges map[string]string
	}{
		{
			name:        "protect without negotiated handover",
			state:       StorageHandoverStateProtected,
			config:      map[string]string{},
			expectError: ErrStorageHandoverInactive,
		},
		{
			name:  "protect pending source",
			state: StorageHandoverStateProtected,
			config: map[string]string{
				ConfigVolatileMigrationStorageHandover:     "pending",
				ConfigVolatileMigrationStorageHandoverRole: StorageHandoverRoleSource,
			},
			expectChanges: map[string]string{
				ConfigVolatileMigrationStorageDeleteProtection: "true",
			},
		},
		{
			name:  "protect committed source",
			state: StorageHandoverStateProtected,
			config: map[string]string{
				ConfigVolatileMigrationStorageHandover:     "committed",
				ConfigVolatileMigrationStorageHandoverRole: StorageHandoverRoleSource,
			},
			expectChanges: map[string]string{
				ConfigVolatileMigrationStorageDeleteProtection: "true",
			},
		},
		{
			name:  "protect already protected target",
			state: StorageHandoverStateProtected,
			config: map[string]string{
				ConfigVolatileMigrationStorageDeleteProtection: "true",
				ConfigVolatileMigrationStorageHandoverRole:     StorageHandoverRoleTarget,
			},
			expectChanges: map[string]string{
				ConfigVolatileMigrationStorageDeleteProtection: "true",
			},
		},
		{
			name:  "false protection is not evidence",
			state: StorageHandoverStateProtected,
			config: map[string]string{
				ConfigVolatileMigrationStorageDeleteProtection: "false",
			},
			expectError: ErrStorageHandoverInactive,
		},
		{
			name:  "protect rejects marker without role",
			state: StorageHandoverStateProtected,
			config: map[string]string{
				ConfigVolatileMigrationStorageHandover: "pending",
			},
			expectError: ErrStorageHandoverIncomplete,
		},
		{
			name:  "protect rejects mixed source and target completion",
			state: StorageHandoverStateProtected,
			config: map[string]string{
				ConfigVolatileMigrationStorageHandover:        "pending",
				ConfigVolatileMigrationStorageHandoverRole:    StorageHandoverRoleSource,
				ConfigVolatileMigrationStorageReceiveComplete: "true",
			},
			expectError: ErrStorageHandoverIncomplete,
		},
		{
			name:          "owned without negotiated handover is idempotent",
			state:         StorageHandoverStateOwned,
			config:        map[string]string{},
			expectChanges: map[string]string{},
		},
		{
			name:  "owned rejects committed rollback source",
			state: StorageHandoverStateOwned,
			config: map[string]string{
				ConfigVolatileMigrationStorageHandover:         "committed",
				ConfigVolatileMigrationStorageHandoverRole:     StorageHandoverRoleSource,
				ConfigVolatileMigrationStorageDeleteProtection: "true",
			},
			expectError: ErrStorageHandoverIncomplete,
		},
		{
			name:  "owned rejects pending source",
			state: StorageHandoverStateOwned,
			config: map[string]string{
				ConfigVolatileMigrationStorageHandover:     "pending",
				ConfigVolatileMigrationStorageHandoverRole: StorageHandoverRoleSource,
			},
			expectError: ErrStorageHandoverIncomplete,
		},
		{
			name:  "owned rejects committed marker without role",
			state: StorageHandoverStateOwned,
			config: map[string]string{
				ConfigVolatileMigrationStorageHandover: "committed",
			},
			expectError: ErrStorageHandoverIncomplete,
		},
		{
			name:  "owned rejects incomplete target",
			state: StorageHandoverStateOwned,
			config: map[string]string{
				ConfigVolatileMigrationStorageDeleteProtection: "true",
				ConfigVolatileMigrationStorageHandoverRole:     StorageHandoverRoleTarget,
			},
			expectError: ErrStorageHandoverIncomplete,
		},
		{
			name:  "owned accepts completed target",
			state: StorageHandoverStateOwned,
			config: map[string]string{
				ConfigVolatileMigrationStorageDeleteProtection: "true",
				ConfigVolatileMigrationStorageReceiveComplete:  "true",
				ConfigVolatileMigrationStorageHandoverRole:     StorageHandoverRoleTarget,
			},
			expectChanges: map[string]string{
				ConfigVolatileMigrationStorageDeleteProtection: "",
				ConfigVolatileMigrationStorageReceiveComplete:  "",
				ConfigVolatileMigrationStorageHandover:         "",
				ConfigVolatileMigrationStorageHandoverRole:     "",
			},
		},
		{
			name:       "cephext protected is unsupported",
			state:      StorageHandoverStateProtected,
			driverName: "cephext",
			config: map[string]string{
				ConfigVolatileMigrationStorageHandover:     "committed",
				ConfigVolatileMigrationStorageHandoverRole: StorageHandoverRoleSource,
			},
			expectError: ErrStorageHandoverUnsupported,
		},
		{
			name:       "cephext owned accepts completed target",
			state:      StorageHandoverStateOwned,
			driverName: "cephext",
			config: map[string]string{
				ConfigVolatileMigrationStorageReceiveComplete: "true",
				ConfigVolatileMigrationStorageHandoverRole:    StorageHandoverRoleTarget,
				"ceph.rbd.image_name":                         "volume-test",
			},
			expectChanges: map[string]string{
				ConfigVolatileMigrationStorageDeleteProtection: "",
				ConfigVolatileMigrationStorageReceiveComplete:  "",
				ConfigVolatileMigrationStorageHandover:         "",
				ConfigVolatileMigrationStorageHandoverRole:     "",
			},
		},
		{
			name:       "cephext owned rejects committed rollback source",
			state:      StorageHandoverStateOwned,
			driverName: "cephext",
			config: map[string]string{
				ConfigVolatileMigrationStorageHandover:     "committed",
				ConfigVolatileMigrationStorageHandoverRole: StorageHandoverRoleSource,
				"ceph.rbd.image_name":                      "volume-test",
			},
			expectError: ErrStorageHandoverIncomplete,
		},
		{
			name:       "cephext owned retry is idempotent after clear",
			state:      StorageHandoverStateOwned,
			driverName: "cephext",
			config: map[string]string{
				"ceph.rbd.image_name": "volume-test",
			},
			expectChanges: map[string]string{},
		},
		{
			name:  "owned rejects completion without target protection",
			state: StorageHandoverStateOwned,
			config: map[string]string{
				ConfigVolatileMigrationStorageReceiveComplete: "true",
				ConfigVolatileMigrationStorageHandoverRole:    StorageHandoverRoleTarget,
			},
			expectError: ErrStorageHandoverIncomplete,
		},
		{
			name:  "owned rejects mixed pending target state",
			state: StorageHandoverStateOwned,
			config: map[string]string{
				ConfigVolatileMigrationStorageHandover:         "pending",
				ConfigVolatileMigrationStorageDeleteProtection: "true",
				ConfigVolatileMigrationStorageReceiveComplete:  "true",
				ConfigVolatileMigrationStorageHandoverRole:     StorageHandoverRoleTarget,
			},
			expectError: ErrStorageHandoverIncomplete,
		},
		{
			name:  "source owned clears pending ceph source",
			state: StorageHandoverStateSourceOwned,
			config: map[string]string{
				ConfigVolatileMigrationStorageHandover:     "pending",
				ConfigVolatileMigrationStorageHandoverRole: StorageHandoverRoleSource,
			},
			expectChanges: map[string]string{
				ConfigVolatileMigrationStorageDeleteProtection: "",
				ConfigVolatileMigrationStorageReceiveComplete:  "",
				ConfigVolatileMigrationStorageHandover:         "",
				ConfigVolatileMigrationStorageHandoverRole:     "",
			},
		},
		{
			name:       "source owned clears pending cephext source without changing image",
			state:      StorageHandoverStateSourceOwned,
			driverName: "cephext",
			config: map[string]string{
				ConfigVolatileMigrationStorageHandover:     "pending",
				ConfigVolatileMigrationStorageHandoverRole: StorageHandoverRoleSource,
				"ceph.rbd.image_name":                      "volume-test",
			},
			expectChanges: map[string]string{
				ConfigVolatileMigrationStorageDeleteProtection: "",
				ConfigVolatileMigrationStorageReceiveComplete:  "",
				ConfigVolatileMigrationStorageHandover:         "",
				ConfigVolatileMigrationStorageHandoverRole:     "",
			},
		},
		{
			name:          "source owned is idempotent after clear",
			state:         StorageHandoverStateSourceOwned,
			config:        map[string]string{},
			expectChanges: map[string]string{},
		},
		{
			name:       "source owned cephext is idempotent after clear",
			state:      StorageHandoverStateSourceOwned,
			driverName: "cephext",
			config: map[string]string{
				"ceph.rbd.image_name": "volume-test",
			},
			expectChanges: map[string]string{},
		},
		{
			name:  "source owned rejects missing role",
			state: StorageHandoverStateSourceOwned,
			config: map[string]string{
				ConfigVolatileMigrationStorageHandover: "pending",
			},
			expectError: ErrStorageHandoverIncomplete,
		},
		{
			name:  "source owned rejects pending target",
			state: StorageHandoverStateSourceOwned,
			config: map[string]string{
				ConfigVolatileMigrationStorageHandover:     "pending",
				ConfigVolatileMigrationStorageHandoverRole: StorageHandoverRoleTarget,
			},
			expectError: ErrStorageHandoverIncomplete,
		},
		{
			name:  "source owned clears committed source",
			state: StorageHandoverStateSourceOwned,
			config: map[string]string{
				ConfigVolatileMigrationStorageHandover:     "committed",
				ConfigVolatileMigrationStorageHandoverRole: StorageHandoverRoleSource,
			},
			expectChanges: map[string]string{
				ConfigVolatileMigrationStorageDeleteProtection: "",
				ConfigVolatileMigrationStorageReceiveComplete:  "",
				ConfigVolatileMigrationStorageHandover:         "",
				ConfigVolatileMigrationStorageHandoverRole:     "",
			},
		},
		{
			name:  "source owned clears source deletion protection",
			state: StorageHandoverStateSourceOwned,
			config: map[string]string{
				ConfigVolatileMigrationStorageHandover:         "pending",
				ConfigVolatileMigrationStorageHandoverRole:     StorageHandoverRoleSource,
				ConfigVolatileMigrationStorageDeleteProtection: "true",
			},
			expectChanges: map[string]string{
				ConfigVolatileMigrationStorageDeleteProtection: "",
				ConfigVolatileMigrationStorageReceiveComplete:  "",
				ConfigVolatileMigrationStorageHandover:         "",
				ConfigVolatileMigrationStorageHandoverRole:     "",
			},
		},
		{
			name:  "source owned rejects source with target completion",
			state: StorageHandoverStateSourceOwned,
			config: map[string]string{
				ConfigVolatileMigrationStorageHandover:        "pending",
				ConfigVolatileMigrationStorageHandoverRole:    StorageHandoverRoleSource,
				ConfigVolatileMigrationStorageReceiveComplete: "true",
			},
			expectError: ErrStorageHandoverIncomplete,
		},
		{
			name:  "source owned rejects completed target",
			state: StorageHandoverStateSourceOwned,
			config: map[string]string{
				ConfigVolatileMigrationStorageHandoverRole:     StorageHandoverRoleTarget,
				ConfigVolatileMigrationStorageDeleteProtection: "true",
				ConfigVolatileMigrationStorageReceiveComplete:  "true",
			},
			expectError: ErrStorageHandoverIncomplete,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			driverName := tt.driverName
			if driverName == "" {
				driverName = "ceph"
			}

			changes, err := StorageHandoverAPIConfigChanges(tt.state, tt.config, driverName)
			if !errors.Is(err, tt.expectError) {
				t.Fatalf("StorageHandoverAPIConfigChanges() error = %v, want %v", err, tt.expectError)
			}

			if len(changes) != len(tt.expectChanges) {
				t.Fatalf("StorageHandoverAPIConfigChanges() changes = %#v, want %#v", changes, tt.expectChanges)
			}

			for key, expected := range tt.expectChanges {
				actual := changes[key]
				if actual != expected {
					t.Fatalf("StorageHandoverAPIConfigChanges() change %q = %q, want %q", key, actual, expected)
				}
			}
		})
	}
}

func TestStorageHandoverAPITransitionsAreIdempotent(t *testing.T) {
	config := map[string]string{
		ConfigVolatileMigrationStorageHandoverRole:     StorageHandoverRoleTarget,
		ConfigVolatileMigrationStorageDeleteProtection: "true",
		ConfigVolatileMigrationStorageReceiveComplete:  "true",
	}

	for range 2 {
		changes, err := StorageHandoverAPIConfigChanges(StorageHandoverStateProtected, config, "ceph")
		if err != nil {
			t.Fatalf("Protected transition failed: %v", err)
		}

		applyStorageHandoverChanges(config, changes)
	}

	changes, err := StorageHandoverAPIConfigChanges(StorageHandoverStateOwned, config, "ceph")
	if err != nil {
		t.Fatalf("Owned transition failed: %v", err)
	}

	applyStorageHandoverChanges(config, changes)

	changes, err = StorageHandoverAPIConfigChanges(StorageHandoverStateOwned, config, "ceph")
	if err != nil {
		t.Fatalf("Repeated owned transition failed: %v", err)
	}

	if len(changes) != 0 {
		t.Fatalf("Repeated owned transition returned changes: %#v", changes)
	}
}

func TestStorageHandoverAPISourceOwnedIsIdempotentAndPreservesVolumeConfig(t *testing.T) {
	const imageName = "volume-test"

	config := map[string]string{
		ConfigVolatileMigrationStorageHandover:     "pending",
		ConfigVolatileMigrationStorageHandoverRole: StorageHandoverRoleSource,
		"ceph.rbd.image_name":                      imageName,
	}

	for i := 0; i < 2; i++ {
		changes, err := StorageHandoverAPIConfigChanges(StorageHandoverStateSourceOwned, config, "cephext")
		if err != nil {
			t.Fatalf("Source-owned transition %d failed: %v", i+1, err)
		}

		applyStorageHandoverChanges(config, changes)
		if config["ceph.rbd.image_name"] != imageName {
			t.Fatalf("Source-owned transition changed external RBD image reference to %q", config["ceph.rbd.image_name"])
		}
	}
}

func TestStorageDeleteProtected(t *testing.T) {
	tests := []struct {
		name     string
		config   map[string]string
		expected bool
	}{
		{name: "unprotected", config: map[string]string{}, expected: false},
		{name: "pending handover", config: map[string]string{ConfigVolatileMigrationStorageHandover: "pending"}, expected: true},
		{name: "committed handover", config: map[string]string{ConfigVolatileMigrationStorageHandover: "committed"}, expected: true},
		{name: "unknown handover fails closed", config: map[string]string{ConfigVolatileMigrationStorageHandover: "corrupt"}, expected: true},
		{name: "target protection", config: map[string]string{ConfigVolatileMigrationStorageDeleteProtection: "true"}, expected: true},
		{name: "false target protection", config: map[string]string{ConfigVolatileMigrationStorageDeleteProtection: "false"}, expected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := StorageDeleteProtected(tt.config)
			if actual != tt.expected {
				t.Fatalf("StorageDeleteProtected() = %t, want %t", actual, tt.expected)
			}
		})
	}
}

func TestStorageHandoverDriverSupported(t *testing.T) {
	tests := []struct {
		name       string
		driverName string
		state      string
		expected   bool
	}{
		{name: "ceph protected", driverName: "ceph", state: StorageHandoverStateProtected, expected: true},
		{name: "ceph owned", driverName: "ceph", state: StorageHandoverStateOwned, expected: true},
		{name: "ceph source owned", driverName: "ceph", state: StorageHandoverStateSourceOwned, expected: true},
		{name: "cephext protected", driverName: "cephext", state: StorageHandoverStateProtected, expected: false},
		{name: "cephext owned", driverName: "cephext", state: StorageHandoverStateOwned, expected: true},
		{name: "cephext source owned", driverName: "cephext", state: StorageHandoverStateSourceOwned, expected: true},
		{name: "dir owned", driverName: "dir", state: StorageHandoverStateOwned, expected: false},
		{name: "dir source owned", driverName: "dir", state: StorageHandoverStateSourceOwned, expected: false},
		{name: "invalid state", driverName: "ceph", state: "invalid", expected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := StorageHandoverDriverSupported(tt.driverName, tt.state)
			if actual != tt.expected {
				t.Fatalf("StorageHandoverDriverSupported(%q, %q) = %t, want %t", tt.driverName, tt.state, actual, tt.expected)
			}
		})
	}
}

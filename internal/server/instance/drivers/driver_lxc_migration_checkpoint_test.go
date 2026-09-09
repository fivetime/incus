//go:build linux && cgo && !agent

package drivers

import (
	"testing"
)

func TestMigrationCheckpointRejectsUnsafeRestore(t *testing.T) {
	operation := "10000000-0000-0000-0000-000000000001"
	for name, config := range map[string]map[string]string{
		"no checkpoint":     {},
		"another operation": {migrationCheckpointKey: "20000000-0000-0000-0000-000000000002"},
		"incomplete dump":   {migrationCheckpointKey: operation, migrationCheckpointStateKey: "dumping"},
		"unfenced handover": {migrationCheckpointKey: operation, migrationCheckpointStateKey: "ready", "volatile.migration.storage_handover": "pending"},
		"protected target":  {migrationCheckpointKey: operation, migrationCheckpointStateKey: "ready", "volatile.migration.storage_delete_protection": "true"},
	} {
		t.Run(name, func(t *testing.T) {
			driver := &lxc{common: common{localConfig: config}}
			err := driver.RestoreMigrationCheckpoint(operation)
			if err == nil {
				t.Fatal("Unsafe checkpoint reached runtime restoration")
			}
		})
	}
}

func TestMigrationCheckpointPathRejectsUnboundLocations(t *testing.T) {
	valid := "10000000-0000-0000-0000-000000000001"
	for _, invalid := range []string{"", "../instance", "/tmp/checkpoint", valid + "/../other"} {
		_, err := migrationCheckpointPath(valid, invalid)
		if err == nil {
			t.Fatalf("Invalid operation accepted: %q", invalid)
		}

		_, err = migrationCheckpointPath(invalid, valid)
		if err == nil {
			t.Fatalf("Invalid instance accepted: %q", invalid)
		}
	}
}

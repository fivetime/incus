//go:build linux && cgo && !agent

package drivers

import (
	"os"
	"path/filepath"
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

func TestRemoveMigrationCheckpointRemovesEmptyInstanceDirectory(t *testing.T) {
	instanceDir := filepath.Join(t.TempDir(), "10000000-0000-0000-0000-000000000001")
	first := filepath.Join(instanceDir, "20000000-0000-0000-0000-000000000002")
	second := filepath.Join(instanceDir, "30000000-0000-0000-0000-000000000003")
	for _, dir := range []string{first, second} {
		err := os.MkdirAll(dir, 0o700)
		if err != nil {
			t.Fatal(err)
		}

		err = os.WriteFile(filepath.Join(dir, "pages-1.img"), []byte("x"), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	// Another checkpoint of the same instance keeps the directory.
	err := removeMigrationCheckpoint(first)
	if err != nil {
		t.Fatal(err)
	}

	_, err = os.Stat(filepath.Join(second, "pages-1.img"))
	if err != nil {
		t.Fatalf("Sibling checkpoint was removed: %v", err)
	}

	// The last checkpoint takes the empty instance directory with it.
	err = removeMigrationCheckpoint(second)
	if err != nil {
		t.Fatal(err)
	}

	_, err = os.Stat(instanceDir)
	if !os.IsNotExist(err) {
		t.Fatalf("Empty instance checkpoint directory was left behind: %v", err)
	}

	// Removing an already removed checkpoint is not an error.
	err = removeMigrationCheckpoint(second)
	if err != nil {
		t.Fatal(err)
	}
}

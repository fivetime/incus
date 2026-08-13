package drivers

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveFailedCRIUCheckpointRemovesEntireChain(t *testing.T) {
	checkpointDir := filepath.Join(t.TempDir(), "checkpoint")
	for _, generation := range []string{"001", "002"} {
		err := os.MkdirAll(filepath.Join(checkpointDir, generation), 0o700)
		if err != nil {
			t.Fatalf("Failed creating pre-dump generation: %v", err)
		}

		err = os.WriteFile(filepath.Join(checkpointDir, generation, "pages.img"), []byte("state"), 0o600)
		if err != nil {
			t.Fatalf("Failed creating pre-dump state: %v", err)
		}
	}

	migrationErr := errors.New("pre-dump failed")
	removed, err := removeFailedCRIUCheckpoint(checkpointDir, migrationErr)
	if !errors.Is(err, migrationErr) {
		t.Fatalf("Cleanup error = %v, want original migration error", err)
	}

	if !removed {
		t.Fatal("Checkpoint chain was not reported as removed")
	}

	_, err = os.Stat(checkpointDir)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Checkpoint chain still exists after pre-dump failure: %v", err)
	}
}

func TestRemoveFailedCRIUCheckpointRetainsReferenceOnCleanupFailure(t *testing.T) {
	blockingFile := filepath.Join(t.TempDir(), "file")
	err := os.WriteFile(blockingFile, []byte("not a directory"), 0o600)
	if err != nil {
		t.Fatalf("Failed creating blocking file: %v", err)
	}

	migrationErr := errors.New("dump failed")
	removed, err := removeFailedCRIUCheckpoint(filepath.Join(blockingFile, "checkpoint"), migrationErr)
	if removed {
		t.Fatal("Checkpoint was reported as removed after cleanup failed")
	}

	if !errors.Is(err, migrationErr) {
		t.Fatalf("Cleanup error = %v, want original migration error", err)
	}

	if err == migrationErr {
		t.Fatal("Cleanup failure was not joined to migration error")
	}
}

func TestCleanupFailedCRIUCheckpointBySourceState(t *testing.T) {
	tests := []struct {
		name          string
		sourceRunning bool
		expectRemoved bool
	}{
		{name: "running source", sourceRunning: true, expectRemoved: true},
		{name: "stopped source", sourceRunning: false, expectRemoved: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkpointDir := filepath.Join(t.TempDir(), "checkpoint")
			err := os.MkdirAll(checkpointDir, 0o700)
			if err != nil {
				t.Fatalf("Failed creating checkpoint: %v", err)
			}

			migrationErr := errors.New("final dump failed")
			removed, err := cleanupFailedCRIUCheckpoint(checkpointDir, migrationErr, tt.sourceRunning)
			if !errors.Is(err, migrationErr) {
				t.Fatalf("Cleanup error = %v, want original migration error", err)
			}

			if removed != tt.expectRemoved {
				t.Fatalf("Checkpoint removed = %t, want %t", removed, tt.expectRemoved)
			}

			_, statErr := os.Stat(checkpointDir)
			if tt.expectRemoved && !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("Running-source checkpoint still exists: %v", statErr)
			}

			if !tt.expectRemoved && statErr != nil {
				t.Fatalf("Stopped-source checkpoint was not retained: %v", statErr)
			}
		})
	}
}

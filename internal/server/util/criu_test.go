package util

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestExecutableSHA256(t *testing.T) {
	path := filepath.Join(t.TempDir(), "criu")
	content := []byte("approved runtime")
	err := os.WriteFile(path, content, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	wanted := sha256.Sum256(content)
	actual, err := executableSHA256(path)
	if err != nil || actual != hex.EncodeToString(wanted[:]) {
		t.Fatalf("Unexpected fingerprint %q: %v", actual, err)
	}

	err = os.Chmod(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	_, err = executableSHA256(path)
	if err == nil {
		t.Fatal("Non-executable file was accepted")
	}

	_, err = executableSHA256(filepath.Dir(path))
	if err == nil {
		t.Fatal("Directory was accepted")
	}

	_, err = executableSHA256(path + ".missing")
	if err == nil {
		t.Fatal("Missing executable was accepted")
	}
}

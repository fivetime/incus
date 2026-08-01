package drivers

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func Test_cephext_imageNameValidation(t *testing.T) {
	d := &cephext{}
	validator := d.commonVolumeRules()["ceph.rbd.image_name"]

	valid := []string{
		"",
		"volume-8231d2e8-e306-40e4-8f42-a9d2475f2e05",
		"some.image_name-01",
		strings.Repeat("a", 255),
	}

	for _, name := range valid {
		err := validator(name)
		if err != nil {
			t.Errorf("expected %q to be valid, got: %v", name, err)
		}
	}

	invalid := []string{
		"volume@snap",                 // Snapshot reference.
		"pool/volume",                 // Pool or namespace separator.
		"volume name",                 // Whitespace.
		"volume\tname",                // Control character.
		"volume\x00name",              // NUL.
		"卷名",                          // Non-ASCII.
		strings.Repeat("a", 256),      // Too long.
		"-e xport --something volume", // Anything shell-ish.
	}

	for _, name := range invalid {
		err := validator(name)
		if err == nil {
			t.Errorf("expected %q to be rejected", name)
		}
	}
}

func Test_cephext_deleteVolumeOnlyReleasesLocalClaim(t *testing.T) {
	const imageName = "volume-8231d2e8-e306-40e4-8f42-a9d2475f2e05"

	d := &cephext{}
	vol := NewVolume(d, "pool", VolumeTypeContainer, ContentTypeFS, "instance", map[string]string{
		"ceph.rbd.image_name": imageName,
	}, map[string]string{})
	vol.mountCustomPath = filepath.Join(t.TempDir(), "claim")

	err := os.Mkdir(vol.mountCustomPath, 0o700)
	if err != nil {
		t.Fatalf("Failed creating local claim path: %v", err)
	}

	err = d.DeleteVolume(vol, nil)
	if err != nil {
		t.Fatalf("DeleteVolume failed without access to the external Ceph image: %v", err)
	}

	_, err = os.Stat(vol.mountCustomPath)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Local claim path still exists or could not be checked: %v", err)
	}

	if vol.Config()["ceph.rbd.image_name"] != imageName {
		t.Fatalf("DeleteVolume changed external RBD image reference to %q", vol.Config()["ceph.rbd.image_name"])
	}
}

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

func Test_cephext_idmapProvenanceValidation(t *testing.T) {
	d := &cephext{}
	validator := d.commonVolumeRules()["ceph.rbd.idmap_provenance"]

	for _, value := range []string{"", "normalized"} {
		err := validator(value)
		if err != nil {
			t.Errorf("expected %q to be valid, got: %v", value, err)
		}
	}

	for _, value := range []string{"shifted", "[]", "trusted"} {
		err := validator(value)
		if err == nil {
			t.Errorf("expected %q to be rejected", value)
		}
	}
}

func TestCephextExistingMountMatchesTarget(t *testing.T) {
	validMountinfo := []string{
		"104", "31", "251:0", "/", "/var/lib/incus/storage-pools/cinder/containers/instance", "rw", "-", "ext4", "/dev/rbd7", "rw",
	}

	matchingMapping := []cephRBDMapping{{DevicePath: "/dev/rbd7", PoolID: 29, ImageID: "aaaaaaaaaaaaaa"}}
	if !cephextExistingMountMatchesTarget(validMountinfo, matchingMapping) {
		t.Fatal("Expected exact RBD mount source to be accepted")
	}

	for name, tc := range map[string]struct {
		mountinfo []string
		mappings  []cephRBDMapping
	}{
		"different device": {
			mountinfo: validMountinfo,
			mappings:  []cephRBDMapping{{DevicePath: "/dev/rbd8", PoolID: 29, ImageID: "aaaaaaaaaaaaaa"}},
		},
		"non RBD source": {
			mountinfo: append(append([]string{}, validMountinfo[:8]...), append([]string{"/dev/sda1"}, validMountinfo[9:]...)...),
			mappings:  matchingMapping,
		},
		"no exact mapping": {
			mountinfo: validMountinfo,
			mappings:  nil,
		},
		"multiple matching mappings": {
			mountinfo: validMountinfo,
			mappings: []cephRBDMapping{
				{DevicePath: "/dev/rbd7", PoolID: 29, ImageID: "aaaaaaaaaaaaaa"},
				{DevicePath: "/dev/rbd8", PoolID: 29, ImageID: "aaaaaaaaaaaaaa"},
			},
		},
		"malformed mountinfo": {
			mountinfo: []string{"104", "31", "251:0", "/", "/path", "rw"},
			mappings:  matchingMapping,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if cephextExistingMountMatchesTarget(tc.mountinfo, tc.mappings) {
				t.Fatal("Unexpectedly accepted a non-exact existing mount")
			}
		})
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

package drivers

import (
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

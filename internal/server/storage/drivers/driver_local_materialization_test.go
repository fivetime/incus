package drivers

import (
	"strings"
	"testing"
)

const testMaterializationOwnership = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestValidateMaterializationOwnership(t *testing.T) {
	digest, err := validateMaterializationOwnership(testMaterializationOwnership)
	if err != nil {
		t.Fatal(err)
	}

	if digest != strings.TrimPrefix(testMaterializationOwnership, "sha256:") {
		t.Fatalf("Unexpected digest %q", digest)
	}

	for _, value := range []string{"", "sha256:abc", "sha256:" + strings.Repeat("G", 64), strings.Repeat("0", 64)} {
		_, err := validateMaterializationOwnership(value)
		if err == nil {
			t.Fatalf("Invalid ownership marker %q was accepted", value)
		}
	}
}

func TestParseLVMVolumeIdentity(t *testing.T) {
	identity, err := parseLVMVolumeIdentity("  p3ygAT-VxNd-0z3P-arHa-LYsV-5dDH-1klQ1G\n")
	if err != nil {
		t.Fatal(err)
	}

	if identity != "lvm:p3ygAT-VxNd-0z3P-arHa-LYsV-5dDH-1klQ1G" {
		t.Fatalf("Unexpected LVM identity %q", identity)
	}

	for _, output := range []string{"", "uuid with spaces", "uuid@invalid"} {
		_, err := parseLVMVolumeIdentity(output)
		if err == nil {
			t.Fatalf("Invalid LVM identity %q was accepted", output)
		}
	}
}

func TestParseLVMMaterializationTags(t *testing.T) {
	tag := lvmMaterializationTagPrefix + strings.TrimPrefix(testMaterializationOwnership, "sha256:")
	tags, err := parseLVMMaterializationTags("ordinary," + tag + ",another")
	if err != nil {
		t.Fatal(err)
	}

	if len(tags) != 1 || tags[0] != tag {
		t.Fatalf("Unexpected LVM ownership tags %#v", tags)
	}

	_, err = parseLVMMaterializationTags("ordinary," + lvmMaterializationTagPrefix + "invalid")
	if err == nil {
		t.Fatal("Invalid LVM ownership tag was accepted")
	}
}

func TestParseZFSVolumeIdentity(t *testing.T) {
	identity, err := parseZFSVolumeIdentity("18446744073709551615\n")
	if err != nil {
		t.Fatal(err)
	}

	if identity != "zfs:18446744073709551615" {
		t.Fatalf("Unexpected ZFS identity %q", identity)
	}

	for _, output := range []string{"", "0", "01", "not-a-guid"} {
		_, err := parseZFSVolumeIdentity(output)
		if err == nil {
			t.Fatalf("Invalid ZFS GUID %q was accepted", output)
		}
	}
}

func TestValidateZFSIdentityBoundDeletion(t *testing.T) {
	if err := validateZFSIdentityBoundDeletion("", 1); err != nil {
		t.Fatalf("Ordinary ZFS deletion unexpectedly rejected clones: %v", err)
	}

	if err := validateZFSIdentityBoundDeletion("immutable", 0); err != nil {
		t.Fatalf("Clone-free identity deletion was rejected: %v", err)
	}

	if err := validateZFSIdentityBoundDeletion("immutable", 1); err == nil {
		t.Fatal("Identity-bound ZFS deletion accepted a dependent clone")
	}
}

func TestParseZFSMaterializationOwnership(t *testing.T) {
	ownership, err := parseZFSMaterializationOwnership(testMaterializationOwnership + "\tlocal\n")
	if err != nil {
		t.Fatal(err)
	}

	if ownership != testMaterializationOwnership {
		t.Fatalf("Unexpected ZFS ownership %q", ownership)
	}

	for _, output := range []string{"-\t-", testMaterializationOwnership + "\tinherited from pool", ""} {
		ownership, err := parseZFSMaterializationOwnership(output)
		if err != nil {
			t.Fatal(err)
		}

		if ownership != "" {
			t.Fatalf("Non-local ZFS ownership %q was accepted", ownership)
		}
	}

	_, err = parseZFSMaterializationOwnership("invalid\tlocal")
	if err == nil {
		t.Fatal("Invalid local ZFS ownership was accepted")
	}
}

func TestParseBtrfsVolumeIdentity(t *testing.T) {
	output := `Name: instance
	UUID: 96af5c57-a83a-4db8-86f9-e22ec1365f28
	Parent UUID: -
	Received UUID: -`
	identity, err := parseBtrfsVolumeIdentity(output)
	if err != nil {
		t.Fatal(err)
	}

	if identity != "btrfs:96af5c57-a83a-4db8-86f9-e22ec1365f28" {
		t.Fatalf("Unexpected Btrfs identity %q", identity)
	}

	for _, invalid := range []string{"Name: instance", "UUID: invalid"} {
		_, err := parseBtrfsVolumeIdentity(invalid)
		if err == nil {
			t.Fatalf("Invalid Btrfs output %q was accepted", invalid)
		}
	}
}

func TestDeleteMaterializedVolumeWithIdentity(t *testing.T) {
	vol := NewVolume(nil, "pool", VolumeTypeContainer, ContentTypeFS, "instance", nil, nil)
	actions := 0
	err := deleteMaterializedVolumeWithIdentity(vol, "immutable", func() error {
		actions++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if actions != 1 {
		t.Fatalf("Identity-bound delete action ran %d times", actions)
	}

	err = deleteMaterializedVolumeWithIdentity(vol, "", func() error {
		actions++
		return nil
	})
	if err == nil {
		t.Fatal("Identity-bound delete accepted an empty identity")
	}

	if actions != 1 {
		t.Fatal("Delete action ran without an expected identity")
	}
}

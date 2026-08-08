// Package rootfsidmap records which ID map a rootfs was shifted with, so an
// interrupted shift can be recognised and finished after a restart.
//
// It lives outside internal/instance on purpose: that package is imported by
// the client, and shared/idmap pulls in cgo and libcap, which the client must
// not need in order to build.
package rootfsidmap

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/lxc/incus/v7/shared/idmap"
)

func testRootfsIDMap(hostID int64, size int64) *idmap.Set {
	return &idmap.Set{Entries: []idmap.Entry{{
		IsUID:    true,
		IsGID:    true,
		HostID:   hostID,
		NSID:     0,
		MapRange: size,
	}}}
}

func testRootfsIDMapVolatile(t *testing.T, value *string) RootfsIDMapVolatileSet {
	t.Helper()

	return func(idmapJSON string) error {
		*value = idmapJSON
		return nil
	}
}

func TestRootfsIDMapProvenanceSeedsLegacyState(t *testing.T) {
	volumePath := t.TempDir()
	legacy := testRootfsIDMap(1000000, 65536)
	volatile := ""
	applyCalls := 0
	syncCalls := 0

	stable, err := RecoverRootfsIDMapProvenance(
		volumePath,
		legacy,
		func(from *idmap.Set, to *idmap.Set) error {
			applyCalls++
			return nil
		},
		func() error {
			syncCalls++
			return nil
		},
		testRootfsIDMapVolatile(t, &volatile),
	)
	if err != nil {
		t.Fatalf("Failed recovering missing provenance: %v", err)
	}

	if !stable.Equals(legacy) {
		t.Fatal("Seeded provenance does not match the legacy ID map")
	}

	if applyCalls != 0 || syncCalls != 0 {
		t.Fatalf("Seeding unexpectedly remapped the rootfs (apply=%d sync=%d)", applyCalls, syncCalls)
	}

	legacyJSON, err := legacy.ToJSON()
	if err != nil {
		t.Fatal(err)
	}

	if volatile != legacyJSON {
		t.Fatalf("Unexpected mirrored ID map %q", volatile)
	}

	provenance, exists, err := readRootfsIDMapProvenance(volumePath)
	if err != nil || !exists {
		t.Fatalf("Seeded provenance could not be read: exists=%t err=%v", exists, err)
	}

	if provenance.State != rootfsIDMapStateStable {
		t.Fatalf("Unexpected provenance state %q", provenance.State)
	}
}

func TestRootfsIDMapProvenanceNilMapIsStable(t *testing.T) {
	volumePath := t.TempDir()
	volatile := "stale"
	err := SeedNormalizedRootfsIDMapProvenance(volumePath)
	if err != nil {
		t.Fatal(err)
	}

	stable, err := RecoverRootfsIDMapProvenance(
		volumePath,
		nil,
		func(from *idmap.Set, to *idmap.Set) error {
			t.Fatal("Nil ID map recovery attempted a remap")
			return nil
		},
		func() error {
			t.Fatal("Nil ID map recovery attempted a sync")
			return nil
		},
		testRootfsIDMapVolatile(t, &volatile),
	)
	if err != nil {
		t.Fatal(err)
	}

	if stable != nil || volatile != "[]" {
		t.Fatalf("Unexpected nil-map state stable=%v volatile=%q", stable, volatile)
	}
}

func TestValidateRootfsIDMapProvenance(t *testing.T) {
	t.Run("Missing", func(t *testing.T) {
		err := ValidateRootfsIDMapProvenance(t.TempDir())
		if err == nil {
			t.Fatal("Expected missing rootfs ID map provenance to be rejected")
		}
	})

	t.Run("Stable", func(t *testing.T) {
		volumePath := t.TempDir()
		err := SeedNormalizedRootfsIDMapProvenance(volumePath)
		if err != nil {
			t.Fatal(err)
		}

		err = ValidateRootfsIDMapProvenance(volumePath)
		if err != nil {
			t.Fatalf("Stable rootfs ID map provenance was rejected: %v", err)
		}
	})

	t.Run("Transition", func(t *testing.T) {
		volumePath := t.TempDir()
		err := writeTransitionRootfsIDMapProvenance(
			volumePath,
			testRootfsIDMap(1000000, 65536),
			testRootfsIDMap(2000000, 65536),
		)
		if err != nil {
			t.Fatal(err)
		}

		err = ValidateRootfsIDMapProvenance(volumePath)
		if err != nil {
			t.Fatalf("Recoverable transition provenance was rejected: %v", err)
		}
	})
}

func TestValidateNormalizedRootfsIDMapProvenance(t *testing.T) {
	t.Run("Normalized", func(t *testing.T) {
		volumePath := t.TempDir()
		err := SeedNormalizedRootfsIDMapProvenance(volumePath)
		if err != nil {
			t.Fatal(err)
		}

		err = ValidateNormalizedRootfsIDMapProvenance(volumePath)
		if err != nil {
			t.Fatalf("Normalized provenance was rejected: %v", err)
		}
	})

	t.Run("Shifted", func(t *testing.T) {
		volumePath := t.TempDir()
		err := writeStableRootfsIDMapProvenance(volumePath, testRootfsIDMap(1000000, 65536))
		if err != nil {
			t.Fatal(err)
		}

		err = ValidateNormalizedRootfsIDMapProvenance(volumePath)
		if err == nil {
			t.Fatal("Shifted provenance was accepted as normalized")
		}
	})

	t.Run("Transition", func(t *testing.T) {
		volumePath := t.TempDir()
		err := writeTransitionRootfsIDMapProvenance(volumePath, testRootfsIDMap(1000000, 65536), nil)
		if err != nil {
			t.Fatal(err)
		}

		err = ValidateNormalizedRootfsIDMapProvenance(volumePath)
		if err == nil {
			t.Fatal("Transition provenance was accepted as normalized")
		}
	})
}

func TestSeedNormalizedRootfsIDMapProvenanceRejectsExistingNonNormalizedState(t *testing.T) {
	t.Run("Shifted", func(t *testing.T) {
		volumePath := t.TempDir()
		err := writeStableRootfsIDMapProvenance(volumePath, testRootfsIDMap(1000000, 65536))
		if err != nil {
			t.Fatal(err)
		}

		if err := SeedNormalizedRootfsIDMapProvenance(volumePath); err == nil {
			t.Fatal("Existing shifted provenance was accepted as a normalized seed")
		}
	})

	t.Run("Transition", func(t *testing.T) {
		volumePath := t.TempDir()
		err := writeTransitionRootfsIDMapProvenance(volumePath, testRootfsIDMap(1000000, 65536), nil)
		if err != nil {
			t.Fatal(err)
		}

		if err := SeedNormalizedRootfsIDMapProvenance(volumePath); err == nil {
			t.Fatal("Existing transition provenance was accepted as a normalized seed")
		}
	})
}

func TestValidateRootfsIDMapRejectsInternalOverlap(t *testing.T) {
	tests := []struct {
		name    string
		entries []idmap.Entry
	}{
		{
			name: "host range",
			entries: []idmap.Entry{
				{IsUID: true, HostID: 1000000, NSID: 0, MapRange: 65536},
				{IsUID: true, HostID: 1032768, NSID: 200000, MapRange: 65536},
			},
		},
		{
			name: "namespace range",
			entries: []idmap.Entry{
				{IsGID: true, HostID: 1000000, NSID: 0, MapRange: 65536},
				{IsGID: true, HostID: 2000000, NSID: 32768, MapRange: 65536},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRootfsIDMap(&idmap.Set{Entries: tt.entries})
			if err == nil {
				t.Fatal("Expected overlapping ID map entries to be rejected")
			}
		})
	}
}

func TestValidateRootfsIDMapRejectsValuesOutsideUIDSpace(t *testing.T) {
	validBoundary := &idmap.Set{Entries: []idmap.Entry{{
		IsUID:    true,
		HostID:   rootfsIDMapIDSpaceSize - 1,
		NSID:     rootfsIDMapIDSpaceSize - 1,
		MapRange: 1,
	}}}
	if err := validateRootfsIDMap(validBoundary); err != nil {
		t.Fatalf("Valid 32-bit boundary was rejected: %v", err)
	}

	tests := []idmap.Entry{
		{IsUID: true, HostID: rootfsIDMapIDSpaceSize, NSID: 0, MapRange: 1},
		{IsGID: true, HostID: 0, NSID: rootfsIDMapIDSpaceSize, MapRange: 1},
		{IsUID: true, HostID: rootfsIDMapIDSpaceSize - 1, NSID: 0, MapRange: 2},
		{IsGID: true, HostID: 0, NSID: rootfsIDMapIDSpaceSize - 1, MapRange: 2},
	}

	for i, entry := range tests {
		if err := validateRootfsIDMap(&idmap.Set{Entries: []idmap.Entry{entry}}); err == nil {
			t.Fatalf("Entry %d outside the 32-bit UID/GID space was accepted", i)
		}
	}
}

func TestRootfsIDMapProvenanceMissingWithoutEvidenceFailsClosed(t *testing.T) {
	volumePath := t.TempDir()
	applyCalls := 0
	volatile := "[]"

	_, err := RecoverRootfsIDMapProvenance(
		volumePath,
		nil,
		func(from *idmap.Set, to *idmap.Set) error {
			applyCalls++
			return nil
		},
		func() error { return nil },
		testRootfsIDMapVolatile(t, &volatile),
	)
	if err == nil {
		t.Fatal("Expected markerless root without evidence to fail closed")
	}

	if applyCalls != 0 {
		t.Fatal("Markerless root was modified before provenance was proven")
	}

	_, statErr := os.Stat(filepath.Join(volumePath, RootfsIDMapProvenanceFilename))
	if !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Markerless root unexpectedly gained provenance: %v", statErr)
	}
}

func TestRootfsIDMapTransitionRecovery(t *testing.T) {
	volumePath := t.TempDir()
	from := testRootfsIDMap(1000000, 65536)
	to := testRootfsIDMap(2000000, 65536)
	volatile := ""

	err := writeTransitionRootfsIDMapProvenance(volumePath, from, to)
	if err != nil {
		t.Fatal(err)
	}

	applyCalls := 0
	syncCalls := 0
	stable, err := RecoverRootfsIDMapProvenance(
		volumePath,
		nil,
		func(actualFrom *idmap.Set, actualTo *idmap.Set) error {
			applyCalls++
			if !actualFrom.Equals(from) || !actualTo.Equals(to) {
				t.Fatal("Recovered the wrong ID map transition")
			}

			return nil
		},
		func() error {
			syncCalls++
			return nil
		},
		testRootfsIDMapVolatile(t, &volatile),
	)
	if err != nil {
		t.Fatal(err)
	}

	if applyCalls != 1 || syncCalls != 1 || !stable.Equals(to) {
		t.Fatalf("Unexpected recovery result apply=%d sync=%d stable=%v", applyCalls, syncCalls, stable)
	}

	toJSON, err := to.ToJSON()
	if err != nil {
		t.Fatal(err)
	}

	if volatile != toJSON {
		t.Fatalf("Unexpected mirrored transition target %q", volatile)
	}

	provenance, _, err := readRootfsIDMapProvenance(volumePath)
	if err != nil {
		t.Fatal(err)
	}

	if provenance.State != rootfsIDMapStateStable {
		t.Fatalf("Recovered transition remains in state %q", provenance.State)
	}
}

func TestRootfsIDMapTransitionFailureRemainsRecoverable(t *testing.T) {
	volumePath := t.TempDir()
	from := testRootfsIDMap(1000000, 65536)
	to := testRootfsIDMap(2000000, 65536)
	volatile := ""

	err := writeStableRootfsIDMapProvenance(volumePath, from)
	if err != nil {
		t.Fatal(err)
	}

	expectedErr := errors.New("injected remap failure")
	err = TransitionRootfsIDMapProvenance(
		volumePath,
		from,
		to,
		func(from *idmap.Set, to *idmap.Set) error { return expectedErr },
		func() error { return nil },
		testRootfsIDMapVolatile(t, &volatile),
	)
	if !errors.Is(err, expectedErr) {
		t.Fatalf("Expected remap failure, got %v", err)
	}

	provenance, _, err := readRootfsIDMapProvenance(volumePath)
	if err != nil {
		t.Fatal(err)
	}

	if provenance.State != rootfsIDMapStateTransition {
		t.Fatalf("Failed transition was changed to %q", provenance.State)
	}

	applyCalls := 0
	stable, err := RecoverRootfsIDMapProvenance(
		volumePath,
		from,
		func(actualFrom *idmap.Set, actualTo *idmap.Set) error {
			applyCalls++
			return nil
		},
		func() error { return nil },
		testRootfsIDMapVolatile(t, &volatile),
	)
	if err != nil || applyCalls != 1 || !stable.Equals(to) {
		t.Fatalf("Transition did not recover: stable=%v calls=%d err=%v", stable, applyCalls, err)
	}
}

func TestRootfsIDMapTransitionSyncFailureRemainsJournaled(t *testing.T) {
	volumePath := t.TempDir()
	from := testRootfsIDMap(1000000, 65536)
	to := testRootfsIDMap(2000000, 65536)
	volatile := ""

	err := writeStableRootfsIDMapProvenance(volumePath, from)
	if err != nil {
		t.Fatal(err)
	}

	expectedErr := errors.New("injected sync failure")
	err = TransitionRootfsIDMapProvenance(
		volumePath,
		from,
		to,
		func(from *idmap.Set, to *idmap.Set) error { return nil },
		func() error { return expectedErr },
		testRootfsIDMapVolatile(t, &volatile),
	)
	if !errors.Is(err, expectedErr) {
		t.Fatalf("Expected sync failure, got %v", err)
	}

	provenance, _, err := readRootfsIDMapProvenance(volumePath)
	if err != nil || provenance.State != rootfsIDMapStateTransition {
		t.Fatalf("Sync failure lost its transition journal: state=%v err=%v", provenance, err)
	}
}

func TestRootfsIDMapTransitionVolatileFailureKeepsStableMarker(t *testing.T) {
	volumePath := t.TempDir()
	from := testRootfsIDMap(1000000, 65536)
	to := testRootfsIDMap(2000000, 65536)

	err := writeStableRootfsIDMapProvenance(volumePath, from)
	if err != nil {
		t.Fatal(err)
	}

	expectedErr := errors.New("injected volatile failure")
	err = TransitionRootfsIDMapProvenance(
		volumePath,
		from,
		to,
		func(from *idmap.Set, to *idmap.Set) error { return nil },
		func() error { return nil },
		func(idmapJSON string) error { return expectedErr },
	)
	if !errors.Is(err, expectedErr) {
		t.Fatalf("Expected volatile failure, got %v", err)
	}

	provenance, _, err := readRootfsIDMapProvenance(volumePath)
	if err != nil || provenance.State != rootfsIDMapStateStable {
		t.Fatalf("Volatile failure did not retain stable provenance: state=%v err=%v", provenance, err)
	}

	stable, err := rootfsIDMapFromJSON(provenance.IDMap)
	if err != nil || !stable.Equals(to) {
		t.Fatalf("Stable provenance does not contain the completed target: map=%v err=%v", stable, err)
	}

	applyCalls := 0
	volatile := ""
	stable, err = RecoverRootfsIDMapProvenance(
		volumePath,
		from,
		func(from *idmap.Set, to *idmap.Set) error {
			applyCalls++
			return nil
		},
		func() error { return nil },
		testRootfsIDMapVolatile(t, &volatile),
	)
	if err != nil || applyCalls != 0 || !stable.Equals(to) {
		t.Fatalf("Stable marker did not recover volatile-only failure: map=%v apply=%d err=%v", stable, applyCalls, err)
	}
}

func TestRootfsIDMapTransitionRejectsOverlappingHostRanges(t *testing.T) {
	volumePath := t.TempDir()
	from := testRootfsIDMap(1000000, 65536)
	to := testRootfsIDMap(1032768, 65536)
	volatile := ""

	err := writeStableRootfsIDMapProvenance(volumePath, from)
	if err != nil {
		t.Fatal(err)
	}

	applyCalls := 0
	err = TransitionRootfsIDMapProvenance(
		volumePath,
		from,
		to,
		func(from *idmap.Set, to *idmap.Set) error {
			applyCalls++
			return nil
		},
		func() error { return nil },
		testRootfsIDMapVolatile(t, &volatile),
	)
	if err == nil {
		t.Fatal("Expected overlapping host ranges to be rejected")
	}

	if applyCalls != 0 {
		t.Fatal("Overlapping transition changed the rootfs")
	}

	provenance, _, readErr := readRootfsIDMapProvenance(volumePath)
	if readErr != nil || provenance.State != rootfsIDMapStateStable {
		t.Fatalf("Overlapping transition changed the journal: state=%v err=%v", provenance, readErr)
	}
}

func TestRootfsIDMapTransitionRejectsHostNamespaceOverlap(t *testing.T) {
	volumePath := t.TempDir()
	from := testRootfsIDMap(100, 200)
	volatile := ""

	err := writeStableRootfsIDMapProvenance(volumePath, from)
	if err != nil {
		t.Fatal(err)
	}

	applyCalls := 0
	err = TransitionRootfsIDMapProvenance(
		volumePath,
		from,
		nil,
		func(from *idmap.Set, to *idmap.Set) error {
			applyCalls++
			return nil
		},
		func() error { return nil },
		testRootfsIDMapVolatile(t, &volatile),
	)
	if err == nil {
		t.Fatal("Expected host and namespace range overlap to be rejected")
	}

	if applyCalls != 0 {
		t.Fatal("Unsafe low ID map changed the rootfs")
	}
}

func TestRootfsIDMapProvenanceFollowsCloneAndRemapsOnClaim(t *testing.T) {
	sourcePath := t.TempDir()
	clonePath := t.TempDir()
	sourceMap := testRootfsIDMap(1000000, 65536)
	claimMap := testRootfsIDMap(3000000, 65536)
	volatile := ""

	err := writeStableRootfsIDMapProvenance(sourcePath, sourceMap)
	if err != nil {
		t.Fatal(err)
	}

	marker, err := os.ReadFile(filepath.Join(sourcePath, RootfsIDMapProvenanceFilename))
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(clonePath, RootfsIDMapProvenanceFilename), marker, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	stable, err := RecoverRootfsIDMapProvenance(
		clonePath,
		nil,
		func(from *idmap.Set, to *idmap.Set) error {
			t.Fatal("Stable cloned provenance unexpectedly replayed")
			return nil
		},
		func() error { return nil },
		testRootfsIDMapVolatile(t, &volatile),
	)
	if err != nil || !stable.Equals(sourceMap) {
		t.Fatalf("Clone did not recover source provenance: stable=%v err=%v", stable, err)
	}

	applyCalls := 0
	err = TransitionRootfsIDMapProvenance(
		clonePath,
		stable,
		claimMap,
		func(from *idmap.Set, to *idmap.Set) error {
			applyCalls++
			if !from.Equals(sourceMap) || !to.Equals(claimMap) {
				t.Fatal("Clone claim remapped from or to the wrong ID map")
			}

			return nil
		},
		func() error { return nil },
		testRootfsIDMapVolatile(t, &volatile),
	)
	if err != nil || applyCalls != 1 {
		t.Fatalf("Clone claim remap failed: calls=%d err=%v", applyCalls, err)
	}

	claimed, _, err := readRootfsIDMapProvenance(clonePath)
	if err != nil {
		t.Fatal(err)
	}

	claimedMap, err := rootfsIDMapFromJSON(claimed.IDMap)
	if err != nil || !claimedMap.Equals(claimMap) {
		t.Fatalf("Clone provenance did not converge to the claimant map: map=%v err=%v", claimedMap, err)
	}
}

func TestRootfsIDMapProvenanceRejectsNonRegularMarker(t *testing.T) {
	volumePath := t.TempDir()
	err := os.Mkdir(filepath.Join(volumePath, RootfsIDMapProvenanceFilename), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = readRootfsIDMapProvenance(volumePath)
	if err == nil {
		t.Fatal("Expected non-regular provenance marker to be rejected")
	}
}

package drivers

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/lxc/incus/v7/shared/subprocess"
)

func testRBDIdentity(poolID uint64, id string) cephRBDVolumeIdentity {
	return cephRBDVolumeIdentity{PoolID: poolID, ID: id, BlockNamePrefix: "rbd_data." + id}
}

type fakeCephTrashImage struct {
	entry    cephRBDTrashEntry
	identity cephRBDVolumeIdentity
}

type fakeCephIdentityReleaseStore struct {
	poolID             uint64
	active             map[string]cephRBDVolumeIdentity
	trash              map[string]fakeCephTrashImage
	snapshots          map[string]map[string][]string
	renameHook         func(oldName string, newName string)
	trashMoveHook      func(name string)
	trashMoveAfter     func(name string)
	trashRemoveHook    func(imageID string)
	trashRemoveFailure error
}

func newFakeCephIdentityReleaseStore() *fakeCephIdentityReleaseStore {
	return &fakeCephIdentityReleaseStore{
		poolID:    29,
		active:    map[string]cephRBDVolumeIdentity{},
		trash:     map[string]fakeCephTrashImage{},
		snapshots: map[string]map[string][]string{},
	}
}

func (s *fakeCephIdentityReleaseStore) PoolIdentity() (uint64, error) {
	return s.poolID, nil
}

func (s *fakeCephIdentityReleaseStore) ImageIdentity(name string) (cephRBDVolumeIdentity, error) {
	identity, found := s.active[name]
	if !found {
		return cephRBDVolumeIdentity{}, errCephIdentityImageNotFound
	}

	return identity, nil
}

func (s *fakeCephIdentityReleaseStore) RenameImage(oldName string, newName string) error {
	if s.renameHook != nil {
		hook := s.renameHook
		s.renameHook = nil
		hook(oldName, newName)
	}

	identity, found := s.active[oldName]
	if !found {
		return errCephIdentityImageNotFound
	}
	if _, found := s.active[newName]; found {
		return errors.New("destination exists")
	}

	delete(s.active, oldName)
	s.active[newName] = identity
	if snapshots, found := s.snapshots[oldName]; found {
		delete(s.snapshots, oldName)
		s.snapshots[newName] = snapshots
	}

	return nil
}

func (s *fakeCephIdentityReleaseStore) TrashEntries() ([]cephRBDTrashEntry, error) {
	entries := make([]cephRBDTrashEntry, 0, len(s.trash))
	for _, image := range s.trash {
		entries = append(entries, image.entry)
	}

	sort.Slice(entries, func(i int, j int) bool { return entries[i].ID < entries[j].ID })
	return entries, nil
}

func (s *fakeCephIdentityReleaseStore) TrashMove(name string) error {
	if s.trashMoveHook != nil {
		hook := s.trashMoveHook
		s.trashMoveHook = nil
		hook(name)
	}

	identity, found := s.active[name]
	if !found {
		return errCephIdentityImageNotFound
	}

	delete(s.active, name)
	s.trash[identity.ID] = fakeCephTrashImage{
		entry:    cephRBDTrashEntry{ID: identity.ID, Name: name},
		identity: identity,
	}

	if s.trashMoveAfter != nil {
		hook := s.trashMoveAfter
		s.trashMoveAfter = nil
		hook(name)
	}

	return nil
}

func (s *fakeCephIdentityReleaseStore) TrashRestore(imageID string, name string) error {
	image, found := s.trash[imageID]
	if !found {
		return errCephIdentityImageNotFound
	}
	if _, found := s.active[name]; found {
		return errors.New("destination exists")
	}

	delete(s.trash, imageID)
	s.active[name] = image.identity
	return nil
}

func (s *fakeCephIdentityReleaseStore) TrashRemove(imageID string) error {
	if s.trashRemoveHook != nil {
		hook := s.trashRemoveHook
		s.trashRemoveHook = nil
		hook(imageID)
	}

	if s.trashRemoveFailure != nil {
		err := s.trashRemoveFailure
		s.trashRemoveFailure = nil
		return err
	}

	if _, found := s.trash[imageID]; !found {
		return errCephIdentityImageNotFound
	}

	delete(s.trash, imageID)
	return nil
}

func (s *fakeCephIdentityReleaseStore) SnapshotNames(name string) ([]string, error) {
	if _, found := s.active[name]; !found {
		return nil, errCephIdentityImageNotFound
	}

	names := []string{}
	for snapshotName := range s.snapshots[name] {
		names = append(names, snapshotName)
	}

	sort.Strings(names)
	return names, nil
}

func (s *fakeCephIdentityReleaseStore) SnapshotChildren(name string, snapshotName string) ([]string, error) {
	if _, found := s.active[name]; !found {
		return nil, errCephIdentityImageNotFound
	}

	return append([]string(nil), s.snapshots[name][snapshotName]...), nil
}

func (s *fakeCephIdentityReleaseStore) SnapshotUnprotect(name string, snapshotName string) error {
	if _, found := s.active[name]; !found {
		return errCephIdentityImageNotFound
	}
	if len(s.snapshots[name][snapshotName]) > 0 {
		return errors.New("snapshot has children")
	}

	return nil
}

func (s *fakeCephIdentityReleaseStore) SnapshotPurge(name string) error {
	if _, found := s.active[name]; !found {
		return errCephIdentityImageNotFound
	}

	delete(s.snapshots, name)
	return nil
}

func TestFindRBDMappingsByIdentity(t *testing.T) {
	root := t.TempDir()
	writeMapping := func(index int, poolID uint64, imageID string) {
		dir := filepath.Join(root, fmt.Sprintf("%d", index))
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "pool_id"), []byte(fmt.Sprintf("%d\n", poolID)), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "image_id"), []byte(imageID+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writeMapping(1, 29, "aaaaaaaaaaaaaa")
	writeMapping(2, 29, "bbbbbbbbbbbbbb")
	writeMapping(3, 30, "aaaaaaaaaaaaaa")
	writeMapping(4, 29, "aaaaaaaaaaaaaa")

	mappings, err := findRBDMappingsByIdentity(root, testRBDIdentity(29, "aaaaaaaaaaaaaa"))
	if err != nil {
		t.Fatal(err)
	}
	if len(mappings) != 2 || mappings[0].DevicePath != "/dev/rbd1" || mappings[1].DevicePath != "/dev/rbd4" {
		t.Fatalf("Unexpected exact mappings: %+v", mappings)
	}
}

func TestParseRBDPoolIdentity(t *testing.T) {
	identity, err := parseRBDPoolIdentity(`{"epoch":10,"pool":"incus-rootfs","pool_id":29}`, "incus-rootfs")
	if err != nil || identity.PoolID != 29 {
		t.Fatalf("Pool identity was not parsed: identity=%+v err=%v", identity, err)
	}

	for _, input := range []string{
		`{"pool":"other","pool_id":29}`,
		`{"pool":"incus-rootfs"}`,
		`not-json`,
	} {
		if _, err := parseRBDPoolIdentity(input, "incus-rootfs"); err == nil {
			t.Fatalf("Invalid pool mapping %q was accepted", input)
		}
	}
}

func TestParseRBDTrashEntriesRejectsNonCanonicalIDs(t *testing.T) {
	for _, input := range []string{
		`[{"id":"","name":"image"}]`,
		`[{"id":"ABCDEF","name":"image"}]`,
		`[{"id":"abcdeg","name":"image"}]`,
		`[{"id":"abcdef","name":""}]`,
		`[{"id":"abcdef","name":"one"},{"id":"abcdef","name":"two"}]`,
	} {
		_, err := parseRBDTrashEntries(input)
		if err == nil {
			t.Fatalf("Expected trash input %q to be rejected", input)
		}
	}

	entries, err := parseRBDTrashEntries(`[{"id":"abcdef","name":"image"}]`)
	if err != nil {
		t.Fatalf("Expected valid trash entry: %v", err)
	}
	if len(entries) != 1 || entries[0] != (cephRBDTrashEntry{ID: "abcdef", Name: "image"}) {
		t.Fatalf("Unexpected trash entries: %#v", entries)
	}
}

// Ceph builds image IDs by concatenating an instance ID and a random value
// formatted with std::hex, which does not zero-pad, so odd-length IDs are
// both legal and common. Rejecting them fails roughly half of all builds.
func TestRBDImageIDsMayBeOddLength(t *testing.T) {
	for _, id := range []string{"a", "abc", "e016c6f832e4b5f", "1234567"} {
		identity := cephRBDVolumeIdentity{
			PoolID:          29,
			ID:              id,
			BlockNamePrefix: "rbd_data." + id,
		}

		if err := identity.validate(); err != nil {
			t.Fatalf("Odd-length RBD image ID %q was rejected: %v", id, err)
		}

		entries, err := parseRBDTrashEntries(`[{"id":"` + id + `","name":"image"}]`)
		if err != nil || len(entries) != 1 {
			t.Fatalf("Odd-length trash ID %q was rejected: %v", id, err)
		}
	}

	for _, id := range []string{"", "ABC", "xyz", "abc-def", "12 34"} {
		identity := cephRBDVolumeIdentity{
			PoolID:          29,
			ID:              id,
			BlockNamePrefix: "rbd_data." + id,
		}

		if err := identity.validate(); err == nil {
			t.Fatalf("Non-canonical RBD image ID %q was accepted", id)
		}
	}
}

func TestDeleteRBDVolumeWithIdentity(t *testing.T) {
	expected := testRBDIdentity(29, "aaaaaaaaaaaaaa")
	replacement := testRBDIdentity(29, "bbbbbbbbbbbbbb")
	originalName := "container_nova_instance-00000001"
	tombstoneName := cephIdentityName(cephIdentityReleaseTombstonePrefix, expected)

	t.Run("fresh deletion", func(t *testing.T) {
		store := newFakeCephIdentityReleaseStore()
		store.active[originalName] = expected

		replacementExists, err := deleteRBDVolumeWithIdentity(store, originalName, expected)
		if err != nil || replacementExists {
			t.Fatalf("Exact deletion failed: replacement=%t err=%v", replacementExists, err)
		}
		if len(store.active) != 0 || len(store.trash) != 0 {
			t.Fatalf("Exact image survived deletion: active=%v trash=%v", store.active, store.trash)
		}
	})

	t.Run("stale delete leaves same-name replacement untouched", func(t *testing.T) {
		store := newFakeCephIdentityReleaseStore()
		store.active[originalName] = replacement

		replacementExists, err := deleteRBDVolumeWithIdentity(store, originalName, expected)
		if err != nil || !replacementExists {
			t.Fatalf("Stale delete did not identify replacement: replacement=%t err=%v", replacementExists, err)
		}
		if store.active[originalName] != replacement || len(store.trash) != 0 {
			t.Fatalf("Same-name replacement was changed: active=%v trash=%v", store.active, store.trash)
		}
	})

	t.Run("rename ABA quarantines replacement", func(t *testing.T) {
		store := newFakeCephIdentityReleaseStore()
		store.active[originalName] = expected
		store.renameHook = func(oldName string, _ string) {
			delete(store.active, oldName)
			store.active[oldName] = replacement
		}

		_, err := deleteRBDVolumeWithIdentity(store, originalName, expected)
		if err == nil {
			t.Fatal("Rename ABA was accepted")
		}
		quarantineName := cephIdentityName(cephIdentityReleaseQuarantinePrefix, replacement)
		if store.active[quarantineName] != replacement {
			t.Fatalf("Replacement was not preserved in quarantine: active=%v", store.active)
		}
		if len(store.trash) != 0 {
			t.Fatal("Replacement reached destructive trash removal")
		}
	})

	t.Run("crash after rename resumes tombstone", func(t *testing.T) {
		store := newFakeCephIdentityReleaseStore()
		store.active[tombstoneName] = expected

		_, err := deleteRBDVolumeWithIdentity(store, originalName, expected)
		if err != nil || len(store.active) != 0 || len(store.trash) != 0 {
			t.Fatalf("Tombstone retry did not converge: active=%v trash=%v err=%v", store.active, store.trash, err)
		}
	})

	t.Run("crash after trash move restores and resumes", func(t *testing.T) {
		store := newFakeCephIdentityReleaseStore()
		store.trash[expected.ID] = fakeCephTrashImage{
			entry:    cephRBDTrashEntry{ID: expected.ID, Name: tombstoneName},
			identity: expected,
		}

		_, err := deleteRBDVolumeWithIdentity(store, originalName, expected)
		if err != nil || len(store.active) != 0 || len(store.trash) != 0 {
			t.Fatalf("Trash retry did not converge: active=%v trash=%v err=%v", store.active, store.trash, err)
		}
	})

	t.Run("lost final acknowledgement is idempotent", func(t *testing.T) {
		store := newFakeCephIdentityReleaseStore()
		replacementExists, err := deleteRBDVolumeWithIdentity(store, originalName, expected)
		if err != nil || replacementExists {
			t.Fatalf("Already removed exact image was not accepted: replacement=%t err=%v", replacementExists, err)
		}
	})

	t.Run("concurrent trash move is retried", func(t *testing.T) {
		store := newFakeCephIdentityReleaseStore()
		store.active[originalName] = expected
		store.trashMoveAfter = func(string) {
			// Another daemon completes exact removal before this caller observes trash.
			delete(store.trash, expected.ID)
		}

		_, err := deleteRBDVolumeWithIdentity(store, originalName, expected)
		if err != nil {
			t.Fatalf("Concurrent exact trash removal was not idempotent: %v", err)
		}
	})

	t.Run("concurrent restore before trash removal is deleted on retry", func(t *testing.T) {
		store := newFakeCephIdentityReleaseStore()
		store.active[originalName] = expected
		store.trashRemoveHook = func(imageID string) {
			image := store.trash[imageID]
			delete(store.trash, imageID)
			store.active[originalName] = image.identity
		}

		_, err := deleteRBDVolumeWithIdentity(store, originalName, expected)
		if err != nil {
			t.Fatalf("Concurrent exact restore was not reconciled: %v", err)
		}
		if len(store.active) != 0 || len(store.trash) != 0 {
			t.Fatalf("Restored exact image survived a successful delete: active=%v trash=%v", store.active, store.trash)
		}
	})

	t.Run("trash failure is recoverable", func(t *testing.T) {
		store := newFakeCephIdentityReleaseStore()
		store.active[originalName] = expected
		store.trashRemoveFailure = errors.New("temporary Ceph failure")

		_, err := deleteRBDVolumeWithIdentity(store, originalName, expected)
		if err == nil {
			t.Fatal("Trash removal failure was hidden")
		}
		if _, found := store.trash[expected.ID]; !found {
			t.Fatal("Failed removal did not retain exact image in trash")
		}

		_, err = deleteRBDVolumeWithIdentity(store, originalName, expected)
		if err != nil || len(store.trash) != 0 {
			t.Fatalf("Trash retry did not finish: trash=%v err=%v", store.trash, err)
		}
	})

	t.Run("dependent clone fails closed", func(t *testing.T) {
		store := newFakeCephIdentityReleaseStore()
		store.active[originalName] = expected
		store.snapshots[originalName] = map[string][]string{"snap0": {"pool/clone"}}

		_, err := deleteRBDVolumeWithIdentity(store, originalName, expected)
		if err == nil {
			t.Fatal("Dependent clone was silently converted to a completed deletion")
		}
		if store.active[tombstoneName] != expected || len(store.trash) != 0 {
			t.Fatalf("Dependent clone did not retain exact tombstone: active=%v trash=%v", store.active, store.trash)
		}
	})

	t.Run("snapshot without clones is purged", func(t *testing.T) {
		store := newFakeCephIdentityReleaseStore()
		store.active[originalName] = expected
		store.snapshots[originalName] = map[string][]string{"snap0": {}}

		_, err := deleteRBDVolumeWithIdentity(store, originalName, expected)
		if err != nil || len(store.active) != 0 || len(store.trash) != 0 {
			t.Fatalf("Clone-free snapshot deletion failed: active=%v trash=%v err=%v", store.active, store.trash, err)
		}
	})

	t.Run("trash ABA restores replacement to quarantine", func(t *testing.T) {
		store := newFakeCephIdentityReleaseStore()
		store.active[originalName] = expected
		store.trashMoveHook = func(name string) {
			delete(store.active, name)
			store.active[name] = replacement
		}

		_, err := deleteRBDVolumeWithIdentity(store, originalName, expected)
		if err == nil {
			t.Fatal("Trash ABA was accepted")
		}
		quarantineName := cephIdentityName(cephIdentityReleaseQuarantinePrefix, replacement)
		if store.active[quarantineName] != replacement {
			t.Fatalf("Trashed replacement was not restored to quarantine: active=%v trash=%v", store.active, store.trash)
		}
	})
}

func TestCephIdentityReleaseIntegration(t *testing.T) {
	poolName := os.Getenv("INCUS_TEST_CEPH_IDENTITY_RELEASE_POOL")
	if poolName == "" {
		t.Skip("Set INCUS_TEST_CEPH_IDENTITY_RELEASE_POOL to run the destructive unique-name Ceph probe")
	}

	clusterName := os.Getenv("INCUS_TEST_CEPH_CLUSTER")
	if clusterName == "" {
		clusterName = CephDefaultCluster
	}
	userName := os.Getenv("INCUS_TEST_CEPH_USER")
	if userName == "" {
		userName = CephDefaultUser
	}

	driver := &ceph{common: common{
		name: "identity-release-probe",
		config: map[string]string{
			"ceph.cluster_name":  clusterName,
			"ceph.user.name":     userName,
			"ceph.osd.pool_name": poolName,
		},
	}}
	logicalName := fmt.Sprintf("codex-identity-release-%d", time.Now().UnixNano())
	vol := NewVolume(driver, driver.name, VolumeTypeContainer, ContentTypeFS, logicalName, nil, driver.config)
	rawName := driver.getRBDVolumeName(vol, "", false)

	_, err := subprocess.RunCommand("rbd", "--id", userName, "--cluster", clusterName, "--pool", poolName, "create", "--size", "16M", rawName)
	if err != nil {
		t.Fatal(err)
	}

	var expected cephRBDVolumeIdentity
	defer func() {
		names := []string{rawName}
		if expected.ID != "" {
			names = append(names, cephIdentityName(cephIdentityReleaseTombstonePrefix, expected), cephIdentityName(cephIdentityReleaseQuarantinePrefix, expected))
		}

		for _, name := range names {
			_, _ = subprocess.RunCommand("rbd", "--id", userName, "--cluster", clusterName, "--pool", poolName, "snap", "purge", name)
			_, _ = subprocess.RunCommand("rbd", "--id", userName, "--cluster", clusterName, "--pool", poolName, "rm", name)
		}
		if expected.ID != "" {
			_, _ = subprocess.RunCommand("rbd", "--id", userName, "--cluster", clusterName, "--pool", poolName, "trash", "rm", expected.ID, "--force")
		}
	}()

	identityValue, err := driver.GetVolumeIdentity(vol)
	if err != nil {
		t.Fatal(err)
	}
	expected, err = parseCanonicalRBDVolumeIdentity(identityValue)
	if err != nil {
		t.Fatal(err)
	}

	_, err = subprocess.RunCommand("rbd", "--id", userName, "--cluster", clusterName, "--pool", poolName, "map", rawName)
	if err != nil {
		t.Fatal(err)
	}

	hasLocalState, err := driver.HasVolumeLocalState(vol, identityValue)
	if err != nil || !hasLocalState {
		t.Fatalf("Exact mapped image was not detected: state=%t err=%v", hasLocalState, err)
	}
	mappings, err := findRBDMappingsByIdentity("/sys/devices/rbd", expected)
	if err != nil || len(mappings) == 0 {
		t.Fatalf("Exact mapped image identity was not found in sysfs: mappings=%v err=%v", mappings, err)
	}
	for _, mapping := range mappings {
		if mapping.PoolID != expected.PoolID || mapping.ImageID != expected.ID {
			t.Fatalf("Unexpected mapped image identity: %#v expected=%#v", mapping, expected)
		}
	}

	store := cephIdentityReleaseAdapter{driver: driver}
	tombstoneName := cephIdentityName(cephIdentityReleaseTombstonePrefix, expected)
	err = store.RenameImage(rawName, tombstoneName)
	if err != nil {
		t.Fatal(err)
	}

	// Recreate the logical name to prove that exact deletion removes A without touching same-name B.
	_, err = subprocess.RunCommand("rbd", "--id", userName, "--cluster", clusterName, "--pool", poolName, "create", "--size", "16M", rawName)
	if err != nil {
		t.Fatal(err)
	}
	replacementIdentityValue, err := driver.GetVolumeIdentity(vol)
	if err != nil {
		t.Fatal(err)
	}
	replacementIdentity, err := parseCanonicalRBDVolumeIdentity(replacementIdentityValue)
	if err != nil {
		t.Fatal(err)
	}
	if replacementIdentity.PoolID != expected.PoolID || replacementIdentity.ID == expected.ID {
		t.Fatalf("Same-name replacement did not receive a distinct immutable identity: A=%#v B=%#v", expected, replacementIdentity)
	}
	t.Logf("Exact identity probe: A pool_id=%d image_id=%s; B pool_id=%d image_id=%s", expected.PoolID, expected.ID, replacementIdentity.PoolID, replacementIdentity.ID)

	err = driver.deleteVolumeWithExactIdentity(vol, identityValue)
	if err != nil {
		t.Fatal(err)
	}

	hasIdentity, err := driver.HasVolumeIdentity(vol, identityValue)
	if err != nil || hasIdentity {
		t.Fatalf("Exact image survived successful deletion: exists=%t err=%v", hasIdentity, err)
	}
	hasLocalState, err = driver.HasVolumeLocalState(vol, identityValue)
	if err != nil || hasLocalState {
		t.Fatalf("Exact mapping survived successful deletion: state=%t err=%v", hasLocalState, err)
	}

	currentReplacementIdentity, err := driver.GetVolumeIdentity(vol)
	if err != nil {
		t.Fatal(err)
	}
	if currentReplacementIdentity != replacementIdentityValue {
		t.Fatalf("Same-name replacement changed during exact deletion: before=%q after=%q", replacementIdentityValue, currentReplacementIdentity)
	}

	_, tombstoneExists, err := imageIdentityOrMissing(store, tombstoneName)
	if err != nil || tombstoneExists {
		t.Fatalf("Exact tombstone survived successful deletion: exists=%t err=%v", tombstoneExists, err)
	}
	trash, err := store.TrashEntries()
	if err != nil {
		t.Fatal(err)
	}
	if _, found := findTrashIdentity(trash, expected.ID); found {
		t.Fatal("Exact image survived successful deletion in trash")
	}
}

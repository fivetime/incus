package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lxc/incus/v7/internal/server/auth"
	"github.com/lxc/incus/v7/internal/server/db"
	"github.com/lxc/incus/v7/internal/server/storage/drivers"
	"github.com/lxc/incus/v7/internal/server/storagematerializationattempt"
	"github.com/lxc/incus/v7/internal/server/storagereleasereceipt"
	"github.com/lxc/incus/v7/shared/api"
)

type testStorageReleaseReceiptLocalStateProvider struct {
	hasState         bool
	err              error
	expectedIdentity string
}

type testDeletedStorageIdentityProvider struct {
	logicalIdentity string
	exactExists     bool
}

func (p *testDeletedStorageIdentityProvider) GetVolumeIdentity(drivers.Volume) (string, error) {
	return p.logicalIdentity, nil
}

func (p *testDeletedStorageIdentityProvider) HasVolumeIdentity(drivers.Volume, string) (bool, error) {
	return p.exactExists, nil
}

func (p *testStorageReleaseReceiptLocalStateProvider) HasVolumeLocalState(_ drivers.Volume, expectedStorageIdentity string) (bool, error) {
	p.expectedIdentity = expectedStorageIdentity
	return p.hasState, p.err
}

func testStorageReleaseReceipt() *db.StorageReleaseReceipt {
	return &db.StorageReleaseReceipt{
		Token:              "06ada2e7-67f9-4c4e-b071-da45f25cfc67",
		Owner:              "11111111-1111-1111-1111-111111111111",
		AllocationID:       "22222222-2222-2222-2222-222222222222",
		ComputeID:          "33333333-3333-3333-3333-333333333333",
		MaterializationID:  "06ada2e7-67f9-4c4e-b071-da45f25cfc67",
		Project:            "nova",
		InstanceName:       "instance-00000001",
		IDMapBase:          1000000,
		IDMapSize:          65536,
		StorageDriver:      "cephext",
		StoragePool:        "cinder-bfv",
		StorageVolume:      "nova_instance-00000001",
		RBDImage:           "volume-11111111-1111-1111-1111-111111111111",
		StorageIdentity:    "rbd_data.1234567890abcdef",
		BaselineClean:      true,
		CleanupDisposition: storagematerializationattempt.CleanupDetach,
		Outcome:            storagereleasereceipt.OutcomeNormalized,
		State:              storagereleasereceipt.StateComplete,
	}
}

func TestStorageReleaseReceiptRequiresInstanceAbsent(t *testing.T) {
	exists, err := storageReleaseReceiptInstancePresence(nil)
	if err != nil || !exists {
		t.Fatalf("Existing instance was not detected: exists=%t err=%v", exists, err)
	}

	exists, err = storageReleaseReceiptInstancePresence(api.StatusErrorf(http.StatusNotFound, "not found"))
	if err != nil || exists {
		t.Fatalf("Instance 404 did not allow receipt recovery: exists=%t err=%v", exists, err)
	}

	loadErr := errors.New("database unavailable")
	exists, err = storageReleaseReceiptInstancePresence(loadErr)
	if !errors.Is(err, loadErr) || exists {
		t.Fatalf("Instance lookup failure was not preserved: exists=%t err=%v", exists, err)
	}
}

func TestStorageReleaseReceiptEndpointAuthorization(t *testing.T) {
	if storageReleaseReceiptCmd.Get.Handler == nil || storageReleaseReceiptCmd.Get.AccessHandler == nil {
		t.Fatal("Storage release receipt GET is missing its handler or authorization gate")
	}

	if storageReleaseReceiptCmd.Delete.Handler == nil || storageReleaseReceiptCmd.Delete.AccessHandler == nil {
		t.Fatal("Storage release receipt DELETE is missing its handler or authorization gate")
	}

	if auth.ObjectServer().Type() != auth.ObjectTypeServer {
		t.Fatal("Storage release receipt endpoint must use server authorization")
	}
}

func TestStorageReleaseReceiptRequestRequiresExactBinding(t *testing.T) {
	receipt := testStorageReleaseReceipt()
	request := httptest.NewRequest("GET", "/1.0/storage-release-receipts/"+receipt.Token+
		"?project=nova&rootfs-idmap-release-owner="+receipt.Owner+
		"&rootfs-idmap-allocation-id="+receipt.AllocationID+
		"&rootfs-idmap-compute-id="+receipt.ComputeID+
		"&instance="+receipt.InstanceName+
		"&idmap-base=1000000&idmap-size=65536", nil)

	expected, err := storageReleaseReceiptBindingFromRequest(request, receipt)
	if err != nil {
		t.Fatal(err)
	}

	if !storageReleaseReceiptBindingMatches(receipt, expected) {
		t.Fatal("Exact storage release receipt binding did not match")
	}

	expected.IDMapBase++
	if storageReleaseReceiptBindingMatches(receipt, expected) {
		t.Fatal("Different ID map generation matched the receipt")
	}
}

func TestStorageReleaseReceiptDigestCoversStorageIdentity(t *testing.T) {
	receipt := testStorageReleaseReceipt()
	digest, err := storageReleaseReceiptDigest(receipt)
	if err != nil {
		t.Fatal(err)
	}

	recreated := *receipt
	recreated.StorageIdentity = "rbd_data.ffffffffffffffff"
	recreatedDigest, err := storageReleaseReceiptDigest(&recreated)
	if err != nil {
		t.Fatal(err)
	}

	if digest == recreatedDigest {
		t.Fatal("Recreated storage object retained the prior receipt digest")
	}

	rebound := *receipt
	rebound.AllocationID = "44444444-4444-4444-4444-444444444444"
	reboundDigest, err := storageReleaseReceiptDigest(&rebound)
	if err != nil {
		t.Fatal(err)
	}

	if digest == reboundDigest {
		t.Fatal("Different allocation retained the prior receipt digest")
	}

	differentBaseline := *receipt
	differentBaseline.BaselineClean = false
	differentBaselineDigest, err := storageReleaseReceiptDigest(&differentBaseline)
	if err != nil {
		t.Fatal(err)
	}
	if digest == differentBaselineDigest {
		t.Fatal("Different materialization baseline retained the prior receipt digest")
	}

	differentDisposition := *receipt
	differentDisposition.CleanupDisposition = storagematerializationattempt.CleanupDelete
	differentDispositionDigest, err := storageReleaseReceiptDigest(&differentDisposition)
	if err != nil {
		t.Fatal(err)
	}
	if digest == differentDispositionDigest {
		t.Fatal("Different cleanup disposition retained the prior receipt digest")
	}
}

func TestStorageReleaseReceiptCanonicalDigest(t *testing.T) {
	receipt := testStorageReleaseReceipt()
	receipt.CreatedAt = 1722499200
	receipt.CompletedAt = 1722499260

	canonical, err := json.Marshal(storageReleaseReceiptToAPICanonical(receipt))
	if err != nil {
		t.Fatal(err)
	}

	const expectedCanonical = `{"digest":"","token":"06ada2e7-67f9-4c4e-b071-da45f25cfc67","allocation_id":"22222222-2222-2222-2222-222222222222","compute_id":"33333333-3333-3333-3333-333333333333","materialization_id":"06ada2e7-67f9-4c4e-b071-da45f25cfc67","owner":"11111111-1111-1111-1111-111111111111","project":"nova","instance_name":"instance-00000001","idmap_base":1000000,"idmap_size":65536,"storage_driver":"cephext","storage_pool":"cinder-bfv","storage_volume":"nova_instance-00000001","rbd_image":"volume-11111111-1111-1111-1111-111111111111","storage_identity":"rbd_data.1234567890abcdef","baseline_clean":true,"cleanup_disposition":"detach","outcome":"normalized","state":"complete","created_at":1722499200,"completed_at":1722499260}`
	if string(canonical) != expectedCanonical {
		t.Fatalf("Canonical receipt changed:\nexpected: %s\nactual:   %s", expectedCanonical, canonical)
	}

	digest, err := storageReleaseReceiptDigest(receipt)
	if err != nil {
		t.Fatal(err)
	}

	const expectedDigest = "sha256:55ec35e696e6e75f855daef4fda1d536a904e296380c29aba12fc202032f9429"
	if digest != expectedDigest {
		t.Fatalf("Canonical receipt digest changed: expected %q, got %q", expectedDigest, digest)
	}
}

func TestStorageReleaseReceiptDigestSurvivesRetirement(t *testing.T) {
	receipt := testStorageReleaseReceipt()
	digest, err := storageReleaseReceiptDigest(receipt)
	if err != nil {
		t.Fatal(err)
	}

	receipt.State = storagereleasereceipt.StateRetired
	retiredDigest, err := storageReleaseReceiptDigest(receipt)
	if err != nil {
		t.Fatal(err)
	}

	if digest != retiredDigest {
		t.Fatal("Receipt retirement changed its acknowledgement digest")
	}
}

func TestStorageReleaseReceiptRejectsNonCanonicalBindingUUIDs(t *testing.T) {
	receipt := testStorageReleaseReceipt()
	queryNames := []string{"rootfs-idmap-release-owner", "rootfs-idmap-allocation-id", "rootfs-idmap-compute-id"}
	for _, queryName := range queryNames {
		request := httptest.NewRequest("GET", "/1.0/storage-release-receipts/"+receipt.Token+
			"?project=nova&rootfs-idmap-release-owner="+receipt.Owner+
			"&rootfs-idmap-allocation-id="+receipt.AllocationID+
			"&rootfs-idmap-compute-id="+receipt.ComputeID+
			"&instance="+receipt.InstanceName+"&idmap-base=1000000&idmap-size=65536", nil)
		values := request.URL.Query()
		values.Set(queryName, "URN:UUID:11111111-1111-1111-1111-111111111111")
		request.URL.RawQuery = values.Encode()
		_, err := storageReleaseReceiptBindingFromRequest(request, receipt)
		if err == nil {
			t.Fatalf("Non-canonical %s was accepted", queryName)
		}
	}

	for _, token := range []string{
		"06ADA2E7-67F9-4C4E-B071-DA45F25CFC67",
		"06ada2e767f94c4eb071da45f25cfc67",
		"urn:uuid:06ada2e7-67f9-4c4e-b071-da45f25cfc67",
	} {
		if validateCanonicalStorageMaterializationUUID(token) == nil {
			t.Fatalf("Non-canonical receipt token %q was accepted", token)
		}
	}
}

func TestStorageReleaseReceiptRejectsResidualLocalState(t *testing.T) {
	vol := drivers.NewVolume(nil, "pool", drivers.VolumeTypeContainer, drivers.ContentTypeFS, "instance", nil, nil)
	receipt := testStorageReleaseReceipt()
	receipt.Outcome = storagereleasereceipt.OutcomeDeleted
	provider := &testStorageReleaseReceiptLocalStateProvider{hasState: true}
	if err := validateRequiredStorageReleaseReceiptLocalState(receipt, provider, vol); err == nil {
		t.Fatal("First GET or acknowledgement accepted a deleted receipt with a residual mapping")
	}

	provider.hasState = false
	if err := validateRequiredStorageReleaseReceiptLocalState(receipt, provider, vol); err != nil {
		t.Fatal(err)
	}
	if provider.expectedIdentity != receipt.StorageIdentity {
		t.Fatalf("Local-state proof received %q instead of receipt identity %q", provider.expectedIdentity, receipt.StorageIdentity)
	}

	receipt.StorageIdentity = ""
	if err := validateRequiredStorageReleaseReceiptLocalState(receipt, provider, vol); err == nil {
		t.Fatal("Receipt without an immutable identity reached local-state proof")
	}
	receipt.StorageIdentity = "rbd_data.1234567890abcdef"

	inspectErr := errors.New("local state inspection failed")
	provider.err = inspectErr
	if err := validateRequiredStorageReleaseReceiptLocalState(receipt, provider, vol); !errors.Is(err, inspectErr) {
		t.Fatalf("Receipt validation did not preserve local state inspection failure: %v", err)
	}
}

func TestDeletedStorageReceiptAllowsDifferentSameNameGeneration(t *testing.T) {
	vol := drivers.NewVolume(nil, "pool", drivers.VolumeTypeContainer, drivers.ContentTypeFS, "instance", nil, nil)
	receipt := testStorageReleaseReceipt()
	receipt.StorageDriver = "ceph"
	receipt.Outcome = storagereleasereceipt.OutcomeDeleted
	provider := &testDeletedStorageIdentityProvider{logicalIdentity: "second-generation", exactExists: true}

	if err := validateDeletedStorageReleaseReceiptIdentity(provider, vol, true, receipt); err == nil {
		t.Fatal("Receipt GET accepted the old generation while it remained in tombstone or trash")
	}

	provider.exactExists = false
	if err := validateDeletedStorageReleaseReceiptIdentity(provider, vol, true, receipt); err != nil {
		t.Fatalf("Different same-name generation blocked old receipt retirement: %v", err)
	}

	provider.logicalIdentity = receipt.StorageIdentity
	if err := validateDeletedStorageReleaseReceiptIdentity(provider, vol, true, receipt); err == nil {
		t.Fatal("Same exact generation reappeared under the logical name")
	}

	if err := validateDeletedStorageReleaseReceiptIdentity(provider, vol, false, receipt); err != nil {
		t.Fatal(err)
	}
}

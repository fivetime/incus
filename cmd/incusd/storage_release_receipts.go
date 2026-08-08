package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	internalInstance "github.com/lxc/incus/v7/internal/instance"
	"github.com/lxc/incus/v7/internal/server/auth"
	"github.com/lxc/incus/v7/internal/server/db"
	serverInstance "github.com/lxc/incus/v7/internal/server/instance"
	"github.com/lxc/incus/v7/internal/server/response"
	"github.com/lxc/incus/v7/internal/server/state"
	storagePools "github.com/lxc/incus/v7/internal/server/storage"
	"github.com/lxc/incus/v7/internal/server/storage/drivers"
	"github.com/lxc/incus/v7/internal/server/storagereleasereceipt"
	"github.com/lxc/incus/v7/shared/api"
)

const (
	storageReleaseReceiptInstanceQueryParam  = "instance"
	storageReleaseReceiptIDMapBaseQueryParam = "idmap-base"
	storageReleaseReceiptIDMapSizeQueryParam = "idmap-size"
	storageReleaseReceiptDigestQueryParam    = "receipt-digest"
)

var storageReleaseReceiptCmd = APIEndpoint{
	Name: "storageReleaseReceipt",
	Path: "storage-release-receipts/{token}",

	Get:    APIEndpointAction{Handler: storageReleaseReceiptGet, AccessHandler: allowPermission(auth.ObjectTypeServer, auth.EntitlementCanEdit)},
	Delete: APIEndpointAction{Handler: storageReleaseReceiptDelete, AccessHandler: allowPermission(auth.ObjectTypeServer, auth.EntitlementCanEdit)},
}

// swagger:operation GET /1.0/storage-release-receipts/{token} storage-release-receipts storage_release_receipt_get
//
//	Get completed root storage release proof
//
//	The receipt remains queryable after the corresponding instance record is
//	deleted. Every mutable name and ID map field supplied by the caller must
//	match the immutable receipt. The daemon returns and revalidates the full
//	storage binding, including the current object's immutable identity when it
//	still exists.
//
//	---
//
//	produces:
//	  - application/json
//	parameters:
//	  - in: path
//	    name: token
//	    type: string
//	    required: true
//	  - in: query
//	    name: project
//	    type: string
//	    required: true
//	  - in: query
//	    name: rootfs-idmap-release-owner
//	    type: string
//	    required: true
//	  - in: query
//	    name: rootfs-idmap-allocation-id
//	    type: string
//	    required: true
//	  - in: query
//	    name: rootfs-idmap-compute-id
//	    type: string
//	    required: true
//	  - in: query
//	    name: instance
//	    type: string
//	    required: true
//	  - in: query
//	    name: idmap-base
//	    type: integer
//	    required: true
//	  - in: query
//	    name: idmap-size
//	    type: integer
//	    required: true
//	responses:
//	  "200":
//	    description: Completed storage release receipt
//	    schema:
//	      type: object
//	      properties:
//	        metadata:
//	          $ref: "#/definitions/StorageReleaseReceipt"
func storageReleaseReceiptGet(d *Daemon, r *http.Request) response.Response {
	receipt, resp := loadAndValidateStorageReleaseReceipt(d, r, false)
	if resp != nil {
		return resp
	}

	return response.SyncResponse(true, storageReleaseReceiptToAPI(receipt))
}

// swagger:operation DELETE /1.0/storage-release-receipts/{token} storage-release-receipts storage_release_receipt_delete
//
//	Acknowledge and retire completed root storage release proof
//
//	The external allocator must persist an immutable copy of the complete
//	receipt before acknowledging it. Binding and storage identity checks are
//	repeated before retirement. The receipt-digest query parameter must be the
//	canonical digest returned by GET.
//
//	---
//
//	parameters:
//	  - in: path
//	    name: token
//	    type: string
//	    required: true
//	  - in: query
//	    name: project
//	    type: string
//	    required: true
//	  - in: query
//	    name: rootfs-idmap-release-owner
//	    type: string
//	    required: true
//	  - in: query
//	    name: rootfs-idmap-allocation-id
//	    type: string
//	    required: true
//	  - in: query
//	    name: rootfs-idmap-compute-id
//	    type: string
//	    required: true
//	  - in: query
//	    name: instance
//	    type: string
//	    required: true
//	  - in: query
//	    name: idmap-base
//	    type: integer
//	    required: true
//	  - in: query
//	    name: idmap-size
//	    type: integer
//	    required: true
//	  - in: query
//	    name: receipt-digest
//	    type: string
//	    required: true
//	responses:
//	  "200":
//	    description: Empty success response
func storageReleaseReceiptDelete(d *Daemon, r *http.Request) response.Response {
	receipt, resp := loadAndValidateStorageReleaseReceipt(d, r, true)
	if resp != nil {
		return resp
	}

	err := storagereleasereceipt.New(d.State().DB.Node).Delete(r.Context(), *receipt)
	if err != nil {
		return storageReleaseReceiptError(err)
	}

	return response.EmptySyncResponse
}

func loadAndValidateStorageReleaseReceipt(d *Daemon, r *http.Request, requireDigest bool) (*db.StorageReleaseReceipt, response.Response) {
	s := d.State()
	resp := forwardedResponseIfTargetIsRemote(s, r)
	if resp != nil {
		return nil, resp
	}

	token, err := pathVar(r, "token")
	if err != nil {
		return nil, response.BadRequest(err)
	}

	err = validateCanonicalStorageMaterializationUUID(token)
	if err != nil {
		return nil, response.BadRequest(err)
	}

	manager := storagereleasereceipt.New(s.DB.Node)
	receipt, err := manager.Get(r.Context(), token)
	if err != nil {
		return nil, storageReleaseReceiptError(err)
	}

	expected, err := storageReleaseReceiptBindingFromRequest(r, receipt)
	if err != nil {
		return nil, response.BadRequest(err)
	}

	if !storageReleaseReceiptBindingMatches(receipt, expected) {
		return nil, response.Conflict(storagereleasereceipt.ErrBindingMismatch)
	}

	if receipt.State == storagereleasereceipt.StateRetired {
		if !requireDigest {
			return nil, response.NotFound(storagereleasereceipt.ErrNotFound)
		}

		resp := validateStorageReleaseReceiptDigest(r, receipt)
		if resp != nil {
			return nil, resp
		}

		return receipt, nil
	}

	if receipt.State != storagereleasereceipt.StateComplete {
		return nil, response.Conflict(errors.New("Storage release receipt is not complete"))
	}

	_, loadErr := serverInstance.LoadByProjectAndName(s, receipt.Project, receipt.InstanceName)
	instanceExists, err := storageReleaseReceiptInstancePresence(loadErr)
	if err != nil {
		return nil, response.InternalError(err)
	}

	if instanceExists {
		return nil, response.Conflict(errors.New("Storage release receipt is not available until the instance record is absent"))
	}

	err = validateStorageReleaseReceiptObject(s, receipt)
	if err != nil {
		return nil, response.Conflict(err)
	}

	if requireDigest {
		resp := validateStorageReleaseReceiptDigest(r, receipt)
		if resp != nil {
			return nil, resp
		}
	}

	return receipt, nil
}

func validateStorageReleaseReceiptDigest(r *http.Request, receipt *db.StorageReleaseReceipt) response.Response {
	expectedDigest := r.URL.Query().Get(storageReleaseReceiptDigestQueryParam)
	if expectedDigest == "" {
		return response.BadRequest(fmt.Errorf("Missing required query parameter %q", storageReleaseReceiptDigestQueryParam))
	}

	actualDigest, err := storageReleaseReceiptDigest(receipt)
	if err != nil {
		return response.InternalError(err)
	}

	if subtle.ConstantTimeCompare([]byte(actualDigest), []byte(expectedDigest)) != 1 {
		return response.Conflict(storagereleasereceipt.ErrBindingMismatch)
	}

	return nil
}

func storageReleaseReceiptInstancePresence(loadErr error) (bool, error) {
	if loadErr == nil {
		return true, nil
	}

	if response.IsNotFoundError(loadErr) {
		return false, nil
	}

	return false, loadErr
}

func storageReleaseReceiptBindingFromRequest(r *http.Request, receipt *db.StorageReleaseReceipt) (*db.StorageReleaseReceipt, error) {
	required := func(name string) (string, error) {
		value := r.URL.Query().Get(name)
		if value == "" {
			return "", fmt.Errorf("Missing required query parameter %q", name)
		}

		return value, nil
	}

	owner, err := required(internalInstance.RootfsIDMapReleaseOwnerQueryParam)
	if err != nil {
		return nil, err
	}

	err = validateCanonicalStorageMaterializationUUID(owner)
	if err != nil {
		return nil, fmt.Errorf("Invalid release owner: %w", err)
	}

	allocationID, err := required(internalInstance.RootfsIDMapAllocationIDQueryParam)
	if err != nil {
		return nil, err
	}

	err = validateCanonicalStorageMaterializationUUID(allocationID)
	if err != nil {
		return nil, fmt.Errorf("Invalid allocation ID: %w", err)
	}

	computeID, err := required(internalInstance.RootfsIDMapComputeIDQueryParam)
	if err != nil {
		return nil, err
	}

	err = validateCanonicalStorageMaterializationUUID(computeID)
	if err != nil {
		return nil, fmt.Errorf("Invalid compute ID: %w", err)
	}

	instanceName, err := required(storageReleaseReceiptInstanceQueryParam)
	if err != nil {
		return nil, err
	}

	idmapBaseValue, err := required(storageReleaseReceiptIDMapBaseQueryParam)
	if err != nil {
		return nil, err
	}

	idmapBase, err := strconv.ParseInt(idmapBaseValue, 10, 64)
	if err != nil || idmapBase < 0 {
		return nil, fmt.Errorf("Invalid idmap base %q", idmapBaseValue)
	}

	idmapSizeValue, err := required(storageReleaseReceiptIDMapSizeQueryParam)
	if err != nil {
		return nil, err
	}

	idmapSize, err := strconv.ParseInt(idmapSizeValue, 10, 64)
	if err != nil || idmapSize <= 0 {
		return nil, fmt.Errorf("Invalid idmap size %q", idmapSizeValue)
	}

	const idmapIDSpaceSize = int64(1 << 32)
	if idmapSize > idmapIDSpaceSize || idmapBase > idmapIDSpaceSize-idmapSize {
		return nil, errors.New("ID map range exceeds the 32-bit UID/GID space")
	}

	projectName, err := required("project")
	if err != nil {
		return nil, err
	}

	return &db.StorageReleaseReceipt{
		Token:              receipt.Token,
		AllocationID:       allocationID,
		ComputeID:          computeID,
		MaterializationID:  receipt.Token,
		Owner:              owner,
		Project:            projectName,
		InstanceName:       instanceName,
		IDMapBase:          idmapBase,
		IDMapSize:          idmapSize,
		StorageDriver:      receipt.StorageDriver,
		StoragePool:        receipt.StoragePool,
		StorageVolume:      receipt.StorageVolume,
		RBDImage:           receipt.RBDImage,
		StorageIdentity:    receipt.StorageIdentity,
		BaselineClean:      receipt.BaselineClean,
		CleanupDisposition: receipt.CleanupDisposition,
		Outcome:            receipt.Outcome,
	}, nil
}

func storageReleaseReceiptBindingMatches(receipt *db.StorageReleaseReceipt, expected *db.StorageReleaseReceipt) bool {
	return receipt != nil && expected != nil &&
		receipt.Token == expected.Token &&
		receipt.AllocationID == expected.AllocationID &&
		receipt.ComputeID == expected.ComputeID &&
		receipt.MaterializationID == expected.MaterializationID &&
		receipt.Owner == expected.Owner &&
		receipt.Project == expected.Project &&
		receipt.InstanceName == expected.InstanceName &&
		receipt.IDMapBase == expected.IDMapBase &&
		receipt.IDMapSize == expected.IDMapSize &&
		receipt.StorageDriver == expected.StorageDriver &&
		receipt.StoragePool == expected.StoragePool &&
		receipt.StorageVolume == expected.StorageVolume &&
		receipt.RBDImage == expected.RBDImage &&
		receipt.StorageIdentity == expected.StorageIdentity &&
		receipt.BaselineClean == expected.BaselineClean &&
		receipt.CleanupDisposition == expected.CleanupDisposition &&
		receipt.Outcome == expected.Outcome
}

func validateStorageReleaseReceiptObject(s *state.State, receipt *db.StorageReleaseReceipt) error {
	pool, err := storagePools.LoadByName(s, receipt.StoragePool)
	if err != nil {
		return fmt.Errorf("Load receipt storage pool: %w", err)
	}

	if pool.Driver().Info().Name != receipt.StorageDriver {
		return errors.New("Storage release receipt driver no longer matches its pool")
	}

	var config map[string]string
	if receipt.RBDImage != "" {
		config = map[string]string{"ceph.rbd.image_name": receipt.RBDImage}
	}

	vol := pool.GetVolume(drivers.VolumeTypeContainer, drivers.ContentTypeFS, receipt.StorageVolume, config)
	exists, err := pool.Driver().HasVolume(vol)
	if err != nil {
		return fmt.Errorf("Check receipt storage volume: %w", err)
	}

	err = validateRequiredStorageReleaseReceiptLocalState(receipt, pool.Driver(), vol)
	if err != nil {
		return err
	}

	switch receipt.Outcome {
	case storagereleasereceipt.OutcomeDeleted:
		return validateDeletedStorageReleaseReceiptIdentity(pool.Driver(), vol, exists, receipt)
	case storagereleasereceipt.OutcomeNormalized, storagereleasereceipt.OutcomeDetached:
		if !exists {
			// The external owner may delete the RBD after the complete receipt is
			// persisted. Absence cannot expose an object-name ABA collision.
			return nil
		}

		provider, ok := pool.Driver().(drivers.VolumeIdentityProvider)
		if !ok {
			return errors.New("Storage driver cannot revalidate immutable volume identity")
		}

		identity, err := provider.GetVolumeIdentity(vol)
		if err != nil {
			return fmt.Errorf("Read current storage identity: %w", err)
		}

		if identity == "" || identity != receipt.StorageIdentity {
			return errors.New("Storage release receipt refers to another storage object")
		}

		return nil
	default:
		return fmt.Errorf("Unsupported storage release outcome %q", receipt.Outcome)
	}
}

func validateDeletedStorageReleaseReceiptIdentity(driver any, vol drivers.Volume, logicalNameExists bool, receipt *db.StorageReleaseReceipt) error {
	if receipt == nil {
		return errors.New("Storage release receipt is required")
	}

	if receipt.StorageDriver == "ceph" {
		presenceProvider, ok := driver.(drivers.VolumeIdentityPresenceProvider)
		if !ok {
			return errors.New("Ceph storage driver cannot prove exact identity deletion")
		}

		exists, err := presenceProvider.HasVolumeIdentity(vol, receipt.StorageIdentity)
		if err != nil {
			return fmt.Errorf("Inspect exact deleted storage identity: %w", err)
		}

		if exists {
			return errors.New("Storage recorded as deleted still has its exact identity")
		}
	}

	if !logicalNameExists {
		return nil
	}

	provider, ok := driver.(drivers.VolumeIdentityProvider)
	if !ok {
		return errors.New("Storage recorded as deleted exists again")
	}

	identity, err := provider.GetVolumeIdentity(vol)
	if err != nil {
		return fmt.Errorf("Read replacement storage identity: %w", err)
	}

	if identity == receipt.StorageIdentity {
		return errors.New("Storage recorded as deleted exists again with the same identity")
	}

	return nil
}

func validateRequiredStorageReleaseReceiptLocalState(receipt *db.StorageReleaseReceipt, driver any, vol drivers.Volume) error {
	if receipt == nil || (receipt.StorageDriver != "ceph" && receipt.StorageDriver != "cephext") {
		return nil
	}

	localStateProvider, ok := driver.(drivers.VolumeLocalStateProvider)
	if !ok {
		return errors.New("Storage driver cannot prove volume local state release")
	}

	return validateStorageReleaseReceiptLocalState(localStateProvider, vol, receipt.StorageIdentity)
}

func validateStorageReleaseReceiptLocalState(provider drivers.VolumeLocalStateProvider, vol drivers.Volume, expectedStorageIdentity string) error {
	if expectedStorageIdentity == "" {
		return errors.New("Storage release receipt has no immutable storage identity")
	}

	hasLocalState, err := provider.HasVolumeLocalState(vol, expectedStorageIdentity)
	if err != nil {
		return fmt.Errorf("Inspect receipt volume local state: %w", err)
	}

	if hasLocalState {
		return errors.New("Storage release receipt has residual host-local volume state")
	}

	return nil
}

func storageReleaseReceiptToAPI(receipt *db.StorageReleaseReceipt) *api.StorageReleaseReceipt {
	result := &api.StorageReleaseReceipt{
		Token:              receipt.Token,
		AllocationID:       receipt.AllocationID,
		ComputeID:          receipt.ComputeID,
		MaterializationID:  receipt.MaterializationID,
		Owner:              receipt.Owner,
		Project:            receipt.Project,
		InstanceName:       receipt.InstanceName,
		IDMapBase:          receipt.IDMapBase,
		IDMapSize:          receipt.IDMapSize,
		StorageDriver:      receipt.StorageDriver,
		StoragePool:        receipt.StoragePool,
		StorageVolume:      receipt.StorageVolume,
		RBDImage:           receipt.RBDImage,
		StorageIdentity:    receipt.StorageIdentity,
		BaselineClean:      receipt.BaselineClean,
		CleanupDisposition: receipt.CleanupDisposition,
		Outcome:            receipt.Outcome,
		State:              receipt.State,
		CreatedAt:          receipt.CreatedAt,
		CompletedAt:        receipt.CompletedAt,
	}

	result.Digest, _ = storageReleaseReceiptDigest(receipt)
	return result
}

func storageReleaseReceiptDigest(receipt *db.StorageReleaseReceipt) (string, error) {
	canonical := storageReleaseReceiptToAPICanonical(receipt)
	data, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}

	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func storageReleaseReceiptToAPICanonical(receipt *db.StorageReleaseReceipt) api.StorageReleaseReceipt {
	canonicalState := receipt.State
	if canonicalState == storagereleasereceipt.StateRetired {
		canonicalState = storagereleasereceipt.StateComplete
	}

	return api.StorageReleaseReceipt{
		Token:              receipt.Token,
		AllocationID:       receipt.AllocationID,
		ComputeID:          receipt.ComputeID,
		MaterializationID:  receipt.MaterializationID,
		Owner:              receipt.Owner,
		Project:            receipt.Project,
		InstanceName:       receipt.InstanceName,
		IDMapBase:          receipt.IDMapBase,
		IDMapSize:          receipt.IDMapSize,
		StorageDriver:      receipt.StorageDriver,
		StoragePool:        receipt.StoragePool,
		StorageVolume:      receipt.StorageVolume,
		RBDImage:           receipt.RBDImage,
		StorageIdentity:    receipt.StorageIdentity,
		BaselineClean:      receipt.BaselineClean,
		CleanupDisposition: receipt.CleanupDisposition,
		Outcome:            receipt.Outcome,
		State:              canonicalState,
		CreatedAt:          receipt.CreatedAt,
		CompletedAt:        receipt.CompletedAt,
	}
}

func storageReleaseReceiptError(err error) response.Response {
	switch {
	case errors.Is(err, storagereleasereceipt.ErrNotFound):
		return response.NotFound(err)
	case errors.Is(err, storagereleasereceipt.ErrBindingMismatch):
		return response.Conflict(err)
	default:
		return response.InternalError(err)
	}
}

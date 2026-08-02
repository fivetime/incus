package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/lxc/incus/v7/internal/server/auth"
	"github.com/lxc/incus/v7/internal/server/db"
	serverInstance "github.com/lxc/incus/v7/internal/server/instance"
	"github.com/lxc/incus/v7/internal/server/locking"
	"github.com/lxc/incus/v7/internal/server/operations"
	"github.com/lxc/incus/v7/internal/server/project"
	"github.com/lxc/incus/v7/internal/server/request"
	"github.com/lxc/incus/v7/internal/server/response"
	"github.com/lxc/incus/v7/internal/server/state"
	storagePools "github.com/lxc/incus/v7/internal/server/storage"
	"github.com/lxc/incus/v7/internal/server/storage/drivers"
	"github.com/lxc/incus/v7/internal/server/storagematerializationattempt"
	"github.com/lxc/incus/v7/shared/api"
)

var storageMaterializationAttemptCmd = APIEndpoint{
	Name:   "storageMaterializationAttempt",
	Path:   "storage-materialization-attempts/{token}",
	Get:    APIEndpointAction{Handler: storageMaterializationAttemptGet, AccessHandler: allowAuthenticated},
	Put:    APIEndpointAction{Handler: storageMaterializationAttemptPut, AccessHandler: allowAuthenticated},
	Delete: APIEndpointAction{Handler: storageMaterializationAttemptDelete, AccessHandler: allowAuthenticated},
}

// swagger:operation GET /1.0/storage-materialization-attempts/{token} storage-materialization-attempts storage_materialization_attempt_get
//
//	Get a rootfs materialization attempt
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
//	responses:
//	  "200":
//	    description: Storage materialization attempt
//	    schema:
//	      type: object
//	      properties:
//	        metadata:
//	          $ref: "#/definitions/StorageMaterializationAttempt"
func storageMaterializationAttemptGet(d *Daemon, r *http.Request) response.Response {
	attempt, resp := loadStorageMaterializationAttempt(d, r)
	if resp != nil {
		return resp
	}
	return response.SyncResponse(true, storageMaterializationAttemptToAPI(attempt))
}

// swagger:operation PUT /1.0/storage-materialization-attempts/{token} storage-materialization-attempts storage_materialization_attempt_put
//
//	Register, abort or reconcile a rootfs materialization attempt
//
//	---
//
//	consumes:
//	  - application/json
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
//	  - in: body
//	    name: attempt
//	    schema:
//	      $ref: "#/definitions/StorageMaterializationAttemptPut"
//	responses:
//	  "200":
//	    description: Storage materialization attempt
func storageMaterializationAttemptPut(d *Daemon, r *http.Request) response.Response {
	s := d.State()
	if resp := forwardedResponseIfTargetIsRemote(s, r); resp != nil {
		return resp
	}

	token, err := storageMaterializationAttemptToken(r)
	if err != nil {
		return response.BadRequest(err)
	}
	req := api.StorageMaterializationAttemptPut{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return response.BadRequest(err)
	}

	manager := storagematerializationattempt.New(s.DB.Node)
	projectName := request.ProjectParam(r)
	switch req.State {
	case storagematerializationattempt.StateActive:
		expected, err := storageMaterializationAttemptFromPut(token, projectName, req)
		if err != nil {
			return response.BadRequest(err)
		}
		if err := storageMaterializationAttemptPermission(s, r, projectName, expected.InstanceName); err != nil {
			return response.SmartError(err)
		}
		unlock, err := locking.Lock(r.Context(), "storage_materialization_instance_"+expected.Project+"_"+expected.InstanceName)
		if err != nil {
			return response.InternalError(err)
		}
		defer unlock()

		if err := validateStorageMaterializationRegistrationBaseline(r.Context(), s, expected); err != nil {
			return response.Conflict(err)
		}

		attempt, err := manager.Register(r.Context(), *expected)
		if err != nil {
			return storageMaterializationAttemptError(err)
		}
		return response.SyncResponse(true, storageMaterializationAttemptToAPI(attempt))
	case storagematerializationattempt.StateAborted:
		attempt, resp := loadStorageMaterializationAttempt(d, r)
		if resp != nil {
			return resp
		}
		attempt, err = manager.Abort(r.Context(), token)
		if err != nil {
			return storageMaterializationAttemptError(err)
		}
		if attempt.OperationUUID != "" {
			op, opErr := operations.OperationGetInternal(attempt.OperationUUID)
			if opErr == nil && op.Status() == api.Running {
				_, _ = op.Cancel()
			}
		}
		return response.SyncResponse(true, storageMaterializationAttemptToAPI(attempt))
	case storagematerializationattempt.RequestStateSettled:
		attempt, resp := loadStorageMaterializationAttempt(d, r)
		if resp != nil {
			return resp
		}
		if !attempt.Started {
			return response.SyncResponse(true, storageMaterializationAttemptToAPI(attempt))
		}
		if err := storageMaterializationAttemptOperationFinished(s, attempt); err != nil {
			return response.Conflict(err)
		}
		err = reconcileStorageMaterializationAttempt(r.Context(), s, attempt, false)
		if err != nil {
			return response.Conflict(err)
		}
		attempt, err = manager.Get(r.Context(), token)
		if err != nil {
			return storageMaterializationAttemptError(err)
		}
		return response.SyncResponse(true, storageMaterializationAttemptToAPI(attempt))
	default:
		return response.BadRequest(fmt.Errorf("Unsupported storage materialization attempt state %q", req.State))
	}
}

// validateStorageMaterializationRegistrationBaseline proves what existed
// before a materialization generation was registered. The caller holds the
// per-project instance materialization lock, which is also used by storage
// creation and reconciliation.
func validateStorageMaterializationRegistrationBaseline(ctx context.Context, s *state.State, expected *db.StorageMaterializationAttempt) error {
	if expected == nil {
		return errors.New("Storage materialization attempt is required")
	}

	_, err := serverInstance.LoadByProjectAndName(s, expected.Project, expected.InstanceName)
	if err == nil {
		return errors.New("Cannot register storage materialization for an existing instance")
	}
	if !response.IsNotFoundError(err) {
		return fmt.Errorf("Check materialization instance baseline: %w", err)
	}

	pool, err := storagePools.LoadByName(s, expected.StoragePool)
	if err != nil {
		return fmt.Errorf("Load materialization storage pool: %w", err)
	}
	if pool.Driver().Info().Name != expected.StorageDriver {
		return errors.New("Materialization storage driver does not match its pool")
	}
	if err := validateStorageMaterializationCleanupCapabilities(pool.Driver(), expected.CleanupDisposition); err != nil {
		return err
	}

	expectedVolumeName := project.Instance(expected.Project, expected.InstanceName)
	if expected.StorageVolume != expectedVolumeName {
		return fmt.Errorf("Materialization storage volume must be %q", expectedVolumeName)
	}

	_, err = storagePools.VolumeDBGet(pool, expected.Project, expected.InstanceName, drivers.VolumeTypeContainer)
	if err == nil {
		return errors.New("Cannot register materialization while its local volume claim exists")
	}
	if !response.IsNotFoundError(err) {
		return fmt.Errorf("Check materialization volume database baseline: %w", err)
	}

	snapshots, err := storagePools.VolumeDBSnapshotsGet(pool, expected.Project, expected.InstanceName, drivers.VolumeTypeContainer)
	if err != nil {
		return fmt.Errorf("Check materialization snapshot baseline: %w", err)
	}
	if len(snapshots) != 0 {
		return errors.New("Cannot register materialization while local volume snapshot claims exist")
	}

	config := map[string]string{}
	if expected.RBDImage != "" {
		config["ceph.rbd.image_name"] = expected.RBDImage
	}
	vol := pool.GetVolume(drivers.VolumeTypeContainer, drivers.ContentTypeFS, expected.StorageVolume, config)
	exists, err := pool.Driver().HasVolume(vol)
	if err != nil {
		return fmt.Errorf("Check materialization storage baseline: %w", err)
	}

	switch expected.CleanupDisposition {
	case storagematerializationattempt.CleanupDelete:
		if exists {
			return errors.New("Cannot register new root storage materialization over an existing backend volume")
		}
		expected.StorageIdentity = ""
	case storagematerializationattempt.CleanupDetach, storagematerializationattempt.CleanupHandover:
		if !exists {
			return errors.New("Cannot register shared root storage materialization for an absent backend volume")
		}
		localStateProvider, ok := pool.Driver().(drivers.VolumeLocalStateProvider)
		if !ok {
			return errors.New("Shared root storage driver cannot prove its local claim baseline")
		}
		identityProvider, ok := pool.Driver().(drivers.VolumeIdentityProvider)
		if !ok {
			return errors.New("Shared root storage driver cannot provide immutable identity")
		}
		identity, err := identityProvider.GetVolumeIdentity(vol)
		if err != nil {
			return fmt.Errorf("Read shared root storage baseline identity: %w", err)
		}
		if identity == "" {
			return errors.New("Shared root storage baseline identity is empty")
		}
		hasLocalState, err := localStateProvider.HasVolumeLocalState(vol, identity)
		if err != nil {
			return fmt.Errorf("Check materialization local storage baseline: %w", err)
		}
		if hasLocalState {
			return errors.New("Cannot register materialization while local storage state exists")
		}
		expected.StorageIdentity = identity
	default:
		return errors.New("Storage materialization cleanup disposition is invalid")
	}
	expected.BaselineClean = true

	return nil
}

func validateStorageMaterializationCleanupCapabilities(driver drivers.Driver, cleanupDisposition string) error {
	if cleanupDisposition != storagematerializationattempt.CleanupDelete && cleanupDisposition != storagematerializationattempt.CleanupHandover {
		return nil
	}

	if _, ok := driver.(drivers.VolumeIdentityProvider); !ok {
		return errors.New("Storage materialization cleanup requires immutable volume identity support")
	}
	if _, ok := driver.(drivers.VolumeMaterializationOwnershipProvider); !ok {
		return errors.New("Storage materialization cleanup requires durable volume ownership support")
	}
	if _, ok := driver.(drivers.VolumeIdentityBoundDeleter); !ok {
		return errors.New("Storage materialization cleanup requires identity-bound volume deletion support")
	}

	return nil
}

func storageMaterializationAttemptOperationFinished(s *state.State, attempt *db.StorageMaterializationAttempt) error {
	if attempt.OperationUUID == "" {
		if attempt.DaemonStart == s.StartTime.UnixNano() {
			return errors.New("Cannot settle a started materialization attempt before its target operation has been identified")
		}

		return nil
	}

	op, err := operations.OperationGetInternal(attempt.OperationUUID)
	if err != nil {
		return nil
	}

	if op.Status() == api.Pending || op.Status() == api.Running || op.Status() == api.Cancelling {
		return errors.New("Cannot settle a materialization attempt while its target operation is still running")
	}

	return nil
}

// swagger:operation DELETE /1.0/storage-materialization-attempts/{token} storage-materialization-attempts storage_materialization_attempt_delete
//
//	Retire a terminal rootfs materialization attempt
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
//	responses:
//	  "200":
//	    description: Empty success response
func storageMaterializationAttemptDelete(d *Daemon, r *http.Request) response.Response {
	attempt, resp := loadStorageMaterializationAttempt(d, r)
	if resp != nil {
		return resp
	}
	if err := validateStorageMaterializationAcknowledgement(r, attempt); err != nil {
		return response.Conflict(err)
	}
	err := storagematerializationattempt.New(d.State().DB.Node).Delete(r.Context(), attempt.Token)
	if err != nil {
		return storageMaterializationAttemptError(err)
	}
	return response.EmptySyncResponse
}

func validateStorageMaterializationAcknowledgement(r *http.Request, attempt *db.StorageMaterializationAttempt) error {
	if attempt.ProofDigest == "" || attempt.ProofOutcome == "" {
		return errors.New("Storage materialization attempt has no cleanup proof to acknowledge")
	}

	required := map[string]string{
		"proof-digest":  attempt.ProofDigest,
		"allocation-id": attempt.AllocationID,
		"compute-id":    attempt.ComputeID,
		"owner":         attempt.Owner,
		"instance":      attempt.InstanceName,
		"idmap-base":    strconv.FormatInt(attempt.IDMapBase, 10),
		"idmap-size":    strconv.FormatInt(attempt.IDMapSize, 10),
	}
	for name, expected := range required {
		actual := r.URL.Query().Get(name)
		if actual == "" {
			return fmt.Errorf("Missing required query parameter %q", name)
		}
		if subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) != 1 {
			return storagematerializationattempt.ErrBindingMismatch
		}
	}

	return nil
}

func loadStorageMaterializationAttempt(d *Daemon, r *http.Request) (*db.StorageMaterializationAttempt, response.Response) {
	s := d.State()
	if resp := forwardedResponseIfTargetIsRemote(s, r); resp != nil {
		return nil, resp
	}
	token, err := storageMaterializationAttemptToken(r)
	if err != nil {
		return nil, response.BadRequest(err)
	}
	attempt, err := storagematerializationattempt.New(s.DB.Node).Get(r.Context(), token)
	if err != nil {
		return nil, storageMaterializationAttemptError(err)
	}
	if attempt.Project != request.ProjectParam(r) {
		return nil, response.NotFound(storagematerializationattempt.ErrNotFound)
	}
	if err := storageMaterializationAttemptPermission(s, r, attempt.Project, attempt.InstanceName); err != nil {
		return nil, response.SmartError(err)
	}
	return attempt, nil
}

func storageMaterializationAttemptToken(r *http.Request) (string, error) {
	token, err := pathVar(r, "token")
	if err != nil {
		return "", err
	}
	if err := validateCanonicalStorageMaterializationUUID(token); err != nil {
		return "", err
	}
	return token, nil
}

func storageMaterializationAttemptFromPut(token string, projectName string, req api.StorageMaterializationAttemptPut) (*db.StorageMaterializationAttempt, error) {
	for name, value := range map[string]string{"allocation_id": req.AllocationID, "compute_id": req.ComputeID, "owner": req.Owner} {
		if err := validateCanonicalStorageMaterializationUUID(value); err != nil {
			return nil, fmt.Errorf("Invalid %s: %w", name, err)
		}
	}
	if req.IDMapBase == nil || req.IDMapSize == nil || *req.IDMapBase < 0 || *req.IDMapSize <= 0 || *req.IDMapSize > 1<<32 || *req.IDMapBase > (1<<32)-*req.IDMapSize {
		return nil, errors.New("A fixed idmap_base and idmap_size are required")
	}
	if req.InstanceName == "" || req.StorageDriver == "" || req.StoragePool == "" || req.StorageVolume == "" {
		return nil, errors.New("Instance and storage binding are required")
	}
	if err := serverInstance.ValidName(req.InstanceName, false); err != nil {
		return nil, fmt.Errorf("Invalid instance name: %w", err)
	}
	if err := validateStorageMaterializationName("storage driver", req.StorageDriver, 64, false); err != nil {
		return nil, err
	}
	if err := validateStorageMaterializationName("storage pool", req.StoragePool, 255, true); err != nil {
		return nil, err
	}
	if err := validateStorageMaterializationName("storage volume", req.StorageVolume, 255, true); err != nil {
		return nil, err
	}
	if req.RBDImage != "" {
		if err := validateStorageMaterializationName("RBD image", req.RBDImage, 255, true); err != nil {
			return nil, err
		}
	}
	if req.CleanupDisposition != storagematerializationattempt.CleanupDelete && req.CleanupDisposition != storagematerializationattempt.CleanupDetach && req.CleanupDisposition != storagematerializationattempt.CleanupHandover {
		return nil, errors.New("cleanup_disposition must be delete, detach, or handover")
	}
	if req.StorageDriver == "cephext" && req.CleanupDisposition != storagematerializationattempt.CleanupDetach {
		return nil, errors.New("cephext materialization requires detach cleanup")
	}
	if req.CleanupDisposition == storagematerializationattempt.CleanupHandover && req.StorageDriver != "ceph" {
		return nil, errors.New("handover materialization requires the ceph storage driver")
	}
	return &db.StorageMaterializationAttempt{Token: token, AllocationID: req.AllocationID, ComputeID: req.ComputeID, Owner: req.Owner, Project: projectName, InstanceName: req.InstanceName, IDMapBase: *req.IDMapBase, IDMapSize: *req.IDMapSize, StorageDriver: req.StorageDriver, StoragePool: req.StoragePool, StorageVolume: req.StorageVolume, RBDImage: req.RBDImage, CleanupDisposition: req.CleanupDisposition}, nil
}

func validateStorageMaterializationName(label string, value string, maxLength int, allowDot bool) error {
	if value == "" || len(value) > maxLength {
		return fmt.Errorf("Invalid %s length", label)
	}
	for _, r := range value {
		valid := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_'
		if allowDot {
			valid = valid || r == '.'
		}
		if !valid {
			return fmt.Errorf("Invalid character %q in %s", r, label)
		}
	}

	return nil
}

func validateCanonicalStorageMaterializationUUID(value string) error {
	parsed, err := uuid.Parse(value)
	if err != nil {
		return err
	}
	if parsed.String() != value {
		return errors.New("UUID must use canonical lowercase 8-4-4-4-12 form")
	}

	return nil
}

func storageMaterializationAttemptToAPI(a *db.StorageMaterializationAttempt) *api.StorageMaterializationAttempt {
	result := &api.StorageMaterializationAttempt{Token: a.Token, AllocationID: a.AllocationID, ComputeID: a.ComputeID, Owner: a.Owner, Project: a.Project, InstanceName: a.InstanceName, IDMapBase: a.IDMapBase, IDMapSize: a.IDMapSize, StorageDriver: a.StorageDriver, StoragePool: a.StoragePool, StorageVolume: a.StorageVolume, RBDImage: a.RBDImage, StorageIdentity: a.StorageIdentity, BaselineClean: a.BaselineClean, CleanupDisposition: a.CleanupDisposition, State: a.State, StoragePhase: a.StoragePhase, Started: a.Started, Finished: a.Finished, OperationUUID: a.OperationUUID, DaemonStart: a.DaemonStart}
	if a.ProofOutcome != "" && a.ProofDigest != "" {
		result.Proof = &api.StorageMaterializationProof{Outcome: a.ProofOutcome, Digest: a.ProofDigest}
	}
	return result
}

func storageMaterializationAttemptPermission(s *state.State, r *http.Request, projectName string, instanceName string) error {
	return s.Authorizer.CheckPermission(r.Context(), r, auth.ObjectInstance(projectName, instanceName), auth.EntitlementCanEdit)
}

func storageMaterializationAttemptError(err error) response.Response {
	switch {
	case errors.Is(err, storagematerializationattempt.ErrNotFound):
		return response.NotFound(err)
	case errors.Is(err, storagematerializationattempt.ErrBindingMismatch), errors.Is(err, storagematerializationattempt.ErrAlreadyStarted), errors.Is(err, storagematerializationattempt.ErrAborted), errors.Is(err, storagematerializationattempt.ErrCommitted), errors.Is(err, storagematerializationattempt.ErrNotMaterialized), errors.Is(err, storagematerializationattempt.ErrFinished):
		return response.Conflict(err)
	default:
		return response.InternalError(err)
	}
}

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	internalInstance "github.com/lxc/incus/v7/internal/instance"
	"github.com/lxc/incus/v7/internal/server/auth"
	"github.com/lxc/incus/v7/internal/server/db"
	"github.com/lxc/incus/v7/internal/server/instance"
	"github.com/lxc/incus/v7/internal/server/instance/operationlock"
	"github.com/lxc/incus/v7/internal/server/migrationattempt"
	"github.com/lxc/incus/v7/internal/server/request"
	"github.com/lxc/incus/v7/internal/server/response"
	storagePools "github.com/lxc/incus/v7/internal/server/storage"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/validate"
)

// swagger:operation PUT /1.0/instances/{name}/storage-handover instances instance_storage_handover_put
//
//	Update shared storage ownership state
//
//	Marks an Incus-owned shared Ceph root as protected from local deletion or
//	clears completed target handover state for Ceph and externally owned Ceph
//	roots, or restores an explicitly identified fenced source after the migration
//	target has been fenced and cleaned up.
//	Protection requires an existing negotiated handover marker. Ownership
//	requires a completed target receive and its committed migration-attempt
//	proof.
//	Source ownership recovery only clears local migration metadata and never
//	deletes or otherwise modifies the shared storage.
//
//	---
//	consumes:
//	  - application/json
//	produces:
//	  - application/json
//	parameters:
//	  - in: path
//	    name: name
//	    description: Instance name
//	    type: string
//	    required: true
//	  - in: query
//	    name: project
//	    description: Project name
//	    type: string
//	    x-example: default
//	  - in: body
//	    name: state
//	    description: Shared storage ownership state
//	    schema:
//	      $ref: "#/definitions/InstanceStorageHandoverPut"
//	responses:
//	  "200":
//	    $ref: "#/responses/EmptySyncResponse"
//	  "400":
//	    $ref: "#/responses/BadRequest"
//	  "403":
//	    $ref: "#/responses/Forbidden"
//	  "404":
//	    $ref: "#/responses/NotFound"
//	  "409":
//	    $ref: "#/responses/Conflict"
//	  "500":
//	    $ref: "#/responses/InternalServerError"
func instanceStorageHandoverPut(d *Daemon, r *http.Request) response.Response {
	<-d.waitReady.Done()

	s := d.State()
	projectName := request.ProjectParam(r)
	name, err := pathVar(r, "name")
	if err != nil {
		return response.SmartError(err)
	}

	if internalInstance.IsSnapshot(name) {
		return response.BadRequest(errors.New("Invalid instance name"))
	}

	resp, err := forwardedResponseIfInstanceIsRemote(s, r, projectName, name)
	if err != nil {
		return response.SmartError(err)
	}

	if resp != nil {
		return resp
	}

	req := api.InstanceStorageHandoverPut{}
	err = json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	switch req.State {
	case internalInstance.StorageHandoverStateProtected:
		if req.MigrationAttempt != "" || req.OperationUUID != "" {
			return response.BadRequest(errors.New("Migration attempt proof is only valid for the owned state"))
		}

	case internalInstance.StorageHandoverStateOwned:
		if req.MigrationAttempt == "" || req.OperationUUID == "" {
			return response.BadRequest(errors.New("The owned state requires migration_attempt and operation_uuid"))
		}

		err = validate.IsUUID(req.MigrationAttempt)
		if err != nil {
			return response.BadRequest(fmt.Errorf("Invalid migration_attempt: %w", err))
		}

		err = validate.IsUUID(req.OperationUUID)
		if err != nil {
			return response.BadRequest(fmt.Errorf("Invalid operation_uuid: %w", err))
		}

	case internalInstance.StorageHandoverStateDetached:
		if req.MigrationAttempt != "" || req.OperationUUID != "" {
			return response.BadRequest(errors.New("Migration attempt proof is only valid for the owned state"))
		}

		// Detachment asserts that shared storage ownership was disposed of
		// externally (e.g. a fence-retired claim). Like restoring source
		// ownership, only a server administrator can make that assertion.
		err = s.Authorizer.CheckPermission(
			r.Context(),
			r,
			instanceStorageHandoverSourceOwnedAuthObject(),
			instanceStorageHandoverSourceOwnedAuthEntitlement,
		)
		if err != nil {
			return response.SmartError(err)
		}

	case internalInstance.StorageHandoverStateSourceOwned:
		if req.MigrationAttempt != "" || req.OperationUUID != "" {
			return response.BadRequest(errors.New("Migration attempt proof is only valid for the owned state"))
		}

		// Restoring source ownership depends on external proof that the target
		// has been fenced and cleaned up. Only a server administrator can make
		// that assertion; a project-restricted migration identity cannot.
		err = s.Authorizer.CheckPermission(
			r.Context(),
			r,
			instanceStorageHandoverSourceOwnedAuthObject(),
			instanceStorageHandoverSourceOwnedAuthEntitlement,
		)
		if err != nil {
			return response.SmartError(err)
		}

	default:
		return response.BadRequest(fmt.Errorf("Invalid storage handover state %q", req.State))
	}

	op, err := operationlock.CreateWaitGet(projectName, name, nil, operationlock.ActionUpdate, nil, false, false)
	if err != nil {
		return response.SmartError(err)
	}

	defer func() {
		op.Done(err)
	}()

	inst, err := instance.LoadByProjectAndName(s, projectName, name)
	if err != nil {
		return response.SmartError(err)
	}

	if req.State == internalInstance.StorageHandoverStateOwned {
		var attempt *db.MigrationAttempt
		attempt, err = migrationattempt.New(s.DB.Node).Get(r.Context(), req.MigrationAttempt)
		if errors.Is(err, migrationattempt.ErrNotFound) {
			err = storageHandoverOwnershipProofError()
			return response.Conflict(err)
		}

		if err != nil {
			return response.InternalError(err)
		}

		if !storageHandoverOwnershipProofMatches(attempt, projectName, name, req.OperationUUID) {
			err = storageHandoverOwnershipProofError()
			return response.Conflict(err)
		}
	}

	pool, err := storagePools.LoadByInstance(s, inst)
	if err != nil {
		return response.SmartError(err)
	}

	driverName := pool.Driver().Info().Name
	if !internalInstance.StorageHandoverDriverSupported(driverName, req.State) {
		err = fmt.Errorf("Storage handover state %q is not supported by storage driver %q", req.State, driverName)
		return response.BadRequest(err)
	}

	changes, err := internalInstance.StorageHandoverAPIConfigChanges(req.State, inst.LocalConfig(), driverName)
	if errors.Is(err, internalInstance.ErrStorageHandoverInactive) {
		return response.Conflict(err)
	}

	if errors.Is(err, internalInstance.ErrStorageHandoverIncomplete) {
		return response.Conflict(err)
	}

	if err != nil {
		return response.BadRequest(err)
	}

	if len(changes) > 0 {
		err = inst.VolatileSet(changes)
	}

	if err != nil {
		return response.SmartError(err)
	}

	return response.EmptySyncResponse
}

func instanceStorageHandoverSourceOwnedAuthObject() auth.Object {
	return auth.ObjectServer()
}

func storageHandoverOwnershipProofMatches(attempt *db.MigrationAttempt, projectName string, instanceName string, operationUUID string) bool {
	return attempt != nil &&
		attempt.Project == projectName &&
		attempt.ResourceType == migrationattempt.ResourceTypeInstance &&
		attempt.ResourceName == instanceName &&
		attempt.State == migrationattempt.StateCommitted &&
		attempt.Started &&
		attempt.Finished &&
		attempt.OperationUUID != "" &&
		attempt.OperationUUID == operationUUID
}

func storageHandoverOwnershipProofError() error {
	return fmt.Errorf("%w: committed target migration proof is invalid", internalInstance.ErrStorageHandoverIncomplete)
}

package main

import (
	"errors"
	"fmt"
	"net/http"

	internalInstance "github.com/lxc/incus/v7/internal/instance"
	"github.com/lxc/incus/v7/internal/server/db/operationtype"
	"github.com/lxc/incus/v7/internal/server/instance"
	"github.com/lxc/incus/v7/internal/server/locking"
	"github.com/lxc/incus/v7/internal/server/operations"
	"github.com/lxc/incus/v7/internal/server/request"
	"github.com/lxc/incus/v7/internal/server/response"
	"github.com/lxc/incus/v7/internal/version"
	"github.com/lxc/incus/v7/shared/api"
)

// swagger:operation DELETE /1.0/instances/{name} instances instance_delete
//
//	Delete an instance
//
//	Deletes a specific instance.
//
//	This also deletes anything owned by the instance such as snapshots and backups.
//
//	---
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
//	  - in: query
//	    name: rootfs-idmap-release-token
//	    description: Rootfs materialization UUID used for a durable storage release receipt
//	    type: string
//	  - in: query
//	    name: rootfs-idmap-release-owner
//	    description: External owner UUID bound to the release receipt
//	    type: string
//	  - in: query
//	    name: rootfs-idmap-allocation-id
//	    description: Fleet-wide ID map allocation generation UUID
//	    type: string
//	  - in: query
//	    name: rootfs-idmap-compute-id
//	    description: Persistent Nova compute-node UUID
//	    type: string
//	responses:
//	  "202":
//	    $ref: "#/responses/Operation"
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
func instanceDelete(d *Daemon, r *http.Request) response.Response {
	// Don't mess with instance while in setup mode.
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

	// Handle requests targeted to a container on a different node
	resp, err := forwardedResponseIfInstanceIsRemote(s, r, projectName, name)
	if err != nil {
		return response.SmartError(err)
	}

	if resp != nil {
		return resp
	}

	inst, err := instance.LoadByProjectAndName(s, projectName, name)
	if err != nil {
		return response.SmartError(err)
	}

	if inst.IsRunning() {
		return response.BadRequest(errors.New("Instance is running"))
	}

	releaseToken := r.URL.Query().Get(internalInstance.RootfsIDMapReleaseTokenQueryParam)
	releaseOwner := r.URL.Query().Get(internalInstance.RootfsIDMapReleaseOwnerQueryParam)
	allocationID := r.URL.Query().Get(internalInstance.RootfsIDMapAllocationIDQueryParam)
	computeID := r.URL.Query().Get(internalInstance.RootfsIDMapComputeIDQueryParam)
	supplied := 0
	for _, value := range []string{releaseToken, releaseOwner, allocationID, computeID} {
		if value != "" {
			supplied++
		}
	}

	if supplied != 0 && supplied != 4 {
		return response.BadRequest(errors.New("Rootfs ID map release token, owner, allocation ID and compute ID must be supplied together"))
	}

	requiresRelease, err := rootfsMaterializationBindingRequiresRelease(inst.LocalConfig())
	if err != nil {
		return response.Conflict(err)
	}

	if requiresRelease && supplied != 4 {
		return response.Conflict(errors.New("Instance rootfs materialization requires a complete release receipt binding"))
	}

	if releaseToken != "" {
		err = validateCanonicalStorageMaterializationUUID(releaseToken)
		if err != nil {
			return response.BadRequest(fmt.Errorf("Invalid rootfs ID map release token: %w", err))
		}

		err = validateCanonicalStorageMaterializationUUID(releaseOwner)
		if err != nil {
			return response.BadRequest(fmt.Errorf("Invalid rootfs ID map release owner: %w", err))
		}

		err = validateCanonicalStorageMaterializationUUID(allocationID)
		if err != nil {
			return response.BadRequest(fmt.Errorf("Invalid rootfs ID map allocation ID: %w", err))
		}

		err = validateCanonicalStorageMaterializationUUID(computeID)
		if err != nil {
			return response.BadRequest(fmt.Errorf("Invalid rootfs ID map compute ID: %w", err))
		}

		err = validateRootfsIDMapReleaseBinding(inst.LocalConfig(), releaseToken, releaseOwner, allocationID, computeID)
		if err != nil {
			return response.Conflict(err)
		}
	}

	unlock, err := locking.Lock(r.Context(), fmt.Sprintf("rootfs-idmap-release_%s_%s", projectName, name))
	if err != nil {
		return response.InternalError(err)
	}

	config := inst.LocalConfig()
	armRelease, err := rootfsIDMapReleaseBindingNeedsArm(config, releaseToken, releaseOwner, allocationID, computeID)
	if err != nil {
		unlock()
		return response.Conflict(err)
	}

	if armRelease {
		err = inst.VolatileSet(map[string]string{
			internalInstance.ConfigVolatileRootfsIDMapReleaseToken: releaseToken,
			internalInstance.ConfigVolatileRootfsIDMapReleaseOwner: releaseOwner,
			internalInstance.ConfigVolatileRootfsIDMapAllocationID: allocationID,
			internalInstance.ConfigVolatileRootfsIDMapComputeID:    computeID,
		})
		if err != nil {
			unlock()
			return response.SmartError(err)
		}
	}

	unlock()

	run := func(op *operations.Operation) error {
		inst.SetOperation(op)
		return inst.Delete(false, true)
	}

	resources := map[string][]api.URL{}
	resources["instances"] = []api.URL{*api.NewURL().Path(version.APIVersion, "instances", name)}

	op, err := operations.OperationCreate(s, projectName, operations.OperationClassTask, operationtype.InstanceDelete, resources, nil, run, nil, nil, r)
	if err != nil {
		return response.InternalError(err)
	}

	return operations.OperationResponse(op)
}

func rootfsMaterializationBindingRequiresRelease(localConfig map[string]string) (bool, error) {
	allocationID := localConfig[internalInstance.ConfigOpenStackIDMapAllocationID]
	computeID := localConfig[internalInstance.ConfigOpenStackComputeID]
	token := localConfig[internalInstance.ConfigOpenStackRootfsMaterializationID]
	owner := localConfig["user.openstack.uuid"]
	idmapBase := localConfig["security.idmap.base"]
	idmapSize := localConfig["security.idmap.size"]
	values := []string{
		allocationID,
		computeID,
		token,
		owner,
	}

	protocolConfigured := allocationID != "" || computeID != "" || token != ""
	openStackIDMapConfigured := owner != "" && (idmapBase != "" || idmapSize != "")
	if !protocolConfigured && !openStackIDMapConfigured {
		return false, nil
	}

	for _, value := range values {
		if value == "" {
			return true, errors.New("Instance has incomplete rootfs materialization A/H/T/U provenance")
		}
	}

	for _, value := range values {
		err := validateCanonicalStorageMaterializationUUID(value)
		if err != nil {
			return true, fmt.Errorf("Instance has invalid rootfs materialization provenance: %w", err)
		}
	}

	return true, nil
}

func validateRootfsIDMapReleaseBinding(localConfig map[string]string, releaseToken string, releaseOwner string, allocationID string, computeID string) error {
	instanceOwner := localConfig["user.openstack.uuid"]
	if instanceOwner == "" {
		return errors.New("Instance has no authoritative local OpenStack owner UUID")
	}

	if releaseOwner != instanceOwner {
		return errors.New("Rootfs ID map release owner does not match the instance owner")
	}

	if releaseToken != localConfig[internalInstance.ConfigOpenStackRootfsMaterializationID] {
		return errors.New("Rootfs ID map release token does not match the instance materialization ID")
	}

	if allocationID != localConfig[internalInstance.ConfigOpenStackIDMapAllocationID] {
		return errors.New("Rootfs ID map allocation ID does not match the instance allocation")
	}

	if computeID != localConfig[internalInstance.ConfigOpenStackComputeID] {
		return errors.New("Rootfs ID map compute ID does not match the instance compute")
	}

	return nil
}

func rootfsIDMapReleaseBindingNeedsArm(localConfig map[string]string, releaseToken string, releaseOwner string, allocationID string, computeID string) (bool, error) {
	existingToken := localConfig[internalInstance.ConfigVolatileRootfsIDMapReleaseToken]
	existingOwner := localConfig[internalInstance.ConfigVolatileRootfsIDMapReleaseOwner]
	existingAllocationID := localConfig[internalInstance.ConfigVolatileRootfsIDMapAllocationID]
	existingComputeID := localConfig[internalInstance.ConfigVolatileRootfsIDMapComputeID]
	existing := 0
	for _, value := range []string{existingToken, existingOwner, existingAllocationID, existingComputeID} {
		if value != "" {
			existing++
		}
	}

	if existing != 0 && existing != 4 {
		return false, errors.New("Instance has an incomplete rootfs ID map release binding")
	}

	if existingToken == "" {
		return releaseToken != "", nil
	}

	if releaseToken != "" && (releaseToken != existingToken || releaseOwner != existingOwner || allocationID != existingAllocationID || computeID != existingComputeID) {
		return false, errors.New("Instance rootfs ID map release is bound to another generation")
	}

	return false, nil
}

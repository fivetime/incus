package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"

	"github.com/lxc/incus/v7/internal/server/auth"
	"github.com/lxc/incus/v7/internal/server/db"
	"github.com/lxc/incus/v7/internal/server/idmapreservation"
	"github.com/lxc/incus/v7/internal/server/instance"
	"github.com/lxc/incus/v7/internal/server/instance/instancetype"
	"github.com/lxc/incus/v7/internal/server/locking"
	"github.com/lxc/incus/v7/internal/server/migrationattempt"
	"github.com/lxc/incus/v7/internal/server/operations"
	"github.com/lxc/incus/v7/internal/server/request"
	"github.com/lxc/incus/v7/internal/server/response"
	"github.com/lxc/incus/v7/internal/server/state"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/util"
	"github.com/lxc/incus/v7/shared/validate"
)

const migrationAttemptResourceEntitlement = auth.EntitlementCanEdit

var migrationAttemptCmd = APIEndpoint{
	Name: "migrationAttempt",
	Path: "migration-attempts/{token}",

	Get:    APIEndpointAction{Handler: migrationAttemptGet, AccessHandler: allowAuthenticated},
	Put:    APIEndpointAction{Handler: migrationAttemptPut, AccessHandler: allowAuthenticated},
	Delete: APIEndpointAction{Handler: migrationAttemptDelete, AccessHandler: allowAuthenticated},
}

// swagger:operation GET /1.0/migration-attempts/{token} migration-attempts migration_attempt_get
//
//	Get an inbound migration attempt
//
//	Returns the durable target-side state and operation UUID for an inbound
//	migration attempt.
//
//	---
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
//	    description: Migration attempt
//	    schema:
//	      type: object
//	      properties:
//	        metadata:
//	          $ref: "#/definitions/MigrationAttempt"
func migrationAttemptGet(d *Daemon, r *http.Request) response.Response {
	s := d.State()
	resp := forwardedResponseIfTargetIsRemote(s, r)
	if resp != nil {
		return resp
	}

	token, err := migrationAttemptToken(r)
	if err != nil {
		return response.BadRequest(err)
	}

	manager := migrationattempt.New(s.DB.Node)
	attempt, err := manager.Get(r.Context(), token)
	if err != nil {
		return migrationAttemptError(err)
	}

	if !migrationAttemptVisibleInProject(attempt, request.ProjectParam(r)) {
		return response.NotFound(migrationattempt.ErrNotFound)
	}

	err = migrationAttemptCheckResourcePermission(s, r, attempt.Project, attempt.ResourceName)
	if err != nil {
		return response.SmartError(err)
	}

	return response.SyncResponse(true, migrationAttemptToAPI(attempt))
}

// swagger:operation PUT /1.0/migration-attempts/{token} migration-attempts migration_attempt_put
//
//	Register or abort an inbound migration attempt
//
//	An active registration must precede an instance migration create using the
//	same token. An abort permanently fences a late create or receive unless the
//	target already committed.
//
//	---
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
//	      $ref: "#/definitions/MigrationAttemptPut"
//	responses:
//	  "200":
//	    description: Migration attempt
//	    schema:
//	      type: object
//	      properties:
//	        metadata:
//	          $ref: "#/definitions/MigrationAttempt"
func migrationAttemptPut(d *Daemon, r *http.Request) response.Response {
	s := d.State()
	resp := forwardedResponseIfTargetIsRemote(s, r)
	if resp != nil {
		return resp
	}

	token, err := migrationAttemptToken(r)
	if err != nil {
		return response.BadRequest(err)
	}

	req := api.MigrationAttemptPut{}
	err = json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	projectName := request.ProjectParam(r)
	manager := migrationattempt.New(s.DB.Node)

	switch req.State {
	case migrationattempt.StateActive:
		if req.ResourceType != migrationattempt.ResourceTypeInstance {
			return response.BadRequest(fmt.Errorf("Unsupported migration attempt resource type %q", req.ResourceType))
		}

		err = instance.ValidName(req.ResourceName, false)
		if err != nil {
			return response.BadRequest(err)
		}

		err = migrationAttemptCheckResourcePermission(s, r, projectName, req.ResourceName)
		if err != nil {
			return response.SmartError(err)
		}

		idmapBase, idmapSize, err := migrationAttemptIDMap(req)
		if err != nil {
			return response.BadRequest(err)
		}

		unlock, err := locking.Lock(r.Context(), idmapreservation.LockName)
		if err != nil {
			return response.InternalError(err)
		}

		defer unlock()

		current, getErr := manager.Get(r.Context(), token)
		if getErr == nil {
			if !migrationAttemptVisibleInProject(current, projectName) {
				return response.NotFound(migrationattempt.ErrNotFound)
			}

			err = migrationAttemptCheckResourcePermission(s, r, current.Project, current.ResourceName)
			if err != nil {
				return response.SmartError(err)
			}

			attempt, err := manager.Register(r.Context(), token, projectName, req.ResourceType, req.ResourceName, idmapBase, idmapSize)
			if err != nil {
				return migrationAttemptError(err)
			}

			return response.SyncResponse(true, migrationAttemptToAPI(attempt))
		}

		if !errors.Is(getErr, migrationattempt.ErrNotFound) {
			return migrationAttemptError(getErr)
		}

		err = checkMigrationAttemptIDMapAvailable(s, idmapBase, idmapSize)
		if err != nil {
			return response.Conflict(err)
		}

		attempt, err := manager.Register(r.Context(), token, projectName, req.ResourceType, req.ResourceName, idmapBase, idmapSize)
		if err != nil {
			return migrationAttemptError(err)
		}

		return response.SyncResponse(true, migrationAttemptToAPI(attempt))

	case migrationattempt.StateAborted:
		current, err := manager.Get(r.Context(), token)
		if err != nil {
			return migrationAttemptError(err)
		}

		if !migrationAttemptVisibleInProject(current, projectName) {
			return response.NotFound(migrationattempt.ErrNotFound)
		}

		err = migrationAttemptCheckResourcePermission(s, r, current.Project, current.ResourceName)
		if err != nil {
			return response.SmartError(err)
		}

		attempt, err := manager.Abort(r.Context(), token)
		if err != nil {
			return migrationAttemptError(err)
		}

		if attempt.OperationUUID != "" {
			op, opErr := operations.OperationGetInternal(attempt.OperationUUID)
			if opErr == nil && op.Status() == api.Running {
				_, _ = op.Cancel()
			}
		}

		return response.SyncResponse(true, migrationAttemptToAPI(attempt))

	case migrationattempt.RequestStateSettled:
		current, err := manager.Get(r.Context(), token)
		if err != nil {
			return migrationAttemptError(err)
		}

		if !migrationAttemptVisibleInProject(current, projectName) {
			return response.NotFound(migrationattempt.ErrNotFound)
		}

		err = migrationAttemptCheckResourcePermission(s, r, current.Project, current.ResourceName)
		if err != nil {
			return response.SmartError(err)
		}

		attempt, err := settleAbortedMigrationAttempt(r.Context(), s, manager, token, projectName)
		if err != nil {
			if errors.Is(err, migrationattempt.ErrNotFound) {
				return response.NotFound(err)
			}

			return response.Conflict(err)
		}

		return response.SyncResponse(true, migrationAttemptToAPI(attempt))

	default:
		return response.BadRequest(fmt.Errorf("Unsupported migration attempt state %q", req.State))
	}
}

// swagger:operation DELETE /1.0/migration-attempts/{token} migration-attempts migration_attempt_delete
//
//	Delete a terminal inbound migration attempt
//
//	Only attempts whose target work and rollback have finished can be deleted.
//	A deleted token remains invalid for any late instance create request.
//
//	---
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
func migrationAttemptDelete(d *Daemon, r *http.Request) response.Response {
	s := d.State()
	resp := forwardedResponseIfTargetIsRemote(s, r)
	if resp != nil {
		return resp
	}

	token, err := migrationAttemptToken(r)
	if err != nil {
		return response.BadRequest(err)
	}

	manager := migrationattempt.New(s.DB.Node)
	attempt, err := manager.Get(r.Context(), token)
	if err != nil {
		return migrationAttemptError(err)
	}

	if !migrationAttemptVisibleInProject(attempt, request.ProjectParam(r)) {
		return response.NotFound(migrationattempt.ErrNotFound)
	}

	err = migrationAttemptCheckResourcePermission(s, r, attempt.Project, attempt.ResourceName)
	if err != nil {
		return response.SmartError(err)
	}

	err = manager.Delete(r.Context(), token)
	if err != nil {
		return migrationAttemptError(err)
	}

	return response.EmptySyncResponse
}

func migrationAttemptToken(r *http.Request) (string, error) {
	token, err := pathVar(r, "token")
	if err != nil {
		return "", err
	}

	err = validate.IsUUID(token)
	if err != nil {
		return "", err
	}

	return token, nil
}

func migrationAttemptIDMap(req api.MigrationAttemptPut) (int64, int64, error) {
	if req.IDMapBase == nil && req.IDMapSize == nil {
		return -1, 0, nil
	}

	if req.IDMapBase == nil || req.IDMapSize == nil {
		return 0, 0, errors.New("idmap_base and idmap_size must be specified together")
	}

	if *req.IDMapBase < 0 || *req.IDMapSize <= 0 {
		return 0, 0, errors.New("idmap_base must be non-negative and idmap_size must be positive")
	}

	if *req.IDMapBase > math.MaxInt64-*req.IDMapSize {
		return 0, 0, errors.New("idmap reservation exceeds the supported host ID range")
	}

	return *req.IDMapBase, *req.IDMapSize, nil
}

func checkMigrationAttemptIDMapAvailable(s *state.State, base int64, size int64) error {
	if base < 0 {
		return nil
	}

	for instanceID, reservation := range idmapreservation.Transient() {
		if idmapreservation.RangesOverlap(base, size, reservation.Base, reservation.Size) {
			return fmt.Errorf("Requested isolated idmap range %d-%d overlaps in-flight instance %d range %d-%d",
				base, base+size-1, instanceID, reservation.Base, reservation.Base+reservation.Size-1)
		}
	}

	instances, err := instance.LoadNodeAll(s, instancetype.Container)
	if err != nil {
		return err
	}

	for _, inst := range instances {
		if inst.IsPrivileged() || util.IsFalseOrEmpty(inst.ExpandedConfig()["security.idmap.isolated"]) {
			continue
		}

		baseValue := inst.ExpandedConfig()["volatile.idmap.base"]
		if baseValue == "" {
			baseValue = inst.ExpandedConfig()["security.idmap.base"]
		}

		if baseValue == "" {
			continue
		}

		instanceBase, err := strconv.ParseInt(baseValue, 10, 64)
		if err != nil {
			return fmt.Errorf("Invalid isolated idmap base for instance %q: %w", inst.Name(), err)
		}

		sizeValue := inst.ExpandedConfig()["security.idmap.size"]
		instanceSize := int64(65536)
		if sizeValue != "" && sizeValue != "auto" {
			instanceSize, err = strconv.ParseInt(sizeValue, 10, 64)
			if err != nil {
				return fmt.Errorf("Invalid isolated idmap size for instance %q: %w", inst.Name(), err)
			}
		}

		if idmapreservation.RangesOverlap(base, size, instanceBase, instanceSize) {
			return fmt.Errorf("Requested isolated idmap range %d-%d overlaps instance %q range %d-%d",
				base, base+size-1, inst.Name(), instanceBase, instanceBase+instanceSize-1)
		}
	}

	return nil
}

func settleAbortedMigrationAttempt(ctx context.Context, s *state.State, manager *migrationattempt.Manager, token string, projectName string) (*db.MigrationAttempt, error) {
	attempt, err := manager.Get(ctx, token)
	if err != nil {
		return nil, err
	}

	if attempt.Project != projectName {
		return nil, migrationattempt.ErrNotFound
	}

	if attempt.State != migrationattempt.StateAborted {
		return nil, migrationattempt.ErrFinished
	}

	if attempt.Finished {
		return attempt, nil
	}

	if attempt.OperationUUID == "" && attempt.DaemonStart == s.StartTime.UnixNano() {
		return nil, errors.New("Cannot settle a started migration attempt before its target operation has been identified")
	}

	if attempt.OperationUUID != "" {
		_, err = operations.OperationGetInternal(attempt.OperationUUID)
		if err == nil {
			return nil, errors.New("Cannot settle a migration attempt while its target operation is still retained")
		}
	}

	_, err = instance.LoadByProjectAndName(s, attempt.Project, attempt.ResourceName)
	if err == nil || !response.IsNotFoundError(err) {
		if err == nil {
			return nil, errors.New("Cannot settle a migration attempt while its target resource exists")
		}

		return nil, err
	}

	err = manager.SettleAborted(ctx, attempt.Token)
	if err != nil {
		return nil, err
	}

	settled, err := manager.Get(ctx, attempt.Token)
	if err != nil {
		return nil, err
	}

	return settled, nil
}

func migrationAttemptToAPI(attempt *db.MigrationAttempt) *api.MigrationAttempt {
	result := &api.MigrationAttempt{
		Token:         attempt.Token,
		Project:       attempt.Project,
		ResourceType:  attempt.ResourceType,
		ResourceName:  attempt.ResourceName,
		State:         attempt.State,
		Started:       attempt.Started,
		Finished:      attempt.Finished,
		OperationUUID: attempt.OperationUUID,
	}

	if attempt.IDMapBase >= 0 {
		result.IDMapBase = &attempt.IDMapBase
		result.IDMapSize = &attempt.IDMapSize
	}

	return result
}

func migrationAttemptVisibleInProject(attempt *db.MigrationAttempt, projectName string) bool {
	return attempt != nil &&
		attempt.State != migrationattempt.StateRetired &&
		attempt.Project == projectName
}

func migrationAttemptCheckResourcePermission(s *state.State, r *http.Request, projectName string, resourceName string) error {
	return s.Authorizer.CheckPermission(
		r.Context(),
		r,
		auth.ObjectInstance(projectName, resourceName),
		migrationAttemptResourceEntitlement,
	)
}

func migrationAttemptError(err error) response.Response {
	switch {
	case errors.Is(err, migrationattempt.ErrNotFound):
		return response.NotFound(err)
	case errors.Is(err, migrationattempt.ErrInvalidIDMap):
		return response.BadRequest(err)
	case errors.Is(err, migrationattempt.ErrBindingMismatch),
		errors.Is(err, migrationattempt.ErrOperationMismatch),
		errors.Is(err, migrationattempt.ErrAlreadyStarted),
		errors.Is(err, migrationattempt.ErrAborted),
		errors.Is(err, migrationattempt.ErrCommitted),
		errors.Is(err, migrationattempt.ErrFinished),
		errors.Is(err, migrationattempt.ErrIDMapOverlap):
		return response.Conflict(err)
	default:
		return response.InternalError(err)
	}
}

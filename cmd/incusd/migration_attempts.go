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
	localUtil "github.com/lxc/incus/v7/internal/server/util"
	"github.com/lxc/incus/v7/internal/version"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/logger"
	"github.com/lxc/incus/v7/shared/util"
	"github.com/lxc/incus/v7/shared/validate"
)

const migrationAttemptResourceEntitlement = auth.EntitlementCanEdit

var migrationAttemptsCmd = APIEndpoint{
	Name: "migrationAttempts",
	Path: "migration-attempts",

	Get: APIEndpointAction{Handler: migrationAttemptsGet, AccessHandler: allowAuthenticated},
}

var migrationAttemptCmd = APIEndpoint{
	Name: "migrationAttempt",
	Path: "migration-attempts/{token}",

	Get:    APIEndpointAction{Handler: migrationAttemptGet, AccessHandler: allowAuthenticated},
	Put:    APIEndpointAction{Handler: migrationAttemptPut, AccessHandler: allowAuthenticated},
	Delete: APIEndpointAction{Handler: migrationAttemptDelete, AccessHandler: allowAuthenticated},
}

// swagger:operation GET /1.0/migration-attempts migration-attempts migration_attempts_get
//
//	Get the inbound migration attempts awaiting garbage collection
//
//	Returns every node-local migration attempt that has not been retired,
//	including terminal ones whose orchestrator still owes a delete. Retired
//	token tombstones are never listed.
//
//	---
//	produces:
//	  - application/json
//	parameters:
//	  - in: query
//	    name: project
//	    type: string
//	  - in: query
//	    name: all-projects
//	    type: boolean
//	  - in: query
//	    name: recursion
//	    type: integer
//	responses:
//	  "200":
//	    description: API endpoints
//	    schema:
//	      type: object
//	      properties:
//	        metadata:
//	          type: array
//	          items:
//	            type: string
func migrationAttemptsGet(d *Daemon, r *http.Request) response.Response {
	s := d.State()
	resp := forwardedResponseIfTargetIsRemote(s, r)
	if resp != nil {
		return resp
	}

	recursion := localUtil.IsRecursionRequest(r)
	allProjects := util.IsTrue(request.QueryParam(r, "all-projects"))
	projectName := request.ProjectParam(r)

	canEdit, err := s.Authorizer.GetPermissionChecker(r.Context(), r, migrationAttemptResourceEntitlement, auth.ObjectTypeInstance)
	if err != nil {
		return response.SmartError(err)
	}

	attempts, err := migrationattempt.New(s.DB.Node).ListPending(r.Context())
	if err != nil {
		return response.SmartError(err)
	}

	urls := []string{}
	results := []*api.MigrationAttempt{}
	for _, attempt := range attempts {
		if !allProjects && attempt.Project != projectName {
			continue
		}

		if !canEdit(auth.ObjectInstance(attempt.Project, attempt.ResourceName)) {
			continue
		}

		if !recursion {
			urls = append(urls, api.NewURL().Path(version.APIVersion, "migration-attempts", attempt.Token).Project(attempt.Project).String())
			continue
		}

		results = append(results, migrationAttemptToAPI(&attempt, s.StartTime.UnixNano()))
	}

	if !recursion {
		return response.SyncResponse(true, urls)
	}

	return response.SyncResponse(true, results)
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

	return response.SyncResponse(true, migrationAttemptToAPI(attempt, s.StartTime.UnixNano()))
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

			attempt, err := manager.Register(r.Context(), token, projectName, req.ResourceType, req.ResourceName, idmapBase, idmapSize, s.StartTime.UnixNano())
			if err != nil {
				return migrationAttemptError(err)
			}

			return response.SyncResponse(true, migrationAttemptToAPI(attempt, s.StartTime.UnixNano()))
		}

		if !errors.Is(getErr, migrationattempt.ErrNotFound) {
			return migrationAttemptError(getErr)
		}

		err = checkMigrationAttemptIDMapAvailable(s, idmapBase, idmapSize)
		if err != nil {
			return response.Conflict(err)
		}

		attempt, err := manager.Register(r.Context(), token, projectName, req.ResourceType, req.ResourceName, idmapBase, idmapSize, s.StartTime.UnixNano())
		if err != nil {
			return migrationAttemptError(err)
		}

		return response.SyncResponse(true, migrationAttemptToAPI(attempt, s.StartTime.UnixNano()))

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

		return response.SyncResponse(true, migrationAttemptToAPI(attempt, s.StartTime.UnixNano()))

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

		return response.SyncResponse(true, migrationAttemptToAPI(attempt, s.StartTime.UnixNano()))

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

// reconcileMigrationAttemptsAfterRestart settles the inbound migration attempts
// that an earlier daemon process left unfinished. Their target operations died
// with that process, so nothing else will ever complete or roll them back, and
// their ID map reservations would otherwise fence their ranges forever.
//
// Settlement still fails closed and reuses the same checks an orchestrator
// drives through the API: an attempt whose target instance survived keeps its
// record for reconciliation, and that instance continues to fence its own ID
// map range. Settling does not retire the token, so the orchestrator's own
// abort, settle and delete requests all stay valid and idempotent afterwards.
//
// Attempts that were registered but never started are left alone: their create
// request can still arrive, so their reservation is still owed to them.
func reconcileMigrationAttemptsAfterRestart(ctx context.Context, s *state.State) {
	manager := migrationattempt.New(s.DB.Node)

	attempts, err := manager.ListPending(ctx)
	if err != nil {
		logger.Error("Failed loading pending migration attempts", logger.Ctx{"err": err})
		return
	}

	for i := range attempts {
		attempt := &attempts[i]
		if attempt.Finished || !attempt.Started || attempt.DaemonStart == s.StartTime.UnixNano() {
			continue
		}

		_, err := manager.Abort(ctx, attempt.Token)
		if err == nil {
			_, err = settleAbortedMigrationAttempt(ctx, s, manager, attempt.Token, attempt.Project)
		}

		if err != nil {
			logger.Error("Migration attempt recovery remains uncertain", logger.Ctx{
				"attempt":  attempt.Token,
				"project":  attempt.Project,
				"instance": attempt.ResourceName,
				"err":      err,
			})

			continue
		}

		logger.Info("Settled a migration attempt stranded by an earlier daemon", logger.Ctx{
			"attempt":  attempt.Token,
			"project":  attempt.Project,
			"instance": attempt.ResourceName,
		})
	}
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

func migrationAttemptToAPI(attempt *db.MigrationAttempt, daemonStart int64) *api.MigrationAttempt {
	result := &api.MigrationAttempt{
		Token:         attempt.Token,
		Project:       attempt.Project,
		ResourceType:  attempt.ResourceType,
		ResourceName:  attempt.ResourceName,
		State:         attempt.State,
		Started:       attempt.Started,
		Finished:      attempt.Finished,
		OperationUUID: attempt.OperationUUID,
		DaemonStart:   attempt.DaemonStart,
	}

	if attempt.IDMapBase >= 0 {
		result.IDMapBase = &attempt.IDMapBase
		result.IDMapSize = &attempt.IDMapSize
		result.IDMapActive = migrationAttemptIDMapActive(attempt, daemonStart)
	}

	return result
}

// migrationAttemptIDMapActive mirrors the reservation filter in
// GetMigrationAttemptIDMapReservations so that an operator can tell a
// reservation that still fences new attempts from one that was stranded by an
// earlier daemon generation and is now inert.
func migrationAttemptIDMapActive(attempt *db.MigrationAttempt, daemonStart int64) bool {
	if attempt.Finished || attempt.IDMapBase < 0 || attempt.IDMapSize <= 0 {
		return false
	}

	return !attempt.Started || attempt.DaemonStart == daemonStart
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

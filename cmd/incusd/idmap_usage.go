package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/lxc/incus/v7/internal/server/auth"
	"github.com/lxc/incus/v7/internal/server/db"
	"github.com/lxc/incus/v7/internal/server/response"
	"github.com/lxc/incus/v7/shared/validate"
)

const (
	idmapUsageOwnerQueryParam = "owner"
	idmapUsageBaseQueryParam  = "base"
	idmapUsageSizeQueryParam  = "size"
)

var idmapUsageCmd = APIEndpoint{
	Name: "idmapUsage",
	Path: "idmap-usage",

	Get: APIEndpointAction{Handler: idmapUsageGet, AccessHandler: allowPermission(auth.ObjectTypeServer, auth.EntitlementCanEdit)},
}

// swagger:operation GET /1.0/idmap-usage idmap-usage idmap_usage_get
//
//	Get exact ID map usage
//
//	Returns instances and profiles across all projects whose effective ID map
//	owner matches or whose effective ID map overlaps the supplied range.
//
//	---
//
//	produces:
//	  - application/json
//	parameters:
//	  - in: query
//	    name: owner
//	    type: string
//	    required: true
//	  - in: query
//	    name: base
//	    type: integer
//	    required: true
//	  - in: query
//	    name: size
//	    type: integer
//	    required: true
//	responses:
//	  "200":
//	    description: Matching instances and profiles
//	  "400":
//	    $ref: "#/responses/BadRequest"
//	  "403":
//	    $ref: "#/responses/Forbidden"
//	  "500":
//	    $ref: "#/responses/InternalServerError"
func idmapUsageGet(d *Daemon, r *http.Request) response.Response {
	owner := r.FormValue(idmapUsageOwnerQueryParam)
	if owner == "" {
		return response.BadRequest(errors.New("Missing ID map owner"))
	}

	err := validate.IsUUID(owner)
	if err != nil {
		return response.BadRequest(fmt.Errorf("Invalid ID map owner: %w", err))
	}

	base, err := uint32Query(r, idmapUsageBaseQueryParam)
	if err != nil {
		return response.BadRequest(err)
	}

	size, err := uint32Query(r, idmapUsageSizeQueryParam)
	if err != nil {
		return response.BadRequest(err)
	}

	err = validateIDMapUsageRange(base, size)
	if err != nil {
		return response.BadRequest(err)
	}

	resources := []db.IDMapUsageResource{}
	err = d.State().DB.Cluster.Transaction(r.Context(), func(ctx context.Context, tx *db.ClusterTx) error {
		resources, err = tx.GetIDMapUsage(ctx, owner, base, size)
		return err
	})
	if err != nil {
		return response.SmartError(err)
	}

	return response.SyncResponse(true, resources)
}

func uint32Query(r *http.Request, name string) (uint64, error) {
	value := r.FormValue(name)
	if value == "" {
		return 0, fmt.Errorf("Missing ID map %s", name)
	}

	parsed, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("Invalid ID map %s: %w", name, err)
	}

	return parsed, nil
}

func validateIDMapUsageRange(base uint64, size uint64) error {
	if size == 0 {
		return errors.New("ID map size must be greater than zero")
	}

	if base+size > uint64(1)<<32 {
		return errors.New("ID map range exceeds uint32")
	}

	return nil
}

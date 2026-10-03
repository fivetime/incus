package main

import (
	"errors"
	"net/http"
	"strconv"

	internalInstance "github.com/lxc/incus/v7/internal/instance"
	"github.com/lxc/incus/v7/internal/server/auth"
	"github.com/lxc/incus/v7/internal/server/instance"
	"github.com/lxc/incus/v7/internal/server/locking"
	"github.com/lxc/incus/v7/internal/server/project"
	"github.com/lxc/incus/v7/internal/server/request"
	"github.com/lxc/incus/v7/internal/server/response"
	storagePools "github.com/lxc/incus/v7/internal/server/storage"
	"github.com/lxc/incus/v7/internal/server/storage/drivers"
	"github.com/lxc/incus/v7/internal/server/storagematerializationattempt"
	"github.com/lxc/incus/v7/shared/api"
)

var sharedRootManifestCmd = APIEndpoint{
	Name: "sharedRootManifest", Path: "shared-root-manifests/{name}",
	Post: APIEndpointAction{Handler: sharedRootManifestPost, AccessHandler: allowPermission(auth.ObjectTypeServer, auth.EntitlementCanEdit)},
}

func sharedRootManifestPost(d *Daemon, r *http.Request) response.Response {
	s := d.State()
	if s.ServerClustered {
		return response.BadRequest(errors.New("Shared-root manifests require an independent Incus node"))
	}

	name, err := pathVar(r, "name")
	if err != nil {
		return response.BadRequest(err)
	}

	projectName := request.ProjectParam(r)
	unlock, err := locking.Lock(r.Context(), "storage_materialization_instance_"+projectName+"_"+name)
	if err != nil {
		return response.InternalError(err)
	}

	defer unlock()
	inst, err := instance.LoadByProjectAndName(s, projectName, name)
	if err != nil {
		return response.SmartError(err)
	}

	snapshots, err := inst.Snapshots()
	if err != nil {
		return response.SmartError(err)
	}

	if len(snapshots) != 0 {
		return response.Conflict(errors.New("Shared-root recovery does not support instances with snapshots"))
	}

	config := inst.LocalConfig()
	if internalInstance.StorageDeleteProtected(config) {
		return response.Conflict(errors.New("Protected shared root cannot publish recovery ownership"))
	}

	attempt, err := storagematerializationattempt.New(s.DB.Node).Get(r.Context(), config[internalInstance.ConfigOpenStackRootfsMaterializationID])
	if err != nil || attempt.State != storagematerializationattempt.StateCommitted || attempt.StorageDriver != "ceph" ||
		attempt.ComputeID != config[internalInstance.ConfigOpenStackComputeID] || attempt.Owner != config["user.openstack.uuid"] {
		return response.Conflict(errors.New("Manifest requires the exact committed local Ceph materialization"))
	}

	pool, err := storagePools.LoadByInstance(s, inst)
	if err != nil {
		return response.SmartError(err)
	}

	dbVol, err := storagePools.VolumeDBGet(pool, projectName, name, drivers.VolumeTypeContainer)
	if err != nil {
		return response.SmartError(err)
	}

	vol := pool.GetVolume(drivers.VolumeTypeContainer, drivers.ContentTypeFS, project.Instance(projectName, name), dbVol.Config)
	identityProvider, ok := pool.Driver().(drivers.VolumeIdentityProvider)
	if !ok {
		return response.Conflict(errors.New("Manifest requires immutable volume identity"))
	}

	identity, err := identityProvider.GetVolumeIdentity(vol)
	if err != nil || identity != attempt.StorageIdentity {
		return response.Conflict(errors.New("Manifest original-root identity differs from the committed claim"))
	}

	markerProvider, ok := pool.Driver().(drivers.VolumeMaterializationOwnershipProvider)
	if !ok {
		return response.Conflict(errors.New("Manifest requires storage ownership metadata"))
	}

	marker, err := markerProvider.GetVolumeMaterializationOwnership(vol)
	if err != nil || marker == "" {
		return response.Conflict(errors.New("Manifest requires original-root ownership evidence"))
	}

	if config["volatile.shared_root_takeover.ownership"] != "" && config["volatile.shared_root_takeover.ownership"] != marker {
		return response.Conflict(errors.New("Root ownership moved; stale source cannot republish it"))
	}

	if attempt.CleanupDisposition == storagematerializationattempt.CleanupDelete {
		expected, err := storagematerializationattempt.OwnershipMarker(attempt, identity)
		if err != nil || expected != marker {
			return response.Conflict(errors.New("Root ownership differs from source materialization"))
		}
	}

	clusterProvider, ok := pool.Driver().(drivers.StorageClusterIdentityProvider)
	if !ok {
		return response.Conflict(errors.New("Manifest requires physical Ceph cluster identity"))
	}

	fsid, err := clusterProvider.GetStorageClusterIdentity()
	if err != nil {
		return response.SmartError(err)
	}

	generation := uint64(1)
	if config["volatile.shared_root_takeover.generation"] != "" {
		generation, err = strconv.ParseUint(config["volatile.shared_root_takeover.generation"], 10, 64)
		if err != nil || generation == 0 {
			return response.Conflict(errors.New("Invalid original-root ownership generation"))
		}
	}

	err = inst.VolatileSet(map[string]string{
		"volatile.shared_root_takeover.ownership":        marker,
		"volatile.shared_root_takeover.storage_identity": identity, "volatile.shared_root_takeover.cluster_fsid": fsid,
		"volatile.shared_root_takeover.generation": strconv.FormatUint(generation, 10),
	})
	if err != nil {
		return response.SmartError(err)
	}

	// Re-read identity and marker before publishing a manifest that authorizes later recovery.
	verifiedIdentity, err := identityProvider.GetVolumeIdentity(vol)
	if err != nil || verifiedIdentity != identity {
		return response.Conflict(errors.New("Root identity changed during manifest capture"))
	}

	verifiedMarker, err := markerProvider.GetVolumeMaterializationOwnership(vol)
	if err != nil || verifiedMarker != marker {
		return response.Conflict(errors.New("Root ownership changed during manifest capture"))
	}

	rendered, _, err := inst.Render()
	if err != nil {
		return response.SmartError(err)
	}

	container, ok := rendered.(*api.Instance)
	if !ok {
		return response.Conflict(errors.New("Only system containers support shared-root manifests"))
	}

	// Nova stores this under its exact etcd source claim before advertising recovery eligibility.
	return response.SyncResponse(true, map[string]any{
		"version": 1, "owner": attempt.Owner, "allocation_id": attempt.AllocationID,
		"source_compute_id": attempt.ComputeID, "source_materialization_id": attempt.Token,
		"project": projectName, "instance_name": name, "storage_pool": pool.Name(), "storage_volume": vol.Name(),
		"cluster_fsid": fsid, "storage_identity": identity, "source_ownership": marker,
		"source_generation": generation, "idmap_base": attempt.IDMapBase, "idmap_size": attempt.IDMapSize,
		"instance": container, "volume": dbVol.StorageVolume,
	})
}

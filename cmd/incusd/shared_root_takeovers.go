package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	internalInstance "github.com/lxc/incus/v7/internal/instance"
	"github.com/lxc/incus/v7/internal/server/auth"
	backupConfig "github.com/lxc/incus/v7/internal/server/backup/config"
	"github.com/lxc/incus/v7/internal/server/db"
	"github.com/lxc/incus/v7/internal/server/instance"
	"github.com/lxc/incus/v7/internal/server/locking"
	"github.com/lxc/incus/v7/internal/server/project"
	"github.com/lxc/incus/v7/internal/server/request"
	"github.com/lxc/incus/v7/internal/server/response"
	"github.com/lxc/incus/v7/internal/server/sharedroottakeover"
	"github.com/lxc/incus/v7/internal/server/state"
	storagePools "github.com/lxc/incus/v7/internal/server/storage"
	"github.com/lxc/incus/v7/internal/server/storage/drivers"
	"github.com/lxc/incus/v7/shared/api"
)

var sharedRootTakeoverCmd = APIEndpoint{
	Name: "sharedRootTakeover", Path: "shared-root-takeovers/{token}",
	Get: APIEndpointAction{Handler: sharedRootTakeoverGet, AccessHandler: allowPermission(auth.ObjectTypeServer, auth.EntitlementCanEdit)},
	Put: APIEndpointAction{Handler: sharedRootTakeoverPut, AccessHandler: allowPermission(auth.ObjectTypeServer, auth.EntitlementCanEdit)},
}

type sharedRootTakeoverRequest struct {
	Action  string                     `json:"action"`
	Binding sharedroottakeover.Binding `json:"binding"`
}

type sharedRootTakeoverConfiguration struct {
	Instance api.Instance      `json:"instance"`
	Volume   api.StorageVolume `json:"volume"`
}

func sharedRootTakeoverToAPI(record *db.SharedRootTakeover) map[string]any {
	return map[string]any{
		"token": record.Token, "phase": record.Phase,
		"binding": json.RawMessage(record.Binding), "last_error": record.LastError,
	}
}

func sharedRootTakeoverGet(d *Daemon, r *http.Request) response.Response {
	token, err := storageMaterializationAttemptToken(r)
	if err != nil {
		return response.BadRequest(err)
	}

	record, err := sharedroottakeover.New(d.State().DB.Node).Get(r.Context(), token)
	if errors.Is(err, sharedroottakeover.ErrNotFound) {
		return response.NotFound(err)
	}
	if err != nil {
		return response.SmartError(err)
	}

	if record.Project != request.ProjectParam(r) {
		return response.NotFound(errors.New("Takeover belongs to another project"))
	}

	return response.SyncResponse(true, sharedRootTakeoverToAPI(record))
}

// The privileged Nova caller must verify fencing and hold the external exact target grant.
func sharedRootTakeoverPut(d *Daemon, r *http.Request) response.Response {
	s := d.State()
	if s.ServerClustered {
		return response.BadRequest(errors.New("Fenced shared-root takeover is for independent Incus nodes"))
	}

	req := sharedRootTakeoverRequest{}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	err = decoder.Decode(&struct{}{})
	if !errors.Is(err, io.EOF) {
		return response.BadRequest(errors.New("Takeover request must contain exactly one JSON object"))
	}

	b := req.Binding
	token, err := storageMaterializationAttemptToken(r)
	if err != nil {
		return response.BadRequest(err)
	}

	if b.Token != token || b.Project != request.ProjectParam(r) ||
		b.StorageVolume != project.Instance(b.Project, b.InstanceName) {
		return response.BadRequest(errors.New("Takeover path, project or root volume binding differs"))
	}

	configuration := sharedRootTakeoverConfiguration{}
	err = json.Unmarshal(b.Configuration, &configuration)
	if err != nil {
		return response.BadRequest(err)
	}

	err = validateSharedRootTakeoverConfiguration(b, configuration)
	if err != nil {
		return response.BadRequest(err)
	}

	unlock, err := locking.Lock(r.Context(), "storage_materialization_instance_"+b.Project+"_"+b.InstanceName)
	if err != nil {
		return response.InternalError(err)
	}

	defer unlock()
	manager := sharedroottakeover.New(s.DB.Node)
	record, err := manager.Get(r.Context(), b.Token)
	if errors.Is(err, sharedroottakeover.ErrNotFound) && req.Action == "prepare" {
		_, loadErr := instance.LoadByProjectAndName(s, b.Project, b.InstanceName)
		if loadErr == nil || !response.IsNotFoundError(loadErr) {
			return response.Conflict(errors.New("Takeover requires an absent target instance record"))
		}

		pool, vol, loadErr := loadSharedRootTakeoverVolume(s, b, configuration)
		if loadErr != nil {
			return response.Conflict(loadErr)
		}

		provider, ok := pool.Driver().(drivers.VolumeLocalStateProvider)
		if !ok {
			return response.Conflict(errors.New("Takeover requires exact local-state inspection"))
		}

		present, loadErr := provider.HasVolumeLocalState(vol, b.StorageIdentity)
		if loadErr != nil || present {
			return response.Conflict(errors.New("Takeover target has unresolved original-root local state"))
		}

		record, err = manager.Register(r.Context(), b)
	} else if err == nil {
		record, err = manager.Register(r.Context(), b)
	}

	if err != nil {
		return response.Conflict(err)
	}

	err = executeSharedRootTakeover(r.Context(), s, manager, b, configuration, req.Action, record.Phase)
	if err != nil {
		_ = manager.RecordFailure(r.Context(), b.Token, err)
		return response.Conflict(err)
	}

	record, err = manager.Get(r.Context(), b.Token)
	if err != nil {
		return response.SmartError(err)
	}

	return response.SyncResponse(true, sharedRootTakeoverToAPI(record))
}

func validateSharedRootTakeoverConfiguration(b sharedroottakeover.Binding, configuration sharedRootTakeoverConfiguration) error {
	i := configuration.Instance
	if i.Name != b.InstanceName || i.Type != string(api.InstanceTypeContainer) || i.Ephemeral || i.Stateful || i.Restore != "" {
		return errors.New("Takeover requires an ordinary stopped container configuration")
	}

	config := i.Config
	if config[internalInstance.ConfigOpenStackComputeID] != b.TargetComputeID ||
		config[internalInstance.ConfigOpenStackIDMapAllocationID] != b.AllocationID ||
		config[internalInstance.ConfigOpenStackRootfsMaterializationID] != b.Token ||
		config["user.openstack.uuid"] != b.Owner || config["security.privileged"] == "true" ||
		config["security.idmap.base"] != strconv.FormatInt(b.IDMapBase, 10) ||
		config["security.idmap.size"] != strconv.FormatInt(b.IDMapSize, 10) {
		return errors.New("Takeover configuration differs from owner, target or fixed ID-map binding")
	}

	roots := 0
	for _, device := range i.Devices {
		if device["type"] == "disk" && device["path"] == "/" {
			if device["pool"] != b.StoragePool || device["source"] != "" {
				return errors.New("Takeover root must use the exact Incus-managed pool")
			}

			roots++
		}
	}

	if roots != 1 {
		return errors.New("Takeover requires one explicit root device")
	}

	return nil
}

func loadSharedRootTakeoverVolume(s *state.State, b sharedroottakeover.Binding, configuration sharedRootTakeoverConfiguration) (storagePools.Pool, drivers.Volume, error) {
	pool, err := storagePools.LoadByName(s, b.StoragePool)
	if err != nil {
		return nil, drivers.Volume{}, err
	}

	if pool.Driver().Info().Name != "ceph" {
		return nil, drivers.Volume{}, errors.New("Takeover requires Incus-managed Ceph")
	}

	clusterProvider, ok := pool.Driver().(drivers.StorageClusterIdentityProvider)
	if !ok {
		return nil, drivers.Volume{}, errors.New("Takeover requires physical cluster identity")
	}

	fsid, err := clusterProvider.GetStorageClusterIdentity()
	if err != nil || fsid != b.ClusterFSID {
		return nil, drivers.Volume{}, errors.New("Takeover pool points to another physical Ceph cluster")
	}

	vol := pool.GetVolume(drivers.VolumeTypeContainer, drivers.ContentTypeFS, b.StorageVolume, configuration.Volume.Config)
	provider, ok := pool.Driver().(drivers.VolumeIdentityProvider)
	if !ok {
		return nil, vol, errors.New("Takeover requires immutable root identity")
	}

	identity, err := provider.GetVolumeIdentity(vol)
	if err != nil {
		return nil, vol, err
	}

	if identity != b.StorageIdentity {
		return nil, vol, errors.New("Takeover root name points to a different RBD image")
	}

	return pool, vol, nil
}

func executeSharedRootTakeover(ctx context.Context, s *state.State, manager *sharedroottakeover.Manager, b sharedroottakeover.Binding, configuration sharedRootTakeoverConfiguration, action string, phase string) error {
	if action == "prepare" {
		return nil
	}

	pool, vol, err := loadSharedRootTakeoverVolume(s, b, configuration)
	if err != nil {
		return err
	}

	marker, err := sharedroottakeover.OwnershipMarker(b)
	if err != nil {
		return err
	}

	if action == "claim" {
		if phase == sharedroottakeover.Prepared {
			_, err = manager.Advance(ctx, b, sharedroottakeover.Prepared, sharedroottakeover.Claiming)
			if err != nil {
				return err
			}

			phase = sharedroottakeover.Claiming
		}

		if phase != sharedroottakeover.Claiming && phase != sharedroottakeover.Claimed {
			return sharedroottakeover.ErrPhaseConflict
		}

		provider, ok := pool.Driver().(drivers.VolumeFencedOwnershipTransferProvider)
		if !ok {
			return errors.New("Storage cannot transfer fenced original-root ownership")
		}

		err = provider.TransferVolumeMaterializationOwnership(vol, b.StorageIdentity, b.ClusterFSID, b.SourceOwnership, marker)
		if err != nil {
			return err
		}

		_, err = manager.Advance(ctx, b, sharedroottakeover.Claiming, sharedroottakeover.Claimed)
		return err
	}

	provider, ok := pool.Driver().(drivers.VolumeMaterializationOwnershipProvider)
	if !ok {
		return errors.New("Storage cannot verify takeover ownership")
	}

	current, err := provider.GetVolumeMaterializationOwnership(vol)
	if err != nil || current != marker {
		return errors.New("Original-root ownership differs from the exact takeover transaction")
	}

	if action == "import" {
		if phase == sharedroottakeover.Claimed {
			_, err = manager.Advance(ctx, b, sharedroottakeover.Claimed, sharedroottakeover.Importing)
			if err != nil {
				return err
			}

			phase = sharedroottakeover.Importing
		}

		if phase != sharedroottakeover.Importing && phase != sharedroottakeover.Imported {
			return sharedroottakeover.ErrPhaseConflict
		}

		inst, err := instance.LoadByProjectAndName(s, b.Project, b.InstanceName)
		if response.IsNotFoundError(err) {
			configuration.Instance.Config["volatile.shared_root_takeover.token"] = b.Token
			configuration.Instance.Config["volatile.shared_root_takeover.ownership"] = marker
			configuration.Instance.Config["volatile.shared_root_takeover.storage_identity"] = b.StorageIdentity
			configuration.Instance.Config["volatile.shared_root_takeover.cluster_fsid"] = b.ClusterFSID
			configuration.Instance.Config["volatile.shared_root_takeover.generation"] = strconv.FormatUint(b.SourceGeneration+1, 10)
			configuration.Instance.Config[internalInstance.ConfigVolatileMigrationStorageDeleteProtection] = "true"
			backup := &backupConfig.Config{Container: &configuration.Instance, Volume: &configuration.Volume}
			inst, _, err = internalRecoverImportInstance(s, pool, b.Project, backup, nil)
		}

		if err != nil {
			return err
		}

		if inst.LocalConfig()["volatile.shared_root_takeover.token"] != b.Token || inst.IsRunning() {
			return errors.New("Import retry found another or running instance record")
		}

		_, volumeErr := storagePools.VolumeDBGet(pool, b.Project, b.InstanceName, drivers.VolumeTypeContainer)
		if volumeErr != nil && !response.IsNotFoundError(volumeErr) {
			return volumeErr
		}

		var backup *backupConfig.Config
		if response.IsNotFoundError(volumeErr) {
			backup = &backupConfig.Config{Container: &configuration.Instance, Volume: &configuration.Volume}
		}

		_, err = pool.ImportInstance(inst, backup, nil)
		if err != nil {
			return fmt.Errorf("Import exact original root records: %w", err)
		}

		_, err = manager.Advance(ctx, b, sharedroottakeover.Importing, sharedroottakeover.Imported)
		return err
	}

	inst, err := instance.LoadByProjectAndName(s, b.Project, b.InstanceName)
	if err != nil || inst.LocalConfig()["volatile.shared_root_takeover.token"] != b.Token {
		return errors.New("Takeover instance record is absent or belongs to another transaction")
	}

	switch action {
	case "commit":
		_, err = manager.Commit(ctx, b)
		if err != nil {
			return err
		}

		return inst.VolatileSet(map[string]string{internalInstance.ConfigVolatileMigrationStorageDeleteProtection: "false"})
	case "start":
		if phase == sharedroottakeover.Committed {
			_, err = manager.Advance(ctx, b, sharedroottakeover.Committed, sharedroottakeover.Starting)
			if err != nil {
				return err
			}

			phase = sharedroottakeover.Starting
		}

		if phase != sharedroottakeover.Starting && phase != sharedroottakeover.Started {
			return sharedroottakeover.ErrPhaseConflict
		}

		if !inst.IsRunning() {
			err = inst.Start(false)
			if err != nil {
				return err
			}
		}

		_, err = manager.Advance(ctx, b, sharedroottakeover.Starting, sharedroottakeover.Started)
		return err
	case "complete":
		if !inst.IsRunning() {
			return errors.New("Takeover cannot complete before the target is running")
		}

		_, err = manager.Advance(ctx, b, sharedroottakeover.Started, sharedroottakeover.Completed)
		return err
	default:
		return errors.New("Unknown shared-root takeover action")
	}
}

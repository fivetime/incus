package api

// StorageMaterializationAttemptPut registers or fences one rootfs materialization attempt.
//
// swagger:model
//
// API extension: storage_materialization_attempt_v1.
type StorageMaterializationAttemptPut struct {
	State string `json:"state" yaml:"state"`

	AllocationID       string `json:"allocation_id,omitempty" yaml:"allocation_id,omitempty"`
	ComputeID          string `json:"compute_id,omitempty" yaml:"compute_id,omitempty"`
	Owner              string `json:"owner,omitempty" yaml:"owner,omitempty"`
	InstanceName       string `json:"instance_name,omitempty" yaml:"instance_name,omitempty"`
	IDMapBase          *int64 `json:"idmap_base,omitempty" yaml:"idmap_base,omitempty"`
	IDMapSize          *int64 `json:"idmap_size,omitempty" yaml:"idmap_size,omitempty"`
	StorageDriver      string `json:"storage_driver,omitempty" yaml:"storage_driver,omitempty"`
	StoragePool        string `json:"storage_pool,omitempty" yaml:"storage_pool,omitempty"`
	StorageVolume      string `json:"storage_volume,omitempty" yaml:"storage_volume,omitempty"`
	RBDImage           string `json:"rbd_image,omitempty" yaml:"rbd_image,omitempty"`
	CleanupDisposition string `json:"cleanup_disposition,omitempty" yaml:"cleanup_disposition,omitempty"`
}

// StorageMaterializationProof proves either that materialization never began or that all local artifacts were reconciled.
//
// swagger:model
//
// API extension: storage_materialization_attempt_v1.
type StorageMaterializationProof struct {
	Outcome string `json:"outcome" yaml:"outcome"`
	Digest  string `json:"digest" yaml:"digest"`
}

// StorageMaterializationAttempt is a durable rootfs create fence and cleanup record.
//
// swagger:model
//
// API extension: storage_materialization_attempt_v1.
type StorageMaterializationAttempt struct {
	Token              string                       `json:"token" yaml:"token"`
	AllocationID       string                       `json:"allocation_id" yaml:"allocation_id"`
	ComputeID          string                       `json:"compute_id" yaml:"compute_id"`
	Owner              string                       `json:"owner" yaml:"owner"`
	Project            string                       `json:"project" yaml:"project"`
	InstanceName       string                       `json:"instance_name" yaml:"instance_name"`
	IDMapBase          int64                        `json:"idmap_base" yaml:"idmap_base"`
	IDMapSize          int64                        `json:"idmap_size" yaml:"idmap_size"`
	StorageDriver      string                       `json:"storage_driver" yaml:"storage_driver"`
	StoragePool        string                       `json:"storage_pool" yaml:"storage_pool"`
	StorageVolume      string                       `json:"storage_volume" yaml:"storage_volume"`
	RBDImage           string                       `json:"rbd_image,omitempty" yaml:"rbd_image,omitempty"`
	StorageIdentity    string                       `json:"storage_identity,omitempty" yaml:"storage_identity,omitempty"`
	BaselineClean      bool                         `json:"baseline_clean" yaml:"baseline_clean"`
	CleanupDisposition string                       `json:"cleanup_disposition" yaml:"cleanup_disposition"`
	State              string                       `json:"state" yaml:"state"`
	StoragePhase       string                       `json:"storage_phase" yaml:"storage_phase"`
	Started            bool                         `json:"started" yaml:"started"`
	Finished           bool                         `json:"finished" yaml:"finished"`
	OperationUUID      string                       `json:"operation_uuid,omitempty" yaml:"operation_uuid,omitempty"`
	DaemonStart        int64                        `json:"daemon_start" yaml:"daemon_start"`
	Proof              *StorageMaterializationProof `json:"proof,omitempty" yaml:"proof,omitempty"`
}

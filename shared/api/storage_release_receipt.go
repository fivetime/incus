package api

// StorageReleaseReceipt is durable node-local proof that one rootfs materialization was released.
//
// swagger:model
//
// API extension: storage_release_receipt_v2.
type StorageReleaseReceipt struct {
	// Canonical SHA-256 digest required to acknowledge this complete receipt
	Digest string `json:"digest" yaml:"digest"`

	// Materialization UUID used as the receipt token
	Token string `json:"token" yaml:"token"`

	// Fleet-wide ID map allocation generation UUID
	AllocationID string `json:"allocation_id" yaml:"allocation_id"`

	// Persistent Nova compute-node UUID
	ComputeID string `json:"compute_id" yaml:"compute_id"`

	// Per-create rootfs materialization UUID
	MaterializationID string `json:"materialization_id" yaml:"materialization_id"`

	// External owner UUID
	Owner string `json:"owner" yaml:"owner"`

	// Project containing the deleted instance
	Project string `json:"project" yaml:"project"`

	// Deleted instance name
	InstanceName string `json:"instance_name" yaml:"instance_name"`

	// Released host ID map base
	IDMapBase int64 `json:"idmap_base" yaml:"idmap_base"`

	// Released host ID map size
	IDMapSize int64 `json:"idmap_size" yaml:"idmap_size"`

	// Storage driver
	StorageDriver string `json:"storage_driver" yaml:"storage_driver"`

	// Storage pool
	StoragePool string `json:"storage_pool" yaml:"storage_pool"`

	// Internal storage volume name
	StorageVolume string `json:"storage_volume" yaml:"storage_volume"`

	// External RBD image name, when applicable
	RBDImage string `json:"rbd_image,omitempty" yaml:"rbd_image,omitempty"`

	// Immutable storage object identity
	StorageIdentity string `json:"storage_identity,omitempty" yaml:"storage_identity,omitempty"`

	// Whether the materialization began from a proven clean storage baseline
	BaselineClean bool `json:"baseline_clean" yaml:"baseline_clean"`

	// Materialization cleanup disposition
	CleanupDisposition string `json:"cleanup_disposition" yaml:"cleanup_disposition"`

	// Proven local storage outcome (deleted, normalized or detached)
	Outcome string `json:"outcome" yaml:"outcome"`

	// Receipt state
	State string `json:"state" yaml:"state"`

	// Unix timestamp when pending intent was persisted
	CreatedAt int64 `json:"created_at" yaml:"created_at"`

	// Unix timestamp when the storage outcome completed
	CompletedAt int64 `json:"completed_at" yaml:"completed_at"`
}

package instance

const (
	// ConfigVolatileRootfsIDMapReleaseToken binds root storage release to one materialization attempt.
	ConfigVolatileRootfsIDMapReleaseToken = "volatile.rootfs_idmap.release_token"

	// RootfsIDMapReleaseTokenQueryParam supplies the external generation on final instance deletion.
	RootfsIDMapReleaseTokenQueryParam = "rootfs-idmap-release-token"

	// ConfigVolatileRootfsIDMapReleaseOwner binds final root storage release to an external owner.
	ConfigVolatileRootfsIDMapReleaseOwner = "volatile.rootfs_idmap.release_owner"

	// RootfsIDMapReleaseOwnerQueryParam supplies the external owner on final instance deletion.
	RootfsIDMapReleaseOwnerQueryParam = "rootfs-idmap-release-owner"

	// ConfigVolatileRootfsIDMapAllocationID binds a receipt to the fleet-wide allocation generation.
	ConfigVolatileRootfsIDMapAllocationID = "volatile.rootfs_idmap.allocation_id"

	// RootfsIDMapAllocationIDQueryParam supplies the fleet-wide allocation generation.
	RootfsIDMapAllocationIDQueryParam = "rootfs-idmap-allocation-id"

	// ConfigVolatileRootfsIDMapComputeID binds a receipt to one persistent compute identity.
	ConfigVolatileRootfsIDMapComputeID = "volatile.rootfs_idmap.compute_id"

	// RootfsIDMapComputeIDQueryParam supplies the persistent compute identity.
	RootfsIDMapComputeIDQueryParam = "rootfs-idmap-compute-id"

	// ConfigOpenStackIDMapAllocationID is the authoritative instance-local allocation identity.
	ConfigOpenStackIDMapAllocationID = "user.openstack.idmap_allocation_id"

	// ConfigOpenStackComputeID is the authoritative instance-local compute identity.
	ConfigOpenStackComputeID = "user.openstack.compute_id"

	// ConfigOpenStackRootfsMaterializationID is the authoritative instance-local materialization identity.
	ConfigOpenStackRootfsMaterializationID = "user.openstack.rootfs_materialization_id"
)

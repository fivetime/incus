package drivers

// StorageClusterIdentityProvider identifies the physical storage cluster independently of its local name.
type StorageClusterIdentityProvider interface {
	GetStorageClusterIdentity() (string, error)
}

// VolumeFencedOwnershipTransferProvider atomically replaces ownership on an exact, fenced original root.
type VolumeFencedOwnershipTransferProvider interface {
	TransferVolumeMaterializationOwnership(vol Volume, expectedIdentity string, clusterFSID string, previous string, next string) error
}

// VolumeMaterializationOwnershipProvider stores durable ownership evidence on
// a storage object. The evidence is used only to recover a create attempt that
// crashed before its immutable storage identity reached the local database.
type VolumeMaterializationOwnershipProvider interface {
	GetVolumeMaterializationOwnership(vol Volume) (string, error)
	SetVolumeMaterializationOwnership(vol Volume, ownership string) error
}

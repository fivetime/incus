package drivers

import (
	"io"
	"net/url"

	"github.com/lxc/incus/v7/internal/instancewriter"
	"github.com/lxc/incus/v7/internal/server/backup"
	"github.com/lxc/incus/v7/internal/server/migration"
	"github.com/lxc/incus/v7/internal/server/operations"
	"github.com/lxc/incus/v7/internal/server/state"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/logger"
	"github.com/lxc/incus/v7/shared/revert"
)

// driver is the extended internal interface.
type driver interface {
	Driver

	init(s *state.State, name string, config map[string]string, log logger.Logger, volIDFunc func(volType VolumeType, volName string) (int64, error), commonRules *Validators)
	load() error
	isRemote() bool
}

// VolumeIdentityProvider returns a storage-system identity that survives renames but changes on object recreation.
type VolumeIdentityProvider interface {
	GetVolumeIdentity(vol Volume) (string, error)
}

// VolumeIdentityPresenceProvider reports whether an exact storage-system object still exists.
type VolumeIdentityPresenceProvider interface {
	HasVolumeIdentity(vol Volume, expectedStorageIdentity string) (bool, error)
}

// VolumeIdentityInitializer creates a fresh storage-system identity for a newly materialized volume.
type VolumeIdentityInitializer interface {
	InitializeVolumeIdentity(vol Volume) error
}

// VolumeLocalStateProvider reports whether an exact storage object still has host-local state.
type VolumeLocalStateProvider interface {
	HasVolumeLocalState(vol Volume, expectedStorageIdentity string) (bool, error)
}

// VolumeLocalStateReleaser safely removes host-local volume state without changing the backing object.
type VolumeLocalStateReleaser interface {
	VolumeLocalStateProvider
	ReleaseVolumeLocalState(vol Volume, expectedStorageIdentity string) error
}

// VolumeDetachedLocalStateReleaser tears down host-local state that provably belongs to a failed
// or abandoned claim. Unlike VolumeLocalStateReleaser it treats leftover mounts and in-memory
// mount references as the stale state being released rather than as evidence of a live user:
// a legitimate user always holds the volume's database record, which the caller has proven
// absent (or is deleting) before invoking this.
type VolumeDetachedLocalStateReleaser interface {
	VolumeLocalStateProvider
	ReleaseVolumeDetachedLocalState(vol Volume, expectedStorageIdentity string) error
}

// VolumeIdentityBoundDeleter deletes or releases a volume while holding its local volume lock after identity verification.
type VolumeIdentityBoundDeleter interface {
	DeleteVolumeWithIdentity(vol Volume, expectedStorageIdentity string, op *operations.Operation) error
}

// Driver represents a low-level storage driver.
type Driver interface {
	// Internal.
	Info() Info
	HasVolume(vol Volume) (bool, error)
	IsImageCloneSourceReady(vol Volume) (bool, error)
	roundVolumeBlockSizeBytes(vol Volume, sizeBytes int64) (int64, error)
	isBlockBacked(vol Volume) bool

	// Export struct details.
	Name() string
	Config() map[string]string
	Logger() logger.Logger

	// Pool.
	FillConfig() error
	Create() error
	Delete(op *operations.Operation) error
	// Mount mounts a storage pool if needed, returns true if we caused a new mount, false if already mounted.
	Mount() (bool, error)

	// Unmount unmounts a storage pool if needed, returns true if unmounted, false if was not mounted.
	Unmount() (bool, error)
	GetResources() (*api.ResourcesStoragePool, error)
	Validate(config map[string]string) error
	Update(changedConfig map[string]string) error
	ApplyPatch(name string) error

	// Buckets.
	ValidateBucket(bucket Volume) error
	GetBucketURL(bucketName string) *url.URL
	CreateBucket(bucket Volume, op *operations.Operation) error
	DeleteBucket(bucket Volume, op *operations.Operation) error
	UpdateBucket(bucket Volume, changedConfig map[string]string) error
	ValidateBucketKey(keyName string, creds S3Credentials, roleName string) error
	CreateBucketKey(bucket Volume, keyName string, creds S3Credentials, roleName string, op *operations.Operation) (*S3Credentials, error)
	UpdateBucketKey(bucket Volume, keyName string, creds S3Credentials, roleName string, op *operations.Operation) (*S3Credentials, error)
	DeleteBucketKey(bucket Volume, keyName string, op *operations.Operation) error

	// Volumes.
	FillVolumeConfig(vol Volume) error
	ValidateVolume(vol Volume, removeUnknownKeys bool) error
	CreateVolume(vol Volume, filler *VolumeFiller, op *operations.Operation) error
	CreateVolumeFromCopy(vol Volume, srcVol Volume, copySnapshots bool, allowInconsistent bool, op *operations.Operation) error
	RefreshVolume(vol Volume, srcVol Volume, srcSnapshots []Volume, allowInconsistent bool, op *operations.Operation) error
	DeleteVolume(vol Volume, op *operations.Operation) error
	RenameVolume(vol Volume, newName string, op *operations.Operation) error
	UpdateVolume(vol Volume, changedConfig map[string]string) error
	GetVolumeUsage(vol Volume) (int64, error)
	SetVolumeQuota(vol Volume, size string, allowUnsafeResize bool, op *operations.Operation) error
	GetVolumeDiskPath(vol Volume) (string, error)
	ListVolumes() ([]Volume, error)

	// ActivateTask is a low-level access function to get to the underlying storage.
	ActivateTask(vol Volume, task func(devPath string, op *operations.Operation) error, op *operations.Operation) error

	// MountVolume mounts a storage volume (if not mounted) and increments reference counter.
	MountVolume(vol Volume, op *operations.Operation) error

	// MountVolumeSnapshot mounts a storage volume snapshot as readonly.
	MountVolumeSnapshot(snapVol Volume, op *operations.Operation) error

	// CanDelegateVolume checks whether the volume can be delegated.
	CanDelegateVolume(vol Volume) bool

	// DelegateVolume allows for the volume to be managed by the instance.
	DelegateVolume(vol Volume, pid int) error

	// UnmountVolume unmounts a storage volume, returns true if unmounted, false if was not
	// mounted.
	UnmountVolume(vol Volume, keepBlockDev bool, op *operations.Operation) (bool, error)

	// UnmountVolume unmounts a storage volume snapshot, returns true if unmounted, false if was
	// not mounted.
	UnmountVolumeSnapshot(snapVol Volume, op *operations.Operation) (bool, error)

	CanRestoreVolume(vol Volume, snapshotName string) error
	CreateVolumeSnapshot(snapVol Volume, op *operations.Operation) error
	GetQcow2BackingFilePath(vol Volume) (string, error)
	DeleteVolumeSnapshot(snapVol Volume, op *operations.Operation) error
	RenameVolumeSnapshot(snapVol Volume, newSnapshotName string, op *operations.Operation) error
	VolumeSnapshots(vol Volume, op *operations.Operation) ([]string, error)
	RestoreVolume(vol Volume, snapshotName string, op *operations.Operation) error
	Qcow2DeletionCleanup(vol Volume, childName string) error

	// Migration.
	MigrationTypes(contentType ContentType, refresh bool, copySnapshots bool, clusterMove bool, storageMove bool) []migration.Type
	MigrateVolume(vol Volume, conn io.ReadWriteCloser, volSrcArgs *migration.VolumeSourceArgs, op *operations.Operation) error
	CreateVolumeFromMigration(vol Volume, conn io.ReadWriteCloser, volTargetArgs migration.VolumeTargetArgs, preFiller *VolumeFiller, op *operations.Operation) error

	// Backup.
	BackupVolume(vol Volume, writer instancewriter.InstanceWriter, basePrefix string, optimized bool, snapshots []string, op *operations.Operation) error
	CreateVolumeFromBackup(vol Volume, srcBackup backup.Info, srcData io.ReadSeeker, basePrefix string, op *operations.Operation) (VolumePostHook, revert.Hook, error)
}

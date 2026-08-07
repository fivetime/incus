//go:build linux && cgo && !agent

package db

import (
	"context"
)

// StorageReleaseReceipt is durable node-local evidence for one rootfs materialization release.
type StorageReleaseReceipt struct {
	Token              string
	AllocationID       string
	ComputeID          string
	MaterializationID  string
	Owner              string
	Project            string
	InstanceName       string
	IDMapBase          int64
	IDMapSize          int64
	StorageDriver      string
	StoragePool        string
	StorageVolume      string
	RBDImage           string
	StorageIdentity    string
	BaselineClean      bool
	CleanupDisposition string
	Outcome            string
	State              string
	CreatedAt          int64
	CompletedAt        int64
}

// GetStorageReleaseReceipt returns the receipt with the supplied token.
func (n *NodeTx) GetStorageReleaseReceipt(ctx context.Context, token string) (*StorageReleaseReceipt, error) {
	receipt := &StorageReleaseReceipt{}
	err := n.tx.QueryRowContext(ctx, `
SELECT token, allocation_id, compute_id, materialization_id, owner,
       project, instance_name, idmap_base, idmap_size,
       storage_driver, storage_pool, storage_volume, rbd_image,
       storage_identity, baseline_clean, cleanup_disposition,
       outcome, state, created_at, completed_at
FROM storage_release_receipts
WHERE token = ?
`, token).Scan(
		&receipt.Token,
		&receipt.AllocationID,
		&receipt.ComputeID,
		&receipt.MaterializationID,
		&receipt.Owner,
		&receipt.Project,
		&receipt.InstanceName,
		&receipt.IDMapBase,
		&receipt.IDMapSize,
		&receipt.StorageDriver,
		&receipt.StoragePool,
		&receipt.StorageVolume,
		&receipt.RBDImage,
		&receipt.StorageIdentity,
		&receipt.BaselineClean,
		&receipt.CleanupDisposition,
		&receipt.Outcome,
		&receipt.State,
		&receipt.CreatedAt,
		&receipt.CompletedAt,
	)
	if err != nil {
		return nil, err
	}

	return receipt, nil
}

// CreateStorageReleaseReceipt creates a pending receipt.
func (n *NodeTx) CreateStorageReleaseReceipt(ctx context.Context, receipt StorageReleaseReceipt) error {
	_, err := n.tx.ExecContext(ctx, `
INSERT INTO storage_release_receipts
    (token, allocation_id, compute_id, materialization_id, owner,
     project, instance_name, idmap_base, idmap_size,
     storage_driver, storage_pool, storage_volume, rbd_image,
     storage_identity, baseline_clean, cleanup_disposition,
     outcome, state, created_at, completed_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`,
		receipt.Token,
		receipt.AllocationID,
		receipt.ComputeID,
		receipt.MaterializationID,
		receipt.Owner,
		receipt.Project,
		receipt.InstanceName,
		receipt.IDMapBase,
		receipt.IDMapSize,
		receipt.StorageDriver,
		receipt.StoragePool,
		receipt.StorageVolume,
		receipt.RBDImage,
		receipt.StorageIdentity,
		receipt.BaselineClean,
		receipt.CleanupDisposition,
		receipt.Outcome,
		receipt.State,
		receipt.CreatedAt,
		receipt.CompletedAt,
	)

	return err
}

// CompleteStorageReleaseReceipt marks a matching pending receipt complete.
func (n *NodeTx) CompleteStorageReleaseReceipt(ctx context.Context, token string, completedAt int64) (bool, error) {
	result, err := n.tx.ExecContext(ctx, `
UPDATE storage_release_receipts
SET state = 'complete', completed_at = ?
WHERE token = ? AND state = 'pending'
`, completedAt, token)
	if err != nil {
		return false, err
	}

	count, err := result.RowsAffected()
	if err != nil {
		return false, err
	}

	return count > 0, nil
}

// SupersedeStorageReleaseReceiptOutcome rewrites the outcome of one pending
// receipt whose release work never ran. It matches the current outcome
// explicitly so a concurrent transition cannot be overwritten blindly.
func (n *NodeTx) SupersedeStorageReleaseReceiptOutcome(ctx context.Context, token string, fromOutcome string, toOutcome string) (bool, error) {
	result, err := n.tx.ExecContext(ctx, `
UPDATE storage_release_receipts
SET outcome = ?
WHERE token = ? AND state = 'pending' AND outcome = ?
`, toOutcome, token, fromOutcome)
	if err != nil {
		return false, err
	}

	count, err := result.RowsAffected()
	if err != nil {
		return false, err
	}

	return count > 0, nil
}

// RetireStorageReleaseReceipt retains an acknowledged token tombstone.
func (n *NodeTx) RetireStorageReleaseReceipt(ctx context.Context, token string) (bool, error) {
	result, err := n.tx.ExecContext(ctx, `
UPDATE storage_release_receipts
SET state = 'retired'
WHERE token = ? AND state = 'complete'
`, token)
	if err != nil {
		return false, err
	}

	return rowsChanged(result)
}

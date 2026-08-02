//go:build linux && cgo && !agent

package db

import (
	"context"
)

// StorageMaterializationAttempt is a durable local rootfs create fence.
type StorageMaterializationAttempt struct {
	Token              string
	AllocationID       string
	ComputeID          string
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
	ProofOutcome       string
	ProofDigest        string
	State              string
	StoragePhase       string
	Started            bool
	Finished           bool
	OperationUUID      string
	DaemonStart        int64
}

const storageMaterializationAttemptColumns = `token, allocation_id, compute_id, owner, project,
instance_name, idmap_base, idmap_size, storage_driver, storage_pool, storage_volume,
rbd_image, storage_identity, baseline_clean, cleanup_disposition, proof_outcome, proof_digest, state, storage_phase, started, finished, operation_uuid, daemon_start`

func scanStorageMaterializationAttempt(row interface{ Scan(...any) error }) (*StorageMaterializationAttempt, error) {
	attempt := &StorageMaterializationAttempt{}
	err := row.Scan(&attempt.Token, &attempt.AllocationID, &attempt.ComputeID, &attempt.Owner, &attempt.Project,
		&attempt.InstanceName, &attempt.IDMapBase, &attempt.IDMapSize, &attempt.StorageDriver, &attempt.StoragePool,
		&attempt.StorageVolume, &attempt.RBDImage, &attempt.StorageIdentity, &attempt.BaselineClean, &attempt.CleanupDisposition, &attempt.ProofOutcome, &attempt.ProofDigest, &attempt.State, &attempt.StoragePhase,
		&attempt.Started, &attempt.Finished, &attempt.OperationUUID, &attempt.DaemonStart)
	return attempt, err
}

// GetStorageMaterializationAttempt returns the attempt identified by token.
func (n *NodeTx) GetStorageMaterializationAttempt(ctx context.Context, token string) (*StorageMaterializationAttempt, error) {
	return scanStorageMaterializationAttempt(n.tx.QueryRowContext(ctx, `SELECT `+storageMaterializationAttemptColumns+` FROM storage_materialization_attempts WHERE token = ?`, token))
}

// GetStorageMaterializationAttemptByInstance returns the non-retired attempt for an instance name.
func (n *NodeTx) GetStorageMaterializationAttemptByInstance(ctx context.Context, projectName string, instanceName string) (*StorageMaterializationAttempt, error) {
	return scanStorageMaterializationAttempt(n.tx.QueryRowContext(ctx, `SELECT `+storageMaterializationAttemptColumns+` FROM storage_materialization_attempts WHERE project = ? AND instance_name = ? AND state != 'retired'`, projectName, instanceName))
}

// GetUnfinishedStorageMaterializationAttempts returns started attempts requiring crash recovery.
func (n *NodeTx) GetUnfinishedStorageMaterializationAttempts(ctx context.Context) ([]StorageMaterializationAttempt, error) {
	rows, err := n.tx.QueryContext(ctx, `SELECT `+storageMaterializationAttemptColumns+` FROM storage_materialization_attempts WHERE started = 1 AND finished = 0 AND state IN ('active', 'aborted') ORDER BY token`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	attempts := []StorageMaterializationAttempt{}
	for rows.Next() {
		attempt, err := scanStorageMaterializationAttempt(rows)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, *attempt)
	}

	return attempts, rows.Err()
}

// CreateStorageMaterializationAttempt registers a new active attempt.
func (n *NodeTx) CreateStorageMaterializationAttempt(ctx context.Context, attempt StorageMaterializationAttempt) error {
	_, err := n.tx.ExecContext(ctx, `INSERT INTO storage_materialization_attempts (`+storageMaterializationAttemptColumns+`)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		attempt.Token, attempt.AllocationID, attempt.ComputeID, attempt.Owner, attempt.Project, attempt.InstanceName,
		attempt.IDMapBase, attempt.IDMapSize, attempt.StorageDriver, attempt.StoragePool, attempt.StorageVolume,
		attempt.RBDImage, attempt.StorageIdentity, attempt.BaselineClean, attempt.CleanupDisposition, attempt.ProofOutcome, attempt.ProofDigest, attempt.State, attempt.StoragePhase, attempt.Started,
		attempt.Finished, attempt.OperationUUID, attempt.DaemonStart)
	return err
}

// StartStorageMaterializationAttempt atomically begins, binds and marks an operation pending.
func (n *NodeTx) StartStorageMaterializationAttempt(ctx context.Context, token string, daemonStart int64, operationUUID string) (bool, error) {
	result, err := n.tx.ExecContext(ctx, `UPDATE storage_materialization_attempts SET started = 1, daemon_start = ?, operation_uuid = ?, storage_phase = 'pending' WHERE token = ? AND state = 'active' AND storage_phase = 'none' AND started = 0 AND finished = 0 AND operation_uuid = ''`, daemonStart, operationUUID, token)
	if err != nil {
		return false, err
	}
	return rowsChanged(result)
}

// BindStorageMaterializationOperation binds a started materialization intent to
// the operation that will perform its side effects.
func (n *NodeTx) BindStorageMaterializationOperation(ctx context.Context, token string, daemonStart int64, operationUUID string) (bool, error) {
	result, err := n.tx.ExecContext(ctx, `UPDATE storage_materialization_attempts SET operation_uuid = ? WHERE token = ? AND state = 'active' AND storage_phase = 'pending' AND started = 1 AND finished = 0 AND daemon_start = ? AND operation_uuid = ''`, operationUUID, token, daemonStart)
	if err != nil {
		return false, err
	}

	return rowsChanged(result)
}

func (n *NodeTx) AbortStorageMaterializationAttempt(ctx context.Context, token string, proofOutcome string, proofDigest string) (bool, error) {
	result, err := n.tx.ExecContext(ctx, `UPDATE storage_materialization_attempts SET state = 'aborted', finished = CASE WHEN started = 0 THEN 1 ELSE finished END, proof_outcome = CASE WHEN started = 0 THEN ? ELSE proof_outcome END, proof_digest = CASE WHEN started = 0 THEN ? ELSE proof_digest END WHERE token = ? AND state = 'active' AND finished = 0`, proofOutcome, proofDigest, token)
	if err != nil {
		return false, err
	}
	return rowsChanged(result)
}

func (n *NodeTx) CommitStorageMaterializationAttempt(ctx context.Context, token string) (bool, error) {
	result, err := n.tx.ExecContext(ctx, `UPDATE storage_materialization_attempts SET state = 'committed', finished = 1 WHERE token = ? AND state = 'active' AND started = 1 AND finished = 0 AND storage_phase = 'materialized'`, token)
	if err != nil {
		return false, err
	}
	return rowsChanged(result)
}

func (n *NodeTx) SetStorageMaterializationPhase(ctx context.Context, token string, phase string, identity string) (bool, error) {
	result, err := n.tx.ExecContext(ctx, `UPDATE storage_materialization_attempts SET storage_phase = ?, storage_identity = CASE WHEN ? = '' THEN storage_identity ELSE ? END WHERE token = ? AND started = 1 AND finished = 0 AND state IN ('active', 'aborted') AND ((? = 'pending' AND storage_phase IN ('none', 'pending')) OR (? = 'materialized' AND storage_phase IN ('pending', 'materialized'))) AND (storage_identity = '' OR ? = '' OR storage_identity = ?)`, phase, identity, identity, token, phase, phase, identity, identity)
	if err != nil {
		return false, err
	}
	return rowsChanged(result)
}

func (n *NodeTx) FinishStorageMaterializationClean(ctx context.Context, token string, proofOutcome string, proofDigest string) (bool, error) {
	result, err := n.tx.ExecContext(ctx, `UPDATE storage_materialization_attempts SET state = 'clean', storage_phase = 'clean', finished = 1, operation_uuid = '', proof_outcome = ?, proof_digest = ? WHERE token = ? AND started = 1 AND finished = 0 AND state = 'aborted'`, proofOutcome, proofDigest, token)
	if err != nil {
		return false, err
	}
	return rowsChanged(result)
}

func (n *NodeTx) RetireStorageMaterializationAttempt(ctx context.Context, token string) (bool, error) {
	result, err := n.tx.ExecContext(ctx, `UPDATE storage_materialization_attempts SET state = 'retired', operation_uuid = '' WHERE token = ? AND finished = 1 AND state IN ('aborted', 'committed', 'clean', 'retired')`, token)
	if err != nil {
		return false, err
	}
	return rowsChanged(result)
}

//go:build linux && cgo && !agent

package db

import (
	"context"
)

// SharedRootTakeover is a local journal; the external coordinator grants cross-host authority.
type SharedRootTakeover struct {
	Token           string
	Project         string
	InstanceName    string
	ClusterFSID     string
	StorageIdentity string
	Binding         string
	Phase           string
	LastError       string
}

// GetSharedRootTakeover returns a transaction by its immutable token.
func (n *NodeTx) GetSharedRootTakeover(ctx context.Context, token string) (*SharedRootTakeover, error) {
	record := &SharedRootTakeover{}
	err := n.tx.QueryRowContext(ctx, `SELECT token, project, instance_name, cluster_fsid,
storage_identity, binding, phase, last_error FROM shared_root_takeovers WHERE token = ?`, token).Scan(
		&record.Token, &record.Project, &record.InstanceName, &record.ClusterFSID,
		&record.StorageIdentity, &record.Binding, &record.Phase, &record.LastError)
	return record, err
}

// CreateSharedRootTakeover persists a prepared transaction before storage changes.
func (n *NodeTx) CreateSharedRootTakeover(ctx context.Context, record SharedRootTakeover) error {
	_, err := n.tx.ExecContext(ctx, `INSERT INTO shared_root_takeovers
(token, project, instance_name, cluster_fsid, storage_identity, binding, phase, last_error)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, record.Token, record.Project, record.InstanceName,
		record.ClusterFSID, record.StorageIdentity, record.Binding, record.Phase, record.LastError)
	return err
}

// AdvanceSharedRootTakeover changes a phase only if the caller's previous phase still holds.
func (n *NodeTx) AdvanceSharedRootTakeover(ctx context.Context, token string, previous string, next string) (bool, error) {
	result, err := n.tx.ExecContext(ctx, `UPDATE shared_root_takeovers SET phase = ?, last_error = '' WHERE token = ? AND phase = ?`, next, token, previous)
	if err != nil {
		return false, err
	}

	return rowsChanged(result)
}

// FailSharedRootTakeover preserves the phase and original binding for a later exact retry.
func (n *NodeTx) FailSharedRootTakeover(ctx context.Context, token string, reason string) error {
	_, err := n.tx.ExecContext(ctx, `UPDATE shared_root_takeovers SET last_error = ? WHERE token = ?`, reason, token)
	return err
}

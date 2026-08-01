//go:build linux && cgo && !agent

package db

import (
	"context"
	"database/sql"
)

// MigrationAttempt is a durable local fence for an inbound migration request.
type MigrationAttempt struct {
	Token         string
	Project       string
	ResourceType  string
	ResourceName  string
	State         string
	Started       bool
	Finished      bool
	OperationUUID string
	IDMapBase     int64
	IDMapSize     int64
	DaemonStart   int64
}

// GetMigrationAttempt returns the migration attempt with the supplied token.
func (n *NodeTx) GetMigrationAttempt(ctx context.Context, token string) (*MigrationAttempt, error) {
	attempt := &MigrationAttempt{}
	err := n.tx.QueryRowContext(ctx, `
SELECT token, project, resource_type, resource_name, state, started, finished, operation_uuid,
       idmap_base, idmap_size, daemon_start
FROM migration_attempts
WHERE token = ?
`, token).Scan(
		&attempt.Token,
		&attempt.Project,
		&attempt.ResourceType,
		&attempt.ResourceName,
		&attempt.State,
		&attempt.Started,
		&attempt.Finished,
		&attempt.OperationUUID,
		&attempt.IDMapBase,
		&attempt.IDMapSize,
		&attempt.DaemonStart,
	)
	if err != nil {
		return nil, err
	}

	return attempt, nil
}

// CreateMigrationAttempt creates a new active migration attempt.
func (n *NodeTx) CreateMigrationAttempt(ctx context.Context, attempt MigrationAttempt) error {
	_, err := n.tx.ExecContext(ctx, `
INSERT INTO migration_attempts
    (token, project, resource_type, resource_name, state, started, finished, operation_uuid,
     idmap_base, idmap_size, daemon_start)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`,
		attempt.Token,
		attempt.Project,
		attempt.ResourceType,
		attempt.ResourceName,
		attempt.State,
		attempt.Started,
		attempt.Finished,
		attempt.OperationUUID,
		attempt.IDMapBase,
		attempt.IDMapSize,
		attempt.DaemonStart,
	)

	return err
}

// GetMigrationAttemptIDMapReservations returns unfinished isolated idmap reservations.
func (n *NodeTx) GetMigrationAttemptIDMapReservations(ctx context.Context) ([]MigrationAttempt, error) {
	rows, err := n.tx.QueryContext(ctx, `
SELECT token, project, resource_type, resource_name, state, started, finished, operation_uuid,
       idmap_base, idmap_size, daemon_start
FROM migration_attempts
WHERE finished = 0 AND idmap_base >= 0 AND idmap_size > 0
ORDER BY idmap_base
`)
	if err != nil {
		return nil, err
	}

	defer func() { _ = rows.Close() }()

	attempts := []MigrationAttempt{}
	for rows.Next() {
		attempt := MigrationAttempt{}
		err = rows.Scan(
			&attempt.Token,
			&attempt.Project,
			&attempt.ResourceType,
			&attempt.ResourceName,
			&attempt.State,
			&attempt.Started,
			&attempt.Finished,
			&attempt.OperationUUID,
			&attempt.IDMapBase,
			&attempt.IDMapSize,
			&attempt.DaemonStart,
		)
		if err != nil {
			return nil, err
		}

		attempts = append(attempts, attempt)
	}

	err = rows.Err()
	if err != nil {
		return nil, err
	}

	return attempts, nil
}

// BeginMigrationAttempt marks an active attempt as having entered instance creation.
func (n *NodeTx) BeginMigrationAttempt(ctx context.Context, token string, daemonStart int64) (bool, error) {
	result, err := n.tx.ExecContext(ctx, `
UPDATE migration_attempts
SET started = 1, daemon_start = ?
WHERE token = ? AND state = 'active' AND started = 0 AND finished = 0
`, daemonStart, token)
	if err != nil {
		return false, err
	}

	return rowsChanged(result)
}

// BindMigrationAttemptOperation records the target operation UUID.
func (n *NodeTx) BindMigrationAttemptOperation(ctx context.Context, token string, operationUUID string) (bool, error) {
	result, err := n.tx.ExecContext(ctx, `
UPDATE migration_attempts
SET operation_uuid = ?
WHERE token = ? AND started = 1 AND finished = 0
  AND state IN ('active', 'aborted') AND operation_uuid = ''
`, operationUUID, token)
	if err != nil {
		return false, err
	}

	return rowsChanged(result)
}

// CommitMigrationAttempt atomically wins against an abort fence.
func (n *NodeTx) CommitMigrationAttempt(ctx context.Context, token string) (bool, error) {
	result, err := n.tx.ExecContext(ctx, `
UPDATE migration_attempts
SET state = 'committed', finished = 1
WHERE token = ? AND state = 'active' AND started = 1 AND finished = 0
`, token)
	if err != nil {
		return false, err
	}

	return rowsChanged(result)
}

// AbortMigrationAttempt atomically fences an attempt.
func (n *NodeTx) AbortMigrationAttempt(ctx context.Context, token string) (bool, error) {
	result, err := n.tx.ExecContext(ctx, `
UPDATE migration_attempts
SET state = 'aborted', finished = CASE WHEN started = 0 THEN 1 ELSE finished END
WHERE token = ? AND state = 'active' AND finished = 0
`, token)
	if err != nil {
		return false, err
	}

	return rowsChanged(result)
}

// FinishMigrationAttemptFailure marks cleanup complete without overriding an abort fence.
func (n *NodeTx) FinishMigrationAttemptFailure(ctx context.Context, token string) (bool, error) {
	result, err := n.tx.ExecContext(ctx, `
UPDATE migration_attempts
SET state = CASE WHEN state = 'active' THEN 'failed' ELSE state END,
    finished = 1,
    operation_uuid = ''
WHERE token = ? AND started = 1 AND finished = 0 AND state IN ('active', 'aborted')
`, token)
	if err != nil {
		return false, err
	}

	return rowsChanged(result)
}

// SettleAbortedMigrationAttempt marks an aborted attempt clean after external reconciliation.
func (n *NodeTx) SettleAbortedMigrationAttempt(ctx context.Context, token string) (bool, error) {
	result, err := n.tx.ExecContext(ctx, `
UPDATE migration_attempts
SET finished = 1, operation_uuid = ''
WHERE token = ? AND state = 'aborted' AND finished = 0
`, token)
	if err != nil {
		return false, err
	}

	return rowsChanged(result)
}

// DeleteMigrationAttempt retires a terminal migration attempt while retaining its token tombstone.
func (n *NodeTx) DeleteMigrationAttempt(ctx context.Context, token string) (bool, error) {
	result, err := n.tx.ExecContext(ctx, `
UPDATE migration_attempts
SET project = '', resource_type = '', resource_name = '',
    state = 'retired', started = 0, operation_uuid = '',
    idmap_base = -1, idmap_size = 0, daemon_start = 0
WHERE token = ? AND finished = 1 AND state IN ('aborted', 'committed', 'failed', 'retired')
`, token)
	if err != nil {
		return false, err
	}

	return rowsChanged(result)
}

func rowsChanged(result sql.Result) (bool, error) {
	count, err := result.RowsAffected()
	if err != nil {
		return false, err
	}

	return count > 0, nil
}

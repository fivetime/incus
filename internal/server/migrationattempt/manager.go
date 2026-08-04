//go:build linux && cgo && !agent

package migrationattempt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	"github.com/lxc/incus/v7/internal/server/db"
)

const (
	// StateActive permits the target migration to proceed.
	StateActive = "active"

	// StateAborted permanently fences this token from committing.
	StateAborted = "aborted"

	// StateCommitted records that the target won the commit race.
	StateCommitted = "committed"

	// StateFailed records that target rollback completed after a failure.
	StateFailed = "failed"

	// StateRetired is a permanent tombstone for a garbage-collected token.
	StateRetired = "retired"

	// ResourceTypeInstance identifies an instance migration attempt.
	ResourceTypeInstance = "instance"

	// RequestStateSettled explicitly confirms cleanup of an aborted target after reconciliation.
	RequestStateSettled = "settled"
)

var (
	// ErrNotFound indicates that a token was never registered or was garbage collected.
	ErrNotFound = errors.New("Migration attempt not found")

	// ErrBindingMismatch indicates that a token was reused for a different resource.
	ErrBindingMismatch = errors.New("Migration attempt token is bound to another resource")

	// ErrAlreadyStarted indicates that the same target request has already started.
	ErrAlreadyStarted = errors.New("Migration attempt has already started")

	// ErrOperationMismatch indicates that a started attempt was rebound to another operation.
	ErrOperationMismatch = errors.New("Migration attempt is bound to another operation")

	// ErrAborted indicates that the attempt was fenced before commit.
	ErrAborted = errors.New("Migration attempt is aborted")

	// ErrCommitted indicates that the target committed before an abort request.
	ErrCommitted = errors.New("Migration attempt is already committed")

	// ErrFinished indicates that a terminal attempt cannot be changed.
	ErrFinished = errors.New("Migration attempt is already finished")

	// ErrInvalidIDMap indicates an incomplete or invalid isolated idmap reservation.
	ErrInvalidIDMap = errors.New("Invalid migration attempt idmap reservation")

	// ErrIDMapOverlap indicates that another unfinished attempt owns an overlapping range.
	ErrIDMapOverlap = errors.New("Migration attempt idmap reservation overlaps another attempt")
)

// Manager coordinates durable local migration attempt fences.
type Manager struct {
	node *db.Node
}

// New returns a migration attempt manager backed by the node-local database.
func New(node *db.Node) *Manager {
	return &Manager{node: node}
}

// Register creates an active token or returns the existing matching
// registration. daemonStart identifies the current daemon generation so that
// reservations stranded by an earlier one do not fence a new attempt.
func (m *Manager) Register(ctx context.Context, token string, projectName string, resourceType string, resourceName string, idmapBase int64, idmapSize int64, daemonStart int64) (*db.MigrationAttempt, error) {
	if !validIDMapReservation(idmapBase, idmapSize) {
		return nil, ErrInvalidIDMap
	}

	var attempt *db.MigrationAttempt
	err := m.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		current, err := tx.GetMigrationAttempt(ctx, token)
		if err == nil {
			if current.State == StateRetired {
				return ErrNotFound
			}

			if !matches(current, projectName, resourceType, resourceName, idmapBase, idmapSize) {
				return ErrBindingMismatch
			}

			if current.State != StateActive {
				return stateError(current)
			}

			attempt = current
			return nil
		}

		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		if idmapBase >= 0 {
			reservations, err := tx.GetMigrationAttemptIDMapReservations(ctx, daemonStart)
			if err != nil {
				return err
			}

			for _, reservation := range reservations {
				if rangesOverlap(idmapBase, idmapSize, reservation.IDMapBase, reservation.IDMapSize) {
					return fmt.Errorf("%w: requested %d-%d conflicts with token %q range %d-%d",
						ErrIDMapOverlap,
						idmapBase,
						idmapBase+idmapSize-1,
						reservation.Token,
						reservation.IDMapBase,
						reservation.IDMapBase+reservation.IDMapSize-1)
				}
			}
		}

		current = &db.MigrationAttempt{
			Token:        token,
			Project:      projectName,
			ResourceType: resourceType,
			ResourceName: resourceName,
			State:        StateActive,
			IDMapBase:    idmapBase,
			IDMapSize:    idmapSize,
		}

		err = tx.CreateMigrationAttempt(ctx, *current)
		if err != nil {
			return err
		}

		attempt = current
		return nil
	})
	if err != nil {
		return nil, err
	}

	return attempt, nil
}

// Get returns a migration attempt.
func (m *Manager) Get(ctx context.Context, token string) (*db.MigrationAttempt, error) {
	var attempt *db.MigrationAttempt
	err := m.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		var err error
		attempt, err = tx.GetMigrationAttempt(ctx, token)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	return attempt, nil
}

// Begin binds the first inbound create request to the registered token.
func (m *Manager) Begin(ctx context.Context, token string, projectName string, resourceType string, resourceName string, daemonStart int64) (*db.MigrationAttempt, error) {
	var attempt *db.MigrationAttempt
	err := m.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		current, err := tx.GetMigrationAttempt(ctx, token)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}

		if err != nil {
			return err
		}

		if current.State == StateRetired {
			return ErrNotFound
		}

		if !matchesResource(current, projectName, resourceType, resourceName) {
			return ErrBindingMismatch
		}

		if current.State != StateActive {
			return stateError(current)
		}

		if current.Started {
			return ErrAlreadyStarted
		}

		changed, err := tx.BeginMigrationAttempt(ctx, token, daemonStart)
		if err != nil {
			return err
		}

		if !changed {
			return ErrAlreadyStarted
		}

		current.Started = true
		current.DaemonStart = daemonStart
		attempt = current
		return nil
	})
	if err != nil {
		return nil, err
	}

	return attempt, nil
}

// IDMapReservations returns the isolated idmap reservations that are still
// live in the daemon generation identified by daemonStart.
func (m *Manager) IDMapReservations(ctx context.Context, daemonStart int64) ([]db.MigrationAttempt, error) {
	var attempts []db.MigrationAttempt
	err := m.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		var err error
		attempts, err = tx.GetMigrationAttemptIDMapReservations(ctx, daemonStart)
		return err
	})

	return attempts, err
}

// ListPending returns every attempt that has not been retired.
func (m *Manager) ListPending(ctx context.Context) ([]db.MigrationAttempt, error) {
	var attempts []db.MigrationAttempt
	err := m.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		var err error
		attempts, err = tx.GetPendingMigrationAttempts(ctx)
		return err
	})

	return attempts, err
}

// BindOperation records the operation used by a started attempt.
func (m *Manager) BindOperation(ctx context.Context, token string, operationUUID string) error {
	return m.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		current, err := tx.GetMigrationAttempt(ctx, token)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}

		if err != nil {
			return err
		}

		if current.OperationUUID != "" {
			if current.OperationUUID == operationUUID {
				return nil
			}

			return ErrOperationMismatch
		}

		changed, err := tx.BindMigrationAttemptOperation(ctx, token, operationUUID)
		if err != nil {
			return err
		}

		if changed {
			return nil
		}

		current, err = tx.GetMigrationAttempt(ctx, token)
		if err == nil && current.OperationUUID != "" {
			if current.OperationUUID == operationUUID {
				return nil
			}

			return ErrOperationMismatch
		}

		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		return currentStateError(ctx, tx, token)
	})
}

// CheckActive rejects a fenced or terminal attempt.
func (m *Manager) CheckActive(ctx context.Context, token string) error {
	return m.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		attempt, err := tx.GetMigrationAttempt(ctx, token)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}

		if err != nil {
			return err
		}

		if attempt.State != StateActive {
			return stateError(attempt)
		}

		if !attempt.Started || attempt.Finished {
			return ErrFinished
		}

		return nil
	})
}

// Commit atomically transitions an active target to committed.
func (m *Manager) Commit(ctx context.Context, token string) error {
	return m.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		changed, err := tx.CommitMigrationAttempt(ctx, token)
		if err != nil {
			return err
		}

		if changed {
			return nil
		}

		return currentStateError(ctx, tx, token)
	})
}

// Abort installs a durable fence. It is idempotent for an already aborted attempt.
func (m *Manager) Abort(ctx context.Context, token string) (*db.MigrationAttempt, error) {
	var attempt *db.MigrationAttempt
	err := m.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		current, err := tx.GetMigrationAttempt(ctx, token)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}

		if err != nil {
			return err
		}

		switch current.State {
		case StateAborted, StateFailed:
			attempt = current
			return nil
		case StateCommitted:
			return ErrCommitted
		case StateActive:
			changed, err := tx.AbortMigrationAttempt(ctx, token)
			if err != nil {
				return err
			}

			if !changed {
				return currentStateError(ctx, tx, token)
			}

			current.State = StateAborted
			if !current.Started {
				current.Finished = true
			}

			attempt = current
			return nil
		default:
			return fmt.Errorf("%w: state %q", ErrFinished, current.State)
		}
	})
	if err != nil {
		return nil, err
	}

	return attempt, nil
}

// FinishFailure records that target rollback has completed.
func (m *Manager) FinishFailure(ctx context.Context, token string) error {
	return m.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		changed, err := tx.FinishMigrationAttemptFailure(ctx, token)
		if err != nil {
			return err
		}

		if changed {
			return nil
		}

		attempt, err := tx.GetMigrationAttempt(ctx, token)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}

		if err != nil {
			return err
		}

		if attempt.Finished {
			return nil
		}

		return stateError(attempt)
	})
}

// SettleAborted marks cleanup complete after no target resource or operation remains.
func (m *Manager) SettleAborted(ctx context.Context, token string) error {
	return m.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		changed, err := tx.SettleAbortedMigrationAttempt(ctx, token)
		if err != nil {
			return err
		}

		if changed {
			return nil
		}

		attempt, err := tx.GetMigrationAttempt(ctx, token)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}

		if err != nil {
			return err
		}

		if attempt.State == StateAborted && attempt.Finished {
			return nil
		}

		return stateError(attempt)
	})
}

// Delete garbage collects a terminal attempt while retaining a permanent token tombstone.
func (m *Manager) Delete(ctx context.Context, token string) error {
	return m.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		changed, err := tx.DeleteMigrationAttempt(ctx, token)
		if err != nil {
			return err
		}

		if changed {
			return nil
		}

		attempt, err := tx.GetMigrationAttempt(ctx, token)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}

		if err != nil {
			return err
		}

		return stateError(attempt)
	})
}

func currentStateError(ctx context.Context, tx *db.NodeTx, token string) error {
	attempt, err := tx.GetMigrationAttempt(ctx, token)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}

	if err != nil {
		return err
	}

	return stateError(attempt)
}

func stateError(attempt *db.MigrationAttempt) error {
	switch attempt.State {
	case StateAborted:
		return ErrAborted
	case StateCommitted:
		return ErrCommitted
	case StateFailed:
		return ErrFinished
	case StateRetired:
		return ErrNotFound
	case StateActive:
		if attempt.Started {
			return ErrAlreadyStarted
		}

		if attempt.Finished {
			return ErrFinished
		}
	}

	return fmt.Errorf("%w: state %q", ErrFinished, attempt.State)
}

func matches(attempt *db.MigrationAttempt, projectName string, resourceType string, resourceName string, idmapBase int64, idmapSize int64) bool {
	return matchesResource(attempt, projectName, resourceType, resourceName) &&
		attempt.IDMapBase == idmapBase &&
		attempt.IDMapSize == idmapSize
}

func matchesResource(attempt *db.MigrationAttempt, projectName string, resourceType string, resourceName string) bool {
	return attempt.Project == projectName &&
		attempt.ResourceType == resourceType &&
		attempt.ResourceName == resourceName
}

func validIDMapReservation(base int64, size int64) bool {
	return (base == -1 && size == 0) || (base >= 0 && size > 0 && base <= math.MaxInt64-size)
}

func rangesOverlap(baseA int64, sizeA int64, baseB int64, sizeB int64) bool {
	return baseA < baseB+sizeB && baseB < baseA+sizeA
}

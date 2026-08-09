//go:build linux && cgo && !agent

package storagematerializationattempt

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/lxc/incus/v7/internal/server/db"
)

// States of a storage materialization attempt. An attempt is active until it either commits
// or aborts; an aborted attempt becomes clean once its storage side effects are undone, and a
// finished attempt is retired so a later attempt can bind the same instance.
const (
	StateActive    = "active"
	StateAborted   = "aborted"
	StateCommitted = "committed"
	StateClean     = "clean"
	StateRetired   = "retired"

	PhaseNone         = "none"
	PhasePending      = "pending"
	PhaseMaterialized = "materialized"
	PhaseClean        = "clean"

	RequestStateSettled  = "settled"
	ProofNotMaterialized = "not-materialized"
	ProofReconciledClean = "reconciled-clean"

	CleanupDelete   = "delete"
	CleanupDetach   = "detach"
	CleanupHandover = "handover"
)

// Errors reported by Manager. Callers distinguish them to decide whether a request is a
// retry of their own attempt, a collision with somebody else's, or a lost race.
var (
	ErrNotFound                        = errors.New("Storage materialization attempt not found")
	ErrBindingMismatch                 = errors.New("Storage materialization attempt token is bound to another rootfs")
	ErrAlreadyStarted                  = errors.New("Storage materialization attempt has already started")
	ErrAborted                         = errors.New("Storage materialization attempt is aborted")
	ErrCommitted                       = errors.New("Storage materialization attempt is committed")
	ErrNotMaterialized                 = errors.New("Storage materialization attempt has not recorded a materialized rootfs")
	ErrHandoverRequiresMigrationCommit = errors.New("Storage handover materialization must commit with its migration attempt")
	ErrFinished                        = errors.New("Storage materialization attempt is finished")
)

// Manager owns the node-local record of storage materialization attempts, the marker that
// lets an interrupted rootfs materialization be recognised and undone after a restart.
type Manager struct{ node *db.Node }

// New returns a Manager backed by the node database.
func New(node *db.Node) *Manager { return &Manager{node: node} }

// Get returns the attempt with the given token, or ErrNotFound.
func (m *Manager) Get(ctx context.Context, token string) (*db.StorageMaterializationAttempt, error) {
	var attempt *db.StorageMaterializationAttempt
	err := m.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		var err error
		attempt, err = tx.GetStorageMaterializationAttempt(ctx, token)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}

		return err
	})
	return attempt, err
}

// GetByInstance returns the non-retired attempt for an instance name.
func (m *Manager) GetByInstance(ctx context.Context, projectName string, instanceName string) (*db.StorageMaterializationAttempt, error) {
	var attempt *db.StorageMaterializationAttempt
	err := m.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		var err error
		attempt, err = tx.GetStorageMaterializationAttemptByInstance(ctx, projectName, instanceName)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}

		return err
	})

	return attempt, err
}

// ListUnfinished returns started non-terminal attempts that require reconciliation.
func (m *Manager) ListUnfinished(ctx context.Context) ([]db.StorageMaterializationAttempt, error) {
	var attempts []db.StorageMaterializationAttempt
	err := m.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		var err error
		attempts, err = tx.GetUnfinishedStorageMaterializationAttempts(ctx)
		return err
	})

	return attempts, err
}

// Register records a new attempt, or returns the caller's own existing one so a retried request
// is not treated as a collision. An attempt bound to a different rootfs is refused.
func (m *Manager) Register(ctx context.Context, expected db.StorageMaterializationAttempt) (*db.StorageMaterializationAttempt, error) {
	err := validateBinding(&expected)
	if err != nil {
		return nil, err
	}

	var attempt *db.StorageMaterializationAttempt
	err = m.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		current, err := tx.GetStorageMaterializationAttempt(ctx, expected.Token)
		if err == nil {
			if current.State == StateRetired || !SameBinding(current, &expected) {
				return ErrBindingMismatch
			}

			if current.State != StateActive || current.Started || current.Finished {
				return stateError(current)
			}

			attempt = current
			return nil
		}

		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		_, err = tx.GetStorageMaterializationAttemptByInstance(ctx, expected.Project, expected.InstanceName)
		if err == nil {
			return ErrBindingMismatch
		}

		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		expected.State = StateActive
		expected.StoragePhase = PhaseNone
		err = tx.CreateStorageMaterializationAttempt(ctx, expected)
		if err != nil {
			return err
		}

		attempt = &expected
		return nil
	})
	return attempt, err
}

// Start atomically begins an attempt and marks storage pending. The operation
// UUID can be bound later when callers must persist intent before constructing
// the operation and its target instance.
func (m *Manager) Start(ctx context.Context, token string, expected db.StorageMaterializationAttempt, daemonStart int64, operationUUID string) (*db.StorageMaterializationAttempt, error) {
	var attempt *db.StorageMaterializationAttempt
	err := m.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		current, err := tx.GetStorageMaterializationAttempt(ctx, token)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}

		if err != nil {
			return err
		}

		if !SameBinding(current, &expected) {
			return ErrBindingMismatch
		}

		if current.State != StateActive {
			return stateError(current)
		}

		if current.Started {
			return ErrAlreadyStarted
		}

		changed, err := tx.StartStorageMaterializationAttempt(ctx, token, daemonStart, operationUUID)
		if err != nil {
			return err
		}

		if !changed {
			return currentStateError(ctx, tx, token)
		}

		current.Started = true
		current.DaemonStart = daemonStart
		current.OperationUUID = operationUUID
		current.StoragePhase = PhasePending
		attempt = current
		return nil
	})

	return attempt, err
}

// BindOperation binds a started pending attempt to its operation. Repeating an
// exact binding is idempotent; rebinding a generation is rejected.
func (m *Manager) BindOperation(ctx context.Context, token string, daemonStart int64, operationUUID string) error {
	if operationUUID == "" {
		return errors.New("Storage materialization operation UUID is required")
	}

	return m.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		current, err := tx.GetStorageMaterializationAttempt(ctx, token)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}

		if err != nil {
			return err
		}

		if current.State != StateActive || !current.Started || current.Finished || current.StoragePhase != PhasePending || current.DaemonStart != daemonStart {
			return stateError(current)
		}

		if current.OperationUUID == operationUUID {
			return nil
		}

		if current.OperationUUID != "" {
			return ErrBindingMismatch
		}

		changed, err := tx.BindStorageMaterializationOperation(ctx, token, daemonStart, operationUUID)
		if err != nil {
			return err
		}

		if !changed {
			return currentStateError(ctx, tx, token)
		}

		return nil
	})
}

// Abort marks an attempt aborted. One that never started is finished immediately, since it has
// no storage side effects to undo.
func (m *Manager) Abort(ctx context.Context, token string) (*db.StorageMaterializationAttempt, error) {
	var attempt *db.StorageMaterializationAttempt
	err := m.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		current, err := tx.GetStorageMaterializationAttempt(ctx, token)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}

		if err != nil {
			return err
		}

		switch current.State {
		case StateAborted, StateClean:
			attempt = current
			return nil
		case StateCommitted:
			return ErrCommitted
		case StateActive:
			proofOutcome := ""
			proofDigest := ""
			if !current.Started {
				proofOutcome = ProofNotMaterialized
				proofDigest, err = ProofDigest(current, proofOutcome)
				if err != nil {
					return err
				}
			}

			changed, err := tx.AbortStorageMaterializationAttempt(ctx, token, proofOutcome, proofDigest)
			if err != nil {
				return err
			}

			if !changed {
				return currentStateError(ctx, tx, token)
			}

			current.State = StateAborted
			if !current.Started {
				current.Finished = true
				current.ProofOutcome = proofOutcome
				current.ProofDigest = proofDigest
			}

			attempt = current
			return nil
		default:
			return stateError(current)
		}
	})
	return attempt, err
}

// Commit finishes a started attempt whose rootfs is materialized.
func (m *Manager) Commit(ctx context.Context, token string) error {
	return m.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		current, err := tx.GetStorageMaterializationAttempt(ctx, token)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}

		if err != nil {
			return err
		}

		if current.CleanupDisposition == CleanupHandover {
			return ErrHandoverRequiresMigrationCommit
		}

		if current.StoragePhase != PhaseMaterialized {
			return ErrNotMaterialized
		}

		if (current.StorageDriver == "ceph" || current.StorageDriver == "cephext") && current.StorageIdentity == "" {
			return errors.New("Ceph materialization is missing its immutable storage identity")
		}

		changed, err := tx.CommitStorageMaterializationAttempt(ctx, token)
		if err != nil {
			return err
		}

		if !changed {
			return currentStateError(ctx, tx, token)
		}

		return nil
	})
}

// CommitWithMigration atomically commits a migration handover and its rootfs materialization.
func (m *Manager) CommitWithMigration(ctx context.Context, migrationToken string, token string) error {
	return m.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		migrationAttempt, err := tx.GetMigrationAttempt(ctx, migrationToken)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}

		if err != nil {
			return err
		}

		materializationAttempt, err := tx.GetStorageMaterializationAttempt(ctx, token)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}

		if err != nil {
			return err
		}

		if migrationAttempt.ResourceType != "instance" || migrationAttempt.Project != materializationAttempt.Project || migrationAttempt.ResourceName != materializationAttempt.InstanceName {
			return ErrBindingMismatch
		}

		if !migrationAttempt.Started || !materializationAttempt.Started || migrationAttempt.OperationUUID == "" || migrationAttempt.OperationUUID != materializationAttempt.OperationUUID {
			return ErrBindingMismatch
		}

		if materializationAttempt.StoragePhase != PhaseMaterialized {
			return ErrNotMaterialized
		}

		if (materializationAttempt.StorageDriver == "ceph" || materializationAttempt.StorageDriver == "cephext") && materializationAttempt.StorageIdentity == "" {
			return errors.New("Ceph materialization is missing its immutable storage identity")
		}

		migrationChanged, err := tx.CommitMigrationAttempt(ctx, migrationToken)
		if err != nil {
			return err
		}

		if !migrationChanged {
			return errors.New("Migration attempt cannot be committed")
		}

		materializationChanged, err := tx.CommitStorageMaterializationAttempt(ctx, token)
		if err != nil {
			return err
		}

		if !materializationChanged {
			return errors.New("Storage materialization attempt cannot be committed")
		}

		return nil
	})
}

// SetStoragePhase advances how far the rootfs has been materialized, refusing a move that skips
// a phase or that would rebind the attempt to a different storage identity.
func (m *Manager) SetStoragePhase(ctx context.Context, token string, phase string, identity string) error {
	if phase != PhasePending && phase != PhaseMaterialized {
		return fmt.Errorf("Invalid storage materialization phase %q", phase)
	}

	return m.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		current, err := tx.GetStorageMaterializationAttempt(ctx, token)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}

		if err != nil {
			return err
		}

		if current.State != StateActive && current.State != StateAborted {
			return stateError(current)
		}

		if !current.Started || current.Finished {
			return stateError(current)
		}

		if phase == PhasePending && current.StoragePhase != PhaseNone && current.StoragePhase != PhasePending {
			return errors.New("Storage materialization phase cannot move backwards to pending")
		}

		if phase == PhaseMaterialized && current.StoragePhase != PhasePending && current.StoragePhase != PhaseMaterialized {
			return errors.New("Storage materialization can only become materialized after pending")
		}

		if current.StorageIdentity != "" && identity != "" && current.StorageIdentity != identity {
			return errors.New("Storage materialization identity is immutable")
		}

		if phase == PhaseMaterialized && (current.StorageDriver == "ceph" || current.StorageDriver == "cephext") && identity == "" && current.StorageIdentity == "" {
			return errors.New("Ceph materialization requires an immutable storage identity")
		}

		changed, err := tx.SetStorageMaterializationPhase(ctx, token, phase, identity)
		if err != nil {
			return err
		}

		if !changed {
			return currentStateError(ctx, tx, token)
		}

		return nil
	})
}

// FinishClean records that an aborted attempt's storage side effects are gone, which is the
// proof an orchestrator needs before retiring the allocation behind it.
func (m *Manager) FinishClean(ctx context.Context, token string) error {
	return m.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		current, err := tx.GetStorageMaterializationAttempt(ctx, token)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}

		if err != nil {
			return err
		}

		if current.State == StateClean && current.Finished {
			return nil
		}

		if current.State != StateAborted || !current.Started || current.Finished {
			return stateError(current)
		}

		proofDigest, err := ProofDigest(current, ProofReconciledClean)
		if err != nil {
			return err
		}

		changed, err := tx.FinishStorageMaterializationClean(ctx, token, ProofReconciledClean, proofDigest)
		if err != nil {
			return err
		}

		if changed {
			return nil
		}

		current, getErr := tx.GetStorageMaterializationAttempt(ctx, token)
		if getErr == nil && current.State == StateClean && current.Finished {
			return nil
		}

		if getErr != nil {
			return getErr
		}

		return stateError(current)
	})
}

// ProofDigest returns the stable digest of an immutable materialization binding and terminal outcome.
func ProofDigest(attempt *db.StorageMaterializationAttempt, outcome string) (string, error) {
	canonical := struct {
		Token              string `json:"token"`
		AllocationID       string `json:"allocation_id"`
		ComputeID          string `json:"compute_id"`
		Owner              string `json:"owner"`
		Project            string `json:"project"`
		InstanceName       string `json:"instance_name"`
		IDMapBase          int64  `json:"idmap_base"`
		IDMapSize          int64  `json:"idmap_size"`
		StorageDriver      string `json:"storage_driver"`
		StoragePool        string `json:"storage_pool"`
		StorageVolume      string `json:"storage_volume"`
		RBDImage           string `json:"rbd_image"`
		StorageIdentity    string `json:"storage_identity"`
		BaselineClean      bool   `json:"baseline_clean"`
		CleanupDisposition string `json:"cleanup_disposition"`
		Outcome            string `json:"outcome"`
	}{
		Token: attempt.Token, AllocationID: attempt.AllocationID, ComputeID: attempt.ComputeID,
		Owner: attempt.Owner, Project: attempt.Project, InstanceName: attempt.InstanceName,
		IDMapBase: attempt.IDMapBase, IDMapSize: attempt.IDMapSize, StorageDriver: attempt.StorageDriver,
		StoragePool: attempt.StoragePool, StorageVolume: attempt.StorageVolume, RBDImage: attempt.RBDImage,
		StorageIdentity: attempt.StorageIdentity, BaselineClean: attempt.BaselineClean, CleanupDisposition: attempt.CleanupDisposition, Outcome: outcome,
	}

	data, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}

	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// Delete removes a finished attempt's record.
func (m *Manager) Delete(ctx context.Context, token string) error {
	return m.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		changed, err := tx.RetireStorageMaterializationAttempt(ctx, token)
		if err != nil {
			return err
		}

		if !changed {
			return currentStateError(ctx, tx, token)
		}

		return nil
	})
}

// SameBinding reports whether two attempts describe the same rootfs on the same instance, which
// is what makes a repeated request a retry rather than a conflict.
func SameBinding(a *db.StorageMaterializationAttempt, b *db.StorageMaterializationAttempt) bool {
	return a != nil && b != nil && a.Token == b.Token && a.AllocationID == b.AllocationID &&
		a.ComputeID == b.ComputeID && a.Owner == b.Owner && a.Project == b.Project &&
		a.InstanceName == b.InstanceName && a.IDMapBase == b.IDMapBase && a.IDMapSize == b.IDMapSize &&
		a.StorageDriver == b.StorageDriver && a.StoragePool == b.StoragePool &&
		a.StorageVolume == b.StorageVolume && a.RBDImage == b.RBDImage &&
		a.StorageIdentity == b.StorageIdentity && a.BaselineClean == b.BaselineClean && a.CleanupDisposition == b.CleanupDisposition
}

func validateBinding(a *db.StorageMaterializationAttempt) error {
	if a == nil || a.Token == "" || a.AllocationID == "" || a.ComputeID == "" || a.Owner == "" || a.Project == "" || a.InstanceName == "" || a.StorageDriver == "" || a.StoragePool == "" || a.StorageVolume == "" {
		return errors.New("Storage materialization binding is incomplete")
	}

	for _, value := range []string{a.Token, a.AllocationID, a.ComputeID, a.Owner} {
		parsed, err := uuid.Parse(value)
		if err != nil || parsed.String() != value {
			return errors.New("Storage materialization UUIDs must use canonical lowercase form")
		}
	}
	if a.IDMapBase < 0 || a.IDMapSize <= 0 || a.IDMapSize > 1<<32 || a.IDMapBase > (1<<32)-a.IDMapSize {
		return errors.New("Storage materialization idmap is invalid")
	}

	if a.CleanupDisposition != CleanupDelete && a.CleanupDisposition != CleanupDetach && a.CleanupDisposition != CleanupHandover {
		return errors.New("Storage materialization cleanup disposition is invalid")
	}

	if !a.BaselineClean {
		return errors.New("Storage materialization clean baseline is not proven")
	}

	if a.StorageDriver == "cephext" && a.CleanupDisposition != CleanupDetach {
		return errors.New("External Ceph storage materialization must use detach cleanup")
	}

	if a.CleanupDisposition == CleanupDetach && a.StorageDriver != "ceph" && a.StorageDriver != "cephext" {
		return errors.New("Detached storage materialization requires a Ceph storage driver")
	}

	if a.CleanupDisposition == CleanupHandover && a.StorageDriver != "ceph" {
		return errors.New("Storage handover materialization requires the Incus-owned Ceph driver")
	}

	if (a.CleanupDisposition == CleanupDetach || a.CleanupDisposition == CleanupHandover) && a.StorageIdentity == "" {
		return errors.New("Retained storage materialization requires a baseline immutable identity")
	}

	return nil
}

func currentStateError(ctx context.Context, tx *db.NodeTx, token string) error {
	a, err := tx.GetStorageMaterializationAttempt(ctx, token)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}

	if err != nil {
		return err
	}

	return stateError(a)
}

func stateError(a *db.StorageMaterializationAttempt) error {
	switch a.State {
	case StateAborted:
		return ErrAborted
	case StateCommitted:
		return ErrCommitted
	case StateClean, StateRetired:
		return ErrFinished
	case StateActive:
		if a.Started {
			return ErrAlreadyStarted
		}
	}
	return fmt.Errorf("%w: state %q", ErrFinished, a.State)
}

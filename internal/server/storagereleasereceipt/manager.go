//go:build linux && cgo && !agent

package storagereleasereceipt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/lxc/incus/v7/internal/server/db"
	"github.com/lxc/incus/v7/internal/server/storagematerializationattempt"
)

const (
	// StatePending records intent before any root storage side effect.
	StatePending = "pending"

	// StateComplete proves that the recorded root storage outcome completed.
	StateComplete = "complete"

	// StateRetired permanently prevents reuse of an acknowledged materialization UUID.
	StateRetired = "retired"

	// OutcomeNormalized proves that an externally retained root is namespace-owned and locally released.
	OutcomeNormalized = "normalized"

	// OutcomeDeleted proves that an Incus-owned root was deleted.
	OutcomeDeleted = "deleted"

	// OutcomeDetached proves that a protected local claim was removed without deleting shared storage.
	OutcomeDetached = "detached"
)

var (
	// ErrNotFound indicates that the release token has no durable receipt.
	ErrNotFound = errors.New("Storage release receipt not found")

	// ErrBindingMismatch indicates that a token was reused for another storage generation.
	ErrBindingMismatch = errors.New("Storage release receipt token is bound to another root storage generation")
)

// Manager coordinates durable node-local root storage release receipts.
type Manager struct {
	node *db.Node
}

// New returns a storage release receipt manager backed by the node-local database.
func New(node *db.Node) *Manager {
	return &Manager{node: node}
}

// Get returns the receipt for a release token.
func (m *Manager) Get(ctx context.Context, token string) (*db.StorageReleaseReceipt, error) {
	var receipt *db.StorageReleaseReceipt
	err := m.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		var err error
		receipt, err = tx.GetStorageReleaseReceipt(ctx, token)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	return receipt, nil
}

// Begin creates an immutable pending receipt or returns the matching existing receipt.
func (m *Manager) Begin(ctx context.Context, expected db.StorageReleaseReceipt) (*db.StorageReleaseReceipt, error) {
	err := validateExpected(expected)
	if err != nil {
		return nil, err
	}

	var receipt *db.StorageReleaseReceipt
	err = m.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		current, err := tx.GetStorageReleaseReceipt(ctx, expected.Token)
		if err == nil {
			if current.State == StateRetired {
				return ErrBindingMismatch
			}

			if !sameBinding(current, &expected) {
				return ErrBindingMismatch
			}

			if current.State != StatePending && current.State != StateComplete {
				return fmt.Errorf("Invalid storage release receipt state %q", current.State)
			}

			receipt = current
			return nil
		}

		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		expected.State = StatePending
		expected.CreatedAt = time.Now().Unix()
		expected.CompletedAt = 0
		err = tx.CreateStorageReleaseReceipt(ctx, expected)
		if err != nil {
			return err
		}

		receipt, err = tx.GetStorageReleaseReceipt(ctx, expected.Token)
		return err
	})
	if err != nil {
		return nil, err
	}

	return receipt, nil
}

// Complete commits a matching receipt after its storage outcome has completed.
func (m *Manager) Complete(ctx context.Context, expected db.StorageReleaseReceipt) (*db.StorageReleaseReceipt, error) {
	err := validateExpected(expected)
	if err != nil {
		return nil, err
	}

	var receipt *db.StorageReleaseReceipt
	err = m.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		current, err := tx.GetStorageReleaseReceipt(ctx, expected.Token)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}

		if err != nil {
			return err
		}

		if !sameBinding(current, &expected) {
			return ErrBindingMismatch
		}

		if current.State == StateComplete {
			receipt = current
			return nil
		}

		if current.State != StatePending {
			return ErrBindingMismatch
		}

		changed, err := tx.CompleteStorageReleaseReceipt(ctx, expected.Token, time.Now().Unix())
		if err != nil {
			return err
		}

		if !changed {
			return ErrBindingMismatch
		}

		receipt, err = tx.GetStorageReleaseReceipt(ctx, expected.Token)
		return err
	})
	if err != nil {
		return nil, err
	}

	return receipt, nil
}

// Delete atomically retires a matching complete receipt and the committed
// materialization generation it releases after the caller has persisted proof.
func (m *Manager) Delete(ctx context.Context, expected db.StorageReleaseReceipt) error {
	err := validateExpected(expected)
	if err != nil {
		return err
	}

	return m.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		current, err := tx.GetStorageReleaseReceipt(ctx, expected.Token)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}

		if err != nil {
			return err
		}

		if !sameBinding(current, &expected) {
			return ErrBindingMismatch
		}

		attempt, err := tx.GetStorageMaterializationAttempt(ctx, expected.MaterializationID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrBindingMismatch
		}
		if err != nil {
			return err
		}

		if !sameMaterializationBinding(current, attempt) {
			return ErrBindingMismatch
		}

		if current.State == StateRetired && attempt.State == storagematerializationattempt.StateRetired {
			return nil
		}

		if current.State != StateComplete || attempt.State != storagematerializationattempt.StateCommitted || !attempt.Started || !attempt.Finished || attempt.StoragePhase != storagematerializationattempt.PhaseMaterialized {
			return ErrBindingMismatch
		}

		changed, err := tx.RetireStorageReleaseReceipt(ctx, expected.Token)
		if err != nil {
			return err
		}

		if !changed {
			return ErrBindingMismatch
		}

		changed, err = tx.RetireStorageMaterializationAttempt(ctx, attempt.Token)
		if err != nil {
			return err
		}

		if !changed {
			return ErrBindingMismatch
		}

		return nil
	})
}

func validateExpected(receipt db.StorageReleaseReceipt) error {
	if receipt.Token == "" || receipt.AllocationID == "" || receipt.ComputeID == "" || receipt.MaterializationID == "" || receipt.Owner == "" || receipt.Project == "" || receipt.InstanceName == "" {
		return errors.New("Storage release receipt binding is incomplete")
	}

	if receipt.Token != receipt.MaterializationID {
		return errors.New("Storage release receipt token does not match its materialization ID")
	}

	if receipt.IDMapBase < 0 || receipt.IDMapSize <= 0 {
		return errors.New("Storage release receipt ID map is invalid")
	}

	const idmapIDSpaceSize = int64(1 << 32)
	if receipt.IDMapSize > idmapIDSpaceSize || receipt.IDMapBase > idmapIDSpaceSize-receipt.IDMapSize {
		return errors.New("Storage release receipt ID map exceeds the 32-bit UID/GID space")
	}

	if receipt.StorageDriver == "" || receipt.StoragePool == "" || receipt.StorageVolume == "" {
		return errors.New("Storage release receipt identity is incomplete")
	}

	if !receipt.BaselineClean {
		return errors.New("Storage release receipt clean baseline is not proven")
	}

	if receipt.CleanupDisposition != storagematerializationattempt.CleanupDelete && receipt.CleanupDisposition != storagematerializationattempt.CleanupDetach && receipt.CleanupDisposition != storagematerializationattempt.CleanupHandover {
		return errors.New("Storage release receipt cleanup disposition is invalid")
	}

	if receipt.Outcome != OutcomeNormalized && receipt.Outcome != OutcomeDeleted && receipt.Outcome != OutcomeDetached {
		return fmt.Errorf("Invalid storage release receipt outcome %q", receipt.Outcome)
	}

	if (receipt.StorageDriver == "ceph" || receipt.StorageDriver == "cephext") && receipt.StorageIdentity == "" {
		return errors.New("Ceph storage release receipt requires immutable storage identity")
	}

	if receipt.StorageDriver == "cephext" && receipt.CleanupDisposition != storagematerializationattempt.CleanupDetach {
		return errors.New("External Ceph storage release receipt must use detach cleanup")
	}

	if receipt.CleanupDisposition == storagematerializationattempt.CleanupDetach && receipt.StorageDriver != "ceph" && receipt.StorageDriver != "cephext" {
		return errors.New("Detached storage release receipt requires a Ceph storage driver")
	}
	if receipt.CleanupDisposition == storagematerializationattempt.CleanupHandover && receipt.StorageDriver != "ceph" {
		return errors.New("Storage handover release receipt requires the Incus-owned Ceph driver")
	}

	// A source migration keeps its immutable spawn-time CleanupDelete disposition but releases with OutcomeDetached.
	// Only a target handover attempt changes ownership policy, and its final release uses OutcomeDeleted.
	validOutcome := false
	switch receipt.StorageDriver {
	case "cephext":
		validOutcome = receipt.CleanupDisposition == storagematerializationattempt.CleanupDetach &&
			(receipt.Outcome == OutcomeNormalized || receipt.Outcome == OutcomeDetached)
	case "ceph":
		switch receipt.CleanupDisposition {
		case storagematerializationattempt.CleanupDelete, storagematerializationattempt.CleanupHandover:
			validOutcome = receipt.Outcome == OutcomeDeleted || receipt.Outcome == OutcomeDetached
		case storagematerializationattempt.CleanupDetach:
			validOutcome = receipt.Outcome == OutcomeDetached
		}
	default:
		validOutcome = receipt.CleanupDisposition == storagematerializationattempt.CleanupDelete && receipt.Outcome == OutcomeDeleted
	}
	if !validOutcome {
		return fmt.Errorf("Storage release receipt outcome %q is invalid for driver %q with cleanup disposition %q", receipt.Outcome, receipt.StorageDriver, receipt.CleanupDisposition)
	}

	return nil
}

func sameBinding(a *db.StorageReleaseReceipt, b *db.StorageReleaseReceipt) bool {
	return a != nil && b != nil &&
		a.Token == b.Token &&
		a.AllocationID == b.AllocationID &&
		a.ComputeID == b.ComputeID &&
		a.MaterializationID == b.MaterializationID &&
		a.Owner == b.Owner &&
		a.Project == b.Project &&
		a.InstanceName == b.InstanceName &&
		a.IDMapBase == b.IDMapBase &&
		a.IDMapSize == b.IDMapSize &&
		a.StorageDriver == b.StorageDriver &&
		a.StoragePool == b.StoragePool &&
		a.StorageVolume == b.StorageVolume &&
		a.RBDImage == b.RBDImage &&
		a.StorageIdentity == b.StorageIdentity &&
		a.BaselineClean == b.BaselineClean &&
		a.CleanupDisposition == b.CleanupDisposition &&
		a.Outcome == b.Outcome
}

func sameMaterializationBinding(receipt *db.StorageReleaseReceipt, attempt *db.StorageMaterializationAttempt) bool {
	return receipt != nil && attempt != nil &&
		receipt.Token == attempt.Token &&
		receipt.MaterializationID == attempt.Token &&
		receipt.AllocationID == attempt.AllocationID &&
		receipt.ComputeID == attempt.ComputeID &&
		receipt.Owner == attempt.Owner &&
		receipt.Project == attempt.Project &&
		receipt.InstanceName == attempt.InstanceName &&
		receipt.IDMapBase == attempt.IDMapBase &&
		receipt.IDMapSize == attempt.IDMapSize &&
		receipt.StorageDriver == attempt.StorageDriver &&
		receipt.StoragePool == attempt.StoragePool &&
		receipt.StorageVolume == attempt.StorageVolume &&
		receipt.RBDImage == attempt.RBDImage &&
		receipt.StorageIdentity == attempt.StorageIdentity &&
		receipt.BaselineClean == attempt.BaselineClean &&
		receipt.CleanupDisposition == attempt.CleanupDisposition
}

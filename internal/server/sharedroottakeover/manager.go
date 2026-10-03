//go:build linux && cgo && !agent

// Package sharedroottakeover journals a fenced original-root import without deleting its storage.
package sharedroottakeover

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/lxc/incus/v7/internal/server/db"
	"github.com/lxc/incus/v7/internal/server/storagematerializationattempt"
	"github.com/lxc/incus/v7/internal/server/storagereleasereceipt"
)

// Durable takeover phases advance only after the preceding intent and side effect.
const (
	Prepared  = "prepared"
	Claiming  = "claiming"
	Claimed   = "claimed"
	Importing = "importing"
	Imported  = "imported"
	Committed = "committed"
	Starting  = "starting"
	Started   = "started"
	Completed = "completed"
	Retired   = "retired"
)

// Errors identify missing records, foreign bindings and invalid phase transitions.
var (
	ErrNotFound        = errors.New("Shared-root takeover not found")
	ErrBindingMismatch = errors.New("Shared-root takeover token belongs to another binding")
	ErrPhaseConflict   = errors.New("Shared-root takeover phase conflict")
)

// Binding records immutable source identity and the externally authorized target.
type Binding struct {
	Token                   string          `json:"token"`
	Owner                   string          `json:"owner"`
	AllocationID            string          `json:"allocation_id"`
	SourceComputeID         string          `json:"source_compute_id"`
	TargetComputeID         string          `json:"target_compute_id"`
	SourceMaterializationID string          `json:"source_materialization_id"`
	Project                 string          `json:"project"`
	InstanceName            string          `json:"instance_name"`
	StoragePool             string          `json:"storage_pool"`
	StorageVolume           string          `json:"storage_volume"`
	ClusterFSID             string          `json:"cluster_fsid"`
	StorageIdentity         string          `json:"storage_identity"`
	SourceOwnership         string          `json:"source_ownership"`
	SourceGeneration        uint64          `json:"source_generation"`
	IDMapBase               int64           `json:"idmap_base"`
	IDMapSize               int64           `json:"idmap_size"`
	FenceDigest             string          `json:"fence_digest"`
	GrantDigest             string          `json:"grant_digest"`
	ConfigurationDigest     string          `json:"configuration_digest"`
	Configuration           json.RawMessage `json:"configuration"`
}

// Manager persists local intent; it does not manufacture fencing or an external grant.
type Manager struct{ node *db.Node }

// New returns a node-local takeover journal.
func New(node *db.Node) *Manager { return &Manager{node: node} }

func canonicalBinding(binding Binding) (string, error) {
	for _, value := range []string{
		binding.Token, binding.Owner, binding.AllocationID,
		binding.SourceComputeID, binding.TargetComputeID, binding.SourceMaterializationID, binding.ClusterFSID,
	} {
		parsed, err := uuid.Parse(value)
		if err != nil || parsed == uuid.Nil || parsed.String() != value {
			return "", errors.New("Takeover requires canonical nonzero UUID bindings")
		}
	}

	if binding.SourceComputeID == binding.TargetComputeID || binding.SourceGeneration == 0 || binding.SourceGeneration == ^uint64(0) ||
		binding.IDMapBase < 0 || binding.IDMapSize <= 0 || binding.IDMapBase > (1<<32)-binding.IDMapSize {
		return "", errors.New("Invalid takeover source, generation or ID mapping")
	}

	for _, value := range []string{binding.Project, binding.InstanceName, binding.StoragePool, binding.StorageVolume} {
		if value == "" || strings.ContainsAny(value, "\x00\r\n") {
			return "", errors.New("Incomplete takeover resource binding")
		}
	}

	identity := struct {
		PoolID          uint64 `json:"pool_id"`
		ID              string `json:"id"`
		BlockNamePrefix string `json:"block_name_prefix"`
	}{}
	err := json.Unmarshal([]byte(binding.StorageIdentity), &identity)
	if err != nil || identity.ID == "" || strings.Trim(identity.ID, "0123456789abcdef") != "" ||
		identity.BlockNamePrefix != "rbd_data."+identity.ID || identity.PoolID > 1<<63-1 {
		return "", errors.New("Invalid immutable RBD identity")
	}

	encodedIdentity, err := json.Marshal(identity)
	if err != nil || string(encodedIdentity) != binding.StorageIdentity {
		return "", errors.New("RBD identity is not canonical")
	}

	for _, value := range []string{binding.SourceOwnership, binding.FenceDigest, binding.GrantDigest, binding.ConfigurationDigest} {
		if len(value) != 71 || !strings.HasPrefix(value, "sha256:") || strings.Trim(value[7:], "0123456789abcdef") != "" {
			return "", errors.New("Missing canonical takeover evidence digest")
		}
	}

	configuration := map[string]any{}
	if !json.Valid(binding.Configuration) {
		return "", errors.New("Invalid takeover configuration JSON")
	}

	decoder := json.NewDecoder(bytes.NewReader(binding.Configuration))
	decoder.UseNumber()
	err = decoder.Decode(&configuration)
	if err != nil || configuration == nil {
		return "", errors.New("Takeover configuration must be a JSON object")
	}

	canonicalConfiguration, err := json.Marshal(configuration)
	if err != nil {
		return "", err
	}

	digest := sha256.Sum256(canonicalConfiguration)
	if binding.ConfigurationDigest != "sha256:"+hex.EncodeToString(digest[:]) {
		return "", errors.New("Takeover configuration digest does not match its payload")
	}

	binding.Configuration = canonicalConfiguration

	encoded, err := json.Marshal(binding)
	return string(encoded), err
}

// OwnershipMarker binds the new marker to this complete transaction and configuration.
func OwnershipMarker(binding Binding) (string, error) {
	encoded, err := canonicalBinding(binding)
	if err != nil {
		return "", err
	}

	digest := sha256.Sum256([]byte("incus-fenced-shared-root-takeover-v1\n" + encoded))
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// Get returns durable status, including pending side effects after a daemon crash.
func (m *Manager) Get(ctx context.Context, token string) (*db.SharedRootTakeover, error) {
	var record *db.SharedRootTakeover
	err := m.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		var err error
		record, err = tx.GetSharedRootTakeover(ctx, token)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}

		return err
	})
	return record, err
}

// Register prepares a transaction; every replay must retain the complete original binding.
func (m *Manager) Register(ctx context.Context, binding Binding) (*db.SharedRootTakeover, error) {
	encoded, err := canonicalBinding(binding)
	if err != nil {
		return nil, err
	}

	var record *db.SharedRootTakeover
	err = m.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		current, err := tx.GetSharedRootTakeover(ctx, binding.Token)
		if err == nil {
			if current.Binding != encoded || current.Phase == "retired" {
				return ErrBindingMismatch
			}

			record = current
			return nil
		}

		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		record = &db.SharedRootTakeover{
			Token: binding.Token, Project: binding.Project,
			InstanceName: binding.InstanceName, ClusterFSID: binding.ClusterFSID,
			StorageIdentity: binding.StorageIdentity, Binding: encoded, Phase: Prepared,
		}
		return tx.CreateSharedRootTakeover(ctx, *record)
	})
	return record, err
}

// Advance writes intent before effects and completion only after the effects are verified.
func (m *Manager) Advance(ctx context.Context, binding Binding, previous string, next string) (*db.SharedRootTakeover, error) {
	sequence := []string{Prepared, Claiming, Claimed, Importing, Imported, Committed, Starting, Started, Completed}
	allowed := false
	for i := 0; i < len(sequence)-1; i++ {
		if sequence[i] == previous && sequence[i+1] == next {
			allowed = true
		}
	}

	if !allowed || next == Committed {
		return nil, ErrPhaseConflict
	}

	encoded, err := canonicalBinding(binding)
	if err != nil {
		return nil, err
	}

	var record *db.SharedRootTakeover
	err = m.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		current, err := tx.GetSharedRootTakeover(ctx, binding.Token)
		if err != nil {
			return err
		}

		if current.Binding != encoded {
			return ErrBindingMismatch
		}

		if current.Phase == next {
			record = current
			return nil
		}

		changed, err := tx.AdvanceSharedRootTakeover(ctx, binding.Token, previous, next)
		if err != nil {
			return err
		}

		if !changed {
			return ErrPhaseConflict
		}

		current.Phase = next
		current.LastError = ""
		record = current
		return nil
	})
	return record, err
}

// RecordFailure leaves the original image and phase available for exact transaction recovery.
func (m *Manager) RecordFailure(ctx context.Context, token string, failure error) error {
	if failure == nil {
		return errors.New("Takeover failure must describe the error")
	}

	return m.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		_, err := tx.GetSharedRootTakeover(ctx, token)
		if err != nil {
			return fmt.Errorf("Record takeover failure: %w", err)
		}

		return tx.FailSharedRootTakeover(ctx, token, failure.Error())
	})
}

// Commit persists takeover ownership and the normal release binding in one node transaction.
func (m *Manager) Commit(ctx context.Context, binding Binding) (*db.SharedRootTakeover, error) {
	encoded, err := canonicalBinding(binding)
	if err != nil {
		return nil, err
	}

	var record *db.SharedRootTakeover
	err = m.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		current, err := tx.GetSharedRootTakeover(ctx, binding.Token)
		if err != nil {
			return err
		}

		if current.Binding != encoded {
			return ErrBindingMismatch
		}

		if current.Phase == Committed || current.Phase == Starting || current.Phase == Started || current.Phase == Completed {
			record = current
			return nil
		}

		if current.Phase != Imported {
			return ErrPhaseConflict
		}

		attempt := db.StorageMaterializationAttempt{
			Token:        binding.Token,
			AllocationID: binding.AllocationID, ComputeID: binding.TargetComputeID, Owner: binding.Owner,
			Project: binding.Project, InstanceName: binding.InstanceName, IDMapBase: binding.IDMapBase,
			IDMapSize: binding.IDMapSize, StorageDriver: "ceph", StoragePool: binding.StoragePool,
			StorageVolume: binding.StorageVolume, StorageIdentity: binding.StorageIdentity,
			BaselineClean: true, CleanupDisposition: storagematerializationattempt.CleanupHandover,
			State:        storagematerializationattempt.StateCommitted,
			StoragePhase: storagematerializationattempt.PhaseMaterialized, Started: true, Finished: true,
		}
		err = tx.CreateStorageMaterializationAttempt(ctx, attempt)
		if err != nil {
			return err
		}

		changed, err := tx.AdvanceSharedRootTakeover(ctx, binding.Token, Imported, Committed)
		if err != nil {
			return err
		}

		if !changed {
			return ErrPhaseConflict
		}

		current.Phase = Committed
		current.LastError = ""
		record = current
		return nil
	})
	return record, err
}

// Retire releases journal uniqueness only after exact completed storage-release evidence.
func (m *Manager) Retire(ctx context.Context, token string) error {
	return m.node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		record, err := tx.GetSharedRootTakeover(ctx, token)
		if err != nil {
			return err
		}

		if record.Phase == Retired {
			return nil
		}

		if record.Phase != Completed {
			return ErrPhaseConflict
		}

		binding := Binding{}
		err = json.Unmarshal([]byte(record.Binding), &binding)
		if err != nil {
			return err
		}

		receipt, err := tx.GetStorageReleaseReceipt(ctx, token)
		if err != nil {
			return err
		}

		if receipt.State != storagereleasereceipt.StateComplete ||
			(receipt.Outcome != storagereleasereceipt.OutcomeDeleted && receipt.Outcome != storagereleasereceipt.OutcomeDetached) ||
			receipt.StorageDriver != "ceph" || !receipt.BaselineClean ||
			receipt.CleanupDisposition != storagematerializationattempt.CleanupHandover ||
			receipt.Token != binding.Token || receipt.MaterializationID != binding.Token ||
			receipt.AllocationID != binding.AllocationID || receipt.ComputeID != binding.TargetComputeID ||
			receipt.Owner != binding.Owner || receipt.Project != binding.Project || receipt.InstanceName != binding.InstanceName ||
			receipt.StorageIdentity != binding.StorageIdentity || receipt.StoragePool != binding.StoragePool ||
			receipt.StorageVolume != binding.StorageVolume || receipt.IDMapBase != binding.IDMapBase || receipt.IDMapSize != binding.IDMapSize {
			return errors.New("Takeover retirement lacks exact complete storage release evidence")
		}

		changed, err := tx.AdvanceSharedRootTakeover(ctx, token, Completed, Retired)
		if err != nil {
			return err
		}

		if !changed {
			return ErrPhaseConflict
		}

		return nil
	})
}

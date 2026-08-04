package main

import (
	"context"
	"testing"

	"github.com/lxc/incus/v7/internal/server/auth"
	"github.com/lxc/incus/v7/internal/server/db"
	"github.com/lxc/incus/v7/internal/server/instance"
	"github.com/lxc/incus/v7/internal/server/instance/instancetype"
	"github.com/lxc/incus/v7/internal/server/migrationattempt"
	"github.com/lxc/incus/v7/shared/api"
)

func TestMigrationAttemptEndpointRequiresResourceEdit(t *testing.T) {
	if migrationAttemptResourceEntitlement != auth.EntitlementCanEdit {
		t.Fatalf("Migration attempt entitlement = %q, want %q", migrationAttemptResourceEntitlement, auth.EntitlementCanEdit)
	}

	actions := []APIEndpointAction{
		migrationAttemptCmd.Get,
		migrationAttemptCmd.Put,
		migrationAttemptCmd.Delete,
		migrationAttemptsCmd.Get,
	}

	for _, action := range actions {
		if action.Handler == nil || action.AccessHandler == nil {
			t.Fatal("Migration attempt endpoint action is missing its handler or authorization gate")
		}
	}
}

func TestMigrationAttemptIDMapActive(t *testing.T) {
	const currentGeneration = int64(100)

	tests := []struct {
		name     string
		attempt  *db.MigrationAttempt
		expected bool
	}{
		{
			name:     "registered but not started",
			attempt:  &db.MigrationAttempt{IDMapBase: 1000000, IDMapSize: 65536},
			expected: true,
		},
		{
			name:     "started by this daemon",
			attempt:  &db.MigrationAttempt{IDMapBase: 1000000, IDMapSize: 65536, Started: true, DaemonStart: currentGeneration},
			expected: true,
		},
		{
			name:    "stranded by an earlier daemon",
			attempt: &db.MigrationAttempt{IDMapBase: 1000000, IDMapSize: 65536, Started: true, DaemonStart: currentGeneration - 1},
		},
		{
			name:    "finished",
			attempt: &db.MigrationAttempt{IDMapBase: 1000000, IDMapSize: 65536, Started: true, DaemonStart: currentGeneration, Finished: true},
		},
		{
			name:    "no reservation",
			attempt: &db.MigrationAttempt{IDMapBase: -1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := migrationAttemptIDMapActive(tt.attempt, currentGeneration)
			if actual != tt.expected {
				t.Fatalf("migrationAttemptIDMapActive() = %t, want %t", actual, tt.expected)
			}

			reported := migrationAttemptToAPI(tt.attempt, currentGeneration)
			if reported.IDMapActive != tt.expected {
				t.Fatalf("MigrationAttempt.IDMapActive = %t, want %t", reported.IDMapActive, tt.expected)
			}
		})
	}
}

func TestMigrationAttemptVisibleInProject(t *testing.T) {
	tests := []struct {
		name        string
		attempt     *db.MigrationAttempt
		projectName string
		expected    bool
	}{
		{
			name:        "matching project",
			attempt:     &db.MigrationAttempt{Project: "tenant-a", State: migrationattempt.StateActive},
			projectName: "tenant-a",
			expected:    true,
		},
		{
			name:        "different project",
			attempt:     &db.MigrationAttempt{Project: "tenant-a", State: migrationattempt.StateActive},
			projectName: "tenant-b",
		},
		{
			name:        "retired token",
			attempt:     &db.MigrationAttempt{State: migrationattempt.StateRetired},
			projectName: "tenant-a",
		},
		{
			name:        "missing attempt",
			projectName: "tenant-a",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := migrationAttemptVisibleInProject(tt.attempt, tt.projectName)
			if actual != tt.expected {
				t.Fatalf("migrationAttemptVisibleInProject() = %t, want %t", actual, tt.expected)
			}
		})
	}
}

func (s *containerTestSuite) TestMigrationAttempt_reconcileAfterRestart() {
	ctx := context.Background()
	state := s.d.State()
	manager := migrationattempt.New(state.DB.Node)
	previousGeneration := state.StartTime.UnixNano() - 1
	base := s.d.os.IdmapSet.Entries[0].HostID + 5*65536

	const strandedToken = "26e5ba30-fd05-4f1f-9e1e-6b0f2c5a4d77"
	const survivingToken = "5c8f9a71-0d4e-4b2a-8c36-1f7d9e0a3b52"

	// An attempt whose target creation never produced an instance.
	_, err := manager.Register(ctx, strandedToken, api.ProjectDefaultName, migrationattempt.ResourceTypeInstance, "stranded-target", base, 65536, previousGeneration)
	s.Req.NoError(err)
	_, err = manager.Begin(ctx, strandedToken, api.ProjectDefaultName, migrationattempt.ResourceTypeInstance, "stranded-target", previousGeneration)
	s.Req.NoError(err)

	// An attempt whose half-received target instance survived the restart.
	_, err = manager.Register(ctx, survivingToken, api.ProjectDefaultName, migrationattempt.ResourceTypeInstance, "surviving-target", base+65536, 65536, previousGeneration)
	s.Req.NoError(err)
	_, err = manager.Begin(ctx, survivingToken, api.ProjectDefaultName, migrationattempt.ResourceTypeInstance, "surviving-target", previousGeneration)
	s.Req.NoError(err)

	survivor, op, _, err := instance.CreateInternal(state, db.InstanceArgs{
		Type: instancetype.Container,
		Name: "surviving-target",
	}, nil, true, true, false)
	s.Req.NoError(err)
	op.Done(nil)
	defer func() { _ = survivor.Delete(true, true) }()

	reconcileMigrationAttemptsAfterRestart(ctx, state)

	// The stranded attempt is settled, so its range is claimable again.
	settled, err := manager.Get(ctx, strandedToken)
	s.Req.NoError(err)
	s.Req.Equal(migrationattempt.StateAborted, settled.State)
	s.Req.True(settled.Finished)

	// The attempt whose target survived stays unfinished for reconciliation.
	retained, err := manager.Get(ctx, survivingToken)
	s.Req.NoError(err)
	s.Req.False(retained.Finished)

	// Neither reservation fences a new attempt in this generation: the
	// settled one is finished, and the retained one is now represented by
	// the instance that every allocator checks anyway.
	reservations, err := manager.IDMapReservations(ctx, state.StartTime.UnixNano())
	s.Req.NoError(err)
	s.Req.Empty(reservations)
}

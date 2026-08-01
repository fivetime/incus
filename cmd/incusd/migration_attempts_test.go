package main

import (
	"testing"

	"github.com/lxc/incus/v7/internal/server/auth"
	"github.com/lxc/incus/v7/internal/server/db"
	"github.com/lxc/incus/v7/internal/server/migrationattempt"
)

func TestMigrationAttemptEndpointRequiresResourceEdit(t *testing.T) {
	if migrationAttemptResourceEntitlement != auth.EntitlementCanEdit {
		t.Fatalf("Migration attempt entitlement = %q, want %q", migrationAttemptResourceEntitlement, auth.EntitlementCanEdit)
	}

	actions := []APIEndpointAction{
		migrationAttemptCmd.Get,
		migrationAttemptCmd.Put,
		migrationAttemptCmd.Delete,
	}

	for _, action := range actions {
		if action.Handler == nil || action.AccessHandler == nil {
			t.Fatal("Migration attempt endpoint action is missing its handler or authorization gate")
		}
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

package main

import (
	"testing"

	"github.com/lxc/incus/v7/internal/server/auth"
	"github.com/lxc/incus/v7/internal/server/db"
	"github.com/lxc/incus/v7/internal/server/migrationattempt"
)

func TestInstanceStorageHandoverEndpointAuthorization(t *testing.T) {
	if instanceStorageHandoverAuthObjectType != auth.ObjectTypeInstance {
		t.Fatalf("Storage handover authorization object = %q, want instance", instanceStorageHandoverAuthObjectType)
	}

	if instanceStorageHandoverAuthEntitlement != auth.EntitlementCanEdit {
		t.Fatalf("Storage handover entitlement = %q, want can_edit", instanceStorageHandoverAuthEntitlement)
	}

	if instanceStorageHandoverCmd.Put.Handler == nil ||
		instanceStorageHandoverCmd.Put.AccessHandler == nil {
		t.Fatal("Storage handover endpoint is missing its handler or instance authorization gate")
	}

	if instanceStorageHandoverSourceOwnedAuthObject().Type() != auth.ObjectTypeServer {
		t.Fatal("Source-owned storage handover does not require a server authorization object")
	}

	if instanceStorageHandoverSourceOwnedAuthEntitlement != auth.EntitlementCanEdit {
		t.Fatalf("Source-owned storage handover entitlement = %q, want can_edit", instanceStorageHandoverSourceOwnedAuthEntitlement)
	}
}

func TestStorageHandoverOwnershipProofMatches(t *testing.T) {
	const (
		projectName   = "nova"
		instanceName  = "instance-00000001"
		operationUUID = "7a0c9c7e-e55d-4d36-8e3a-5165708bc640"
	)

	valid := func() *db.MigrationAttempt {
		return &db.MigrationAttempt{
			Project:       projectName,
			ResourceType:  migrationattempt.ResourceTypeInstance,
			ResourceName:  instanceName,
			State:         migrationattempt.StateCommitted,
			Started:       true,
			Finished:      true,
			OperationUUID: operationUUID,
		}
	}

	tests := []struct {
		name      string
		mutate    func(*db.MigrationAttempt)
		attempt   *db.MigrationAttempt
		operation string
		expected  bool
	}{
		{name: "committed target proof", expected: true},
		{name: "missing attempt", attempt: nil},
		{name: "wrong project", mutate: func(attempt *db.MigrationAttempt) { attempt.Project = "other" }},
		{name: "wrong resource type", mutate: func(attempt *db.MigrationAttempt) { attempt.ResourceType = "volume" }},
		{name: "wrong instance", mutate: func(attempt *db.MigrationAttempt) { attempt.ResourceName = "other" }},
		{name: "active attempt", mutate: func(attempt *db.MigrationAttempt) { attempt.State = migrationattempt.StateActive }},
		{name: "not started", mutate: func(attempt *db.MigrationAttempt) { attempt.Started = false }},
		{name: "not finished", mutate: func(attempt *db.MigrationAttempt) { attempt.Finished = false }},
		{name: "missing operation binding", mutate: func(attempt *db.MigrationAttempt) { attempt.OperationUUID = "" }},
		{name: "different operation", operation: "f8b091ad-b2ce-46a8-b3db-e37d204490dc"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attempt := tt.attempt
			if tt.name != "missing attempt" {
				attempt = valid()
			}

			if tt.mutate != nil {
				tt.mutate(attempt)
			}

			operation := tt.operation
			if operation == "" {
				operation = operationUUID
			}

			actual := storageHandoverOwnershipProofMatches(attempt, projectName, instanceName, operation)
			if actual != tt.expected {
				t.Fatalf("storageHandoverOwnershipProofMatches() = %t, want %t", actual, tt.expected)
			}
		})
	}
}

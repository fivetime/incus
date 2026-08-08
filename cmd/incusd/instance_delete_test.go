package main

import (
	"testing"

	internalInstance "github.com/lxc/incus/v7/internal/instance"
)

func TestValidateRootfsIDMapReleaseOwner(t *testing.T) {
	const (
		owner        = "11111111-1111-1111-1111-111111111111"
		allocationID = "22222222-2222-2222-2222-222222222222"
		computeID    = "33333333-3333-3333-3333-333333333333"
		token        = "44444444-4444-4444-4444-444444444444"
	)

	matching := map[string]string{
		"user.openstack.uuid":                                   owner,
		internalInstance.ConfigOpenStackIDMapAllocationID:       allocationID,
		internalInstance.ConfigOpenStackComputeID:               computeID,
		internalInstance.ConfigOpenStackRootfsMaterializationID: token,
	}

	tests := []struct {
		name        string
		localConfig map[string]string
		owner       string
		wantErr     bool
	}{
		{
			name:        "matching local owner",
			localConfig: matching,
			owner:       owner,
		},
		{
			name:        "different owner",
			localConfig: matching,
			owner:       "55555555-5555-5555-5555-555555555555",
			wantErr:     true,
		},
		{
			name:        "missing authoritative local owner",
			localConfig: map[string]string{},
			owner:       owner,
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRootfsIDMapReleaseBinding(tt.localConfig, token, tt.owner, allocationID, computeID)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateRootfsIDMapReleaseOwner() error = %v, wantErr %t", err, tt.wantErr)
			}
		})
	}
}

func TestRootfsIDMapReleaseBindingNeedsArm(t *testing.T) {
	const (
		token        = "06ada2e7-67f9-4c4e-b071-da45f25cfc67"
		owner        = "11111111-1111-1111-1111-111111111111"
		allocationID = "22222222-2222-2222-2222-222222222222"
		computeID    = "33333333-3333-3333-3333-333333333333"
	)

	arm, err := rootfsIDMapReleaseBindingNeedsArm(map[string]string{}, token, owner, allocationID, computeID)
	if err != nil || !arm {
		t.Fatalf("New release binding was not armed: arm=%t err=%v", arm, err)
	}

	protected := map[string]string{
		internalInstance.ConfigVolatileMigrationStorageDeleteProtection: "true",
	}

	arm, err = rootfsIDMapReleaseBindingNeedsArm(protected, token, owner, allocationID, computeID)
	if err != nil || !arm {
		t.Fatalf("Protected handover record could not arm detached release: arm=%t err=%v", arm, err)
	}

	config := map[string]string{
		internalInstance.ConfigVolatileRootfsIDMapReleaseToken: token,
		internalInstance.ConfigVolatileRootfsIDMapReleaseOwner: owner,
		internalInstance.ConfigVolatileRootfsIDMapAllocationID: allocationID,
		internalInstance.ConfigVolatileRootfsIDMapComputeID:    computeID,
	}

	arm, err = rootfsIDMapReleaseBindingNeedsArm(config, token, owner, allocationID, computeID)
	if err != nil || arm {
		t.Fatalf("Matching repeated release binding was not idempotent: arm=%t err=%v", arm, err)
	}

	arm, err = rootfsIDMapReleaseBindingNeedsArm(config, "55555555-5555-5555-5555-555555555555", owner, allocationID, computeID)
	if err == nil || arm {
		t.Fatal("Different release generation did not conflict")
	}

	delete(config, internalInstance.ConfigVolatileRootfsIDMapReleaseOwner)
	arm, err = rootfsIDMapReleaseBindingNeedsArm(config, token, owner, allocationID, computeID)
	if err == nil || arm {
		t.Fatal("Incomplete persisted release binding was accepted")
	}
}

func TestRootfsMaterializationBindingRequiresRelease(t *testing.T) {
	const (
		owner        = "11111111-1111-4111-8111-111111111111"
		allocationID = "22222222-2222-4222-8222-222222222222"
		computeID    = "33333333-3333-4333-8333-333333333333"
		token        = "44444444-4444-4444-8444-444444444444"
	)

	complete := map[string]string{
		"user.openstack.uuid":                                   owner,
		"security.idmap.base":                                   "1000000",
		"security.idmap.size":                                   "65536",
		internalInstance.ConfigOpenStackIDMapAllocationID:       allocationID,
		internalInstance.ConfigOpenStackComputeID:               computeID,
		internalInstance.ConfigOpenStackRootfsMaterializationID: token,
	}

	tests := []struct {
		name         string
		config       map[string]string
		wantRequired bool
		wantErr      bool
	}{
		{name: "unmanaged", config: map[string]string{}},
		{name: "legacy owner only", config: map[string]string{"user.openstack.uuid": owner}},
		{name: "generic fixed idmap", config: map[string]string{"security.idmap.base": "1000000", "security.idmap.size": "65536"}},
		{name: "allocation only", config: map[string]string{internalInstance.ConfigOpenStackIDMapAllocationID: allocationID}, wantRequired: true, wantErr: true},
		{name: "compute only", config: map[string]string{internalInstance.ConfigOpenStackComputeID: computeID}, wantRequired: true, wantErr: true},
		{name: "materialization only", config: map[string]string{internalInstance.ConfigOpenStackRootfsMaterializationID: token}, wantRequired: true, wantErr: true},
		{name: "owner and fixed idmap only", config: map[string]string{"user.openstack.uuid": owner, "security.idmap.base": "1000000", "security.idmap.size": "65536"}, wantRequired: true, wantErr: true},
		{name: "complete", config: complete, wantRequired: true},
		{name: "non-canonical materialization", config: map[string]string{
			"user.openstack.uuid":                                   owner,
			internalInstance.ConfigOpenStackIDMapAllocationID:       allocationID,
			internalInstance.ConfigOpenStackComputeID:               computeID,
			internalInstance.ConfigOpenStackRootfsMaterializationID: "44444444444444448444444444444444",
		}, wantRequired: true, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			required, err := rootfsMaterializationBindingRequiresRelease(tt.config)
			if required != tt.wantRequired || (err != nil) != tt.wantErr {
				t.Fatalf("rootfsMaterializationBindingRequiresRelease() required=%t err=%v, want required=%t error=%t", required, err, tt.wantRequired, tt.wantErr)
			}
		})
	}
}

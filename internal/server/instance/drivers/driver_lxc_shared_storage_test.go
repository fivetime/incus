package drivers

import (
	"bytes"
	"errors"
	"slices"
	"testing"

	internalInstance "github.com/lxc/incus/v7/internal/instance"
	serverInstance "github.com/lxc/incus/v7/internal/server/instance"
)

func TestSharedStorageHandoverDriverIdentityIsVersioned(t *testing.T) {
	tests := map[string]string{
		"ceph":    "ceph+ready-v1",
		"cephext": "cephext+ready-v1",
		"dir":     "",
		"":        "",
	}

	for driverName, expected := range tests {
		t.Run(driverName, func(t *testing.T) {
			actual := sharedStorageHandoverDriverIdentity(driverName)
			if actual != expected {
				t.Fatalf("sharedStorageHandoverDriverIdentity(%q) = %q, want %q", driverName, actual, expected)
			}
		})
	}
}

func TestSharedStorageHandoverReadinessFencesClaim(t *testing.T) {
	var conn bytes.Buffer
	err := signalSharedStorageHandoverReady(&conn)
	if err != nil {
		t.Fatalf("Failed writing readiness marker: %v", err)
	}

	order := []string{}
	err = claimSharedStorageMigrationTarget(true, &conn, func() error {
		order = append(order, "guard")
		return nil
	}, func() error {
		order = append(order, "claim")
		return nil
	})
	if err != nil {
		t.Fatalf("Failed claiming after readiness: %v", err)
	}

	if !slices.Equal(order, []string{"guard", "claim"}) {
		t.Fatalf("Claim order = %v, want guard before claim", order)
	}
}

func TestSharedStorageHandoverReleaseFailureSendsNoReadiness(t *testing.T) {
	var conn bytes.Buffer
	releaseErr := errors.New("unmount failed")

	err := releaseAndSignalSharedStorageHandover(func() error {
		return releaseErr
	}, &conn)
	if !errors.Is(err, releaseErr) {
		t.Fatalf("Release error = %v, want %v", err, releaseErr)
	}

	if conn.Len() != 0 {
		t.Fatal("Source signaled readiness after its root release failed")
	}
}

func TestSharedStorageHandoverMissingReadinessFailsClosed(t *testing.T) {
	claimCalled := false
	err := claimSharedStorageMigrationTarget(true, bytes.NewBufferString("invalid"), nil, func() error {
		claimCalled = true
		return nil
	})
	if err == nil {
		t.Fatal("Expected missing readiness marker to fail")
	}

	if claimCalled {
		t.Fatal("Shared storage was claimed without source readiness")
	}
}

func TestSharedStorageHandoverRechecksAttemptAfterReadiness(t *testing.T) {
	var conn bytes.Buffer
	err := signalSharedStorageHandoverReady(&conn)
	if err != nil {
		t.Fatalf("Failed writing readiness marker: %v", err)
	}

	fenceErr := errors.New("attempt aborted")
	claimCalled := false
	err = claimSharedStorageMigrationTarget(true, &conn, func() error {
		return fenceErr
	}, func() error {
		claimCalled = true
		return nil
	})
	if !errors.Is(err, fenceErr) {
		t.Fatalf("Claim error = %v, want attempt fence", err)
	}

	if claimCalled {
		t.Fatal("Shared storage was claimed after the attempt was fenced")
	}
}

func TestMigrationAttemptGuardFencesLateStorageReceive(t *testing.T) {
	fenceErr := errors.New("attempt aborted")
	actionCalled := false

	err := withMigrationAttemptGuard(func() error {
		return fenceErr
	}, "before storage receive", func() error {
		actionCalled = true
		return nil
	})

	if !errors.Is(err, fenceErr) {
		t.Fatalf("Expected migration fence error, got %v", err)
	}

	if actionCalled {
		t.Fatal("Storage receive ran after migration attempt was fenced")
	}
}

func TestMigrationAttemptGuardAllowsActiveStorageReceive(t *testing.T) {
	guardCalled := false
	actionCalled := false

	err := withMigrationAttemptGuard(func() error {
		guardCalled = true
		return nil
	}, "before storage receive", func() error {
		actionCalled = true
		return nil
	})
	if err != nil {
		t.Fatalf("Expected active migration attempt to proceed, got %v", err)
	}

	if !guardCalled || !actionCalled {
		t.Fatalf("Expected guard and action to run, got guard=%t action=%t", guardCalled, actionCalled)
	}
}

func TestIsCephSharedStorageDriver(t *testing.T) {
	tests := map[string]bool{
		"ceph":    true,
		"cephext": true,
		"dir":     false,
		"lvm":     false,
		"zfs":     false,
		"":        false,
	}

	for driverName, expected := range tests {
		t.Run(driverName, func(t *testing.T) {
			actual := isCephSharedStorageDriver(driverName)
			if actual != expected {
				t.Fatalf("isCephSharedStorageDriver(%q) = %t, want %t", driverName, actual, expected)
			}
		})
	}
}

func TestWithSharedStorageMigrationTargetProtection(t *testing.T) {
	tests := []struct {
		name             string
		sharedStorage    bool
		driverName       string
		expectRole       bool
		expectProtection bool
	}{
		{name: "shared ceph", sharedStorage: true, driverName: "ceph", expectRole: true, expectProtection: true},
		{name: "shared cephext", sharedStorage: true, driverName: "cephext", expectRole: true, expectProtection: true},
		{name: "copied ceph", sharedStorage: false, driverName: "ceph"},
		{name: "shared dir", sharedStorage: true, driverName: "dir"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := map[string]string{}
			claimCalled := false
			err := withSharedStorageMigrationTargetProtection(tt.sharedStorage, tt.driverName, func(changes map[string]string) error {
				for key, value := range changes {
					config[key] = value
				}

				return nil
			}, func() error {
				claimCalled = true
				if tt.expectProtection && !internalInstance.StorageDeleteProtected(config) {
					t.Fatal("Storage claim ran before delete protection was persisted")
				}

				if tt.expectRole && config[internalInstance.ConfigVolatileMigrationStorageHandoverRole] != internalInstance.StorageHandoverRoleTarget {
					t.Fatal("Storage claim ran before target role was persisted")
				}

				return nil
			})
			if err != nil {
				t.Fatalf("withSharedStorageMigrationTargetProtection returned error: %v", err)
			}

			if !claimCalled {
				t.Fatal("Storage claim was not called")
			}

			protected := internalInstance.StorageDeleteProtected(config)
			if protected != tt.expectProtection {
				t.Fatalf("Storage protection = %t, want %t", protected, tt.expectProtection)
			}

			hasTargetRole := config[internalInstance.ConfigVolatileMigrationStorageHandoverRole] == internalInstance.StorageHandoverRoleTarget
			if hasTargetRole != tt.expectRole {
				t.Fatalf("Target role = %t, want %t", hasTargetRole, tt.expectRole)
			}
		})
	}
}

func TestWithSharedStorageMigrationTargetProtectionPersistenceFailure(t *testing.T) {
	persistErr := errors.New("persistence failed")

	for _, driverName := range []string{"ceph", "cephext"} {
		t.Run(driverName, func(t *testing.T) {
			claimCalled := false

			err := withSharedStorageMigrationTargetProtection(true, driverName, func(changes map[string]string) error {
				if changes[internalInstance.ConfigVolatileMigrationStorageHandoverRole] != internalInstance.StorageHandoverRoleTarget {
					t.Fatalf("Target role missing from persistence changes: %#v", changes)
				}

				return persistErr
			}, func() error {
				claimCalled = true
				return nil
			})
			if !errors.Is(err, persistErr) {
				t.Fatalf("withSharedStorageMigrationTargetProtection returned %v, want %v", err, persistErr)
			}

			if claimCalled {
				t.Fatal("Storage claim ran after protection persistence failed")
			}
		})
	}
}

func TestWithSharedStorageMigrationTargetProtectionClaimFailure(t *testing.T) {
	claimErr := errors.New("claim failed")
	config := map[string]string{}

	err := withSharedStorageMigrationTargetProtection(true, "ceph", func(changes map[string]string) error {
		for key, value := range changes {
			config[key] = value
		}

		return nil
	}, func() error {
		return claimErr
	})
	if !errors.Is(err, claimErr) {
		t.Fatalf("withSharedStorageMigrationTargetProtection returned %v, want %v", err, claimErr)
	}

	if !internalInstance.StorageDeleteProtected(config) {
		t.Fatal("Storage protection was not retained after claim failure")
	}

	if config[internalInstance.ConfigVolatileMigrationStorageHandoverRole] != internalInstance.StorageHandoverRoleTarget {
		t.Fatal("Storage target role was not retained after claim failure")
	}

	_, err = internalInstance.StorageHandoverAPIConfigChanges(internalInstance.StorageHandoverStateOwned, config, "ceph")
	if !errors.Is(err, internalInstance.ErrStorageHandoverIncomplete) {
		t.Fatalf("Incomplete target owned transition returned %v, want %v", err, internalInstance.ErrStorageHandoverIncomplete)
	}
}

func TestMarkSharedStorageMigrationTargetReceiveComplete(t *testing.T) {
	tests := []struct {
		name          string
		sharedStorage bool
		driverName    string
		expectSet     bool
	}{
		{name: "shared ceph", sharedStorage: true, driverName: "ceph", expectSet: true},
		{name: "shared cephext", sharedStorage: true, driverName: "cephext", expectSet: true},
		{name: "copied ceph", sharedStorage: false, driverName: "ceph", expectSet: false},
		{name: "shared dir", sharedStorage: true, driverName: "dir", expectSet: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := map[string]string{}
			err := markSharedStorageMigrationTargetReceiveComplete(tt.sharedStorage, tt.driverName, func(changes map[string]string) error {
				for key, value := range changes {
					config[key] = value
				}

				return nil
			})
			if err != nil {
				t.Fatalf("markSharedStorageMigrationTargetReceiveComplete returned error: %v", err)
			}

			actual := config[internalInstance.ConfigVolatileMigrationStorageReceiveComplete] == "true"
			if actual != tt.expectSet {
				t.Fatalf("Receive completion proof = %t, want %t", actual, tt.expectSet)
			}
		})
	}
}

func TestMarkSharedStorageMigrationTargetReceiveCompletePersistenceFailure(t *testing.T) {
	persistErr := errors.New("persistence failed")

	for _, driverName := range []string{"ceph", "cephext"} {
		t.Run(driverName, func(t *testing.T) {
			err := markSharedStorageMigrationTargetReceiveComplete(true, driverName, func(changes map[string]string) error {
				if changes[internalInstance.ConfigVolatileMigrationStorageReceiveComplete] != "true" {
					t.Fatalf("Unexpected completion changes: %#v", changes)
				}

				return persistErr
			})
			if !errors.Is(err, persistErr) {
				t.Fatalf("markSharedStorageMigrationTargetReceiveComplete returned %v, want %v", err, persistErr)
			}
		})
	}
}

func TestCompletedSharedCephMigrationTargetCanBecomeOwned(t *testing.T) {
	config := map[string]string{}
	volatileSet := func(changes map[string]string) error {
		for key, value := range changes {
			if value == "" {
				delete(config, key)
				continue
			}

			config[key] = value
		}

		return nil
	}

	err := withSharedStorageMigrationTargetProtection(true, "ceph", volatileSet, func() error {
		return nil
	})
	if err != nil {
		t.Fatalf("Target claim failed: %v", err)
	}

	err = markSharedStorageMigrationTargetReceiveComplete(true, "ceph", volatileSet)
	if err != nil {
		t.Fatalf("Target receive completion failed: %v", err)
	}

	changes, err := internalInstance.StorageHandoverAPIConfigChanges(internalInstance.StorageHandoverStateOwned, config, "ceph")
	if err != nil {
		t.Fatalf("Completed target owned transition failed: %v", err)
	}

	err = volatileSet(changes)
	if err != nil {
		t.Fatalf("Applying owned transition failed: %v", err)
	}

	if internalInstance.StorageDeleteProtected(config) {
		t.Fatal("Owned target remained delete protected")
	}

	if config[internalInstance.ConfigVolatileMigrationStorageReceiveComplete] != "" {
		t.Fatal("Owned target retained receive completion proof")
	}

	if config[internalInstance.ConfigVolatileMigrationStorageHandoverRole] != "" {
		t.Fatal("Owned target retained handover role")
	}
}

func TestFailedSharedCephextMigrationTargetHasNoCompletionProof(t *testing.T) {
	claimErr := errors.New("claim failed")
	config := map[string]string{
		"ceph.rbd.image_name": "volume-test",
	}

	err := withSharedStorageMigrationTargetProtection(true, "cephext", func(changes map[string]string) error {
		for key, value := range changes {
			config[key] = value
		}

		return nil
	}, func() error {
		return claimErr
	})
	if !errors.Is(err, claimErr) {
		t.Fatalf("Target claim returned %v, want %v", err, claimErr)
	}

	if config[internalInstance.ConfigVolatileMigrationStorageReceiveComplete] != "" {
		t.Fatal("Failed cephext target acquired receive completion proof")
	}

	if config[internalInstance.ConfigVolatileMigrationStorageHandoverRole] != internalInstance.StorageHandoverRoleTarget {
		t.Fatal("Failed cephext target did not retain its target role")
	}

	if !internalInstance.StorageDeleteProtected(config) {
		t.Fatal("Failed cephext target did not retain delete protection")
	}

	_, err = internalInstance.StorageHandoverAPIConfigChanges(internalInstance.StorageHandoverStateOwned, config, "cephext")
	if !errors.Is(err, internalInstance.ErrStorageHandoverIncomplete) {
		t.Fatalf("Failed cephext target owned transition returned %v, want %v", err, internalInstance.ErrStorageHandoverIncomplete)
	}

	_, err = internalInstance.StorageHandoverAPIConfigChanges(internalInstance.StorageHandoverStateSourceOwned, config, "cephext")
	if !errors.Is(err, internalInstance.ErrStorageHandoverIncomplete) {
		t.Fatalf("Failed cephext target source-owned transition returned %v, want %v", err, internalInstance.ErrStorageHandoverIncomplete)
	}
}

func TestFailedSharedCephextReceiveHasNoCompletionProof(t *testing.T) {
	config := map[string]string{
		"ceph.rbd.image_name": "volume-test",
	}

	volatileSet := func(changes map[string]string) error {
		for key, value := range changes {
			config[key] = value
		}

		return nil
	}

	err := withSharedStorageMigrationTargetProtection(true, "cephext", volatileSet, func() error {
		return nil
	})
	if err != nil {
		t.Fatalf("Target claim failed: %v", err)
	}

	receive := func() error {
		return errors.New("receive failed")
	}

	err = receive()
	if err == nil {
		err = markSharedStorageMigrationTargetReceiveComplete(true, "cephext", volatileSet)
	}

	if err == nil {
		t.Fatal("Expected receive to fail")
	}

	if config[internalInstance.ConfigVolatileMigrationStorageReceiveComplete] != "" {
		t.Fatal("Failed cephext receive acquired completion proof")
	}

	if !internalInstance.StorageDeleteProtected(config) {
		t.Fatal("Failed cephext receive did not retain delete protection")
	}

	_, err = internalInstance.StorageHandoverAPIConfigChanges(internalInstance.StorageHandoverStateOwned, config, "cephext")
	if !errors.Is(err, internalInstance.ErrStorageHandoverIncomplete) {
		t.Fatalf("Failed cephext receive owned transition returned %v, want %v", err, internalInstance.ErrStorageHandoverIncomplete)
	}
}

func TestCompletedSharedCephextMigrationTargetCanClearHandover(t *testing.T) {
	const imageName = "volume-test"

	config := map[string]string{
		"ceph.rbd.image_name": imageName,
	}

	volatileSet := func(changes map[string]string) error {
		for key, value := range changes {
			if value == "" {
				delete(config, key)
				continue
			}

			config[key] = value
		}

		return nil
	}

	err := withSharedStorageMigrationTargetProtection(true, "cephext", volatileSet, func() error {
		return nil
	})
	if err != nil {
		t.Fatalf("Target claim failed: %v", err)
	}

	if !internalInstance.StorageDeleteProtected(config) {
		t.Fatal("cephext target did not acquire deletion protection before claim")
	}

	err = markSharedStorageMigrationTargetReceiveComplete(true, "cephext", volatileSet)
	if err != nil {
		t.Fatalf("Target receive completion failed: %v", err)
	}

	changes, err := internalInstance.StorageHandoverAPIConfigChanges(internalInstance.StorageHandoverStateOwned, config, "cephext")
	if err != nil {
		t.Fatalf("Completed target owned transition failed: %v", err)
	}

	err = volatileSet(changes)
	if err != nil {
		t.Fatalf("Applying owned transition failed: %v", err)
	}

	if config["ceph.rbd.image_name"] != imageName {
		t.Fatalf("Owned cleanup changed external RBD image reference to %q", config["ceph.rbd.image_name"])
	}

	if internalInstance.StorageDeleteProtected(config) {
		t.Fatal("Owned cephext target acquired Incus-owned deletion protection")
	}

	if config[internalInstance.ConfigVolatileMigrationStorageHandoverRole] != "" {
		t.Fatal("Owned cephext target retained handover role")
	}
}

func TestReleaseSharedStorageMigrationTargetClaim(t *testing.T) {
	running := true
	order := []string{}
	config := map[string]string{}

	err := releaseSharedStorageMigrationTargetClaim(
		"ceph",
		func() bool { return running },
		func() error {
			order = append(order, "stop")
			running = false
			return nil
		},
		func() error {
			order = append(order, "unmount")
			return nil
		},
		func(changes map[string]string) error {
			order = append(order, "protect")
			for key, value := range changes {
				config[key] = value
			}

			return nil
		},
		func() error {
			order = append(order, "delete")
			return nil
		},
	)
	if err != nil {
		t.Fatalf("releaseSharedStorageMigrationTargetClaim returned error: %v", err)
	}

	expectedOrder := []string{"stop", "unmount", "protect", "delete"}
	if !slices.Equal(order, expectedOrder) {
		t.Fatalf("Cleanup order = %v, want %v", order, expectedOrder)
	}

	if config[internalInstance.ConfigVolatileMigrationStorageHandover] != "pending" {
		t.Fatal("Ceph target storage was not protected before deleting its local claim")
	}

	if config[internalInstance.ConfigVolatileMigrationStorageHandoverRole] != internalInstance.StorageHandoverRoleTarget {
		t.Fatal("Ceph target storage did not retain its target role")
	}
}

func TestReleaseSharedStorageMigrationTargetClaimCephext(t *testing.T) {
	order := []string{}
	config := map[string]string{}

	err := releaseSharedStorageMigrationTargetClaim(
		"cephext",
		func() bool { return false },
		func() error {
			t.Fatal("Stopped an already stopped migration target")
			return nil
		},
		func() error {
			order = append(order, "unmount")
			return nil
		},
		func(changes map[string]string) error {
			order = append(order, "protect")
			for key, value := range changes {
				config[key] = value
			}

			return nil
		},
		func() error {
			order = append(order, "delete")
			return nil
		},
	)
	if err != nil {
		t.Fatalf("releaseSharedStorageMigrationTargetClaim returned error: %v", err)
	}

	expectedOrder := []string{"unmount", "protect", "delete"}
	if !slices.Equal(order, expectedOrder) {
		t.Fatalf("Cleanup order = %v, want %v", order, expectedOrder)
	}

	if !internalInstance.StorageDeleteProtected(config) {
		t.Fatal("External RBD claim was not protected before deleting its local record")
	}

	if config[internalInstance.ConfigVolatileMigrationStorageHandoverRole] != internalInstance.StorageHandoverRoleTarget {
		t.Fatal("External RBD claim did not retain its target role")
	}
}

func TestReleaseSharedStorageMigrationTargetClaimFailureIsDurable(t *testing.T) {
	tests := []struct {
		name       string
		driverName string
		running    bool
		failAt     string
	}{
		{name: "stop", driverName: "ceph", running: true, failAt: "stop"},
		{name: "unmount", driverName: "ceph", failAt: "unmount"},
		{name: "protection", driverName: "ceph", failAt: "protect"},
		{name: "delete", driverName: "ceph", failAt: "delete"},
		{name: "external protection", driverName: "cephext", failAt: "protect"},
		{name: "external delete", driverName: "cephext", failAt: "delete"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			injectedErr := errors.New("injected cleanup failure")
			running := tt.running

			fail := func(phase string) error {
				if tt.failAt == phase {
					return injectedErr
				}

				return nil
			}

			err := releaseSharedStorageMigrationTargetClaim(
				tt.driverName,
				func() bool { return running },
				func() error {
					err := fail("stop")
					if err == nil {
						running = false
					}

					return err
				},
				func() error { return fail("unmount") },
				func(changes map[string]string) error { return fail("protect") },
				func() error { return fail("delete") },
			)
			if !errors.Is(err, injectedErr) {
				t.Fatalf("Cleanup error = %v, want injected failure", err)
			}

			if !errors.Is(err, serverInstance.ErrMigrationTargetCleanupIncomplete) {
				t.Fatalf("Cleanup error = %v, want durable incomplete-cleanup sentinel", err)
			}
		})
	}
}

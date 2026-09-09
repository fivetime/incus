//go:build linux && cgo && !agent

package storage

import (
	"errors"
	"testing"

	"github.com/lxc/incus/v7/internal/server/storage/drivers"
)

type testVolumeLocalStateDriver struct {
	identity   string
	hasState   bool
	releaseErr error
	releases   int
}

func (d *testVolumeLocalStateDriver) GetVolumeIdentity(drivers.Volume) (string, error) {
	return d.identity, nil
}

func (d *testVolumeLocalStateDriver) HasVolumeLocalState(_ drivers.Volume, expectedStorageIdentity string) (bool, error) {
	if d.identity != "" && d.identity != expectedStorageIdentity {
		return false, errors.New("identity mismatch")
	}

	return d.hasState, nil
}

func (d *testVolumeLocalStateDriver) ReleaseVolumeLocalState(_ drivers.Volume, expectedStorageIdentity string) error {
	if d.identity != expectedStorageIdentity {
		return errors.New("identity mismatch")
	}

	d.releases++
	if d.releaseErr != nil {
		return d.releaseErr
	}

	d.hasState = false
	return nil
}

func TestReleaseVolumeLocalState(t *testing.T) {
	vol := drivers.NewVolume(nil, "pool", drivers.VolumeTypeContainer, drivers.ContentTypeFS, "instance", nil, nil)

	t.Run("identity-bound release", func(t *testing.T) {
		driver := &testVolumeLocalStateDriver{identity: "immutable", hasState: true}
		err := releaseVolumeLocalState(driver, vol, "immutable")
		if err != nil {
			t.Fatal(err)
		}

		if driver.releases != 1 || driver.hasState {
			t.Fatalf("Local state was not released exactly once: releases=%d state=%t", driver.releases, driver.hasState)
		}
	})

	t.Run("identity mismatch", func(t *testing.T) {
		driver := &testVolumeLocalStateDriver{identity: "recreated", hasState: true}
		err := releaseVolumeLocalState(driver, vol, "immutable")
		if err == nil {
			t.Fatal("Recreated storage object was released")
		}

		if driver.releases != 0 {
			t.Fatal("Identity mismatch reached the release operation")
		}
	})

	t.Run("release failure", func(t *testing.T) {
		releaseErr := errors.New("unmap failed")
		driver := &testVolumeLocalStateDriver{identity: "immutable", hasState: true, releaseErr: releaseErr}
		err := releaseVolumeLocalState(driver, vol, "immutable")
		if !errors.Is(err, releaseErr) {
			t.Fatalf("Release error was not preserved: %v", err)
		}

		if !driver.hasState {
			t.Fatal("Failed release was treated as clean")
		}
	})
}

func TestValidateVolumeLocalStateReleased(t *testing.T) {
	vol := drivers.NewVolume(nil, "pool", drivers.VolumeTypeContainer, drivers.ContentTypeFS, "instance", nil, nil)
	provider := &testVolumeLocalStateDriver{identity: "immutable", hasState: true}
	err := validateVolumeLocalStateReleased(provider, vol, "immutable")
	if err == nil {
		t.Fatal("Residual local state was accepted")
	}

	provider.hasState = false
	err = validateVolumeLocalStateReleased(provider, vol, "immutable")
	if err != nil {
		t.Fatal(err)
	}

	err = validateVolumeLocalStateReleased(provider, vol, "")
	if err == nil {
		t.Fatal("Empty identity was accepted for local-state proof")
	}
}

func TestCompleteStorageReleaseRequiresLocalStateProof(t *testing.T) {
	vol := drivers.NewVolume(nil, "pool", drivers.VolumeTypeContainer, drivers.ContentTypeFS, "instance", nil, nil)
	provider := &testVolumeLocalStateDriver{identity: "immutable", hasState: true}
	completed := false
	err := completeStorageReleaseAfterLocalStateProof(provider, vol, "immutable", func() error {
		completed = true
		return nil
	})
	if err == nil {
		t.Fatal("Storage release completed while local state remained")
	}

	if completed {
		t.Fatal("Completion callback ran without a local state proof")
	}

	provider.hasState = false
	err = completeStorageReleaseAfterLocalStateProof(provider, vol, "immutable", func() error {
		completed = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if !completed {
		t.Fatal("Completion callback did not run after a clean proof")
	}
}

type testVolumeDetachedLocalStateDriver struct {
	testVolumeLocalStateDriver
	detachedReleases int
	retainState      bool
}

func (d *testVolumeDetachedLocalStateDriver) ReleaseVolumeDetachedLocalState(_ drivers.Volume, identity string) error {
	if d.identity != identity {
		return errors.New("identity mismatch")
	}

	d.detachedReleases++
	if d.releaseErr != nil {
		return d.releaseErr
	}

	d.hasState = d.retainState
	return nil
}

func TestReleaseVolumeLocalStateDetached(t *testing.T) {
	vol := drivers.NewVolume(nil, "pool", drivers.VolumeTypeContainer, drivers.ContentTypeFS, "instance", nil, nil)
	for _, tc := range []struct {
		name        string
		identity    string
		retainState bool
		releaseErr  error
		wantError   bool
	}{
		{name: "stale receive mount", identity: "immutable"},
		{name: "foreign identity", identity: "other", wantError: true},
		{name: "mount remains", identity: "immutable", retainState: true, wantError: true},
		{name: "unmount failed", identity: "immutable", releaseErr: errors.New("busy"), wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			driver := &testVolumeDetachedLocalStateDriver{
				testVolumeLocalStateDriver: testVolumeLocalStateDriver{identity: "immutable", hasState: true, releaseErr: tc.releaseErr},
				retainState:                tc.retainState,
			}
			err := releaseVolumeLocalStateDetached(driver, vol, tc.identity)
			if (err != nil) != tc.wantError {
				t.Fatalf("Unexpected release result: %v", err)
			}

			if driver.releases != 0 {
				t.Fatal("Detached release used conservative cleanup")
			}
		})
	}

	fallback := &testVolumeLocalStateDriver{identity: "immutable", hasState: true}
	err := releaseVolumeLocalStateDetached(fallback, vol, "immutable")
	if err != nil || fallback.releases != 1 {
		t.Fatalf("Conservative fallback failed: %v", err)
	}
}

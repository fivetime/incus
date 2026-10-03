package drivers

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus/v7/internal/server/locking"
)

func TestFencedOwnershipTransferWaitsForFlattenAndRejectsReplacement(t *testing.T) {
	commands := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(commands, "rados"), []byte("#!/bin/sh\nprintf '%s' '{\"pools\":[{\"name\":\"test\",\"id\":9}]}'\n"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(commands, "rbd"), []byte("#!/bin/sh\nprintf '%s' '{\"id\":\"def\",\"block_name_prefix\":\"rbd_data.def\"}'\n"), 0o700))
	t.Setenv("PATH", commands)
	d := &ceph{common: common{name: "takeover-test", config: map[string]string{
		"ceph.cluster_name": "test", "ceph.user.name": "test", "ceph.osd.pool_name": "test",
	}}}
	vol := NewVolume(d, d.name, VolumeTypeContainer, ContentTypeFS, "takeover-test", nil, d.config)
	identity, err := (cephRBDVolumeIdentity{PoolID: 9, ID: "abc", BlockNamePrefix: "rbd_data.abc"}).canonical()
	require.NoError(t, err)
	unlock, err := locking.Lock(context.Background(), d.flattenLockName(vol))
	require.NoError(t, err)
	released := false
	defer func() {
		if !released {
			unlock()
		}
	}()

	done := make(chan error, 1)
	go func() {
		done <- d.TransferVolumeMaterializationOwnership(vol, identity,
			"12345678-1234-1234-1234-123456789abc",
			"sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64))
	}()
	select {
	case err = <-done:
		t.Fatalf("Ownership transfer bypassed the flatten lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	unlock()
	released = true
	select {
	case err = <-done:
		require.ErrorContains(t, err, "no longer identifies the takeover root")
	case <-time.After(5 * time.Second):
		t.Fatal("Ownership transfer failed to reject the replacement")
	}
}

func TestParseCephMaterializationOwnership(t *testing.T) {
	marker := `{"version":1,"token":"attempt"}`
	data := `{"another.key":"value","` + cephMaterializationOwnershipKey + `":` + `"{\"version\":1,\"token\":\"attempt\"}"}`

	result, err := parseCephMaterializationOwnership(data)
	require.NoError(t, err)
	require.Equal(t, marker, result)

	result, err = parseCephMaterializationOwnership(`{"another.key":"value"}`)
	require.NoError(t, err)
	require.Empty(t, result)

	// rbd image-meta list prints nothing at all for an image without any
	// metadata keys; that is a pristine volume, not an error.
	for _, empty := range []string{"", "\n", "  \n"} {
		result, err = parseCephMaterializationOwnership(empty)
		require.NoError(t, err)
		require.Empty(t, result)
	}

	_, err = parseCephMaterializationOwnership(`not-json`)
	require.Error(t, err)
}

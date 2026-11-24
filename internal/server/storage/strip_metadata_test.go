package storage

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus/v7/internal/server/storage/rescue"
)

// TestStripMetadataSkipsGuestRoots checks that the metadata strip leaves both the guest rootfs and
// the temporary root of an instance in rescue alone, while still removing a symlink elsewhere.
func TestStripMetadataSkipsGuestRoots(t *testing.T) {
	instPath := t.TempDir()
	rescueRoot := rescue.TemporaryRootPath(instPath)

	for _, dir := range []string{filepath.Join(instPath, "rootfs", "etc"), filepath.Join(rescueRoot, "etc")} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.Symlink("/etc/shadow", filepath.Join(dir, "link")))
	}

	require.NoError(t, os.Symlink("/etc/shadow", filepath.Join(instPath, "templates")))

	removed, err := stripMetadata(instPath)
	require.NoError(t, err)
	require.Equal(t, []string{filepath.Join(instPath, "templates")}, removed)

	for _, dir := range []string{filepath.Join(instPath, "rootfs", "etc"), filepath.Join(rescueRoot, "etc")} {
		_, err := os.Lstat(filepath.Join(dir, "link"))
		require.NoError(t, err)
	}
}

// TestStripMetadataRescueRootSymlink checks that a symlink placed where the rescue root belongs is
// removed rather than skipped.
func TestStripMetadataRescueRootSymlink(t *testing.T) {
	instPath := t.TempDir()
	rescueRoot := rescue.TemporaryRootPath(instPath)

	require.NoError(t, os.MkdirAll(filepath.Dir(rescueRoot), 0o755))
	require.NoError(t, os.Symlink("/", rescueRoot))

	removed, err := stripMetadata(instPath)
	require.NoError(t, err)
	require.Equal(t, []string{rescueRoot}, removed)
}

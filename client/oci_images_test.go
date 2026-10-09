//go:build linux

package incus

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus/v7/shared/ioprogress"
)

type ociFailingWriter struct {
	bytes.Buffer

	remaining int
	err       error
	closed    bool
}

func (w *ociFailingWriter) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		n, _ := w.Buffer.Write(p[:w.remaining])
		w.remaining = 0
		return n, w.err
	}

	w.remaining -= len(p)
	return w.Buffer.Write(p)
}

func (w *ociFailingWriter) Close() error {
	w.closed = true
	return nil
}

func TestWriteOCIImageTarball(t *testing.T) {
	path := t.TempDir()
	content := []byte("OCI archive content\n")
	err := os.WriteFile(filepath.Join(path, "file"), content, 0o640)
	require.NoError(t, err)

	for _, name := range []string{"metadata", "rootfs"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()

			var output bytes.Buffer
			args := []string{"-cf", "-", "-C", path, "file"}
			if name == "rootfs" {
				args = append([]string{"--format=pax", "--xattrs", "--xattrs-include=*", "--acls"}, args...)
			}

			size, err := writeOCIImageTarball(ctx, &output, args, name, func(ioprogress.ProgressData) {})
			require.NoError(t, err)
			require.Equal(t, int64(output.Len()), size)

			reader, err := gzip.NewReader(bytes.NewReader(output.Bytes()))
			require.NoError(t, err)
			t.Cleanup(func() {
				require.NoError(t, reader.Close())
			})

			archive := tar.NewReader(reader)
			header, err := archive.Next()
			require.NoError(t, err)
			require.Equal(t, "file", header.Name)
			data, err := io.ReadAll(archive)
			require.NoError(t, err)
			require.Equal(t, content, data)
			_, err = archive.Next()
			require.ErrorIs(t, err, io.EOF)
			_, err = io.Copy(io.Discard, reader)
			require.NoError(t, err)
		})
	}
}

func TestWriteOCIImageTarballErrors(t *testing.T) {
	path := t.TempDir()
	err := os.WriteFile(filepath.Join(path, "file"), []byte("small buffered archive"), 0o600)
	require.NoError(t, err)

	for _, tt := range []struct {
		name      string
		missing   bool
		startFail bool
		remaining int
	}{
		{name: "tar exit", missing: true, remaining: 1 << 20},
		{name: "gzip close", remaining: 10},
		{name: "tar exit and gzip close", missing: true, remaining: 10},
		{name: "tar start and gzip close", startFail: true, remaining: 10},
		{name: "stdout write", remaining: 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()

			filename := "file"
			if tt.missing {
				filename = "missing"
			}

			if tt.startFail {
				t.Setenv("PATH", t.TempDir())
			}

			writeErr := errors.New("injected destination failure")
			output := &ociFailingWriter{remaining: tt.remaining, err: writeErr}
			size, err := writeOCIImageTarball(ctx, output, []string{"-cf", "-", "-C", path, filename}, "test", nil)
			require.Error(t, err)
			require.Equal(t, int64(output.Len()), size)
			require.False(t, output.closed)

			if tt.remaining <= 10 {
				require.ErrorIs(t, err, writeErr)
			} else {
				reader, gzipErr := gzip.NewReader(bytes.NewReader(output.Bytes()))
				require.NoError(t, gzipErr)
				t.Cleanup(func() {
					require.NoError(t, reader.Close())
				})
				_, gzipErr = io.Copy(io.Discard, reader)
				require.NoError(t, gzipErr)
			}

			if tt.missing {
				var exitErr *exec.ExitError
				require.ErrorAs(t, err, &exitErr)
			}

			if tt.startFail {
				require.ErrorIs(t, err, exec.ErrNotFound)
			}
		})
	}
}

func TestWriteOCIImageTarballCancellation(t *testing.T) {
	path := t.TempDir()
	err := os.WriteFile(filepath.Join(path, "tar"), []byte("#!/bin/sh\nexec /bin/sleep 60\n"), 0o700)
	require.NoError(t, err)
	t.Setenv("PATH", path)

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	var output bytes.Buffer
	_, err = writeOCIImageTarball(ctx, &output, nil, "test", nil)
	require.Error(t, err)
	require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)

	reader, err := gzip.NewReader(bytes.NewReader(output.Bytes()))
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, reader.Close())
	})
	_, err = io.Copy(io.Discard, reader)
	require.NoError(t, err)
}

//go:build linux

package rescue

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRescueRetainsRepairsAcrossInterruptedRestore(t *testing.T) {
	for _, interrupted := range []string{"none", "activate", "restore", "discard-full-root"} {
		t.Run(interrupted, func(t *testing.T) {
			base := t.TempDir()
			err := os.Mkdir(filepath.Join(base, "rootfs"), 0o755)
			if err != nil {
				t.Fatal(err)
			}

			err = os.WriteFile(filepath.Join(base, "rootfs", "original"), []byte("original"), 0o600)
			if err != nil {
				t.Fatal(err)
			}

			token := "a41feaa1-a4c0-41a5-aa9d-f9aa54c25ff9"
			err = Prepare(base, token, strings.Repeat("a", 64), func(path string) error {
				err := os.Mkdir(filepath.Join(path, "rootfs"), 0o755)
				if err != nil {
					return err
				}

				return os.WriteFile(filepath.Join(path, "rootfs", "rescue"), []byte("temporary"), 0o600)
			})
			if err != nil {
				t.Fatal(err)
			}

			err = ValidateActive(base, token)
			if err != nil {
				t.Fatal(err)
			}

			_, err = os.Stat(filepath.Join(base, "rootfs", "original"))
			if err != nil {
				t.Fatal("Rescue moved the canonical original root")
			}

			err = os.WriteFile(filepath.Join(OriginalPath(base), "repair"), []byte("repaired"), 0o600)
			if err != nil {
				t.Fatal(err)
			}

			root, err := os.OpenRoot(base)
			if err != nil {
				t.Fatal(err)
			}

			defer func() { _ = root.Close() }()
			switch interrupted {
			case "discard-full-root":
				err = discardTemporaryContents(root)
				if err != nil {
					t.Fatal(err)
				}

				err = ValidateActive(base, token)
				if err != nil {
					t.Fatal("Emergency cleanup lost directory identity", err)
				}

			case "activate":
				state, err := read(root)
				if err != nil {
					t.Fatal(err)
				}

				state.Phase = "prepared"
				err = save(root, state)
				if err != nil {
					t.Fatal(err)
				}

			case "restore":
				state, err := read(root)
				if err != nil {
					t.Fatal(err)
				}

				state.Phase = "restored"
				err = save(root, state)
				if err != nil {
					t.Fatal(err)
				}
			}

			for range 2 {
				err = Restore(base, token, false)
				if err != nil {
					t.Fatal(err)
				}
			}

			data, err := os.ReadFile(filepath.Join(base, "rootfs", "repair"))
			if err != nil || string(data) != "repaired" {
				t.Fatalf("Original repair was not retained: %q, %v", data, err)
			}

			_, err = os.Stat(filepath.Join(base, "rootfs", "rescue"))
			if !os.IsNotExist(err) {
				t.Fatal("Temporary root remained active")
			}
		})
	}
}

func TestRescueRejectsSymlinkAndForeignOwner(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	err := os.Mkdir(filepath.Join(base, "rootfs"), 0o755)
	if err != nil {
		t.Fatal(err)
	}

	token := "a41feaa1-a4c0-41a5-aa9d-f9aa54c25ff9"
	err = Prepare(base, token, strings.Repeat("a", 64), func(path string) error {
		return os.Symlink(outside, filepath.Join(path, "rootfs"))
	})
	if err == nil {
		t.Fatal("Accepted a symlink rescue root")
	}

	err = Restore(base, "3e7fa11f-dcdc-4293-9b45-c16c5a5f74ad", false)
	if err == nil {
		t.Fatal("Accepted a foreign restore owner")
	}

	err = Restore(base, token, false)
	if err != nil {
		t.Fatal(err)
	}

	_, err = os.Stat(outside)
	if err != nil {
		t.Fatal("Removed a path outside the storage volume")
	}
}

package apparmor

import (
	"strings"
	"testing"

	"github.com/lxc/incus/v7/internal/server/sys"
)

func TestRsyncProfileAllowsRootPathResolution(t *testing.T) {
	profile, err := rsyncProfile(&sys.OS{ExecPath: "/usr/bin/incusd"}, "test-rsync", "/tmp/source", "/tmp/destination")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(profile, "\n  / r,\n") {
		t.Fatal("Rsync profile does not allow secure absolute path resolution from the filesystem root")
	}

	if strings.Contains(profile, "\n  /** r,") {
		t.Fatal("Rsync profile must not grant recursive read access to the filesystem root")
	}
}

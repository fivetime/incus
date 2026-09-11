package apparmor

import (
	"strings"
	"testing"

	"github.com/lxc/incus/v7/internal/server/sys"
)

func TestRsyncProfileAllowsRootPathResolution(t *testing.T) {
	for _, tt := range []struct {
		name        string
		source      string
		destination string
	}{
		{name: "send", source: "/tmp/source"},
		{name: "receive", destination: "/tmp/parent/destination"},
		{name: "local-copy", source: "/tmp/source", destination: "/tmp/parent/destination"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			profile, err := rsyncProfile(&sys.OS{ExecPath: "/usr/bin/incusd"}, "test-rsync", tt.source, tt.destination)
			if err != nil {
				t.Fatal(err)
			}

			if strings.Count(profile, "\n  / r,\n") != 1 {
				t.Fatal("Rsync profile must grant root path resolution exactly once")
			}

			if tt.destination != "" && (!strings.Contains(profile, "\n  /tmp/ r,\n") || !strings.Contains(profile, "\n  /tmp/parent/ r,\n")) {
				t.Fatal("Rsync profile must allow reading destination ancestors")
			}

			if strings.Contains(profile, "\n  /** r,") {
				t.Fatal("Rsync profile must not grant recursive read access to the filesystem root")
			}
		})
	}
}

//go:build linux && cgo

package seccomp

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// configInstance is an Instance with only a configuration.
type configInstance struct {
	Instance
	config map[string]string
}

func (c configInstance) ExpandedConfig() map[string]string {
	return c.config
}

func TestMountSyscallLXCFS(t *testing.T) {
	lxcfs := filepath.Join(t.TempDir(), "proc")
	err := os.Mkdir(lxcfs, 0o755)
	if err != nil {
		t.Fatal(err)
	}

	oldDirs := lxcfsProcDirs
	lxcfsProcDirs = []string{filepath.Join(t.TempDir(), "missing"), lxcfs}
	t.Cleanup(func() { lxcfsProcDirs = oldDirs })

	srv := &Server{}
	on := configInstance{config: map[string]string{"security.syscalls.intercept.mount": "true", "security.syscalls.intercept.mount.lxcfs": "true"}}
	off := configInstance{config: map[string]string{"security.syscalls.intercept.mount": "true"}}

	cases := []struct {
		name   string
		inst   Instance
		args   MountArgs
		dir    string
		wanted bool
	}{
		{"option off", off, MountArgs{fstype: "proc"}, "", false},
		{"other file system", on, MountArgs{fstype: "sysfs"}, "", false},
		{"bind mount", on, MountArgs{fstype: "proc", flags: unix.MS_BIND}, "", false},
		{"remount", on, MountArgs{fstype: "proc", flags: unix.MS_REMOUNT}, "", false},
		{"move", on, MountArgs{fstype: "proc", flags: unix.MS_MOVE}, "", false},
		{"no LXCFS", on, MountArgs{fstype: "proc", flags: unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC}, "", true},
	}

	for _, tc := range cases {
		dir, wanted := srv.mountSyscallLXCFS(tc.inst, &tc.args)
		if dir != tc.dir || wanted != tc.wanted {
			t.Fatal(fmt.Errorf("%s: got (%q, %v), want (%q, %v)", tc.name, dir, wanted, tc.dir, tc.wanted))
		}
	}

	// Once LXCFS is there, the first directory holding meminfo is used.
	err = os.WriteFile(filepath.Join(lxcfs, "meminfo"), nil, 0o644)
	if err != nil {
		t.Fatal(err)
	}

	dir, wanted := srv.mountSyscallLXCFS(on, &MountArgs{fstype: "proc"})
	if dir != lxcfs || !wanted {
		t.Fatal(fmt.Errorf("got (%q, %v), want (%q, true)", dir, wanted, lxcfs))
	}
}

func TestMountFlagsToOpts(t *testing.T) {
	opts := mountFlagsToOpts(knownFlags)
	if opts != "ro,nosuid,nodev,noexec,sync,remount,mand,noatime,nodiratime,bind,strictatime,lazytime" {
		t.Fatal(fmt.Errorf("Mount options parsing failed with invalid option string: %s", opts))
	}

	opts = mountFlagsToOpts(knownFlagsRecursive)
	if opts != "ro,nosuid,nodev,noexec,sync,remount,mand,noatime,nodiratime,rbind,strictatime,lazytime" {
		t.Fatal(fmt.Errorf("Mount options parsing failed with invalid option string: %s", opts))
	}
}

func TestStripMountOption(t *testing.T) {
	cases := map[string]string{
		"":                                  "",
		"ro,noatime":                        "ro,noatime",
		"host_authorized_resize":            "",
		"ro,host_authorized_resize,noatime": "ro,noatime",
		"host_authorized_resize=1,ro":       "ro",
		"ro,,host_authorized_resize=yes,":   "ro",
		"nohost_authorized_resize,ro":       "nohost_authorized_resize,ro",
		"host_authorized_resizex,ro":        "host_authorized_resizex,ro",
	}

	for in, want := range cases {
		got := stripMountOption(in, fuseblkHostAuthorizedResize)
		if got != want {
			t.Fatal(fmt.Errorf("stripMountOption(%q) = %q, want %q", in, got, want))
		}
	}
}

func TestFuseblkRemountAllowed(t *testing.T) {
	cases := []struct {
		flags int
		want  bool
	}{
		// A plain remount, with or without flags mount(8) adds on its own.
		{unix.MS_REMOUNT, true},
		{unix.MS_REMOUNT | unix.MS_SYNCHRONOUS | unix.MS_NOATIME, true},
		{unix.MS_REMOUNT | unix.MS_RDONLY | unix.MS_SILENT, true},

		// Not a remount.
		{0, false},
		{unix.MS_SYNCHRONOUS, false},

		// Bind and move remounts are the kernel's; FUSE refuses mandatory locking.
		{unix.MS_REMOUNT | unix.MS_BIND, false},
		{unix.MS_REMOUNT | unix.MS_MOVE, false},
		{unix.MS_REMOUNT | unix.MS_MANDLOCK, false},
	}

	for _, tc := range cases {
		if got := fuseblkRemountAllowed(tc.flags); got != tc.want {
			t.Fatal(fmt.Errorf("fuseblkRemountAllowed(%#x) = %v, want %v", tc.flags, got, tc.want))
		}
	}
}

func TestMountFuseblkOpts(t *testing.T) {
	cases := []struct {
		name       string
		guest      string
		configured string
		resize     bool
		want       string
	}{
		// Nothing configured, interception off: the guest's options as they are.
		{"guest only", "ro,noatime", "", false, "ro,noatime"},

		// Configured helper options are appended.
		{"configured appended", "ro", "kernel,noexcl", false, "ro,kernel,noexcl"},

		// With the interception on, Incus adds host_authorized_resize itself.
		{"resize added", "ro", "kernel,noexcl", true, "ro,kernel,noexcl,host_authorized_resize"},

		// Neither the guest nor the configuration may add it on their own.
		{"resize stripped", "ro,host_authorized_resize", "kernel,host_authorized_resize=1,noexcl", false, "ro,kernel,noexcl"},

		// And with the interception on it appears exactly once.
		{"resize once", "host_authorized_resize", "host_authorized_resize", true, "host_authorized_resize"},
	}

	for _, c := range cases {
		got := mountFuseblkOpts(c.guest, c.configured, c.resize)
		if got != c.want {
			t.Fatal(fmt.Errorf("%s: mountFuseblkOpts(%q, %q, %v) = %q, want %q", c.name, c.guest, c.configured, c.resize, got, c.want))
		}
	}
}

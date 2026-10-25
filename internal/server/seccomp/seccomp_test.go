//go:build linux && cgo

package seccomp

import (
	"fmt"
	"testing"
)

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

func TestMountFuseblkOpts(t *testing.T) {
	// Nothing configured, interception off: the guest's options as they are.
	if got := mountFuseblkOpts("ro,noatime", "", false); got != "ro,noatime" {
		t.Fatal(fmt.Errorf("unexpected options: %q", got))
	}

	// Configured helper options are appended.
	if got := mountFuseblkOpts("ro", "kernel,noexcl", false); got != "ro,kernel,noexcl" {
		t.Fatal(fmt.Errorf("unexpected options: %q", got))
	}

	// With the interception on, Incus adds host_authorized_resize itself.
	if got := mountFuseblkOpts("ro", "kernel,noexcl", true); got != "ro,kernel,noexcl,host_authorized_resize" {
		t.Fatal(fmt.Errorf("unexpected options: %q", got))
	}

	// Neither the guest nor the configuration may add it on their own.
	if got := mountFuseblkOpts("ro,host_authorized_resize", "kernel,host_authorized_resize=1,noexcl", false); got != "ro,kernel,noexcl" {
		t.Fatal(fmt.Errorf("option not stripped: %q", got))
	}

	// And with the interception on it appears exactly once.
	if got := mountFuseblkOpts("host_authorized_resize", "host_authorized_resize", true); got != "host_authorized_resize" {
		t.Fatal(fmt.Errorf("option duplicated: %q", got))
	}
}

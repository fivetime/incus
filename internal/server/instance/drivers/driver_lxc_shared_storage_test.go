package drivers

import "testing"

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
			if actual := isCephSharedStorageDriver(driverName); actual != expected {
				t.Fatalf("isCephSharedStorageDriver(%q) = %t, want %t", driverName, actual, expected)
			}
		})
	}
}

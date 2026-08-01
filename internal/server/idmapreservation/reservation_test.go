package idmapreservation

import (
	"testing"
)

func TestRangesOverlap(t *testing.T) {
	tests := []struct {
		name     string
		baseA    int64
		sizeA    int64
		baseB    int64
		sizeB    int64
		expected bool
	}{
		{name: "same", baseA: 100000, sizeA: 65536, baseB: 100000, sizeB: 65536, expected: true},
		{name: "partial", baseA: 100000, sizeA: 65536, baseB: 150000, sizeB: 65536, expected: true},
		{name: "adjacent", baseA: 100000, sizeA: 65536, baseB: 165536, sizeB: 65536, expected: false},
		{name: "separate", baseA: 100000, sizeA: 65536, baseB: 300000, sizeB: 65536, expected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := RangesOverlap(tt.baseA, tt.sizeA, tt.baseB, tt.sizeB)
			if actual != tt.expected {
				t.Fatalf("RangesOverlap() = %t, want %t", actual, tt.expected)
			}
		})
	}
}

func TestClearTransientRequiresMatchingReservation(t *testing.T) {
	const instanceID = 42
	first := Reservation{Base: 100000, Size: 65536}
	replacement := Reservation{Base: 200000, Size: 65536}

	SetTransient(instanceID, first)
	SetTransient(instanceID, replacement)
	ClearTransient(instanceID, first)

	actual := Transient()[instanceID]
	if actual != replacement {
		t.Fatalf("ClearTransient() removed replacement reservation %#v", actual)
	}

	ClearTransient(instanceID, replacement)
	_, ok := Transient()[instanceID]
	if ok {
		t.Fatal("ClearTransient() retained matching reservation")
	}
}

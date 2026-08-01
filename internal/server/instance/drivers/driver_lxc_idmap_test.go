package drivers

import (
	"testing"
)

func TestIdmapRangesOverlap(t *testing.T) {
	tests := []struct {
		name     string
		baseA    int64
		sizeA    int64
		baseB    int64
		sizeB    int64
		expected bool
	}{
		{name: "same", baseA: 100000, sizeA: 65536, baseB: 100000, sizeB: 65536, expected: true},
		{name: "contained", baseA: 100000, sizeA: 65536, baseB: 110000, sizeB: 1000, expected: true},
		{name: "partial before", baseA: 90000, sizeA: 20000, baseB: 100000, sizeB: 65536, expected: true},
		{name: "partial after", baseA: 150000, sizeA: 20000, baseB: 100000, sizeB: 65536, expected: true},
		{name: "adjacent before", baseA: 34464, sizeA: 65536, baseB: 100000, sizeB: 65536, expected: false},
		{name: "adjacent after", baseA: 165536, sizeA: 65536, baseB: 100000, sizeB: 65536, expected: false},
		{name: "separate", baseA: 200000, sizeA: 65536, baseB: 100000, sizeB: 65536, expected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := idmapRangesOverlap(tt.baseA, tt.sizeA, tt.baseB, tt.sizeB)
			if actual != tt.expected {
				t.Fatalf("idmapRangesOverlap(%d, %d, %d, %d) = %t, want %t", tt.baseA, tt.sizeA, tt.baseB, tt.sizeB, actual, tt.expected)
			}

			reverse := idmapRangesOverlap(tt.baseB, tt.sizeB, tt.baseA, tt.sizeA)
			if reverse != tt.expected {
				t.Fatalf("Reverse idmapRangesOverlap(%d, %d, %d, %d) = %t, want %t", tt.baseB, tt.sizeB, tt.baseA, tt.sizeA, reverse, tt.expected)
			}
		})
	}
}

func TestIdmapRangeFitsBefore(t *testing.T) {
	tests := []struct {
		name     string
		base     int64
		size     int64
		nextBase int64
		expected bool
	}{
		{name: "exact fit", base: 100000, size: 65536, nextBase: 165536, expected: true},
		{name: "one ID short", base: 100000, size: 65536, nextBase: 165535, expected: false},
		{name: "larger gap", base: 100000, size: 65536, nextBase: 200000, expected: true},
		{name: "overflow", base: 9223372036854775800, size: 100, nextBase: 9223372036854775807, expected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := idmapRangeFitsBefore(tt.base, tt.size, tt.nextBase)
			if actual != tt.expected {
				t.Fatalf("idmapRangeFitsBefore(%d, %d, %d) = %t, want %t", tt.base, tt.size, tt.nextBase, actual, tt.expected)
			}
		})
	}
}

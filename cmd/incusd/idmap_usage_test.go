package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIDMapUsageGetRejectsInvalidRequest(t *testing.T) {
	const owner = "11111111-1111-4111-8111-111111111111"
	tests := []struct {
		name  string
		query string
	}{
		{name: "missing owner", query: "base=1000000&size=65536"},
		{name: "invalid owner", query: "owner=no&base=1000000&size=65536"},
		{name: "missing base", query: "owner=" + owner + "&size=65536"},
		{name: "invalid base", query: "owner=" + owner + "&base=no&size=65536"},
		{name: "missing size", query: "owner=" + owner + "&base=1000000"},
		{name: "invalid size", query: "owner=" + owner + "&base=1000000&size=no"},
		{name: "zero size", query: "owner=" + owner + "&base=1000000&size=0"},
		{name: "range overflow", query: "owner=" + owner + "&base=4294967295&size=2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/1.0/idmap-usage?"+tt.query, nil)
			result := idmapUsageGet(nil, r)
			require.Equal(t, http.StatusBadRequest, result.Code())
		})
	}
}

func TestUint32Query(t *testing.T) {
	tests := []struct {
		name    string
		query   string
		value   uint64
		wantErr bool
	}{
		{name: "missing", wantErr: true},
		{name: "invalid", query: "base=no", wantErr: true},
		{name: "negative", query: "base=-1", wantErr: true},
		{name: "overflow", query: "base=4294967296", wantErr: true},
		{name: "canonical", query: "base=00042", value: 42},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/1.0/idmap-usage?"+tt.query, nil)
			value, err := uint32Query(r, "base")
			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.value, value)
		})
	}
}

func TestValidateIDMapUsageRange(t *testing.T) {
	require.Error(t, validateIDMapUsageRange(1000000, 0))
	require.Error(t, validateIDMapUsageRange(4294967295, 2))
	require.NoError(t, validateIDMapUsageRange(0, 1))
	require.NoError(t, validateIDMapUsageRange(4294967295, 1))
}

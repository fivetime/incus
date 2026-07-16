package device

import (
	"testing"

	deviceConfig "github.com/lxc/incus/v7/internal/server/device/config"
	"github.com/stretchr/testify/require"
)

func TestUnixBlockLimitParse(t *testing.T) {
	config := deviceConfig.Device{
		"type":         "unix-block",
		"limits.read":  "10MB",
		"limits.write": "500iops",
	}

	readBps, readIops, writeBps, writeIops, err := unixBlockLimitParse(config)
	require.NoError(t, err)
	require.Equal(t, int64(10000000), readBps)
	require.Zero(t, readIops)
	require.Zero(t, writeBps)
	require.Equal(t, int64(500), writeIops)
}

func TestUnixBlockLimitValidate(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "bytes", value: "1B"},
		{name: "iops", value: "1iops"},
		{name: "zero bytes", value: "0B", wantErr: true},
		{name: "zero iops", value: "0iops", wantErr: true},
		{name: "invalid", value: "fast", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := deviceConfig.Device{"type": "unix-block", "limits.read": tt.value}
			err := unixBlockLimitValidate(config)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

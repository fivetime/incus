package drivers

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigrationPreDumpSettings(t *testing.T) {
	tests := []struct {
		name           string
		config         map[string]string
		supported      bool
		expectEnabled  bool
		expectAttempts int
		expectProbe    bool
	}{
		{name: "Unset", config: nil},
		{name: "Disabled", config: map[string]string{"migration.incremental.memory": "false"}},
		{name: "Unsupported", config: map[string]string{"migration.incremental.memory": "true"}, expectProbe: true},
		{name: "Default iterations", config: map[string]string{"migration.incremental.memory": "true"}, supported: true, expectEnabled: true, expectAttempts: 10, expectProbe: true},
		{name: "Iteration limit", config: map[string]string{"migration.incremental.memory": "true", "migration.incremental.memory.iterations": "1000"}, supported: true, expectEnabled: true, expectAttempts: 999, expectProbe: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			probed := false
			enabled, attempts := migrationPreDumpSettings(tt.config, func() error {
				probed = true
				if !tt.supported {
					return errors.New("unsupported")
				}

				return nil
			})

			require.Equal(t, tt.expectEnabled, enabled)
			require.Equal(t, tt.expectAttempts, attempts)
			require.Equal(t, tt.expectProbe, probed)
		})
	}
}

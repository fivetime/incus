package migration

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// TestMigrationHeaderFieldNumbers pins the wire numbers of the fields this
// fork adds to MigrationHeader, and of upstream's blockFeatures and
// multifdChannels, which the fork moved off 15 and 16. Deployed fork servers
// read and write these numbers, so an upstream merge that reuses one must be
// resolved, not taken as is.
func TestMigrationHeaderFieldNumbers(t *testing.T) {
	fields := (&MigrationHeader{}).ProtoReflect().Descriptor().Fields()

	for name, number := range map[protoreflect.Name]protoreflect.FieldNumber{
		"cephFsid":        15,
		"cephPool":        16,
		"sharedStorage":   17,
		"cephDriver":      18,
		"blockFeatures":   19,
		"multifdChannels": 20,
	} {
		field := fields.ByName(name)
		require.NotNil(t, field, name)
		require.Equal(t, number, field.Number(), name)
	}

	control := (&MigrationControl{}).ProtoReflect().Descriptor().Fields()
	require.Equal(t, protoreflect.FieldNumber(3), control.ByName("sharedStorageReleased").Number())
}

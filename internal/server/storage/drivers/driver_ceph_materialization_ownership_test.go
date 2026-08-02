package drivers

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseCephMaterializationOwnership(t *testing.T) {
	marker := `{"version":1,"token":"attempt"}`
	data := `{"another.key":"value","` + cephMaterializationOwnershipKey + `":` + `"{\"version\":1,\"token\":\"attempt\"}"}`

	result, err := parseCephMaterializationOwnership(data)
	require.NoError(t, err)
	require.Equal(t, marker, result)

	result, err = parseCephMaterializationOwnership(`{"another.key":"value"}`)
	require.NoError(t, err)
	require.Empty(t, result)

	// rbd image-meta list prints nothing at all for an image without any
	// metadata keys; that is a pristine volume, not an error.
	for _, empty := range []string{"", "\n", "  \n"} {
		result, err = parseCephMaterializationOwnership(empty)
		require.NoError(t, err)
		require.Empty(t, result)
	}

	_, err = parseCephMaterializationOwnership(`not-json`)
	require.Error(t, err)
}

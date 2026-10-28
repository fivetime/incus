package instance

import (
	"testing"
)

func TestOpenStackMaterializationProvenanceIsNotCopied(t *testing.T) {
	for _, key := range []string{
		ConfigOpenStackIDMapAllocationID,
		ConfigOpenStackComputeID,
		ConfigOpenStackRootfsMaterializationID,
		"user.openstack.uuid",
	} {
		if InstanceIncludeWhenCopying(key, false, false) || InstanceIncludeWhenCopying(key, true, false) {
			t.Fatalf("Materialization provenance key %q can be copied", key)
		}
	}
}

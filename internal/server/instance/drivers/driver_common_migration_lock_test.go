//go:build linux && cgo && !agent

package drivers

import (
	"slices"
	"testing"

	"github.com/lxc/incus/v7/internal/server/instance/operationlock"
	"github.com/lxc/incus/v7/internal/server/operations"
	"github.com/lxc/incus/v7/shared/api"
)

func TestStopMigrationOwner(t *testing.T) {
	owner := &operations.Operation{}
	op, err := operationlock.Create("default", t.Name(), nil, operationlock.ActionCreate, false, false)
	if err != nil {
		t.Fatal(err)
	}

	defer op.Done(nil)
	err = op.StartMigration(owner)
	if err != nil {
		t.Fatal(err)
	}

	for _, caller := range []*operations.Operation{owner, {}, nil} {
		d := common{name: t.Name(), project: api.Project{Name: "default"}, op: caller}
		allowed := slices.Contains(d.stopInheritableActions(), operationlock.ActionMigrate)
		if allowed != (caller == owner) {
			t.Fatalf("Unexpected stop inheritance for caller %p", caller)
		}
	}
}

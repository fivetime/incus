package operationlock

import (
	"context"
	"testing"

	"github.com/lxc/incus/v7/internal/server/operations"
)

func TestReceiveMigrationLock(t *testing.T) {
	apiOp := &operations.Operation{}
	op, err := Create("default", t.Name(), nil, ActionCreate, false, false)
	if err != nil {
		t.Fatal(err)
	}

	defer op.Done(nil)
	err = op.StartMigration(apiOp)
	if err != nil {
		t.Fatal(err)
	}

	if op.Action() != ActionMigrate || op.GetOperation() != apiOp {
		t.Fatal("Receive did not retain its lock with the migration owner")
	}

	_, err = CreateWaitGet("default", t.Name(), &operations.Operation{}, ActionStop, nil, false, true)
	if err == nil {
		t.Fatal("Unrelated stop inherited migration lock")
	}

	inherited, err := CreateWaitGet("default", t.Name(), apiOp, ActionStop, []Action{ActionMigrate}, false, true)
	if err != nil || inherited != op {
		t.Fatalf("Rollback did not inherit its lock: %v", err)
	}

	op.Done(nil)
	err = op.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	replacement, err := Create("default", t.Name(), nil, ActionCreate, false, false)
	if err != nil {
		t.Fatal(err)
	}

	defer replacement.Done(nil)
	err = op.StartMigration(apiOp)
	if err == nil {
		t.Fatal("Stale owner rebound the replacement lock")
	}

	if Get("default", t.Name()) != replacement || replacement.Action() != ActionCreate {
		t.Fatal("Stale owner modified replacement")
	}
}

func TestReceiveMigrationLockRequiresOwner(t *testing.T) {
	var missing *InstanceOperation
	err := missing.StartMigration(&operations.Operation{})
	if err == nil {
		t.Fatal("Missing lock accepted")
	}

	op, err := Create("default", t.Name(), nil, ActionCreate, false, false)
	if err != nil {
		t.Fatal(err)
	}

	defer op.Done(nil)
	err = op.StartMigration(nil)
	if err == nil || op.Action() != ActionCreate {
		t.Fatal("Missing API operation accepted")
	}
}

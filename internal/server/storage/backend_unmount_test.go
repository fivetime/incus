//go:build linux && cgo && !agent

package storage

import (
	"errors"
	"fmt"
	"testing"

	"github.com/lxc/incus/v7/internal/server/instance"
	"github.com/lxc/incus/v7/internal/server/instance/instancetype"
	"github.com/lxc/incus/v7/internal/server/operations"
	"github.com/lxc/incus/v7/internal/server/storage/drivers"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/logger"
)

type unmountTestInstance struct {
	instance.Instance
}

func (i unmountTestInstance) ID() int                 { return -1 }
func (i unmountTestInstance) Name() string            { return "handover" }
func (i unmountTestInstance) Type() instancetype.Type { return instancetype.Container }
func (i unmountTestInstance) Project() api.Project    { return api.Project{Name: "default"} }

type unmountTestDriver struct {
	drivers.Driver
	err   error
	calls int
}

func (d *unmountTestDriver) UnmountVolume(drivers.Volume, bool, *operations.Operation) (bool, error) {
	d.calls++
	return false, d.err
}

func TestUnmountInstanceOwnershipRelease(t *testing.T) {
	ioErr := errors.New("unmount I/O failure")
	for _, test := range []struct {
		name        string
		err         error
		ordinaryErr error
	}{
		{name: "released"},
		{name: "still referenced", err: drivers.ErrInUse},
		{name: "wrapped reference", err: fmt.Errorf("root: %w", drivers.ErrInUse)},
		{name: "I/O failure", err: ioErr, ordinaryErr: ioErr},
	} {
		t.Run(test.name, func(t *testing.T) {
			driver := &unmountTestDriver{err: test.err}
			pool := &backend{driver: driver, name: "test", logger: logger.AddContext(nil)}
			inst := unmountTestInstance{}
			err := pool.UnmountInstance(inst, nil)
			if !errors.Is(err, test.ordinaryErr) {
				t.Fatalf("Ordinary unmount returned %v, expected %v", err, test.ordinaryErr)
			}

			err = pool.UnmountInstanceStrict(inst, nil)
			if !errors.Is(err, test.err) {
				t.Fatalf("Ownership release returned %v, expected %v", err, test.err)
			}

			if driver.calls != 2 {
				t.Fatalf("Expected two unmount calls, got %d", driver.calls)
			}
		})
	}
}

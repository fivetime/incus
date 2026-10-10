package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus/v7/internal/server/db/operationtype"
	"github.com/lxc/incus/v7/internal/server/instance"
	"github.com/lxc/incus/v7/internal/server/operations"
	"github.com/lxc/incus/v7/shared/api"
)

type cancelMigrationInstance struct {
	instance.Instance
	send func(instance.MigrateSendArgs) error
}

func (i *cancelMigrationInstance) Name() string {
	return "cancel-source"
}

func (i *cancelMigrationInstance) Project() api.Project {
	return api.Project{Name: "default"}
}

func (i *cancelMigrationInstance) SetOperation(_ *operations.Operation) {}

func (i *cancelMigrationInstance) MigrateSend(args instance.MigrateSendArgs) error {
	return i.send(args)
}

func cancelMigrationSource(t *testing.T, send func(instance.MigrateSendArgs) error) *migrationSourceWs {
	t.Helper()
	source, err := newMigrationSource(&cancelMigrationInstance{send: send}, false, false, false, "", "", nil, nil, nil)
	require.NoError(t, err)
	return source
}

func migrationControlConnection(t *testing.T) *websocket.Conn {
	t.Helper()
	accepted := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
		}

		accepted <- conn
	}))
	t.Cleanup(server.Close)
	peer, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = peer.Close() })
	conn := <-accepted
	require.NotNil(t, conn)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func cancelMigrationOperation(t *testing.T, source *migrationSourceWs, run func(*operations.Operation) error) *operations.Operation {
	t.Helper()
	op, err := operations.OperationCreate(nil, "default", operations.OperationClassWebsocket, operationtype.InstanceMigrate, nil, source.Metadata(), run, source.cancelInstance, source.Connect, nil)
	require.NoError(t, err)
	require.NoError(t, op.Start())
	t.Cleanup(func() {
		synctest.Wait()
		time.Sleep(5 * time.Second)
		synctest.Wait()
	})
	return op
}

func requireMigrationCancelling(t *testing.T, op *operations.Operation, cancelled <-chan error) {
	t.Helper()
	synctest.Wait()
	_, rendered, err := op.Render()
	require.NoError(t, err)
	require.Equal(t, api.Cancelling, rendered.StatusCode)
	select {
	case err := <-cancelled:
		t.Fatalf("Cancellation returned before source execution finished: %v", err)
	default:
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, op.Wait(ctx), context.Canceled)
}

func TestInstanceMigrationCancelBeforeRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source := cancelMigrationSource(t, func(instance.MigrateSendArgs) error {
			t.Error("Migration must not start after cancellation before the control connection")
			return nil
		})
		start := make(chan struct{})
		startRun := sync.OnceFunc(func() { close(start) })
		defer startRun()
		op := cancelMigrationOperation(t, source, func(op *operations.Operation) error {
			<-start
			return source.do(op)
		})
		cancelled, err := op.Cancel()
		require.NoError(t, err)
		requireMigrationCancelling(t, op, cancelled)
		startRun()
		require.NoError(t, <-cancelled)
		<-source.instanceRunDone
	})
}

func TestInstanceMigrationCancelWaitsForRollback(t *testing.T) {
	control := migrationControlConnection(t)

	synctest.Test(t, func(t *testing.T) {
		var run func(instance.MigrateSendArgs) error
		source := cancelMigrationSource(t, func(args instance.MigrateSendArgs) error { return run(args) })
		source.conns[api.SecretNameControl].conn = control
		started := make(chan struct{})
		restore := make(chan struct{})
		finishRestore := sync.OnceFunc(func() { close(restore) })
		defer finishRestore()
		run = func(args instance.MigrateSendArgs) error {
			close(started)
			<-source.conns[api.SecretNameControl].closed
			args.Disconnect()
			<-restore
			return errors.New("Destination restore failed after source checkpoint")
		}

		op := cancelMigrationOperation(t, source, source.do)
		<-started
		cancelled, err := op.Cancel()
		require.NoError(t, err)
		requireMigrationCancelling(t, op, cancelled)
		_, err = op.Cancel()
		require.ErrorContains(t, err, "Only running operations")

		// Repeated transport cancellation must neither return early nor close a channel twice.
		repeated := make(chan error, 1)
		go func() { repeated <- source.cancelInstance(nil) }()
		synctest.Wait()
		select {
		case err := <-repeated:
			t.Fatalf("Repeated cancellation returned during source restore: %v", err)
		default:
		}

		finishRestore()
		require.NoError(t, <-cancelled)
		require.NoError(t, <-repeated)
		<-source.instanceRunDone
	})
}

func TestInstanceMigrationCancelPendingConnection(t *testing.T) {
	for _, name := range []string{api.SecretNameControl, api.SecretNameFilesystem, api.SecretNameState} {
		t.Run(name, func(t *testing.T) {
			var control *websocket.Conn
			if name != api.SecretNameControl {
				control = migrationControlConnection(t)
			}

			synctest.Test(t, func(t *testing.T) {
				var run func(instance.MigrateSendArgs) error
				source := cancelMigrationSource(t, func(args instance.MigrateSendArgs) error { return run(args) })
				source.conns[api.SecretNameControl].conn = control
				if name == api.SecretNameState {
					source.conns[name] = newMigrationConn("state-secret", nil, nil)
				}

				run = func(args instance.MigrateSendArgs) error {
					var err error
					if name == api.SecretNameState {
						_, err = args.StateConn(context.Background())
					} else {
						_, err = args.FilesystemConn(context.Background())
					}

					return err
				}

				op := cancelMigrationOperation(t, source, source.do)
				synctest.Wait()
				cancelled, err := op.Cancel()
				require.NoError(t, err)
				require.NoError(t, <-cancelled)
				<-source.instanceRunDone
			})
		})
	}
}

func TestInstanceMigrationDisconnectDoesNotWaitForItself(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "failure"
		}

		t.Run(name, func(t *testing.T) {
			control := migrationControlConnection(t)
			synctest.Test(t, func(t *testing.T) {
				source := cancelMigrationSource(t, func(args instance.MigrateSendArgs) error {
					args.Disconnect()
					if fail {
						return errors.New("Destination rejected migration")
					}

					return nil
				})
				source.conns[api.SecretNameControl].conn = control
				op := cancelMigrationOperation(t, source, source.do)
				err := op.Wait(context.Background())
				if fail {
					require.ErrorContains(t, err, "Destination rejected migration")
				} else {
					require.NoError(t, err)
				}

				require.NoError(t, source.cancelInstance(nil))
				_, err = op.Cancel()
				require.ErrorContains(t, err, "Only running operations")
			})
		})
	}
}

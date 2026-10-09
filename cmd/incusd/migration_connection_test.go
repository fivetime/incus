package main

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
)

func TestMigrationConnCloseWakesWaiters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		conn := newMigrationConn("secret", nil, nil)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		result := make(chan error, 2)
		for range 2 {
			go func() {
				_, err := conn.WebSocket(ctx)
				result <- err
			}()
		}

		synctest.Wait()
		conn.Close()
		conn.Close()
		synctest.Wait()
		for range 2 {
			select {
			case err := <-result:
				require.ErrorContains(t, err, "already disconnected")
			default:
				t.Fatal("Close did not wake a pending connection waiter")
			}
		}

		ws, err := conn.WebSocket(context.Background())
		require.Nil(t, ws)
		require.ErrorContains(t, err, "already disconnected")
	})
}

func TestMigrationConnCloseAfterConnectionReady(t *testing.T) {
	control := migrationControlConnection(t)
	synctest.Test(t, func(t *testing.T) {
		conn := newMigrationConn("secret", nil, nil)
		completed := make(chan struct{}, 2)
		for range 2 {
			go func() {
				ws, err := conn.WebSocket(context.Background())
				if err == nil {
					if ws == nil {
						t.Error("A ready connection racing Close must never return nil, nil")
					}
				} else if !strings.Contains(err.Error(), "already disconnected") {
					t.Errorf("Unexpected connection error: %v", err)
				}

				completed <- struct{}{}
			}()
		}

		synctest.Wait()
		conn.mu.Lock()
		conn.conn = control
		close(conn.connected)
		conn.mu.Unlock()
		conn.Close()
		for range 2 {
			<-completed
		}
	})
}

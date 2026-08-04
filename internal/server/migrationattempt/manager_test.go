//go:build linux && cgo && !agent

package migrationattempt

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus/v7/internal/server/db"
)

func newTestManager(t *testing.T) (*Manager, func()) {
	t.Helper()

	node, cleanup := db.NewTestNode(t)
	return New(node), cleanup
}

func TestMigrationAttemptCommitWinsAbort(t *testing.T) {
	manager, cleanup := newTestManager(t)
	defer cleanup()

	ctx := context.Background()
	const token = "06ada2e7-67f9-4c4e-b071-da45f25cfc67"

	_, err := manager.Register(ctx, token, "nova", ResourceTypeInstance, "instance-1", -1, 0, 1)
	require.NoError(t, err)

	_, err = manager.Begin(ctx, token, "nova", ResourceTypeInstance, "instance-1", 1)
	require.NoError(t, err)
	require.NoError(t, manager.BindOperation(ctx, token, "operation-1"))
	require.NoError(t, manager.Commit(ctx, token))

	_, err = manager.Abort(ctx, token)
	require.ErrorIs(t, err, ErrCommitted)

	attempt, err := manager.Get(ctx, token)
	require.NoError(t, err)
	require.Equal(t, StateCommitted, attempt.State)
	require.True(t, attempt.Finished)
}

func TestMigrationAttemptRegistrationIsIdempotentAndBound(t *testing.T) {
	manager, cleanup := newTestManager(t)
	defer cleanup()

	ctx := context.Background()
	const token = "95cd7097-b51d-4db1-b5ba-e12fd3af4b93"

	first, err := manager.Register(ctx, token, "nova", ResourceTypeInstance, "instance-bound", 900000, 65536, 1)
	require.NoError(t, err)
	second, err := manager.Register(ctx, token, "nova", ResourceTypeInstance, "instance-bound", 900000, 65536, 1)
	require.NoError(t, err)
	require.Equal(t, first, second)

	_, err = manager.Register(ctx, token, "other", ResourceTypeInstance, "instance-bound", 900000, 65536, 1)
	require.ErrorIs(t, err, ErrBindingMismatch)
	_, err = manager.Register(ctx, token, "nova", ResourceTypeInstance, "different", 900000, 65536, 1)
	require.ErrorIs(t, err, ErrBindingMismatch)
	_, err = manager.Register(ctx, token, "nova", ResourceTypeInstance, "instance-bound", 900001, 65536, 1)
	require.ErrorIs(t, err, ErrBindingMismatch)
}

func TestMigrationAttemptAbortFencesLateCreate(t *testing.T) {
	manager, cleanup := newTestManager(t)
	defer cleanup()

	ctx := context.Background()
	const token = "9716f699-cd3b-4306-88af-b36e60951acd"

	_, err := manager.Register(ctx, token, "nova", ResourceTypeInstance, "instance-2", 1000000, 65536, 1)
	require.NoError(t, err)

	attempt, err := manager.Abort(ctx, token)
	require.NoError(t, err)
	require.Equal(t, StateAborted, attempt.State)
	require.True(t, attempt.Finished)

	_, err = manager.Begin(ctx, token, "nova", ResourceTypeInstance, "instance-2", 1)
	require.ErrorIs(t, err, ErrAborted)

	_, err = manager.Register(ctx, token, "nova", ResourceTypeInstance, "instance-2", 1000000, 65536, 1)
	require.ErrorIs(t, err, ErrAborted)
}

func TestMigrationAttemptAbortDuringReceive(t *testing.T) {
	manager, cleanup := newTestManager(t)
	defer cleanup()

	ctx := context.Background()
	const token = "ad92f629-3f1d-453a-9c55-693f1430be40"

	_, err := manager.Register(ctx, token, "nova", ResourceTypeInstance, "instance-3", 1100000, 65536, 1)
	require.NoError(t, err)
	_, err = manager.Begin(ctx, token, "nova", ResourceTypeInstance, "instance-3", 1)
	require.NoError(t, err)
	require.NoError(t, manager.BindOperation(ctx, token, "operation-3"))

	attempt, err := manager.Abort(ctx, token)
	require.NoError(t, err)
	require.Equal(t, StateAborted, attempt.State)
	require.False(t, attempt.Finished)
	require.ErrorIs(t, manager.CheckActive(ctx, token), ErrAborted)
	require.ErrorIs(t, manager.Commit(ctx, token), ErrAborted)

	require.NoError(t, manager.FinishFailure(ctx, token))
	attempt, err = manager.Get(ctx, token)
	require.NoError(t, err)
	require.Equal(t, StateAborted, attempt.State)
	require.True(t, attempt.Finished)
	require.Empty(t, attempt.OperationUUID)
}

func TestMigrationAttemptCommitAbortRaceHasSingleWinner(t *testing.T) {
	manager, cleanup := newTestManager(t)
	defer cleanup()

	ctx := context.Background()
	for i := range 25 {
		token := fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		name := fmt.Sprintf("instance-race-%d", i)

		_, err := manager.Register(ctx, token, "nova", ResourceTypeInstance, name, -1, 0, 1)
		require.NoError(t, err)
		_, err = manager.Begin(ctx, token, "nova", ResourceTypeInstance, name, 1)
		require.NoError(t, err)

		start := make(chan struct{})
		results := make(chan error, 2)
		go func() {
			<-start
			results <- manager.Commit(ctx, token)
		}()

		go func() {
			<-start
			_, err := manager.Abort(ctx, token)
			results <- err
		}()

		close(start)
		first := <-results
		second := <-results

		successes := 0
		for _, result := range []error{first, second} {
			if result == nil {
				successes++
				continue
			}

			if !errors.Is(result, ErrCommitted) && !errors.Is(result, ErrAborted) {
				t.Fatalf("Unexpected race result for iteration %d: %v", i, result)
			}
		}

		require.Equal(t, 1, successes)

		attempt, err := manager.Get(ctx, token)
		require.NoError(t, err)
		require.Contains(t, []string{StateCommitted, StateAborted}, attempt.State)
	}
}

func TestMigrationAttemptRetiredTokenCannotBeReused(t *testing.T) {
	manager, cleanup := newTestManager(t)
	defer cleanup()

	ctx := context.Background()
	const token = "624ea9ea-8702-4486-b13a-94ea71c8be50"

	_, err := manager.Register(ctx, token, "nova", ResourceTypeInstance, "instance-4", -1, 0, 1)
	require.NoError(t, err)
	_, err = manager.Abort(ctx, token)
	require.NoError(t, err)
	require.NoError(t, manager.Delete(ctx, token))

	attempt, err := manager.Get(ctx, token)
	require.NoError(t, err)
	require.Equal(t, StateRetired, attempt.State)
	require.Empty(t, attempt.Project)
	require.Empty(t, attempt.ResourceType)
	require.Empty(t, attempt.ResourceName)
	require.Empty(t, attempt.OperationUUID)
	require.Equal(t, int64(-1), attempt.IDMapBase)
	require.Zero(t, attempt.IDMapSize)
	require.Zero(t, attempt.DaemonStart)

	_, err = manager.Register(ctx, token, "nova", ResourceTypeInstance, "instance-4", -1, 0, 1)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = manager.Register(ctx, token, "other", ResourceTypeInstance, "different", 3000000, 65536, 1)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = manager.Begin(ctx, token, "nova", ResourceTypeInstance, "instance-4", 1)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestMigrationAttemptOperationBindingCannotBeReassigned(t *testing.T) {
	manager, cleanup := newTestManager(t)
	defer cleanup()

	ctx := context.Background()
	const token = "62277e4a-789e-4e55-a529-34ef3b7cb87e"

	_, err := manager.Register(ctx, token, "nova", ResourceTypeInstance, "instance-operation", -1, 0, 1)
	require.NoError(t, err)
	_, err = manager.Begin(ctx, token, "nova", ResourceTypeInstance, "instance-operation", 1)
	require.NoError(t, err)

	require.NoError(t, manager.BindOperation(ctx, token, "operation-one"))
	require.NoError(t, manager.BindOperation(ctx, token, "operation-one"))
	require.ErrorIs(t, manager.BindOperation(ctx, token, "operation-two"), ErrOperationMismatch)

	attempt, err := manager.Get(ctx, token)
	require.NoError(t, err)
	require.Equal(t, "operation-one", attempt.OperationUUID)
}

func TestMigrationAttemptConcurrentOperationBindingHasSingleWinner(t *testing.T) {
	manager, cleanup := newTestManager(t)
	defer cleanup()

	ctx := context.Background()
	const token = "226208c7-b3af-4c2c-9acc-5d3141fe15a3"

	_, err := manager.Register(ctx, token, "nova", ResourceTypeInstance, "instance-operation-race", -1, 0, 1)
	require.NoError(t, err)
	_, err = manager.Begin(ctx, token, "nova", ResourceTypeInstance, "instance-operation-race", 1)
	require.NoError(t, err)

	start := make(chan struct{})
	results := make(chan error, 2)
	for _, operationUUID := range []string{"operation-one", "operation-two"} {
		go func() {
			<-start
			results <- manager.BindOperation(ctx, token, operationUUID)
		}()
	}

	close(start)
	first := <-results
	second := <-results

	successes := 0
	mismatches := 0
	for _, result := range []error{first, second} {
		switch {
		case result == nil:
			successes++
		case errors.Is(result, ErrOperationMismatch):
			mismatches++
		default:
			t.Fatalf("Unexpected operation binding result: %v", result)
		}
	}

	require.Equal(t, 1, successes)
	require.Equal(t, 1, mismatches)

	attempt, err := manager.Get(ctx, token)
	require.NoError(t, err)
	require.Contains(t, []string{"operation-one", "operation-two"}, attempt.OperationUUID)
}

func TestMigrationAttemptAbortedIDMapRemainsReservedUntilCleanup(t *testing.T) {
	manager, cleanup := newTestManager(t)
	defer cleanup()

	ctx := context.Background()
	const token = "f3d6614e-e39d-43ce-abf0-684bd5119cb7"
	const replacementToken = "af48a050-4c8a-42ff-b5fb-9e628859cc47"

	_, err := manager.Register(ctx, token, "nova", ResourceTypeInstance, "instance-aborted", 3000000, 65536, 1)
	require.NoError(t, err)
	_, err = manager.Begin(ctx, token, "nova", ResourceTypeInstance, "instance-aborted", 1)
	require.NoError(t, err)
	_, err = manager.Abort(ctx, token)
	require.NoError(t, err)

	_, err = manager.Register(ctx, replacementToken, "nova", ResourceTypeInstance, "instance-replacement", 3032768, 65536, 1)
	require.ErrorIs(t, err, ErrIDMapOverlap)

	require.NoError(t, manager.FinishFailure(ctx, token))
	_, err = manager.Register(ctx, replacementToken, "nova", ResourceTypeInstance, "instance-replacement", 3032768, 65536, 1)
	require.NoError(t, err)
}

func TestMigrationAttemptStartedIDMapReservationDiesWithItsDaemon(t *testing.T) {
	manager, cleanup := newTestManager(t)
	defer cleanup()

	ctx := context.Background()
	const token = "4c6e1f1c-2f5a-45c6-8bb2-3f04c9bd4f18"
	const replacementToken = "7d7f7a2c-2d1b-4c9e-9d2c-05a3f1c6c9de"
	const firstGeneration = int64(1)
	const secondGeneration = int64(2)

	_, err := manager.Register(ctx, token, "nova", ResourceTypeInstance, "instance-stranded", 4000000, 65536, firstGeneration)
	require.NoError(t, err)
	_, err = manager.Begin(ctx, token, "nova", ResourceTypeInstance, "instance-stranded", firstGeneration)
	require.NoError(t, err)

	// The daemon that accepted the create still owns the range.
	_, err = manager.Register(ctx, replacementToken, "nova", ResourceTypeInstance, "instance-next", 4000000, 65536, firstGeneration)
	require.ErrorIs(t, err, ErrIDMapOverlap)

	reservations, err := manager.IDMapReservations(ctx, firstGeneration)
	require.NoError(t, err)
	require.Len(t, reservations, 1)

	// After a restart the operation that held the range is gone, so the
	// reservation must stop fencing new attempts even though the record
	// itself is still unfinished and awaiting reconciliation.
	reservations, err = manager.IDMapReservations(ctx, secondGeneration)
	require.NoError(t, err)
	require.Empty(t, reservations)

	_, err = manager.Register(ctx, replacementToken, "nova", ResourceTypeInstance, "instance-next", 4000000, 65536, secondGeneration)
	require.NoError(t, err)

	stranded, err := manager.Get(ctx, token)
	require.NoError(t, err)
	require.False(t, stranded.Finished)
	require.Equal(t, StateActive, stranded.State)
}

func TestMigrationAttemptUnstartedIDMapReservationSurvivesRestart(t *testing.T) {
	manager, cleanup := newTestManager(t)
	defer cleanup()

	ctx := context.Background()
	const token = "b0f3d0d3-8f5e-4d18-9a54-7f8a2b6a0c31"
	const replacementToken = "e2f4c6a8-1b3d-4e5f-8a7c-9d0b1e2f3a4b"

	// A registration whose create request has not arrived yet is still
	// claimable by that request after a restart, so its range stays fenced.
	_, err := manager.Register(ctx, token, "nova", ResourceTypeInstance, "instance-pending", 4100000, 65536, 1)
	require.NoError(t, err)

	reservations, err := manager.IDMapReservations(ctx, 2)
	require.NoError(t, err)
	require.Len(t, reservations, 1)

	_, err = manager.Register(ctx, replacementToken, "nova", ResourceTypeInstance, "instance-other", 4100000, 65536, 2)
	require.ErrorIs(t, err, ErrIDMapOverlap)

	_, err = manager.Begin(ctx, token, "nova", ResourceTypeInstance, "instance-pending", 2)
	require.NoError(t, err)
}

func TestMigrationAttemptListPendingExcludesRetiredTokens(t *testing.T) {
	manager, cleanup := newTestManager(t)
	defer cleanup()

	ctx := context.Background()
	const liveToken = "3a1c0f2e-5b4d-4c6a-8e9f-0a1b2c3d4e5f"
	const retiredToken = "9f8e7d6c-5b4a-4392-8271-6a5b4c3d2e1f"

	_, err := manager.Register(ctx, liveToken, "nova", ResourceTypeInstance, "instance-live", 4200000, 65536, 1)
	require.NoError(t, err)
	_, err = manager.Register(ctx, retiredToken, "nova", ResourceTypeInstance, "instance-retired", -1, 0, 1)
	require.NoError(t, err)
	_, err = manager.Begin(ctx, retiredToken, "nova", ResourceTypeInstance, "instance-retired", 1)
	require.NoError(t, err)
	require.NoError(t, manager.BindOperation(ctx, retiredToken, "operation-retired"))
	require.NoError(t, manager.Commit(ctx, retiredToken))

	pending, err := manager.ListPending(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 2)

	require.NoError(t, manager.Delete(ctx, retiredToken))

	pending, err = manager.ListPending(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, liveToken, pending[0].Token)
}

func TestMigrationAttemptIDMapReservationsAreAtomic(t *testing.T) {
	manager, cleanup := newTestManager(t)
	defer cleanup()

	ctx := context.Background()
	tokens := []string{
		"00045445-7cf3-423f-b892-721b7e1b09aa",
		"1a5060d8-01ce-450c-8987-725806d625fd",
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(tokens))
	for i, token := range tokens {
		wg.Add(1)
		go func(index int, attemptToken string) {
			defer wg.Done()
			_, err := manager.Register(ctx, attemptToken, "nova", ResourceTypeInstance, "instance-"+string(rune('a'+index)), 2000000, 65536, 1)
			errs <- err
		}(i, token)
	}

	wg.Wait()
	close(errs)

	successes := 0
	overlaps := 0
	for err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrIDMapOverlap):
			overlaps++
		default:
			t.Fatalf("Unexpected registration error: %v", err)
		}
	}

	require.Equal(t, 1, successes)
	require.Equal(t, 1, overlaps)
}

func TestMigrationAttemptMissingTokenFailsClosed(t *testing.T) {
	manager, cleanup := newTestManager(t)
	defer cleanup()

	_, err := manager.Begin(context.Background(), "e7da9404-1549-4b4a-8f47-b47650a4508c", "nova", ResourceTypeInstance, "instance-5", 1)
	require.ErrorIs(t, err, ErrNotFound)
}

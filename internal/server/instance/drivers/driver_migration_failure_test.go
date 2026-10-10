package drivers

import (
	"context"
	"errors"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/sync/errgroup"
)

func TestMigrationFailureReachesTargetBeforeDataDisconnect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		primary := errors.New("source checkpoint rejected")
		control := make(chan error, 1)
		targetDone := make(chan struct{})
		allowReply := make(chan struct{})
		targetDataDisconnected := make(chan struct{})
		sourceDataDisconnected := make(chan struct{})
		targetResult := make(chan error, 1)
		sourceResult := make(chan error, 1)

		target, targetCtx := errgroup.WithContext(context.Background())
		target.Go(func() error { return <-control })
		target.Go(func() error {
			<-targetDataDisconnected
			return io.ErrUnexpectedEOF
		})
		go func() {
			<-targetCtx.Done()
			close(targetDataDisconnected)
		}()
		go func() {
			targetResult <- target.Wait()
			<-allowReply
			close(targetDone)
		}()
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			err := notifyMigrationFailure(ctx, primary, func(err error) error {
				control <- err
				return nil
			}, targetDone)
			close(sourceDataDisconnected)
			sourceResult <- err
		}()

		synctest.Wait()
		err := <-targetResult
		if !errors.Is(err, primary) {
			t.Fatalf("Target first error = %v, want source checkpoint failure", err)
		}

		select {
		case <-sourceDataDisconnected:
			t.Fatal("Source closed data connections before the target response")
		default:
		}

		close(allowReply)
		err = <-sourceResult
		if !errors.Is(err, primary) {
			t.Fatalf("Source error = %v, want original checkpoint failure", err)
		}
	})
}

func TestMigrationFailureSendFailurePreservesCause(t *testing.T) {
	primary := errors.New("checkpoint failed")
	sendErr := errors.New("control connection closed")
	err := notifyMigrationFailure(context.Background(), primary, func(error) error {
		return sendErr
	}, make(chan struct{}))
	if !errors.Is(err, primary) || !errors.Is(err, sendErr) {
		t.Fatalf("Failure = %v, want original and notification errors", err)
	}
}

func TestMigrationFailureWaitBoundaries(t *testing.T) {
	for _, result := range []string{"response", "disconnect", "cancel", "timeout"} {
		t.Run(result, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				primary := errors.New("checkpoint failed")
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				targetDone := make(chan struct{})
				notified := make(chan struct{})
				returned := make(chan error, 1)
				go func() {
					returned <- notifyMigrationFailure(ctx, primary, func(error) error {
						close(notified)
						return nil
					}, targetDone)
				}()
				<-notified
				synctest.Wait()
				select {
				case err := <-returned:
					t.Fatalf("Failure acknowledgement returned early: %v", err)
				default:
				}

				switch result {
				case "response", "disconnect":
					close(targetDone)
				case "cancel":
					cancel()
				case "timeout":
					time.Sleep(time.Minute)
				}

				err := <-returned
				if !errors.Is(err, primary) {
					t.Fatalf("Failure = %v, want original checkpoint error", err)
				}

				if result == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("Failure = %v, want acknowledgement deadline", err)
				}

				if result == "cancel" && !errors.Is(err, context.Canceled) {
					t.Fatalf("Failure = %v, want acknowledgement cancellation", err)
				}
			})
		})
	}
}

func TestMigrationFailureObservedResponsePrecedesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	primary := errors.New("checkpoint failed")
	targetDone := make(chan struct{})
	err := notifyMigrationFailure(ctx, primary, func(error) error {
		close(targetDone)
		cancel()
		return nil
	}, targetDone)
	if err != primary {
		t.Fatalf("Failure = %v, want the observed original error without cancellation", err)
	}
}

func TestMigrationFailureNoNotificationAfterCancellationOrSuccess(t *testing.T) {
	primary := errors.New("checkpoint failed")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, cause := range []error{nil, primary} {
		err := notifyMigrationFailure(ctx, cause, func(error) error {
			t.Fatal("A cancelled transfer sent a second failure notification")
			return nil
		}, make(chan struct{}))
		if err != cause {
			t.Fatalf("Failure = %v, want original cause %v", err, cause)
		}
	}
}

func TestMigrationFailureResultPreservesTargetCleanupError(t *testing.T) {
	primary := errors.New("checkpoint failed")
	cleanup := errors.New("target cleanup failed")
	err := migrationFailureResult(primary, cleanup)
	if !errors.Is(err, primary) || !errors.Is(err, cleanup) {
		t.Fatalf("Failure = %v, want source and target cleanup causes", err)
	}

	if migrationFailureResult(primary, primary) != primary {
		t.Fatal("Repeated primary cause was duplicated")
	}

	echo := errors.New("Error from migration control target: Error from migration control source: " + primary.Error())
	if migrationFailureResult(primary, echo) != primary {
		t.Fatal("The target's unchanged echo of the source error was duplicated")
	}

	if migrationFailureResult(nil, cleanup) != cleanup {
		t.Fatal("Target failure was changed without a local failure")
	}
}

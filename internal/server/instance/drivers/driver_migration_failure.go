package drivers

import (
	"context"
	"errors"
	"fmt"
)

// notifyMigrationFailure waits for a peer response or disconnect before closing data connections.
func notifyMigrationFailure(ctx context.Context, primary error, send func(error) error, targetDone <-chan struct{}) error {
	if primary == nil || ctx.Err() != nil {
		return primary
	}

	err := send(primary)
	if err != nil {
		return errors.Join(primary, fmt.Errorf("Failed notifying migration target: %w", err))
	}

	select {
	case <-targetDone:
		return primary
	case <-ctx.Done():
		// A control result can cancel the group after publishing the peer's response.
		select {
		case <-targetDone:
			return primary
		default:
			return errors.Join(primary, fmt.Errorf("Waiting for migration target failure response: %w", ctx.Err()))
		}
	}
}

// migrationFailureResult preserves the local cause and any independent target cleanup failure.
func migrationFailureResult(primary error, target error) error {
	if primary == nil || errors.Is(target, primary) {
		return target
	}

	if target != nil && target.Error() == "Error from migration control target: Error from migration control source: "+primary.Error() {
		return primary
	}

	return errors.Join(primary, target)
}

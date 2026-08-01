package instance

import (
	"errors"
)

// ErrNotImplemented is the "Not implemented" error.
var ErrNotImplemented = errors.New("Not implemented")

// ErrMigrationTargetCleanupIncomplete indicates that a failed migration target
// still has resources that could not be proven to be released.
var ErrMigrationTargetCleanupIncomplete = errors.New("Migration target cleanup is incomplete")

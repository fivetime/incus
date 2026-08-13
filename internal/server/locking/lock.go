package locking

import (
	"context"
	"fmt"
	"sync"
)

// locks is a hashmap that allows functions to check whether the operation they are about to perform
// is already in progress. If it is the channel can be used to wait for the operation to finish. If it is not, the
// function that wants to perform the operation should store its code in the hashmap.
// Note that any access to this map must be done while holding a lock.
var locks = map[string]chan struct{}{}

// locksMutex is used to access locks safely.
var locksMutex sync.Mutex

// UnlockFunc unlocks the lock.
type UnlockFunc func()

// Lock creates a named lock to allow activities that require exclusive access to occur.
// Will block until the lock is established or the context is cancelled.
// On successfully acquiring the lock, it returns an unlock function which needs to be called to unlock the lock.
// If the context is canceled then nil will be returned.
func Lock(ctx context.Context, lockName string) (UnlockFunc, error) {
	for {
		unlock, waitCh := TryLock(lockName)
		if unlock != nil {
			return unlock, nil
		}

		select {
		case <-waitCh:
			continue
		case <-ctx.Done():
			return nil, fmt.Errorf("Failed to obtain lock %q: %w", lockName, ctx.Err())
		}
	}
}

// rwState tracks the readers and writer of a named read-write lock.
type rwState struct {
	// readers is the number of currently held shared locks.
	readers int

	// writer indicates whether the exclusive lock is currently held.
	writer bool

	// writersWaiting is the number of writers blocked waiting for the lock.
	// While non-zero, new readers wait so that a steady stream of readers
	// cannot starve a writer.
	writersWaiting int

	// waitCh is closed and replaced whenever the state changes, waking all waiters.
	waitCh chan struct{}
}

var (
	// rwLocks is the map of named read-write lock states.
	// Note that any access to this map must be done while holding rwLocksMutex.
	rwLocks = map[string]*rwState{}

	// rwLocksMutex is used to access rwLocks safely.
	rwLocksMutex sync.Mutex
)

// rwBroadcast wakes all waiters of the state and prepares a fresh wait channel.
// Must be called while holding rwLocksMutex.
func (st *rwState) rwBroadcast() {
	close(st.waitCh)
	st.waitCh = make(chan struct{})
}

// rwRelease removes the named state from the map if it is fully idle and still
// the current entry for the name. Must be called while holding rwLocksMutex.
func rwRelease(lockName string, st *rwState) {
	cur, ok := rwLocks[lockName]
	if ok && cur == st && st.readers == 0 && !st.writer && st.writersWaiting == 0 {
		delete(rwLocks, lockName)
	}
}

// rwGet returns the current state for the name, creating it if missing.
// Must be called while holding rwLocksMutex.
func rwGet(lockName string) *rwState {
	st, ok := rwLocks[lockName]
	if !ok {
		st = &rwState{waitCh: make(chan struct{})}
		rwLocks[lockName] = st
	}

	return st
}

// rwUnlock returns an idempotent unlock function releasing the given hold.
func rwUnlock(lockName string, st *rwState, release func(st *rwState)) UnlockFunc {
	var once sync.Once

	return func() {
		once.Do(func() {
			rwLocksMutex.Lock()
			release(st)
			st.rwBroadcast()
			rwRelease(lockName, st)
			rwLocksMutex.Unlock()
		})
	}
}

// RWLock creates a named lock and acquires it for exclusive (write) access.
// It blocks until no other readers or writer hold the lock or the context is cancelled.
// Writers are prioritized over new readers so that a steady stream of readers cannot starve a writer.
// On successfully acquiring the lock, it returns an unlock function which needs to be called to unlock the lock.
func RWLock(ctx context.Context, lockName string) (UnlockFunc, error) {
	rwLocksMutex.Lock()

	for {
		// Waiters must re-resolve the state by name on each pass, as an idle
		// state may have been dropped from the map and re-created since.
		st := rwGet(lockName)

		if st.readers == 0 && !st.writer {
			st.writer = true
			rwLocksMutex.Unlock()

			return rwUnlock(lockName, st, func(st *rwState) { st.writer = false }), nil
		}

		// Register as a waiting writer so that new readers hold off.
		// The state cannot be dropped from the map while writersWaiting is non-zero.
		st.writersWaiting++
		waitCh := st.waitCh
		rwLocksMutex.Unlock()

		select {
		case <-waitCh:
		case <-ctx.Done():
			rwLocksMutex.Lock()
			st.writersWaiting--
			st.rwBroadcast() // Readers may be waiting on writersWaiting to drop.
			rwRelease(lockName, st)
			rwLocksMutex.Unlock()

			return nil, fmt.Errorf("Failed to obtain lock %q: %w", lockName, ctx.Err())
		}

		rwLocksMutex.Lock()
		st.writersWaiting--
	}
}

// RLock creates a named lock and acquires it for shared (read) access.
// Multiple readers may hold the lock concurrently; it blocks while a writer
// holds or awaits the lock, or until the context is cancelled.
// On successfully acquiring the lock, it returns an unlock function which needs to be called to unlock the lock.
func RLock(ctx context.Context, lockName string) (UnlockFunc, error) {
	rwLocksMutex.Lock()

	for {
		// Waiters must re-resolve the state by name on each pass, as an idle
		// state may have been dropped from the map and re-created since.
		st := rwGet(lockName)

		if !st.writer && st.writersWaiting == 0 {
			st.readers++
			rwLocksMutex.Unlock()

			return rwUnlock(lockName, st, func(st *rwState) { st.readers-- }), nil
		}

		waitCh := st.waitCh
		rwLocksMutex.Unlock()

		select {
		case <-waitCh:
		case <-ctx.Done():
			return nil, fmt.Errorf("Failed to obtain lock %q: %w", lockName, ctx.Err())
		}

		rwLocksMutex.Lock()
	}
}

// TryLock creates a named lock for activities that require exclusive access.
// It does not block if the lock is already held.
// If the lock is acquired successfully, it returns an unlock function that
// must be called to release the lock.
func TryLock(lockName string) (UnlockFunc, chan struct{}) {
	// Get exclusive access to the map and see if there is already an operation ongoing.
	locksMutex.Lock()
	waitCh, ok := locks[lockName]

	if !ok {
		// No ongoing operation, create a new channel to indicate our new operation.
		waitCh = make(chan struct{})
		locks[lockName] = waitCh
		locksMutex.Unlock()

		// Return a function that will complete the operation.
		return func() {
			// Get exclusive access to the map.
			locksMutex.Lock()
			doneCh, ok := locks[lockName]

			// Load our existing operation, skipping release if the entry
			// now belongs to another operation (repeated unlock call).
			if ok && doneCh == waitCh {
				// Close the channel to indicate to other waiting users
				// they can now try again to create a new operation.
				close(doneCh)

				// Remove our existing operation entry from the map.
				delete(locks, lockName)
			}

			// Release the lock now that the done channel is closed and the
			// map entry has been deleted, this will allow any waiting users
			// to try and get access to the map to create a new operation.
			locksMutex.Unlock()
		}, waitCh
	}

	// An existing operation is ongoing, lets wait for that to finish and then try
	// to get exclusive access to create a new operation again.
	locksMutex.Unlock()

	return nil, waitCh
}

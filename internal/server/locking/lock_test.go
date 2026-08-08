package locking

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitStuck asserts that the channel does not fire within a short grace period.
func waitStuck(t *testing.T, ch chan struct{}, msg string) {
	t.Helper()

	select {
	case <-ch:
		t.Fatal(msg)
	case <-time.After(50 * time.Millisecond):
	}
}

// waitDone asserts that the channel fires within the test timeout.
func waitDone(t *testing.T, ch chan struct{}, msg string) {
	t.Helper()

	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal(msg)
	}
}

func TestRLockConcurrentReaders(t *testing.T) {
	const readers = 10

	var active, maxActive int64

	var wg sync.WaitGroup
	start := make(chan struct{})

	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			unlock, err := RLock(context.Background(), "test-rw")
			if err != nil {
				t.Error(err)
				return
			}

			n := atomic.AddInt64(&active, 1)

			// Track the highest concurrency seen.
			for {
				seen := atomic.LoadInt64(&maxActive)
				if n <= seen || atomic.CompareAndSwapInt64(&maxActive, seen, n) {
					break
				}
			}

			time.Sleep(100 * time.Millisecond)
			atomic.AddInt64(&active, -1)
			unlock()
		}()
	}

	close(start)
	wg.Wait()

	if maxActive < 2 {
		t.Fatalf("readers did not run concurrently: max active %d", maxActive)
	}

	locksMutex.Lock()
	rwLocksMutex.Lock()
	leak := len(rwLocks)
	rwLocksMutex.Unlock()
	locksMutex.Unlock()
	if leak != 0 {
		t.Fatalf("rwLocks map leaked %d entries", leak)
	}
}

func TestRWLockExcludesReaders(t *testing.T) {
	unlockW, err := RWLock(context.Background(), "test-rw")
	if err != nil {
		t.Fatal(err)
	}

	got := make(chan struct{})
	go func() {
		unlockR, err := RLock(context.Background(), "test-rw")
		if err != nil {
			t.Error(err)
			return
		}

		close(got)
		unlockR()
	}()

	waitStuck(t, got, "reader acquired lock while writer held it")

	unlockW()
	waitDone(t, got, "reader never acquired lock after writer released")
}

func TestRWLockWaitsForReaders(t *testing.T) {
	unlockR, err := RLock(context.Background(), "test-rw")
	if err != nil {
		t.Fatal(err)
	}

	got := make(chan struct{})
	go func() {
		unlockW, err := RWLock(context.Background(), "test-rw")
		if err != nil {
			t.Error(err)
			return
		}

		close(got)
		unlockW()
	}()

	waitStuck(t, got, "writer acquired lock while reader held it")

	unlockR()
	waitDone(t, got, "writer never acquired lock after reader released")
}

func TestRWLockWriterPriority(t *testing.T) {
	// Reader 1 holds the lock.
	unlockR1, err := RLock(context.Background(), "test-rw")
	if err != nil {
		t.Fatal(err)
	}

	// Writer starts waiting.
	writerGot := make(chan struct{})
	go func() {
		unlockW, err := RWLock(context.Background(), "test-rw")
		if err != nil {
			t.Error(err)
			return
		}

		close(writerGot)
		time.Sleep(50 * time.Millisecond)
		unlockW()
	}()

	// Give the writer time to register as waiting.
	time.Sleep(50 * time.Millisecond)

	// A new reader must now wait behind the waiting writer.
	reader2Got := make(chan struct{})
	go func() {
		unlockR2, err := RLock(context.Background(), "test-rw")
		if err != nil {
			t.Error(err)
			return
		}

		close(reader2Got)
		unlockR2()
	}()

	waitStuck(t, reader2Got, "new reader bypassed a waiting writer")

	// Release reader 1: writer should get the lock before reader 2.
	unlockR1()
	waitDone(t, writerGot, "writer never acquired the lock")
	waitDone(t, reader2Got, "reader 2 never acquired the lock after the writer")
}

func TestRWLockContextCancel(t *testing.T) {
	unlockR, err := RLock(context.Background(), "test-rw")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	writerErr := make(chan error)
	go func() {
		_, err := RWLock(ctx, "test-rw")
		writerErr <- err
	}()

	// Give the writer time to register as waiting, then cancel it.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-writerErr:
		if err == nil {
			t.Fatal("cancelled writer acquired the lock")
		}

	case <-time.After(5 * time.Second):
		t.Fatal("cancelled writer never returned")
	}

	// The cancelled writer must not block new readers.
	unlockR2, err := RLock(context.Background(), "test-rw")
	if err != nil {
		t.Fatal(err)
	}

	unlockR2()
	unlockR()

	rwLocksMutex.Lock()
	leak := len(rwLocks)
	rwLocksMutex.Unlock()
	if leak != 0 {
		t.Fatalf("rwLocks map leaked %d entries", leak)
	}
}

func TestRWLockRepeatedUnlock(t *testing.T) {
	unlockR1, err := RLock(context.Background(), "test-rw")
	if err != nil {
		t.Fatal(err)
	}

	unlockR2, err := RLock(context.Background(), "test-rw")
	if err != nil {
		t.Fatal(err)
	}

	// Unlocking the same hold twice must not release the other reader's hold.
	unlockR1()
	unlockR1()

	// A writer must still wait for reader 2.
	got := make(chan struct{})
	go func() {
		unlockW, err := RWLock(context.Background(), "test-rw")
		if err != nil {
			t.Error(err)
			return
		}

		close(got)
		unlockW()
	}()

	waitStuck(t, got, "writer acquired lock while reader 2 still held it after repeated unlock")

	unlockR2()
	waitDone(t, got, "writer never acquired lock after reader 2 released")
}

func TestRWLockStress(t *testing.T) {
	const workers = 20
	const iterations = 50

	var readersActive, writersActive int64

	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			for range iterations {
				if i%4 == 0 {
					unlock, err := RWLock(context.Background(), "stress")
					if err != nil {
						t.Error(err)
						return
					}

					if atomic.AddInt64(&writersActive, 1) != 1 || atomic.LoadInt64(&readersActive) != 0 {
						t.Error("writer overlapped with another holder")
					}

					atomic.AddInt64(&writersActive, -1)
					unlock()
				} else {
					unlock, err := RLock(context.Background(), "stress")
					if err != nil {
						t.Error(err)
						return
					}

					atomic.AddInt64(&readersActive, 1)
					if atomic.LoadInt64(&writersActive) != 0 {
						t.Error("reader overlapped with a writer")
					}

					atomic.AddInt64(&readersActive, -1)
					unlock()
				}
			}
		}(i)
	}

	wg.Wait()

	rwLocksMutex.Lock()
	leak := len(rwLocks)
	rwLocksMutex.Unlock()
	if leak != 0 {
		t.Fatalf("rwLocks map leaked %d entries", leak)
	}
}

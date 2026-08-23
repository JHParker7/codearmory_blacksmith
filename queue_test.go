package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// acquireOrFail is the common case: a slot is expected to be free right now.
func acquireOrFail(t *testing.T, q *Queue, prio Priority) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := q.Acquire(ctx, prio)
	if err != nil {
		t.Fatalf("Acquire(%s) = %v, want a slot", prio, err)
	}
	return release
}

// waitFor polls cond until it holds or the deadline passes. Used instead of a
// fixed sleep so the tests are not timing-fragile on a loaded machine.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestQueueAdmitsUpToSlots(t *testing.T) {
	q := NewQueue(4, 8)
	releases := make([]func(), 0, 4)
	for i := range 4 {
		releases = append(releases, acquireOrFail(t, q, PriorityNormal))
		if got := q.InFlight(); got != i+1 {
			t.Fatalf("InFlight() = %d after %d acquires, want %d", got, i+1, i+1)
		}
	}
	for _, r := range releases {
		r()
	}
	if got := q.InFlight(); got != 0 {
		t.Errorf("InFlight() = %d after releasing everything, want 0", got)
	}
}

// Overshooting the slot count is measurably worse than matching it, so the
// queue must hold the extra request rather than pass it through.
func TestQueueBlocksBeyondSlots(t *testing.T) {
	q := NewQueue(2, 8)
	r1 := acquireOrFail(t, q, PriorityNormal)
	r2 := acquireOrFail(t, q, PriorityNormal)

	admitted := make(chan struct{})
	go func() {
		release, err := q.Acquire(context.Background(), PriorityNormal)
		if err != nil {
			return
		}
		close(admitted)
		release()
	}()

	waitFor(t, "the third request to queue", func() bool { return q.Waiting() == 1 })
	select {
	case <-admitted:
		t.Fatal("a third request was admitted against 2 slots")
	default:
	}

	r1()
	waitFor(t, "the queued request to be admitted", func() bool {
		select {
		case <-admitted:
			return true
		default:
			return false
		}
	})
	r2()
}

// Rule 3: highest priority first among waiters.
func TestQueueAdmitsHighestPriorityFirst(t *testing.T) {
	q := NewQueue(1, 8)
	hold := acquireOrFail(t, q, PriorityNormal)

	var mu sync.Mutex
	var order []Priority
	var wg sync.WaitGroup

	for _, prio := range []Priority{PriorityLow, PriorityNormal, PriorityHigh} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := q.Acquire(context.Background(), prio)
			if err != nil {
				return
			}
			mu.Lock()
			order = append(order, prio)
			mu.Unlock()
			release()
		}()
		// Queue them one at a time so the test asserts priority, not a race.
		waitFor(t, "waiter to enqueue", func() bool { return q.Waiting() == int(prio)+1 })
	}

	hold()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	want := []Priority{PriorityHigh, PriorityNormal, PriorityLow}
	if len(order) != len(want) {
		t.Fatalf("admitted %d requests, want %d", len(order), len(want))
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("admission order = %v, want %v", order, want)
			break
		}
	}
}

// Rule 2, the whole point of the critical tier: a critical request waits for the
// box to drain and then runs alone.
func TestQueueCriticalTakesTheWholeBox(t *testing.T) {
	q := NewQueue(4, 8)
	r1 := acquireOrFail(t, q, PriorityNormal)
	r2 := acquireOrFail(t, q, PriorityNormal)

	criticalIn := make(chan struct{})
	go func() {
		release, err := q.Acquire(context.Background(), PriorityCritical)
		if err != nil {
			return
		}
		close(criticalIn)
		time.Sleep(50 * time.Millisecond) // hold the box briefly
		release()
	}()
	waitFor(t, "the critical request to queue", func() bool { return q.Waiting() == 1 })

	// While it waits, the two free slots must NOT be handed to normal work.
	normalIn := make(chan struct{})
	go func() {
		release, err := q.Acquire(context.Background(), PriorityNormal)
		if err != nil {
			return
		}
		close(normalIn)
		release()
	}()
	waitFor(t, "the normal request to queue behind it", func() bool { return q.Waiting() == 2 })

	select {
	case <-normalIn:
		t.Fatal("normal work was admitted into a free slot while a critical request was draining the box")
	case <-criticalIn:
		t.Fatal("the critical request started before in-flight work finished — running work must never be preempted")
	default:
	}

	r1()
	select {
	case <-criticalIn:
		t.Fatal("the critical request started with one request still running")
	default:
	}

	r2()
	waitFor(t, "the critical request to start once the box drained", func() bool {
		select {
		case <-criticalIn:
			return true
		default:
			return false
		}
	})

	// It must own the box: nothing else runs alongside it.
	if got := q.InFlight(); got != 1 {
		t.Errorf("InFlight() = %d while a critical request runs, want 1", got)
	}
	select {
	case <-normalIn:
		t.Error("normal work ran alongside a critical request")
	default:
	}

	waitFor(t, "normal work to resume after the critical request", func() bool {
		select {
		case <-normalIn:
			return true
		default:
			return false
		}
	})
}

// Load shedding: a full queue must fail fast so the ticket stays unclaimed and
// another host can take it.
func TestQueueFullSheds(t *testing.T) {
	q := NewQueue(1, 1)
	hold := acquireOrFail(t, q, PriorityNormal)
	defer hold()

	go func() {
		release, err := q.Acquire(context.Background(), PriorityNormal)
		if err == nil {
			release()
		}
	}()
	waitFor(t, "the queue to fill", func() bool { return q.Waiting() == 1 })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := q.Acquire(ctx, PriorityNormal); !errors.Is(err, ErrQueueFull) {
		t.Errorf("Acquire on a full queue = %v, want ErrQueueFull", err)
	}
}

// A cancelled waiter must leave no trace — otherwise the queue leaks capacity
// and eventually admits nothing.
func TestQueueCancelledWaiterReleasesCapacity(t *testing.T) {
	q := NewQueue(1, 8)
	hold := acquireOrFail(t, q, PriorityNormal)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := q.Acquire(ctx, PriorityNormal)
		done <- err
	}()
	waitFor(t, "the waiter to enqueue", func() bool { return q.Waiting() == 1 })

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Acquire after cancel = %v, want context.Canceled", err)
	}
	waitFor(t, "the waiter to be removed", func() bool { return q.Waiting() == 0 })

	hold()
	if got := q.InFlight(); got != 0 {
		t.Fatalf("InFlight() = %d, want 0: a cancelled waiter leaked a slot", got)
	}
	// The slot must still be usable.
	acquireOrFail(t, q, PriorityNormal)()
}

// Releasing twice is a realistic caller mistake (a defer plus an explicit call
// on an error path); it must not corrupt the counter.
func TestQueueDoubleReleaseIsSafe(t *testing.T) {
	q := NewQueue(2, 4)
	release := acquireOrFail(t, q, PriorityNormal)
	release()
	release()
	if got := q.InFlight(); got != 0 {
		t.Errorf("InFlight() = %d after a double release, want 0", got)
	}
	a := acquireOrFail(t, q, PriorityNormal)
	b := acquireOrFail(t, q, PriorityNormal)
	a()
	b()
}

// An unset priority must sort low so a caller that forgets to set one cannot
// jump ahead of work that was labelled deliberately.
func TestZeroPriorityIsLowest(t *testing.T) {
	var unset Priority
	if unset != PriorityLow {
		t.Errorf("zero Priority = %v, want PriorityLow", unset)
	}
	for _, p := range []Priority{PriorityNormal, PriorityHigh, PriorityCritical} {
		if !(p > unset) {
			t.Errorf("%v is not greater than the zero value", p)
		}
	}
}

func TestParsePriority(t *testing.T) {
	cases := map[string]Priority{
		"critical": PriorityCritical,
		"urgent":   PriorityCritical,
		"p0":       PriorityCritical,
		"high":     PriorityHigh,
		"p1":       PriorityHigh,
		"normal":   PriorityNormal,
		"medium":   PriorityNormal,
		"low":      PriorityLow,
		"":         PriorityLow,
		"nonsense": PriorityLow,
	}
	for label, want := range cases {
		if got := ParsePriority(label); got != want {
			t.Errorf("ParsePriority(%q) = %v, want %v", label, got, want)
		}
	}
}

// A misconfigured zero slot count must not deadlock every caller.
func TestQueueClampsDegenerateSizes(t *testing.T) {
	q := NewQueue(0, 0)
	release := acquireOrFail(t, q, PriorityNormal)
	release()
}

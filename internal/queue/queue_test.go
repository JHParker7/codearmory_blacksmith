package queue

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// waitFor polls a condition rather than sleeping a fixed amount: the assertions
// here are about ORDER, and a fixed sleep makes them about speed instead.
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

// acquirer starts an Acquire in the background and reports when it is admitted.
type acquirer struct {
	admitted chan struct{}
	release  chan struct{}
	err      error
	once     sync.Once
}

func start(t *testing.T, q *Queue, ctx context.Context, prio Priority) *acquirer {
	t.Helper()
	a := &acquirer{admitted: make(chan struct{}), release: make(chan struct{})}
	go func() {
		rel, err := q.Acquire(ctx, prio)
		if err != nil {
			a.err = err
			close(a.admitted)
			return
		}
		close(a.admitted)
		<-a.release
		rel()
	}()
	return a
}

func (a *acquirer) in() bool {
	select {
	case <-a.admitted:
		return a.err == nil
	default:
		return false
	}
}

func (a *acquirer) done() { a.once.Do(func() { close(a.release) }) }

// THE QUEUE'S JOB IS TO KEEP EXACTLY `slots` IN FLIGHT. Oversubscribing a
// serving backend is measurably worse than matching it.
func TestOnlyAsManyRequestsRunAsThereAreSlots(t *testing.T) {
	q := New(2, 10)
	ctx := context.Background()

	a, b := start(t, q, ctx, Normal), start(t, q, ctx, Normal)
	waitFor(t, "two admitted", func() bool { return q.InFlight() == 2 })

	c := start(t, q, ctx, Normal)
	waitFor(t, "the third to queue", func() bool { return q.Waiting() == 1 })
	if c.in() {
		t.Fatal("a third request was admitted to a two-slot queue")
	}

	a.done()
	waitFor(t, "the third to be admitted", func() bool { return c.in() })
	if q.InFlight() != 2 {
		t.Errorf("in flight = %d after a hand-over, want 2", q.InFlight())
	}
	b.done()
	c.done()
	waitFor(t, "the queue to drain", func() bool { return q.InFlight() == 0 })
}

// Higher priority is admitted first, and within a priority the OLDEST waits
// least — otherwise a steady stream of equals starves the first arrival.
func TestPriorityThenAgeDecidesWhoGoesNext(t *testing.T) {
	q := New(1, 10)
	ctx := context.Background()

	holder := start(t, q, ctx, Normal)
	waitFor(t, "the holder to run", func() bool { return q.InFlight() == 1 })

	// Queued in a deliberately unhelpful order: low first, then two highs.
	low := start(t, q, ctx, Low)
	waitFor(t, "low to queue", func() bool { return q.Waiting() == 1 })
	firstHigh := start(t, q, ctx, High)
	waitFor(t, "the first high to queue", func() bool { return q.Waiting() == 2 })
	secondHigh := start(t, q, ctx, High)
	waitFor(t, "the second high to queue", func() bool { return q.Waiting() == 3 })

	holder.done()
	waitFor(t, "a waiter to be admitted", func() bool { return firstHigh.in() || secondHigh.in() || low.in() })
	if low.in() {
		t.Fatal("the low-priority request went before two high ones")
	}
	if secondHigh.in() && !firstHigh.in() {
		t.Fatal("the later of two equal requests went first; the older one can starve")
	}

	firstHigh.done()
	waitFor(t, "the second high", func() bool { return secondHigh.in() })
	if low.in() {
		t.Fatal("the low-priority request went before the second high one")
	}
	secondHigh.done()
	waitFor(t, "low last", func() bool { return low.in() })
	low.done()
}

// RULE 1: WHILE A CRITICAL REQUEST RUNS, NOTHING ELSE IS ADMITTED. It gets the
// box — that is what critical means here.
func TestACriticalRequestOwnsTheBox(t *testing.T) {
	q := New(4, 10)
	ctx := context.Background()

	crit := start(t, q, ctx, Critical)
	waitFor(t, "the critical request to run", func() bool { return q.InFlight() == 1 })

	others := []*acquirer{start(t, q, ctx, High), start(t, q, ctx, Normal)}
	waitFor(t, "the others to queue", func() bool { return q.Waiting() == 2 })
	for i, o := range others {
		if o.in() {
			t.Fatalf("request %d was admitted alongside a critical one on a 4-slot queue", i)
		}
	}

	crit.done()
	waitFor(t, "the others to be admitted", func() bool { return others[0].in() && others[1].in() })
	for _, o := range others {
		o.done()
	}
}

// RULE 2: A CRITICAL REQUEST NEVER PREEMPTS. The tokens already spent on running
// work would be thrown away, and a half-written branch is worse than a late one.
// It waits for the drain, and meanwhile nothing new is let in.
func TestAWaitingCriticalRequestDrainsTheBoxWithoutPreempting(t *testing.T) {
	q := New(2, 10)
	ctx := context.Background()

	running := []*acquirer{start(t, q, ctx, Normal), start(t, q, ctx, Normal)}
	waitFor(t, "both to run", func() bool { return q.InFlight() == 2 })

	crit := start(t, q, ctx, Critical)
	waitFor(t, "the critical request to queue", func() bool { return q.Waiting() == 1 })

	// Nothing was preempted.
	if q.InFlight() != 2 {
		t.Fatalf("in flight = %d; running work was preempted", q.InFlight())
	}
	if crit.in() {
		t.Fatal("the critical request started while other work was still running")
	}

	// A newcomer must be held at the door, or the box never drains.
	newcomer := start(t, q, ctx, High)
	waitFor(t, "the newcomer to queue", func() bool { return q.Waiting() == 2 })
	if newcomer.in() {
		t.Fatal("a new request was admitted while a critical one waited; the drain never finishes")
	}

	running[0].done()
	waitFor(t, "one slot free", func() bool { return q.InFlight() == 1 })
	if crit.in() {
		t.Fatal("the critical request started with the box half full")
	}

	running[1].done()
	waitFor(t, "the critical request to start", func() bool { return crit.in() })
	if q.InFlight() != 1 {
		t.Errorf("in flight = %d with a critical request running, want 1", q.InFlight())
	}
	crit.done()
	waitFor(t, "the newcomer", func() bool { return newcomer.in() })
	newcomer.done()
}

// A FULL QUEUE IS A LOAD-SHEDDING SIGNAL, not a failure: the caller leaves the
// ticket unclaimed so another host, or this one later, picks it up.
func TestAFullQueueShedsTheArrival(t *testing.T) {
	q := New(1, 1)
	ctx := context.Background()

	holder := start(t, q, ctx, Normal)
	waitFor(t, "the holder", func() bool { return q.InFlight() == 1 })

	queued := start(t, q, ctx, Normal)
	waitFor(t, "one waiting", func() bool { return q.Waiting() == 1 })

	if _, err := q.Acquire(ctx, Normal); !errors.Is(err, ErrFull) {
		t.Fatalf("err = %v, want ErrFull", err)
	}
	// THE ARRIVAL IS SHED, not the work already waiting.
	if q.Waiting() != 1 {
		t.Errorf("waiting = %d after shedding; an older waiter was thrown out", q.Waiting())
	}

	holder.done()
	waitFor(t, "the waiter to be admitted", func() bool { return queued.in() })
	queued.done()
}

// A CANCELLED REQUEST MUST NOT LEAK A SLOT. It can be cancelled while queued or
// in the instant after a slot is handed to it, and both must leave the queue
// exactly as it was.
func TestACancelledRequestLeavesNoSlotBehind(t *testing.T) {
	t.Run("cancelled while queued", func(t *testing.T) {
		q := New(1, 10)
		holder := start(t, q, context.Background(), Normal)
		waitFor(t, "the holder", func() bool { return q.InFlight() == 1 })

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := q.Acquire(ctx, Normal)
			done <- err
		}()
		waitFor(t, "the second to queue", func() bool { return q.Waiting() == 1 })

		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want the cancellation", err)
		}
		waitFor(t, "the waiter to be forgotten", func() bool { return q.Waiting() == 0 })

		holder.done()
		waitFor(t, "the queue to drain", func() bool { return q.InFlight() == 0 })
	})

	// The race: cancelled at the moment a slot is handed over. Run repeatedly,
	// because the interleaving is what is being tested and it is not
	// deterministic — a leak here would hold a slot for the life of the process.
	t.Run("cancelled as the slot arrives", func(t *testing.T) {
		for i := 0; i < 200; i++ {
			q := New(1, 10)
			holder, _ := q.Acquire(context.Background(), Normal)

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				rel, err := q.Acquire(ctx, Normal)
				if err == nil {
					rel()
				}
				close(done)
			}()
			waitFor(t, "the second to queue", func() bool { return q.Waiting() == 1 })

			// Free the slot and cancel at the same instant.
			go cancel()
			holder()
			<-done

			if got := q.InFlight(); got != 0 {
				t.Fatalf("round %d: in flight = %d after everything ended; a slot leaked", i, got)
			}
		}
	})
}

// The common caller is a defer in a path that also returns errors, so a double
// release must be a no-op rather than a panic — and must not hand out a slot
// that was never taken.
func TestReleasingTwiceIsHarmless(t *testing.T) {
	q := New(1, 10)
	rel, err := q.Acquire(context.Background(), Normal)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	rel()
	rel()
	rel()
	if got := q.InFlight(); got != 0 {
		t.Errorf("in flight = %d after three releases of one slot", got)
	}
}

// A ZERO-SLOT QUEUE WOULD DEADLOCK every caller rather than fail loudly, and a
// stalled department with no error looks exactly like a busy one.
func TestAQueueAlwaysHasAtLeastOneSlot(t *testing.T) {
	for _, slots := range []int{0, -1} {
		q := New(slots, 0)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		rel, err := q.Acquire(ctx, Normal)
		cancel()
		if err != nil {
			t.Fatalf("New(%d, 0) admitted nothing: %v", slots, err)
		}
		rel()
	}
}

func TestPriorityLabelsMapToAdmissionOrder(t *testing.T) {
	cases := map[string]Priority{
		"critical": Critical, "urgent": Critical, "p0": Critical,
		"high": High, "p1": High,
		"normal": Normal, "medium": Normal, "p2": Normal,
		"low": Low, "": Low,
		// AN UNKNOWN LABEL SORTS LOW. It must never be able to starve work whose
		// label is understood.
		"expedite-please": Low, "P0": Low,
	}
	for label, want := range cases {
		if got := ParsePriority(label); got != want {
			t.Errorf("ParsePriority(%q) = %v, want %v", label, got, want)
		}
	}
	// The zero value is what a caller that set nothing gets.
	var unset Priority
	if unset != Low {
		t.Errorf("the zero priority is %v; an unset priority must not jump the queue", unset)
	}
}

func TestPriorityNamesAreStable(t *testing.T) {
	for p, want := range map[Priority]string{
		Critical: "critical", High: "high", Normal: "normal", Low: "low",
		Priority(99): "critical", Priority(-1): "low",
	} {
		if got := p.String(); got != want {
			t.Errorf("Priority(%d).String() = %q, want %q", p, got, want)
		}
	}
}

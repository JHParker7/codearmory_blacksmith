package main

import (
	"context"
	"errors"
	"sync"
)

// Priority orders admission. The zero value is PriorityNormal's neighbour rather
// than Normal itself on purpose: an unset priority should sort LOW, so a caller
// that forgets to set one cannot accidentally jump the queue.
type Priority int

const (
	PriorityLow Priority = iota
	PriorityNormal
	PriorityHigh
	// PriorityCritical does not merely sort first — it takes the whole box. See
	// the admission rules on Queue.
	PriorityCritical
)

func (p Priority) String() string {
	switch p {
	case PriorityCritical:
		return "critical"
	case PriorityHigh:
		return "high"
	case PriorityNormal:
		return "normal"
	default:
		return "low"
	}
}

// ParsePriority maps a ticket's priority label onto an admission priority.
// Anything unrecognised sorts low, which is the safe direction: an unknown label
// must never be able to starve known work.
func ParsePriority(s string) Priority {
	switch s {
	case "critical", "urgent", "p0":
		return PriorityCritical
	case "high", "p1":
		return PriorityHigh
	case "normal", "medium", "p2":
		return PriorityNormal
	default:
		return PriorityLow
	}
}

// ErrQueueFull is returned when a request arrives at a queue that is already at
// its bound. It is a load-shedding signal, not a failure: the caller should leave
// the ticket unclaimed so another host — or this one, later — can pick it up.
var ErrQueueFull = errors.New("inference queue full")

// Queue is the admission control in front of one model class's serving slots.
//
// The measurements this exists to exploit: a serving backend reaches its
// aggregate throughput only while every slot is busy (~317 tok/s across 4 slots
// against ~160 for one), so the queue's job is to keep `slots` requests in flight
// and no more. Oversubscribing is measurably worse than matching.
//
// Admission rules, in force order:
//
//  1. While a critical request runs, nothing else is admitted. It gets the box.
//  2. While a critical request WAITS, no new non-critical request is admitted;
//     in-flight work drains naturally, and the critical one starts once the box
//     is empty. Running work is never preempted — the tokens already spent on it
//     would be thrown away, and a half-written branch is worse than a late one.
//  3. Otherwise admit up to `slots`, highest priority first, FIFO within a
//     priority.
//
// Rule 2 is what makes "critical tickets are worked one at a time, before
// anything else" true without a separate scheduler.
//
// THIS IS THE ONLY PLACE THAT RULE LIVES. It was briefly enforced in
// planDispatch as well, which could not work: a dispatcher is per stage, so it
// held back its own queue while the other five stages went on hitting this same
// server. The critical ticket lost its siblings and still shared the GPU. Here
// the exclusivity is real, because every agent's request — whatever stage it
// came from — passes through this queue.
//
// Note the honest consequence: if EVERY ticket is critical (a parent's priority
// propagating across a whole decomposition will do it), every request serialises
// and the box runs one stream. That is what critical means. It is the right
// behaviour for a genuine emergency and the wrong thing to hand out freely.
type Queue struct {
	slots    int
	maxQueue int

	mu              sync.Mutex
	running         int
	runningCritical bool
	waiters         []*waiter
	seq             uint64
}

type waiter struct {
	prio Priority
	seq  uint64
	ch   chan struct{}
}

// NewQueue builds a queue admitting `slots` concurrent requests and holding at
// most `maxQueue` waiting ones. Both are clamped to at least 1 — a zero-slot
// queue would deadlock every caller rather than fail loudly, which is the worse
// of the two behaviours.
func NewQueue(slots, maxQueue int) *Queue {
	if slots < 1 {
		slots = 1
	}
	if maxQueue < 1 {
		maxQueue = 1
	}
	return &Queue{slots: slots, maxQueue: maxQueue}
}

// Acquire blocks until a slot is available for prio, the queue is full, or ctx
// ends. The returned release function must be called exactly once; calling it
// more than once is a no-op rather than a panic, because the common caller is a
// defer in a path that also returns errors.
func (q *Queue) Acquire(ctx context.Context, prio Priority) (func(), error) {
	q.mu.Lock()
	w := &waiter{prio: prio, seq: q.seq, ch: make(chan struct{})}
	q.seq++
	q.waiters = append(q.waiters, w)
	q.dispatchLocked()

	admitted := q.indexOfLocked(w) == -1
	if !admitted && len(q.waiters) > q.maxQueue {
		q.removeLocked(w)
		q.mu.Unlock()
		return nil, ErrQueueFull
	}
	q.mu.Unlock()

	select {
	case <-w.ch:
		return q.releaser(prio), nil
	case <-ctx.Done():
		q.mu.Lock()
		if q.removeLocked(w) {
			// Still queued: we never took a slot, so there is nothing to give back.
			q.mu.Unlock()
			return nil, ctx.Err()
		}
		// Raced with dispatch — we hold a slot we are no longer going to use.
		q.mu.Unlock()
		q.releaser(prio)()
		return nil, ctx.Err()
	}
}

// InFlight reports how many requests currently hold a slot. Exported for the
// metrics the gateway publishes, and for tests.
func (q *Queue) InFlight() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.running
}

// Waiting reports how many requests are queued but not yet admitted.
func (q *Queue) Waiting() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.waiters)
}

func (q *Queue) releaser(prio Priority) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			q.mu.Lock()
			defer q.mu.Unlock()
			q.running--
			if prio == PriorityCritical {
				q.runningCritical = false
			}
			q.dispatchLocked()
		})
	}
}

// dispatchLocked admits every waiter it can, best first, until no further
// admission is legal. It must be called with q.mu held.
func (q *Queue) dispatchLocked() {
	for {
		best := -1
		for i, w := range q.waiters {
			if !q.canAdmitLocked(w.prio) {
				continue
			}
			if best == -1 || betterWaiter(w, q.waiters[best]) {
				best = i
			}
		}
		if best == -1 {
			return
		}
		w := q.waiters[best]
		q.waiters = append(q.waiters[:best], q.waiters[best+1:]...)
		q.running++
		if w.prio == PriorityCritical {
			q.runningCritical = true
		}
		close(w.ch)
	}
}

func (q *Queue) canAdmitLocked(prio Priority) bool {
	if q.runningCritical {
		return false // rule 1: a critical request owns the box
	}
	if prio == PriorityCritical {
		return q.running == 0 // rule 2: wait for the box to drain, never preempt
	}
	if q.criticalWaitingLocked() {
		return false // rule 2: hold the door so the drain can finish
	}
	return q.running < q.slots // rule 3
}

func (q *Queue) criticalWaitingLocked() bool {
	for _, w := range q.waiters {
		if w.prio == PriorityCritical {
			return true
		}
	}
	return false
}

func (q *Queue) indexOfLocked(target *waiter) int {
	for i, w := range q.waiters {
		if w == target {
			return i
		}
	}
	return -1
}

func (q *Queue) removeLocked(target *waiter) bool {
	i := q.indexOfLocked(target)
	if i == -1 {
		return false
	}
	q.waiters = append(q.waiters[:i], q.waiters[i+1:]...)
	return true
}

// betterWaiter reports whether a should be admitted before b: higher priority
// wins, and within a priority the older request wins so nothing starves.
func betterWaiter(a, b *waiter) bool {
	if a.prio != b.prio {
		return a.prio > b.prio
	}
	return a.seq < b.seq
}

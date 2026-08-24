// Package queue is the admission control in front of one model class's serving
// slots.
//
// THE MEASUREMENT THIS EXISTS TO EXPLOIT: a serving backend reaches its
// aggregate throughput only while every slot is busy — about 317 tokens/second
// across four slots against about 160 for one — so the job here is to keep
// exactly `slots` requests in flight and no more. Oversubscribing is measurably
// worse than matching.
package queue

import (
	"context"
	"errors"
	"sync"
)

// Priority orders admission.
//
// THE ZERO VALUE IS LOW, deliberately. A caller that forgets to set a priority
// must not be able to jump the queue, so "unset" sorts below everything a caller
// meant.
type Priority int

const (
	Low Priority = iota
	Normal
	High

	// Critical does not merely sort first — it TAKES THE WHOLE BOX. See the
	// admission rules below.
	Critical
)

// String names a priority for a metric label and for the window.
//
// IT FOLLOWS THE COMPARISON, not an exact match. Admission orders these
// numerically, so a value above Critical would be admitted ahead of everything
// while an exact-match switch printed it as "low" — a label that contradicts the
// behaviour it describes, which is the most expensive kind of message this
// repository produces.
func (p Priority) String() string {
	switch {
	case p >= Critical:
		return "critical"
	case p == High:
		return "high"
	case p == Normal:
		return "normal"
	default:
		return "low"
	}
}

// ParsePriority maps a ticket's priority label onto an admission priority.
//
// ANYTHING UNRECOGNISED SORTS LOW, which is the safe direction: an unknown label
// must never be able to starve work whose label is understood.
func ParsePriority(s string) Priority {
	switch s {
	case "critical", "urgent", "p0":
		return Critical
	case "high", "p1":
		return High
	case "normal", "medium", "p2":
		return Normal
	default:
		return Low
	}
}

// ErrFull is returned when a request arrives at a queue already at its bound.
//
// A LOAD-SHEDDING SIGNAL, NOT A FAILURE. The caller should leave the ticket
// unclaimed so another host — or this one, later — picks it up.
var ErrFull = errors.New("inference queue full")

// Queue admits requests to a fixed number of slots.
//
// The admission rules, in force order:
//
//  1. While a critical request RUNS, nothing else is admitted. It gets the box.
//  2. While a critical request WAITS, no new non-critical request is admitted;
//     in-flight work drains naturally and the critical one starts once the box is
//     empty. RUNNING WORK IS NEVER PREEMPTED — the tokens already spent on it
//     would be thrown away, and a half-written branch is worse than a late one.
//  3. Otherwise admit up to `slots`, highest priority first, oldest first within
//     a priority so nothing starves.
//
// Rule 2 is what makes "critical tickets are worked one at a time, before
// anything else" true without a separate scheduler.
//
// THIS IS THE ONLY PLACE THAT RULE LIVES. It was briefly enforced in the
// dispatcher as well, which could not work: a dispatcher is per stage, so it held
// back its own queue while the other five stages went on hitting the same server.
// The critical ticket lost its siblings and still shared the GPU. Here the
// exclusivity is real, because every agent's request — whatever stage it came
// from — passes through this queue.
//
// The honest consequence: if EVERY ticket is critical — a parent's priority
// propagating across a whole decomposition will do it — every request serialises
// and the box runs one stream. That is what critical means. It is right for a
// genuine emergency and wrong to hand out freely.
type Queue struct {
	slots int
	bound int

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

// New builds a queue admitting `slots` concurrent requests and holding at most
// `bound` waiting ones.
//
// Both are clamped to at least 1. A ZERO-SLOT QUEUE WOULD DEADLOCK every caller
// rather than fail loudly, which is the worse of the two behaviours: a stalled
// department with no error looks exactly like a busy one.
func New(slots, bound int) *Queue {
	if slots < 1 {
		slots = 1
	}
	if bound < 1 {
		bound = 1
	}
	return &Queue{slots: slots, bound: bound}
}

// Slots reports how many requests may run at once.
func (q *Queue) Slots() int { return q.slots }

// Acquire blocks until a slot is available for prio, the queue is full, or ctx
// ends.
//
// The returned release must be called exactly once. Calling it more than once is
// a NO-OP rather than a panic, because the common caller is a defer in a path
// that also returns errors — and a panic there would take down a dispatcher over
// a double release.
func (q *Queue) Acquire(ctx context.Context, prio Priority) (release func(), err error) {
	q.mu.Lock()
	w := &waiter{prio: prio, seq: q.seq, ch: make(chan struct{})}
	q.seq++
	q.waiters = append(q.waiters, w)
	q.dispatchLocked()

	// Admitted means dispatch already took it off the list.
	if q.indexOfLocked(w) != -1 && len(q.waiters) > q.bound {
		// THE ARRIVAL IS WHAT IS SHED, not the work already waiting. Shedding an
		// older waiter would throw away a slot someone has already been queuing for,
		// and the caller of this one can simply not claim its ticket.
		q.removeLocked(w)
		q.mu.Unlock()
		return nil, ErrFull
	}
	q.mu.Unlock()

	select {
	case <-w.ch:
		return q.releaser(prio), nil

	case <-ctx.Done():
		q.mu.Lock()
		queued := q.removeLocked(w)
		q.mu.Unlock()
		if queued {
			// Still waiting, so no slot was ever taken and there is nothing to give
			// back.
			return nil, ctx.Err()
		}
		// RACED WITH DISPATCH: a slot was handed over between the cancellation and
		// this lock, and nobody is going to use it. Releasing it here is what stops
		// a cancelled request leaking a slot for the life of the process.
		q.releaser(prio)()
		return nil, ctx.Err()
	}
}

// InFlight reports how many requests currently hold a slot.
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
			if prio == Critical {
				q.runningCritical = false
			}
			q.dispatchLocked()
		})
	}
}

// dispatchLocked admits every waiter it legally can, best first. Called with
// q.mu held.
func (q *Queue) dispatchLocked() {
	for {
		best := -1
		for i, w := range q.waiters {
			if !q.canAdmitLocked(w.prio) {
				continue
			}
			if best == -1 || better(w, q.waiters[best]) {
				best = i
			}
		}
		if best == -1 {
			return
		}
		w := q.waiters[best]
		q.waiters = append(q.waiters[:best], q.waiters[best+1:]...)
		q.running++
		if w.prio == Critical {
			q.runningCritical = true
		}
		close(w.ch)
	}
}

func (q *Queue) canAdmitLocked(prio Priority) bool {
	if q.runningCritical {
		return false // rule 1: a critical request owns the box
	}
	if prio == Critical {
		return q.running == 0 // rule 2: wait for the drain, never preempt
	}
	if q.criticalWaitingLocked() {
		return false // rule 2: hold the door so the drain can finish
	}
	return q.running < q.slots // rule 3
}

func (q *Queue) criticalWaitingLocked() bool {
	for _, w := range q.waiters {
		if w.prio == Critical {
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

// better reports whether a should be admitted before b: higher priority wins,
// and within a priority the OLDER request wins so nothing starves.
func better(a, b *waiter) bool {
	if a.prio != b.prio {
		return a.prio > b.prio
	}
	return a.seq < b.seq
}

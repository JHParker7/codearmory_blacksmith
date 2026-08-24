// Package wake is the CROSS-STAGE edge trigger.
//
// The dispatchers are goroutines in one process that hand work to each other
// through a database column, and each had only its own poll timer to discover
// that a hand-off happened. So a ticket finishing one stage waited up to a full
// poll interval before the next stage noticed — several times per ticket, since
// the pipeline has several hand-offs, and at the default interval that is around
// twenty seconds of an idle GPU per ticket doing nothing but waiting.
//
// DELIBERATELY NOT A MESSAGE BROKER. The thing being coordinated is two
// goroutines in one address space: a broker would put a network hop and a second
// ordering authority between them, and the claim protocol already depends on the
// ticket store being the only arbiter of who owns a ticket. It is also not a
// webhook, which would need an inbound listener on the agent host and give up
// the pull-only egress posture the deployment is built around.
//
// THE POLL REMAINS, as the level trigger. This removes only the latency of
// movements made inside this process; a ticket that appears from outside it — a
// person moving a card, a second agent host handing work over — is still found
// by the timer, and that is the case the timer exists for.
package wake

import "sync"

// Trigger fans a signal out to every dispatcher watching it.
type Trigger struct {
	mu   sync.Mutex
	subs []chan struct{}
}

// New builds a trigger with no subscribers.
func New() *Trigger { return &Trigger{} }

// Subscribe returns a channel that receives when any stage moves a ticket.
//
// A NIL TRIGGER RETURNS A NIL CHANNEL, and a receive on one blocks forever —
// which is exactly the right no-op inside a select. It lets a single-stage host,
// and every test, leave the trigger unset without branching around it.
func (t *Trigger) Subscribe() <-chan struct{} {
	if t == nil {
		return nil
	}
	ch := make(chan struct{}, 1)
	t.mu.Lock()
	t.subs = append(t.subs, ch)
	t.mu.Unlock()
	return ch
}

// Signal tells every dispatcher that the board changed.
//
// NEVER BLOCKS THE CALLER. The buffer is one deep and a full buffer is dropped,
// because a wake already pending is as good as two. The signaller is a stage
// finishing a ticket, and making that wait on a slow subscriber would trade the
// latency this removes for a worse one.
func (t *Trigger) Signal() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, ch := range t.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

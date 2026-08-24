package wake

import "sync"

// Wake is the CROSS-STAGE edge trigger.
//
// The dispatchers are goroutines in one process that hand work to each other
// through a database column, and each had only its own poll timer to discover
// that a hand-off happened. So a ticket finishing one stage waited up to a full
// poll interval before the next stage noticed — three times per ticket, since
// the pipeline has three hand-offs, and with the default 15s interval that is
// around 22 seconds of an idle GPU per ticket doing nothing but waiting.
//
// This is deliberately NOT a message broker. The thing being coordinated is two
// goroutines in one address space: a broker would put a network hop and a second
// ordering authority between them, and the claim protocol already depends on the
// ticket store being the only arbiter of who owns a ticket. It is also not a
// webhook, which would require an inbound listener on the agent host and give up
// the pull-only egress posture the deployment is built around.
//
// The poll REMAINS, as the level trigger. This only removes the latency of
// movements made inside this process; a ticket that appears from outside it — a
// person moving a card, a second agent host handing work over — is still found
// by the timer, and that is the case the timer exists for.
type Wake struct {
	mu   sync.Mutex
	subs []chan struct{}
}

func New() *Wake { return &Wake{} }

// Subscribe returns a channel that receives when any stage moves a ticket.
func (w *Wake) Subscribe() <-chan struct{} {
	if w == nil {
		return nil // a nil receive blocks forever, which is the correct no-op in a select
	}
	ch := make(chan struct{}, 1)
	w.mu.Lock()
	w.subs = append(w.subs, ch)
	w.mu.Unlock()
	return ch
}

// Signal tells every dispatcher that the board changed.
//
// Never blocks, and never blocks the caller: the buffer is one deep and a full
// buffer is dropped, because a wake already pending is as good as two. The
// signaller is a stage finishing a ticket, and making that wait on a slow
// subscriber would trade the latency this removes for a worse one.
func (w *Wake) Signal() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, ch := range w.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

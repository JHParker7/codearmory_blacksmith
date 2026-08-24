package wake

import (
	"testing"
	"time"
)

func received(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	case <-time.After(time.Second):
		return false
	}
}

func pending(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestEverySubscriberIsWoken(t *testing.T) {
	tr := New()
	a, b, c := tr.Subscribe(), tr.Subscribe(), tr.Subscribe()

	tr.Signal()
	for i, ch := range []<-chan struct{}{a, b, c} {
		if !received(ch) {
			t.Errorf("subscriber %d was not woken; its stage waits a full poll for a hand-off", i)
		}
	}
}

// SIGNALLING MUST NOT BLOCK THE CALLER. The signaller is a stage finishing a
// ticket, and making that wait on a slow subscriber would trade the latency this
// removes for a worse one.
func TestSignallingNeverBlocksOnAFullSubscriber(t *testing.T) {
	tr := New()
	ch := tr.Subscribe()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			tr.Signal()
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Signal blocked on a subscriber that was not reading")
	}

	// A wake already pending is as good as two, so exactly one is held.
	if !pending(ch) {
		t.Error("a thousand signals left nothing pending")
	}
	if pending(ch) {
		t.Error("more than one signal was buffered; a wake already pending is as good as two")
	}
}

// A NIL TRIGGER IS A WORKING NO-OP. A single-stage host, and every test, leaves
// it unset — and a nil channel blocks forever, which is what a select wants.
func TestANilTriggerIsUsableWithoutBranching(t *testing.T) {
	var tr *Trigger

	ch := tr.Subscribe()
	if ch != nil {
		t.Error("a nil trigger handed out a real channel")
	}
	tr.Signal() // must not panic

	// The point of the nil channel: it is safe inside a select and simply never
	// fires, so the poll timer alone drives the loop.
	select {
	case <-ch:
		t.Error("a nil channel fired")
	case <-time.After(10 * time.Millisecond):
	}
}

// A subscriber that arrives after a signal must not receive the old one: it
// woke for work it has not missed, and a stale wake costs a wasted poll.
func TestALateSubscriberDoesNotInheritAnOldSignal(t *testing.T) {
	tr := New()
	tr.Signal()

	ch := tr.Subscribe()
	if pending(ch) {
		t.Error("a new subscriber inherited a signal sent before it existed")
	}
	tr.Signal()
	if !received(ch) {
		t.Error("a new subscriber missed the next signal")
	}
}

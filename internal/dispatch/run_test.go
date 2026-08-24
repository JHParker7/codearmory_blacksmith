package dispatch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/transcript"
	"github.com/code-armory-app/blacksmith/internal/wake"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

// eventually waits for a condition rather than sleeping a fixed amount: these
// assertions are about what the loop does, not how fast the machine is.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRunWorksTheQueueAndStopsWhenTheContextEnds(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	tk := b.add(ticket.Ticket{Status: st.Ready})

	h := &stubHandler{role: workflow.RoleDev}
	d := newDispatcher(t, b, h, Options{Poll: 5 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- d.Run(ctx) }()

	eventually(t, "the ticket to be worked", func() bool {
		return b.get(tk.ID).Status == st.Success
	})

	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want the cancellation", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
}

// RUN RECONCILES BEFORE IT POLLS. A ticket this host was killed holding must be
// recovered at startup rather than waiting for its claim to age out.
func TestRunReconcilesAtStartup(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	tk := claimedBy(t, b, st, "gpu-1")

	h := &stubHandler{role: workflow.RoleDev}
	// One attempt configured, so the ticket is only claimable again if the
	// interrupted attempt was refunded.
	d := newDispatcher(t, b, h, Options{Host: "gpu-1", Poll: 5 * time.Millisecond, MaxAttempts: 1})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	eventually(t, "the interrupted ticket to be picked up", func() bool {
		return b.get(tk.ID).Status == st.Success
	})
}

// A RECONCILE FAILURE IS NOT FATAL. It leaves some tickets claimed until the
// next restart, which is better than refusing to start at all.
func TestRunStartsEvenWhenReconcileFails(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	held := claimedBy(t, b, st, "gpu-1")
	b.failGet[held.ID] = true
	fresh := b.add(ticket.Ticket{Status: st.Ready})

	h := &stubHandler{role: workflow.RoleDev}
	d := newDispatcher(t, b, h, Options{Host: "gpu-1", Poll: 5 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	eventually(t, "the loop to work despite a failed reconcile", func() bool {
		return b.get(fresh.ID).Status == st.Success
	})
}

// THE WAKE IS AN EDGE TRIGGER ACROSS STAGES: a ticket handed over must be picked
// up without waiting for the poll timer.
func TestRunPicksUpWorkOnAWakeRatherThanWaitingForTheTimer(t *testing.T) {
	b := newBoard()
	st := devStage(t)

	tr := wake.New()
	h := &stubHandler{role: workflow.RoleDev}
	// A poll interval far longer than the test: if the ticket is worked, it was
	// the wake that did it.
	d := newDispatcher(t, b, h, Options{Poll: time.Hour, Wake: tr})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	// Let the first fill happen against an empty board.
	eventually(t, "the loop to settle", func() bool { return d.InFlight() == 0 })

	tk := b.add(ticket.Ticket{Status: st.Ready})
	tr.Signal()

	eventually(t, "the woken stage to work the hand-off", func() bool {
		return b.get(tk.ID).Status == st.Success
	})
}

// A LISTING FAILURE MUST NOT KILL THE LOOP. The store is on the other side of a
// network from an intermittent host; the next tick tries again.
func TestRunSurvivesAStoreThatComesAndGoes(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	tk := b.add(ticket.Ticket{Status: st.Ready})

	failing := &flakyBoard{board: b, fail: true}
	h := &stubHandler{role: workflow.RoleDev}
	d, err := New(failing, h, workflow.New(workflow.Options{}), Options{
		Host: "gpu-1", Poll: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.now = func() time.Time { return b.now }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	time.Sleep(20 * time.Millisecond)
	if got := b.get(tk.ID).Status; got != st.Ready {
		t.Fatalf("the ticket moved to %q while the store was unreachable", got)
	}

	failing.setFail(false)
	eventually(t, "the loop to recover once the store returns", func() bool {
		return b.get(tk.ID).Status == st.Success
	})
}

// DRAIN WAITS FOR IN-FLIGHT WORK so shutdown logging is honest about what was
// still running.
func TestDrainAccountsForWorkStillRunning(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	b.add(ticket.Ticket{Status: st.Ready})

	h := &stubHandler{role: workflow.RoleDev, block: make(chan struct{})}
	d := newDispatcher(t, b, h, Options{})

	done := make(chan string, 4)
	if err := d.fill(context.Background(), done); err != nil {
		t.Fatalf("fill: %v", err)
	}
	if got := d.InFlight(); got != 1 {
		t.Fatalf("in flight = %d", got)
	}

	close(h.block)
	d.drain(done)
	if got := d.InFlight(); got != 0 {
		t.Errorf("in flight = %d after draining; shutdown would report work that had finished", got)
	}
}

// EVERY STAGE IS RECORDED, here rather than in each agent — so an agent written
// tomorrow is captured the day it is written.
func TestTheDispatcherRecordsTheWholeAttempt(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	tk := b.add(ticket.Ticket{Status: st.Ready})

	sink := &countingSink{}
	rec := transcript.New(sink, "gpu-1")
	h := &stubHandler{role: workflow.RoleDev, detail: "built and pushed"}
	d := newDispatcher(t, b, h, Options{Recorder: rec})
	drainOnce(t, d, 1)

	kinds := sink.kinds()
	if kinds[transcript.KindStart] != 1 {
		t.Errorf("the attempt was not opened: %v", kinds)
	}
	if kinds[transcript.KindOutcome] != 1 {
		t.Errorf("the attempt was not closed: %v; a transcript with no outcome reads as a task that never finished", kinds)
	}
	if got := sink.lastStatus(); got != workflow.OutcomeSuccess {
		t.Errorf("the recorded outcome is %q", got)
	}
	if got := sink.taskID(); got != tk.ID {
		t.Errorf("the records name task %q, want %q", got, tk.ID)
	}
}

func TestStageReportsTheRoutingEntry(t *testing.T) {
	b := newBoard()
	d := newDispatcher(t, b, &stubHandler{role: workflow.RoleDev}, Options{})
	if got := d.Stage(); got.Role != workflow.RoleDev || got.Ready != devStage(t).Ready {
		t.Errorf("Stage() = %+v", got)
	}
}

func TestClipCutsOnARuneBoundary(t *testing.T) {
	long := ""
	for i := 0; i < 300; i++ {
		long += "—"
	}
	got := clip(long, 200)
	if len([]rune(got)) != 201 {
		t.Errorf("clip kept %d runes, want 200 and an ellipsis", len([]rune(got)))
	}
	if got := clip("short", 200); got != "short" {
		t.Errorf("clip(short) = %q", got)
	}
}

// flakyBoard is a store that can be taken away and given back.
type flakyBoard struct {
	*board
	fail bool
}

func (f *flakyBoard) setFail(v bool) {
	f.board.mu.Lock()
	defer f.board.mu.Unlock()
	f.fail = v
}

func (f *flakyBoard) failing() bool {
	f.board.mu.Lock()
	defer f.board.mu.Unlock()
	return f.fail
}

func (f *flakyBoard) List(ctx context.Context, opts ticket.ListOpts) ([]ticket.Ticket, error) {
	if f.failing() {
		return nil, errors.New("the store could not be reached")
	}
	return f.board.List(ctx, opts)
}

// countingSink records what kinds of transcript record were written.
type countingSink struct {
	mu      sync.Mutex
	records []transcript.Record
}

func (s *countingSink) Write(r transcript.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, r)
	return nil
}

func (s *countingSink) Close() error { return nil }

func (s *countingSink) kinds() map[transcript.Kind]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[transcript.Kind]int{}
	for _, r := range s.records {
		out[r.Kind]++
	}
	return out
}

func (s *countingSink) lastStatus() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.records) - 1; i >= 0; i-- {
		if s.records[i].Kind == transcript.KindOutcome {
			return s.records[i].Status
		}
	}
	return ""
}

func (s *countingSink) taskID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.records) == 0 {
		return ""
	}
	return s.records[0].TaskID
}

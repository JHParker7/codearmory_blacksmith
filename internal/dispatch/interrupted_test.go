package dispatch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/code-armory-app/blacksmith/internal/record"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

// A SHUTDOWN IS NOT A FAILED ATTEMPT.
//
// The handler returns an error because ITS context ended, and by the error alone
// that is indistinguishable from a real failure — "terminated signal received"
// from a chat mid-flight reads exactly like a model that broke. But the ceiling
// counts claims, so restarting to install a fix charges the ticket you were
// fixing, and three restarts in a morning make it unworkable with nothing on the
// board to say why.
//
// Observed while restarting this very department: a spec-agent's chat was cut
// off by SIGTERM and the stage recorded status=failed.
func TestAnAttemptCutOffByShutdownCostsTheTicketNothing(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	tk := b.add(ticket.Ticket{Status: st.Ready})

	ctx, cancel := context.WithCancel(context.Background())

	// A handler that blocks until the department is stopping, then reports the
	// error a cancelled context produces.
	h := &ctxHandler{role: workflow.RoleDev, started: make(chan struct{})}
	d := newDispatcher(t, b, h, Options{Poll: 5 * time.Millisecond})

	go func() { _ = d.Run(ctx) }()
	select {
	case <-h.started:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("the handler never ran")
	}
	cancel()

	eventually(t, "the ticket to come back to its queue", func() bool {
		return b.get(tk.ID).Status == st.Ready
	})
	eventually(t, "the interrupted attempt to be forgiven", func() bool {
		return strings.Contains(commentsOn(b, tk.ID), record.ReturnedMarker)
	})

	// THE COUNT IS THE POINT. A claim was written when the work started; the note
	// is what stops it being charged.
	if n := record.Attempts(b.get(tk.ID), workflow.RoleDev); n != 0 {
		t.Errorf("the interrupted attempt cost the ticket %d of its attempts; "+
			"a few restarts would make it permanently unclaimable", n)
	}
}

// AN ORDINARY FAILURE IS STILL CHARGED. The forgiveness turns on the context
// having ended, not on the handler having returned an error — otherwise the
// ceiling would never be reached and a ticket that cannot succeed retries
// forever.
func TestAnOrdinaryFailureStillSpendsAnAttempt(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	tk := b.add(ticket.Ticket{Status: st.Ready})

	h := &stubHandler{role: workflow.RoleDev, err: errors.New("the sandbox never booted")}
	d := newDispatcher(t, b, h, Options{Poll: 5 * time.Millisecond})
	drainOnce(t, d, 1)

	if n := record.Attempts(b.get(tk.ID), workflow.RoleDev); n != 1 {
		t.Errorf("an ordinary failure counted %d attempts, want 1", n)
	}
	if strings.Contains(commentsOn(b, tk.ID), record.ReturnedMarker) {
		t.Error("an ordinary failure was forgiven as an interrupted one; the ceiling " +
			"can never be reached and the ticket retries forever")
	}
}

// ctxHandler blocks until its context ends, which is what a stage mid-model-call
// does when the department is stopping.
type ctxHandler struct {
	stubHandler
	role    string
	started chan struct{}
	once    bool
}

func (h *ctxHandler) Role() string { return h.role }

func (h *ctxHandler) Wants(ticket.Ticket) bool { return true }

func (h *ctxHandler) Handle(ctx context.Context, _ ticket.Ticket) (workflow.Outcome, string, error) {
	if !h.once {
		h.once = true
		close(h.started)
	}
	<-ctx.Done()
	return workflow.OutcomeFailed, "", ctx.Err()
}

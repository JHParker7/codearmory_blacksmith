package dispatch

import (
	"strings"
	"testing"
	"time"

	"github.com/code-armory-app/blacksmith/internal/record"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

func commentsOn(b *board, id string) string {
	var sb strings.Builder
	for _, c := range b.get(id).Comments {
		sb.WriteString(c.Body)
		sb.WriteString("\n")
	}
	return sb.String()
}

// A TICKET BEHIND A BLOCKED ONE IS STRANDED, NOT WAITING.
//
// Ready needs every blocker to reach done, and ColBlocked is terminal without
// being done — so nothing will ever move it. Rejecting it quietly on every poll
// is how a chain of six tickets lost its second and left four sitting in
// ready_for_dev indefinitely, looking queued and consuming nothing.
func TestATicketBehindABlockedPrerequisiteIsEscalated(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	tk := b.add(ticket.Ticket{Status: st.Ready, DependsOn: []ticket.Dependency{
		{ID: "dead", Title: "The store", Status: workflow.ColBlocked},
	}})

	h := &stubHandler{role: workflow.RoleDev}
	d := newDispatcher(t, b, h, Options{Poll: 5 * time.Millisecond})
	drainOnce(t, d, 0)

	if got := b.get(tk.ID).Status; got != st.Exhausted {
		t.Fatalf("a stranded ticket is still in %q; it will sit there forever "+
			"looking queued", got)
	}
	said := commentsOn(b, tk.ID)
	if !strings.Contains(said, record.EscalatedMarker) {
		t.Errorf("the ticket was escalated without the marker:\n%s", said)
	}
	if !strings.Contains(said, "The store") {
		t.Errorf("the note does not name the blocker, so the reader has to go and "+
			"find it:\n%s", said)
	}
	if worked := h.worked(); len(worked) != 0 {
		t.Errorf("the handler ran on a ticket that cannot start: %v", worked)
	}
}

// AN UNFINISHED BLOCKER IS NOT A DEAD ONE. A ticket waiting on work still in
// flight is doing exactly what it should, and sweeping it would escalate the
// whole board every time a dependency was mid-pipeline.
func TestATicketWaitingOnLiveWorkIsLeftAlone(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	tk := b.add(ticket.Ticket{Status: st.Ready, DependsOn: []ticket.Dependency{
		{ID: "busy", Title: "The store", Status: workflow.ColInDev},
	}})

	d := newDispatcher(t, b, &stubHandler{role: workflow.RoleDev},
		Options{Poll: 5 * time.Millisecond})
	drainOnce(t, d, 0)

	if got := b.get(tk.ID).Status; got != st.Ready {
		t.Errorf("a ticket waiting on work in progress was moved to %q", got)
	}
	if said := commentsOn(b, tk.ID); strings.Contains(said, record.EscalatedMarker) {
		t.Errorf("a ticket waiting on live work was escalated:\n%s", said)
	}
}

// ColConflicted IS NOT ColBlocked. That is the resolver's queue, so a conflicted
// blocker is still on its way to done and the ticket behind it is still waiting.
func TestATicketBehindAConflictedPrerequisiteIsLeftAlone(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	tk := b.add(ticket.Ticket{Status: st.Ready, DependsOn: []ticket.Dependency{
		{ID: "clash", Title: "The store", Status: workflow.ColConflicted},
	}})

	d := newDispatcher(t, b, &stubHandler{role: workflow.RoleDev},
		Options{Poll: 5 * time.Millisecond})
	drainOnce(t, d, 0)

	if got := b.get(tk.ID).Status; got != st.Ready {
		t.Errorf("a ticket behind a conflicted blocker was escalated to %q; the "+
			"resolver has not had its turn yet", got)
	}
}

// THE SWEEP COSTS NO ATTEMPT. The ticket never ran and never will, so charging
// it one would put a number on the board suggesting the work had been tried.
func TestSweepingAStrandedTicketSpendsNoAttempt(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	tk := b.add(ticket.Ticket{Status: st.Ready, DependsOn: []ticket.Dependency{
		{ID: "dead", Title: "The store", Status: workflow.ColBlocked},
	}})

	d := newDispatcher(t, b, &stubHandler{role: workflow.RoleDev},
		Options{Poll: 5 * time.Millisecond})
	drainOnce(t, d, 0)

	if n := record.Attempts(b.get(tk.ID), workflow.RoleDev); n != 0 {
		t.Errorf("the sweep spent %d attempts on a ticket that never ran", n)
	}
}

package main

import (
	"context"
	"strings"
	"testing"
)

// mustDepend wires the dependency through the API rather than setting DependsOn
// on the fixture: the fake fills DependsOn from its own map on every read, the
// way the real service does, so a field set at construction is silently dropped.
func mustDepend(t *testing.T, api *CodeArmory, ticketID, blockerID string) {
	t.Helper()
	if err := api.AddDependency(context.Background(), ticketID, blockerID); err != nil {
		t.Fatalf("AddDependency(%s -> %s) = %v", ticketID, blockerID, err)
	}
}

// A ticket stranded behind a blocker that later recovered must come back.
//
// The bug this pins: the cascade healed in one direction only. Stranding moves a
// ticket to blocked, and no dispatcher polls blocked, so a blocker reaching done
// — the ordinary outcome of reviving it — left the tickets behind it parked for
// the rest of the run. Measured on r70: blocked=12 done=6, nothing running,
// nothing wrong, and it had to be put back by hand.
func TestUnstrandReturnsTicketsWhoseBlockerRecovered(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "waiter", Status: ColBlocked, CreatedBy: "alice"})
	f.addTicket(t, Ticket{TicketID: "blocker", Title: "the blocker", Status: ColDone, CreatedBy: "alice"})
	mustDepend(t, api, "waiter", "blocker")
	f.seedComment(t, "waiter", "**Stranded.** waits on \"the blocker\".\n\n"+strandedMarker+ColReadyForSpec+" -->")

	d := NewDispatcher(api, nil, &stubHandler{role: "spec-agent"}, DispatcherOpts{Host: "osiris"})
	d.unstrand(context.Background())

	if got := f.get("waiter").Status; got != ColReadyForSpec {
		t.Fatalf("status = %q, want %q: a ticket stranded behind a recovered blocker never came back", got, ColReadyForSpec)
	}
	// The returned marker matters as much as the move: without it the attempts
	// spent before the strand still count, and a ticket that was never actually
	// attempted can arrive back at its queue already at the ceiling.
	var sawReturn bool
	for _, c := range f.get("waiter").Comments {
		if strings.Contains(c.Body, returnedMarker) {
			sawReturn = true
		}
	}
	if !sawReturn {
		t.Error("no returned marker written; the ticket comes back with its old attempts still charged")
	}
}

// Still blocked means still stranded. Unstranding on a blocker that has not
// recovered would put the ticket straight back into a queue it cannot leave,
// which is the churn the strand exists to stop.
func TestUnstrandLeavesTicketsWhoseBlockerIsStillBlocked(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "waiter", Status: ColBlocked, CreatedBy: "alice"})
	f.addTicket(t, Ticket{TicketID: "blocker", Title: "the blocker", Status: ColBlocked, CreatedBy: "alice"})
	mustDepend(t, api, "waiter", "blocker")
	f.seedComment(t, "waiter", "**Stranded.**\n\n"+strandedMarker+ColReadyForSpec+" -->")

	d := NewDispatcher(api, nil, &stubHandler{role: "spec-agent"}, DispatcherOpts{Host: "osiris"})
	d.unstrand(context.Background())

	if got := f.get("waiter").Status; got != ColBlocked {
		t.Errorf("status = %q, want it left blocked: the blocker has not recovered", got)
	}
}

// ONE STAGE TAKES IT, not seven. Every dispatcher sweeps the same blocked column,
// so the column recorded in the marker is what keeps them from fighting over the
// same ticket and bouncing it between queues.
func TestUnstrandIgnoresOtherStagesTickets(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "waiter", Status: ColBlocked, CreatedBy: "alice"})
	f.addTicket(t, Ticket{TicketID: "blocker", Title: "b", Status: ColDone, CreatedBy: "alice"})
	mustDepend(t, api, "waiter", "blocker")
	f.seedComment(t, "waiter", "**Stranded.**\n\n"+strandedMarker+ColReadyForDev+" -->")

	d := NewDispatcher(api, nil, &stubHandler{role: "spec-agent"}, DispatcherOpts{Host: "osiris"})
	d.unstrand(context.Background())

	if got := f.get("waiter").Status; got != ColBlocked {
		t.Errorf("status = %q, want it left for the dev stage: the spec stage claimed another queue's ticket", got)
	}
}

// A ticket blocked by EXHAUSTION has no stranding marker and must stay put — that
// one is a real answer waiting on a person, not a casualty of someone else's
// failure.
func TestUnstrandLeavesExhaustedTickets(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "spent", Status: ColBlocked, CreatedBy: "alice"})

	d := NewDispatcher(api, nil, &stubHandler{role: "spec-agent"}, DispatcherOpts{Host: "osiris"})
	d.unstrand(context.Background())

	if got := f.get("spent").Status; got != ColBlocked {
		t.Errorf("status = %q, want it left blocked: an exhausted ticket was revived as though it had been stranded", got)
	}
}

// The last word wins. A ticket can be stranded, revived, worked and stranded
// again; anything that moved it forward since settles the question, or a stale
// note would drag it out of a queue it is legitimately sitting in.
func TestStrandedFromIsSupersededByLaterProgress(t *testing.T) {
	tk := Ticket{Comments: []Comment{
		{CommentID: "c1", Body: strandedMarker + ColReadyForSpec + " -->"},
		{CommentID: "c2", Body: "**Unstranded.** back you go\n\n" + returnedMarker},
	}}
	if col, ok := strandedFrom(tk); ok {
		t.Errorf("strandedFrom() = %q, true; want false after a return superseded the strand", col)
	}
	tk.Comments = append(tk.Comments, Comment{CommentID: "c3", Body: strandedMarker + ColReadyForDev + " -->"})
	if col, ok := strandedFrom(tk); !ok || col != ColReadyForDev {
		t.Errorf("strandedFrom() = %q, %v; want the most recent strand", col, ok)
	}
}

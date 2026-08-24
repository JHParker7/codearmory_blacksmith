package main

import (
	"testing"
	"time"
)

// THE BOARD ANSWERS "WHAT IS HAPPENING NOW".
//
// Once several runs had been through one board the view stopped answering that.
// rank takes the most urgent descendant, which is right within a run and wrong
// between them: one blocked section in a run that ended days ago ranks the whole
// dead run above today's work, because a blocked ticket is the most urgent thing
// there is.

// run builds a root and its children at one remove.
func run(id string, rootStatus string, childStatuses ...string) []Ticket {
	out := []Ticket{{TicketID: id, Title: "request " + id, Status: rootStatus, CreatedAt: time.Now()}}
	for i, st := range childStatuses {
		parent := id
		out = append(out, Ticket{
			TicketID: id + "-c" + string(rune('a'+i)), Title: "task", Status: st,
			ParentID: &parent, CreatedAt: time.Now(),
		})
	}
	return out
}

func TestALiveRunSortsAboveAFinishedOne(t *testing.T) {
	rows := append(run("old00000", ColDone, ColDone, ColDone), run("new00000", ColTracking, ColInDev, ColReadyForDev)...)

	got, _, _, _ := orderByParent(rows, true)

	if got[0].TicketID != "new00000" {
		t.Errorf("first row is %q, want the live run; the board opens on what is happening now", got[0].TicketID)
	}
}

// THE CASE THAT MADE IT MUDDY. A blocked ticket is the most urgent thing on a
// board, so a finished run containing one used to outrank everything.
func TestAFinishedRunWithABlockedTicketStaysBelowLiveWork(t *testing.T) {
	rows := append(run("old00000", ColDone, ColDone, ColBlocked), run("new00000", ColTracking, ColInDev)...)

	got, _, _, _ := orderByParent(rows, true)

	if got[0].TicketID != "new00000" {
		t.Errorf("first row is %q; a blocked ticket in a dead run outranked today's work", got[0].TicketID)
	}
}

// Finished runs are history, not the board — off unless asked for.
func TestFinishedRunsAreHiddenAndCounted(t *testing.T) {
	rows := append(run("old00000", ColDone, ColDone), run("new00000", ColTracking, ColInDev)...)

	got, _, _, hidden := orderByParent(rows, false)

	for _, r := range got {
		if r.TicketID == "old00000" {
			t.Error("a finished run was listed with the toggle off")
		}
	}
	if hidden != 1 {
		t.Errorf("hidden = %d, want 1: a board that silently drops rows is worse than a busy one", hidden)
	}
	// And showing them brings it back, or the toggle is a delete.
	shown, _, _, _ := orderByParent(rows, true)
	if len(shown) <= len(got) {
		t.Error("turning the toggle on did not bring the finished run back")
	}
}

// A RUN ENDING IN A BLOCK IS ALSO OVER. Nothing under it can move on its own, so
// leaving it on the board is the same noise as leaving a finished one.
func TestARunThatEndedBlockedCountsAsFinished(t *testing.T) {
	rows := run("old00000", ColTracking, ColDone, ColBlocked)

	if !runFinished(rows[0], map[string][]Ticket{"old00000": rows[1:]}) {
		t.Error("a run whose remaining work is blocked was treated as live")
	}
}

// A run with anything still moving is live, however much of it is done.
func TestARunWithWorkLeftIsLive(t *testing.T) {
	rows := run("new00000", ColTracking, ColDone, ColDone, ColInDev)

	if runFinished(rows[0], map[string][]Ticket{"new00000": rows[1:]}) {
		t.Error("a run with a ticket in development was treated as finished")
	}
}

// A lone request nobody has scoped yet is live: it is waiting to be picked up,
// which is the most interesting state a new request can be in.
func TestAnUnscopedRequestIsLive(t *testing.T) {
	if runFinished(Ticket{TicketID: "req00000", Status: ColInbox}, nil) {
		t.Error("a request waiting in the inbox was hidden as finished")
	}
}

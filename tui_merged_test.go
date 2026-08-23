package main

import "testing"

// MERGED IS NOT BUILT.
//
// The ticket-merge stage folds several tickets into one and closes the rest, so
// four of them reach done within seconds of the product manager finishing — which
// on a board of "done" labels reads as four pieces of work completed instantly.
//
// Reported from the window on r90. The distinction was in the ticket's comment
// and nowhere on the row, which is where it gets read.

func TestAMergedTicketIsNotShownAsFinishedWork(t *testing.T) {
	tk := Ticket{TicketID: "task0002", Status: ColDone, Comments: []Comment{
		{Body: mergedIntoMarker + "\n\nIts requirements were carried into `task0001` unchanged."},
	}}
	if !mergedAway(tk) {
		t.Error("a merged ticket reads as ordinary finished work; four of them land at once and look built")
	}
	if got := mergedInto(tk); got != "task0001" {
		t.Errorf("mergedInto = %q, want the ticket that absorbed it", got)
	}
}

// Work that genuinely finished must still read as finished.
func TestRealFinishedWorkIsUnaffected(t *testing.T) {
	tk := Ticket{TicketID: "task0001", Status: ColDone, Comments: []Comment{
		{Body: branchMarker + " pushed"},
		{Body: mergedFromMarker + " this ticket carries the work of 5 tasks"},
	}}
	if mergedAway(tk) {
		t.Error("the ticket that ABSORBED the others reads as merged away; it is the one doing the work")
	}
}

// A merge comment that cannot be parsed still says merged rather than done —
// which is the part that matters.
func TestAnUnparseableMergeCommentStillReadsAsMerged(t *testing.T) {
	tk := Ticket{TicketID: "task0003", Status: ColDone, Comments: []Comment{
		{Body: mergedIntoMarker + " no id here"},
	}}
	if !mergedAway(tk) {
		t.Error("a merged ticket with an odd comment fell back to looking finished")
	}
	if got := mergedInto(tk); got == "" {
		t.Error("mergedInto returned nothing; the row would say \"merged into \"")
	}
}

package main

import "testing"

// A SECTION SENT STRAIGHT TO INTEGRATION MUST BE ONE THE INTEGRATOR WILL TAKE.
//
// A section whose tests already pass against the code on the branch is routed to
// ready_for_integration so the tests themselves get merged. It arrives carrying
// the test author's marker and the already-satisfied note, and the integrator
// admits tickets on hasBranch — which looked only for the DEVELOPER's marker, one
// a specification author never writes.
//
// Measured on r86: the section sat in ready_for_integration for good, its task
// waited on "sections 0/1 done" that would never come, and the three tasks behind
// it never started. Every ticket was in a legitimate column and no stage had
// failed, so nothing anywhere reported a problem.

func TestAnAlreadySatisfiedSectionIsMergeable(t *testing.T) {
	tk := Ticket{Comments: []Comment{
		{Body: testsWrittenMarker + " wrote them"},
		{Body: alreadySatisfiedMarker + " nothing to implement"},
	}}
	if !hasBranch(tk) {
		t.Error("the integrator will not take a section routed to it; the board deadlocks behind that task")
	}
}

// AND A TESTS-ONLY BRANCH STILL IS NOT. This is the distinction the whole fix
// turns on: failing tests with no implementation must never look mergeable, or
// the integrator merges unfinished work.
func TestATestsOnlyBranchIsStillNotMergeable(t *testing.T) {
	tk := Ticket{Comments: []Comment{{Body: testsWrittenMarker + " wrote them"}}}
	if hasBranch(tk) {
		t.Error("a branch carrying only failing tests reads as mergeable; the integrator would take unfinished work")
	}
}

// The developer's ordinary push is unaffected.
func TestADevelopersPushIsStillMergeable(t *testing.T) {
	tk := Ticket{Comments: []Comment{{Body: branchMarker + " pushed"}}}
	if !hasBranch(tk) {
		t.Error("the developer's push no longer registers as a branch to integrate")
	}
}

// A ticket nothing has pushed is not mergeable either.
func TestAnUntouchedTicketHasNoBranch(t *testing.T) {
	if hasBranch(Ticket{Comments: []Comment{{Body: "**Triage** some notes"}}}) {
		t.Error("a ticket with no push at all reads as having a branch")
	}
}

package main

import (
	"context"
	"strings"
	"testing"
)

// A BLOCK IS USUALLY A STAGE HAVING BEEN WRONG, NOT AN ANSWER.
//
// handleDeadPrerequisite already revives a blocked ticket that something else
// waits on, because the dependents make it urgent. A ticket nobody depends on —
// or the last one in a chain — had no way back at all, and stayed blocked until a
// person noticed, however transient the cause.
//
// Measured on r79: a reviewer answered that a specification could not be
// implemented while its own reason said every assertion was satisfiable, and five
// tasks blocked behind that one misread verdict. Nothing was wrong with any of
// them and there was no retry to find that out.

func blockedTicket(t *testing.T, f *fakePlatform, api *CodeArmory, id string) {
	t.Helper()
	f.addTicket(t, Ticket{TicketID: id, Title: "a task", Status: ColBlocked, CreatedBy: "alice"})
	// The claim history is what says which stage owns it — a task and a section
	// look alike, and only their history tells them apart.
	f.addClaim(t, id, "osiris", "dev-agent", f.tick())
}

func devDispatcher(api *CodeArmory) *Dispatcher {
	return NewDispatcher(api, nil, &stubHandler{role: "dev-agent"}, DispatcherOpts{Host: "osiris"})
}

func TestABlockedTicketIsPutBackForAnotherAttempt(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	blockedTicket(t, f, api, "task0001")

	devDispatcher(api).retryBlocked(context.Background())

	if got := f.get("task0001").Status; got != ColReadyForDev {
		t.Fatalf("status = %q, want %q: a blocked ticket nobody depends on had no way back", got, ColReadyForDev)
	}
	// The budget has to be reset with it, or the retry buys one turn.
	var reset bool
	for _, c := range f.get("task0001").Comments {
		if strings.Contains(c.Body, retryMarker) && strings.Contains(c.Body, returnedMarker) {
			reset = true
		}
	}
	if !reset {
		t.Error("the retry did not carry a returned marker, so the attempt count was not reset")
	}
}

// THREE AND NO MORE. A ticket that fails the same way three times is telling you
// something a fourth will not, and a queue that never gives up looks identical to
// one that is working.
func TestARetriedTicketStopsAfterThree(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	blockedTicket(t, f, api, "task0001")
	for range maxBlockedRetries {
		f.seedComment(t, "task0001", retryMarker+" an earlier go")
	}

	devDispatcher(api).retryBlocked(context.Background())

	if got := f.get("task0001").Status; got != ColBlocked {
		t.Errorf("status = %q, want %q: it has had its three and a person decides now", got, ColBlocked)
	}
}

// THE PATH IS RESET, NOT JUST THE QUEUE. A ticket put back with its hand-backs
// already spent gets one turn and blocks again, which is a slower way to reach
// the same place rather than a retry.
func TestARetryResetsTheSpecificationHandBacks(t *testing.T) {
	tk := Ticket{TicketID: "task0001", Comments: []Comment{
		{Body: specRepairMarker + " first"},
		{Body: specRepairMarker + " second"},
		{Body: retryMarker + " put back"},
	}}
	if got := specRepairsSoFar(tk); got != 0 {
		t.Errorf("spec repairs = %d after a retry, want 0; the ticket arrives with its budget already spent", got)
	}
	// And a hand-back after the retry counts again, or the reset would be a
	// permanent exemption.
	tk.Comments = append(tk.Comments, Comment{Body: specRepairMarker + " after"})
	if got := specRepairsSoFar(tk); got != 1 {
		t.Errorf("spec repairs = %d, want 1", got)
	}
}

// A ticket stranded behind a blocker belongs to unstrand: putting it back before
// its blocker recovers only fails it again.
func TestAStrandedTicketIsLeftToUnstrand(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	// NO CLAIM: a stranded ticket was stranded precisely because nothing ever got
	// to work on it. Measured on r70, all twelve had zero claims between them —
	// and a claim is what settles the question more recently than the stranding
	// note, so adding one here would describe a ticket that cannot exist.
	f.addTicket(t, Ticket{TicketID: "task0001", Title: "a task", Status: ColBlocked, CreatedBy: "alice"})
	f.seedComment(t, "task0001", "**Stranded.** waits on \"something\".\n\n"+strandedMarker+ColReadyForDev+" -->")

	devDispatcher(api).retryBlocked(context.Background())

	if got := f.get("task0001").Status; got != ColBlocked {
		t.Errorf("status = %q: a stranded ticket was retried instead of being left to unstrand", got)
	}
}

// Each stage takes only its own. A task retried into the section author's queue
// was measured being read a hundred times and killed.
func TestAStageDoesNotRetryAnotherStagesTicket(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	blockedTicket(t, f, api, "task0001")

	other := NewDispatcher(api, nil, &stubHandler{role: "spec-agent"}, DispatcherOpts{Host: "osiris"})
	other.retryBlocked(context.Background())

	if got := f.get("task0001").Status; got != ColBlocked {
		t.Errorf("the spec stage retried a ticket the dev stage owns (status %q)", got)
	}
}

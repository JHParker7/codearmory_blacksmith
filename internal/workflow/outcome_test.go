package workflow

import "testing"

func TestDestinationRoutesEachOutcome(t *testing.T) {
	rev, _ := New(Options{}).For(RoleReview)

	cases := []struct {
		outcome     Outcome
		retriesLeft bool
		want        string
		why         string
	}{
		{OutcomeSuccess, true, rev.Success, "success moves forward"},
		{OutcomeConflicted, true, ColConflicted, "a conflict belongs to the resolver, not to the stage that hit it"},
		{OutcomeBlocked, true, rev.Exhausted, "blocked skips the retries it cannot use"},
		{OutcomeReturned, true, rev.Returns, "a rejected change goes back to the developer"},
		{OutcomeAbandoned, false, rev.Ready, "a shut-down host must not spend the ticket's last attempt"},
		{OutcomeFailed, true, rev.Ready, "a failure with attempts left is retried"},
		{OutcomeFailed, false, rev.Exhausted, "a failure with none left goes in front of a person"},
	}
	for _, c := range cases {
		if got := rev.Destination(c.outcome, c.retriesLeft); got != c.want {
			t.Errorf("Destination(%q, retriesLeft=%v) = %q, want %q — %s",
				c.outcome, c.retriesLeft, got, c.want, c.why)
		}
	}
}

// HANDLED MEANS DO NOT MOVE IT. The stage placed the ticket itself, and a
// dispatcher that moved it anyway would silently undo the handler's decision —
// which is exactly what the product manager does with a request that needs no
// breakdown.
func TestHandledLeavesTheTicketWhereTheStagePutIt(t *testing.T) {
	pm, _ := New(Options{}).For(RoleScoping)
	if got := pm.Destination(OutcomeHandled, true); got != "" {
		t.Errorf("Destination(handled) = %q, want empty; the stage already placed the ticket", got)
	}
}

// ABANDONED IS NOT FAILED. The host went away mid-task, which is not the agent
// getting it wrong, so the work goes back to its queue even with no attempts
// left — routing it to the exhausted column would spend a ticket on a shutdown.
func TestAbandonedGoesBackToTheQueueEvenWithNoAttemptsLeft(t *testing.T) {
	dev, _ := New(Options{}).For(RoleDev)
	if got := dev.Destination(OutcomeAbandoned, false); got != dev.Ready {
		t.Errorf("an abandoned task went to %q, want its queue %q", got, dev.Ready)
	}
	if got := dev.Destination(OutcomeFailed, false); got == dev.Ready {
		t.Error("a failure with no attempts left was retried; abandoned and failed must differ")
	}
}

// A RETURN FROM A STAGE THAT CANNOT RETURN goes in front of a person. Falling
// through to "do not move it" would strand the ticket in the working column with
// nothing coming to collect it — a stall that reads exactly like work in
// progress.
func TestAReturnFromAStageWithNowhereToSendItIsNotSilent(t *testing.T) {
	author, _ := New(Options{}).For(RoleTest)
	if author.Returns != "" {
		t.Fatalf("this test assumes the task author cannot return work; it returns to %q", author.Returns)
	}
	got := author.Destination(OutcomeReturned, true)
	if got == "" {
		t.Fatal("a return from a stage with no Returns column left the ticket in the working column")
	}
	if got != author.Exhausted {
		t.Errorf("Destination(returned) = %q on a stage that cannot return, want %q", got, author.Exhausted)
	}
}

// An outcome nothing recognises must not strand the ticket either: it is treated
// as a failure, which retries or escalates rather than going quiet.
func TestAnUnknownOutcomeIsTreatedAsAFailure(t *testing.T) {
	dev, _ := New(Options{}).For(RoleDev)
	if got := dev.Destination("who-knows", true); got != dev.Ready {
		t.Errorf("an unrecognised outcome routed to %q, want the retry queue %q", got, dev.Ready)
	}
	if got := dev.Destination("who-knows", false); got != dev.Exhausted {
		t.Errorf("an unrecognised outcome with no attempts left routed to %q, want %q", got, dev.Exhausted)
	}
}

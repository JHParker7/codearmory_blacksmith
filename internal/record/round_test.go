package record_test

import (
	"testing"
	"time"

	"github.com/code-armory-app/blacksmith/internal/record"
	"github.com/code-armory-app/blacksmith/internal/ticket"
)

func claimAt(t *testing.T, id string, at time.Time, host, role string) ticket.Comment {
	t.Helper()
	body, err := record.Render(record.Claim{Host: host, Role: role, RunID: "r"})
	if err != nil {
		t.Fatalf("render claim: %v", err)
	}
	return ticket.Comment{ID: id, Body: body, CreatedAt: at}
}

func closeAt(t *testing.T, id string, at time.Time, role string) ticket.Comment {
	t.Helper()
	body, err := record.CloseClaim(role)
	if err != nil {
		t.Fatalf("render close: %v", err)
	}
	return ticket.Comment{ID: id, Body: body, CreatedAt: at}
}

// THE STALL THIS EXISTS FOR. A dev-agent failed once, and every later attempt
// read back its own dead claim as the winner and yielded — so the ticket sat in
// ready_for_dev collecting a claim comment per poll, never moving and never
// escalating. Thirty seconds of the integration plane produced one attempt and
// three claims.
func TestARetryDoesNotLoseToItsOwnPreviousClaim(t *testing.T) {
	base := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	comments := []ticket.Comment{
		claimAt(t, "c-1", base, "host-a", "dev-agent"),
		closeAt(t, "c-2", base.Add(time.Minute), "dev-agent"),
		claimAt(t, "c-3", base.Add(2*time.Minute), "host-a", "dev-agent"),
	}

	winner, ok := record.OldestForRole(comments, "dev-agent")
	if !ok {
		t.Fatal("no claim won at all, so the retry yields and the ticket stalls")
	}
	if winner.ID != "c-3" {
		t.Fatalf("the second attempt lost to its own closed claim %s; it will yield, "+
			"re-claim on the next poll and stall the ticket in its queue", winner.ID)
	}
}

// The close must not disarm arbitration for a round that is genuinely contested.
func TestTwoHostsRacingInsideOneRoundStillAgreeOnOneWinner(t *testing.T) {
	base := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	comments := []ticket.Comment{
		claimAt(t, "c-1", base, "host-a", "dev-agent"),
		closeAt(t, "c-2", base.Add(time.Minute), "dev-agent"),
		claimAt(t, "c-4", base.Add(2*time.Minute), "host-b", "dev-agent"),
		claimAt(t, "c-3", base.Add(2*time.Minute), "host-a", "dev-agent"),
	}

	winner, ok := record.OldestForRole(comments, "dev-agent")
	if !ok {
		t.Fatal("a contested round produced no winner, so BOTH hosts yield")
	}
	// Same instant, so the tie-break decides — and it must decide the same way
	// whichever order the two hosts read the list in.
	if winner.ID != "c-3" {
		t.Fatalf("the tie-break did not pick the lowest id: got %s, want c-3", winner.ID)
	}
}

// A close names its role, and closing one role's round must not release
// another's. The stages run concurrently on the same board.
func TestClosingOneRolesRoundLeavesAnotherRolesClaimStanding(t *testing.T) {
	base := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	comments := []ticket.Comment{
		claimAt(t, "c-1", base, "host-a", "sec-agent"),
		closeAt(t, "c-2", base.Add(time.Minute), "dev-agent"),
	}

	winner, ok := record.OldestForRole(comments, "sec-agent")
	if !ok || winner.ID != "c-1" {
		t.Fatalf("the reviewer's live claim was released by the developer's close; "+
			"two hosts can now work the same ticket (ok=%v)", ok)
	}
}

// A close is round bookkeeping, NOT forgiveness. The attempt was really spent,
// and counting it is what stops a ticket that cannot succeed from retrying
// forever — measured as thirty attempts in thirty seconds with no escalation.
func TestClosingARoundDoesNotRefundTheAttempt(t *testing.T) {
	base := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	tk := ticket.Ticket{Comments: []ticket.Comment{
		claimAt(t, "c-1", base, "host-a", "dev-agent"),
		closeAt(t, "c-2", base.Add(time.Minute), "dev-agent"),
		claimAt(t, "c-3", base.Add(2*time.Minute), "host-a", "dev-agent"),
		closeAt(t, "c-4", base.Add(3*time.Minute), "dev-agent"),
	}}

	if got := record.Attempts(tk, "dev-agent"); got != 2 {
		t.Fatalf("two attempts were made and %d counted; a close that refunds the "+
			"attempt makes the ceiling unreachable and the ticket never escalates", got)
	}
}

// An unreadable close must not be read as "every round is over": that would
// discard a live claim and let a second host onto a ticket already being worked.
func TestAnUnreadableCloseReleasesNothing(t *testing.T) {
	base := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	comments := []ticket.Comment{
		claimAt(t, "c-1", base, "host-a", "dev-agent"),
		{ID: "c-2", Body: record.ClaimClosedMarker + "{not json", CreatedAt: base.Add(time.Minute)},
		claimAt(t, "c-3", base.Add(2*time.Minute), "host-b", "dev-agent"),
	}

	winner, ok := record.OldestForRole(comments, "dev-agent")
	if !ok || winner.ID != "c-1" {
		t.Fatalf("a malformed close released host-a's live claim, so host-b now "+
			"works a ticket host-a is already working (ok=%v)", ok)
	}
}

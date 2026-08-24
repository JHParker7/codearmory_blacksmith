package record

import (
	"strings"
	"testing"
	"time"

	"github.com/code-armory-app/blacksmith/internal/ticket"
)

var base = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func claimComment(id, role, host string, at time.Time) ticket.Comment {
	body, err := Render(Claim{Host: host, Role: role, RunID: "r1"})
	if err != nil {
		panic(err)
	}
	return ticket.Comment{ID: id, Body: body, CreatedAt: at}
}

func TestRenderAndParseRoundTrip(t *testing.T) {
	want := Claim{Host: "gpu-1", Role: "dev-agent", RunID: "r42"}
	body, err := Render(want)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !IsClaim(body) {
		t.Fatalf("Render produced a body IsClaim does not recognise: %q", body)
	}
	got, err := Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got != want {
		t.Errorf("Parse(Render(%+v)) = %+v", want, got)
	}
}

// A CLAIM MUST NOT BE VISIBLE ON THE BOARD. It is machinery, and the marker is
// an HTML comment so a person reading the ticket does not have to skim past it.
func TestAClaimIsWrittenAsAnInvisibleComment(t *testing.T) {
	body, _ := Render(Claim{Host: "h", Role: "r"})
	if !strings.HasPrefix(body, "<!--") || !strings.HasSuffix(body, "-->") {
		t.Errorf("a claim renders as %q, which a person reading the ticket would see", body)
	}
}

func TestParseRejectsWhatIsNotAClaim(t *testing.T) {
	for _, body := range []string{
		"",
		"just a comment",
		BranchMarker,
		ClaimMarker + "{\"host\":\"h\"",  // truncated payload, no terminator
		ClaimMarker + "not json -->",     // terminator, unreadable payload
		"<!-- blacksmith:claimed {} -->", // near miss on the marker
	} {
		if _, err := Parse(body); err == nil {
			t.Errorf("Parse(%q) succeeded; a body that is not a claim must not read as one", body)
		}
	}
}

// THE TIE-BREAK IS THE ARBITRATION. Two hosts writing in the same instant must
// still reach the SAME answer, or each picks itself and both proceed — the one
// unrecoverable outcome of the append protocol.
func TestTheOldestClaimIsTheSameOneWhicheverHostAsks(t *testing.T) {
	a := claimComment("c-b", "dev-agent", "host-b", base)
	b := claimComment("c-a", "dev-agent", "host-a", base) // same instant, lower id

	fromA, ok := Oldest([]ticket.Comment{a, b})
	if !ok {
		t.Fatal("Oldest found no claim")
	}
	fromB, ok := Oldest([]ticket.Comment{b, a}) // the other host's read order
	if !ok {
		t.Fatal("Oldest found no claim")
	}
	if fromA.ID != fromB.ID {
		t.Fatalf("the two hosts disagree on the winner: %q and %q — both would proceed", fromA.ID, fromB.ID)
	}
	if fromA.ID != "c-a" {
		t.Errorf("the winner is %q; ties must break on comment id so the rule is deterministic", fromA.ID)
	}
}

func TestTheEarliestClaimWinsRegardlessOfReadOrder(t *testing.T) {
	late := claimComment("c-2", "dev-agent", "host-late", base.Add(time.Second))
	early := claimComment("c-1", "dev-agent", "host-early", base)
	got, ok := Oldest([]ticket.Comment{late, early})
	if !ok {
		t.Fatal("Oldest found no claim")
	}
	if got.ID != "c-1" {
		t.Errorf("Oldest picked %q, want the earliest claim", got.ID)
	}
}

// ARBITRATION IS PER ROLE. Comparing against every claim would mean losing to
// the stage before — the product manager's claim is older than the developer's
// and would always win, so the developer never gets a ticket that was scoped.
func TestArbitrationIgnoresTheStageBefore(t *testing.T) {
	comments := []ticket.Comment{
		claimComment("c-1", "pm-agent", "host-a", base),
		claimComment("c-2", "dev-agent", "host-b", base.Add(time.Minute)),
		claimComment("c-3", "dev-agent", "host-c", base.Add(2*time.Minute)),
	}
	got, ok := OldestForRole(comments, "dev-agent")
	if !ok {
		t.Fatal("OldestForRole found no developer claim")
	}
	if got.ID != "c-2" {
		t.Errorf("the developer's arbitration picked %q; the product manager's older claim must not win it", got.ID)
	}
	if _, ok := OldestForRole(comments, "sec-agent"); ok {
		t.Error("OldestForRole found a claim for a role that never claimed")
	}
}

// A MALFORMED CLAIM MUST NOT WIN A RACE IT CANNOT NAME. Unparseable means the
// role is unknown, so it cannot be shown to belong to this arbitration.
func TestAnUnreadableClaimDoesNotWinARoleRace(t *testing.T) {
	comments := []ticket.Comment{
		{ID: "c-1", Body: ClaimMarker + "garbage -->", CreatedAt: base},
		claimComment("c-2", "dev-agent", "host-b", base.Add(time.Minute)),
	}
	got, ok := OldestForRole(comments, "dev-agent")
	if !ok {
		t.Fatal("OldestForRole found no claim")
	}
	if got.ID != "c-2" {
		t.Errorf("an unparseable claim won the developer's race as %q", got.ID)
	}
}

// THE COLUMN DECIDES WHETHER A TICKET IS HELD, the comment only records by whom.
// Comments are append-only, so a ticket carries every claim ever made against it
// — and reading "claimed" off that history means it looks claimed forever after
// its first stage, locking each stage out of what the one before handed over.
func TestHeldByNeedsTheColumnToSayItIsHeld(t *testing.T) {
	t1 := ticket.Ticket{Comments: []ticket.Comment{claimComment("c-1", "dev-agent", "gpu-1", base)}}

	if _, ok := HeldBy(t1, false); ok {
		t.Error("HeldBy reported a holder for a ticket sitting in a queue; its claims are history")
	}
	got, ok := HeldBy(t1, true)
	if !ok {
		t.Fatal("HeldBy found no holder for a ticket a stage is working")
	}
	if got.Host != "gpu-1" || got.Role != "dev-agent" {
		t.Errorf("HeldBy = %+v, want the claim that was written", got)
	}
	if _, ok := HeldBy(ticket.Ticket{}, true); ok {
		t.Error("HeldBy invented a holder for a ticket with no claims")
	}
}

func TestAttemptsCountsOnlyThisRole(t *testing.T) {
	tk := ticket.Ticket{Comments: []ticket.Comment{
		claimComment("c-1", "dev-agent", "h", base),
		claimComment("c-2", "sec-agent", "h", base.Add(time.Minute)),
		claimComment("c-3", "dev-agent", "h", base.Add(2*time.Minute)),
		{ID: "c-4", Body: "an ordinary comment", CreatedAt: base.Add(3 * time.Minute)},
	}}
	if got := Attempts(tk, "dev-agent"); got != 2 {
		t.Errorf("Attempts(dev) = %d, want 2", got)
	}
	if got := Attempts(tk, "sec-agent"); got != 1 {
		t.Errorf("Attempts(sec) = %d, want 1", got)
	}
	if got := Attempts(tk, "coverage-agent"); got != 0 {
		t.Errorf("Attempts(coverage) = %d, want 0", got)
	}
}

// A HAND-BACK RESTARTS THE COUNT. The ticket goes back a stage and comes forward
// again, and the attempts it spent getting rejected must not be charged against
// the corrected version — a ticket that reached its ceiling for BOTH roles sat
// in ready_for_dev unclaimable, with nothing in any counter to say why.
func TestAHandBackRestartsTheCount(t *testing.T) {
	for _, marker := range resetsAttempts {
		tk := ticket.Ticket{Comments: []ticket.Comment{
			claimComment("c-1", "dev-agent", "h", base),
			claimComment("c-2", "dev-agent", "h", base.Add(time.Minute)),
			{ID: "c-3", Body: marker, CreatedAt: base.Add(2 * time.Minute)},
			claimComment("c-4", "dev-agent", "h", base.Add(3*time.Minute)),
		}}
		if got := Attempts(tk, "dev-agent"); got != 1 {
			t.Errorf("after %q the developer has %d attempts spent, want 1", marker, got)
		}
	}
}

// THE COUNT DEPENDS ON WHICH SIDE OF A HAND-BACK EACH CLAIM FALLS, so order is
// established here rather than trusted from the platform.
func TestAttemptsSortsBeforeCounting(t *testing.T) {
	// Arranged so that reading in ARRIVAL order gives a different answer from
	// reading in time order: unsorted, the hand-back comes first and clears
	// nothing, and all three claims are counted.
	tk := ticket.Ticket{Comments: []ticket.Comment{
		{ID: "c-3", Body: ReturnedMarker, CreatedAt: base.Add(2 * time.Minute)},
		claimComment("c-1", "dev-agent", "h", base),
		claimComment("c-2", "dev-agent", "h", base.Add(time.Minute)),
		claimComment("c-4", "dev-agent", "h", base.Add(3*time.Minute)),
	}}
	if got := Attempts(tk, "dev-agent"); got != 1 {
		t.Errorf("Attempts on out-of-order comments = %d, want 1 — only the claim after the hand-back is spent", got)
	}
}

func TestAttemptsDoesNotDisturbTheTicket(t *testing.T) {
	tk := ticket.Ticket{Comments: []ticket.Comment{
		claimComment("c-2", "dev-agent", "h", base.Add(time.Minute)),
		claimComment("c-1", "dev-agent", "h", base),
	}}
	_ = Attempts(tk, "dev-agent")
	if tk.Comments[0].ID != "c-2" {
		t.Error("Attempts reordered the caller's comments; the transcript reads them in arrival order")
	}
}

// A CLAIM IS WRITTEN WHEN WORK STARTS and nothing closes it if the process dies.
// Three interrupted runs make a ticket permanently unclaimable with nothing on
// the board to say why — measured, and self-inflicted by restarting to install
// fixes.
func TestPruneStaleForgivesClaimsWhoseProcessIsGone(t *testing.T) {
	now := base.Add(2 * time.Hour)
	tk := ticket.Ticket{Comments: []ticket.Comment{
		claimComment("old", "dev-agent", "h", now.Add(-StaleAfter-time.Minute)),
		claimComment("fresh", "dev-agent", "h", now.Add(-time.Minute)),
		{ID: "note", Body: "an ordinary comment", CreatedAt: now.Add(-2 * time.Hour)},
	}}

	got := PruneStale(tk, now)
	if n := Attempts(got, "dev-agent"); n != 1 {
		t.Errorf("after pruning, the developer has %d attempts spent, want 1", n)
	}
	// ONLY CLAIMS ARE PRUNED. The rest of the history is the audit trail, and a
	// hand-back marker dropped for being old would silently un-reset the count.
	if len(got.Comments) != 2 {
		t.Errorf("PruneStale kept %d comments, want the fresh claim and the note", len(got.Comments))
	}
	if len(tk.Comments) != 3 {
		t.Error("PruneStale modified the ticket it was given")
	}
}

// A claim exactly at the boundary is not yet stale: the rule is "longer than",
// and forgiving one early un-holds work still being worked.
func TestPruneStaleIsExclusiveAtTheBoundary(t *testing.T) {
	now := base.Add(2 * time.Hour)
	tk := ticket.Ticket{Comments: []ticket.Comment{
		claimComment("edge", "dev-agent", "h", now.Add(-StaleAfter)),
	}}
	if n := Attempts(PruneStale(tk, now), "dev-agent"); n != 1 {
		t.Errorf("a claim exactly at the ceiling was pruned; it is not yet abandoned")
	}
}

// A TIMESTAMP THAT IS NOT A REAL TIME CARRIES NO LIVENESS INFORMATION. Reading a
// zero value as "very old" forgives every claim ever made, which un-holds work
// that is actively being worked — the direction that hurts.
func TestPruneStaleKeepsClaimsWithNoUsableTimestamp(t *testing.T) {
	now := base.Add(2 * time.Hour)
	for _, ts := range []time.Time{{}, time.Unix(0, 0).UTC()} {
		tk := ticket.Ticket{Comments: []ticket.Comment{
			claimComment("unstamped", "dev-agent", "h", ts),
		}}
		if n := Attempts(PruneStale(tk, now), "dev-agent"); n != 1 {
			t.Errorf("a claim stamped %v was pruned; an unpopulated field is not evidence of age", ts)
		}
	}
}

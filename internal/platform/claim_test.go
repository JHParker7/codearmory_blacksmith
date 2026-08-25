package platform

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/code-armory-app/blacksmith/internal/record"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/transport"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

func devStage() workflow.Stage {
	st, ok := workflow.New(workflow.Options{}).For(workflow.RoleDev)
	if !ok {
		panic("no developer stage")
	}
	return st
}

func heldBy(t *testing.T, f *fakeStore, id string) string {
	t.Helper()
	tk, ok := f.get(id)
	if !ok {
		t.Fatalf("ticket %s is gone", id)
	}
	if tk.AssigneeID == nil {
		return ""
	}
	return *tk.AssigneeID
}

// THE COLUMN IS THE CLAIM: taking a ticket means moving it OUT of the queue, and
// the write that does so is the arbitration itself.
func TestClaimMovesTheTicketAndRecordsWhoHasIt(t *testing.T) {
	f, store := newFakeStore(t)
	st := devStage()
	tk := f.add(ticket.Ticket{Status: st.Ready})

	err := store.Claim(context.Background(), tk.ID, st, record.Claim{Host: "gpu-1", Role: st.Role, RunID: "r1"})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	got, _ := f.get(tk.ID)
	if got.Status != st.Working {
		t.Errorf("the ticket is in %q, want the working column %q", got.Status, st.Working)
	}
	if h := heldBy(t, f, tk.ID); h != st.Role+"@gpu-1" {
		t.Errorf("assignee = %q, want the role and host that took it", h)
	}
	// The comment is the audit trail AND the attempt counter — the account alone
	// cannot say which of several hosts running this role took the work.
	if n := record.Attempts(got, st.Role); n != 1 {
		t.Errorf("the ticket records %d attempts, want 1", n)
	}
}

// A TICKET THAT IS NOT IN THE QUEUE IS NOT AVAILABLE, and that is a lost race
// rather than a failure: another host moved it between the poll and the claim.
func TestClaimYieldsATicketThatHasAlreadyMoved(t *testing.T) {
	f, store := newFakeStore(t)
	st := devStage()
	tk := f.add(ticket.Ticket{Status: st.Working})

	err := store.Claim(context.Background(), tk.ID, st, record.Claim{Host: "gpu-2", Role: st.Role})
	if !transport.Conflict(err) {
		t.Fatalf("err = %v, want a conflict", err)
	}
	// The message must name the cause: which column it found, and which it wanted.
	if !strings.Contains(err.Error(), st.Working) || !strings.Contains(err.Error(), st.Ready) {
		t.Errorf("the error does not say which column it found: %v", err)
	}
}

// THE VERSION CHECK IS THE ARBITRATION. Two hosts both read the ticket in the
// queue and both try to move it; exactly one can win, because the version is in
// the where clause.
func TestTwoHostsRacingOnAVersionedStoreProduceOneWinner(t *testing.T) {
	f, store := newFakeStore(t)
	st := devStage()
	tk := f.add(ticket.Ticket{Status: st.Ready})
	ctx := context.Background()

	// THE OTHER HOST WINS BETWEEN THIS ONE'S READ AND ITS WRITE — which is the
	// only interleaving that reaches the version check at all. A competitor that
	// wins before the read is caught by the column check, and a test built that
	// way would pass with the conditional write removed entirely.
	f.afterRead = func(id string) {
		f.afterRead = nil
		f.mu.Lock()
		other := *f.tickets[id]
		f.mu.Unlock()
		if other.Status != st.Ready {
			return
		}
		f.mu.Lock()
		f.tickets[id].Status = st.Working
		f.tickets[id].AssigneeID = ticket.Ptr(st.Role + "@gpu-other")
		f.tickets[id].Version++
		f.mu.Unlock()
	}

	err := store.Claim(ctx, tk.ID, st, record.Claim{Host: "gpu-1", Role: st.Role})
	if !transport.Conflict(err) {
		t.Fatalf("the losing claim returned %v; both hosts believe they won", err)
	}
	if h := heldBy(t, f, tk.ID); h != st.Role+"@gpu-other" {
		t.Errorf("the ticket is held by %q, want the host that got there first", h)
	}
}

// A stale version must lose even when the ticket is still in the queue — this is
// the case a check-then-write would get wrong, and it is why the write is
// conditional rather than guarded.
func TestAStaleVersionLosesTheRace(t *testing.T) {
	f, store := newFakeStore(t)
	st := devStage()
	tk := f.add(ticket.Ticket{Status: st.Ready})
	ctx := context.Background()

	// Someone appends a comment between the read and the write, moving the
	// version on without moving the ticket out of its queue.
	f.beforeComment = nil
	_, version, err := store.GetWithVersion(ctx, tk.ID)
	if err != nil {
		t.Fatalf("GetWithVersion: %v", err)
	}
	f.appendComment(tk.ID, "someone", "an ordinary comment")

	err = store.UpdateIfVersion(ctx, tk.ID, ticket.Update{Status: st.Working}, version)
	if !transport.Conflict(err) {
		t.Fatalf("a stale conditional write returned %v, want a conflict", err)
	}
	got, _ := f.get(tk.ID)
	if got.Status != st.Ready {
		t.Errorf("the ticket moved to %q on a lost race", got.Status)
	}
}

// THE FALLBACK IS CHOSEN BY THE ABSENCE OF AN ETAG. A store with no conditional
// write degrades to last-write-wins, which would let two hosts both believe they
// won — so the claim is arbitrated by the append order instead.
func TestOnAStoreWithoutVersionsTheOldestClaimWins(t *testing.T) {
	f, store := newFakeStore(t)
	f.versioned = false
	st := devStage()
	tk := f.add(ticket.Ticket{Status: st.Ready})
	ctx := context.Background()

	// The other host's claim lands FIRST, before this one's comment is appended —
	// the exact interleaving the append protocol exists for.
	f.beforeComment = func(id string) {
		f.beforeComment = nil
		body, _ := record.Render(record.Claim{Host: "gpu-other", Role: st.Role})
		f.appendComment(id, "gpu-other", body)
	}

	err := store.Claim(ctx, tk.ID, st, record.Claim{Host: "gpu-1", Role: st.Role})
	if !transport.Conflict(err) {
		t.Fatalf("err = %v, want a conflict: an older claim for this role is already there", err)
	}
	got, _ := f.get(tk.ID)
	if got.Status != st.Ready {
		t.Errorf("the losing host moved the ticket to %q anyway", got.Status)
	}
}

func TestOnAStoreWithoutVersionsAnUncontestedClaimSucceeds(t *testing.T) {
	f, store := newFakeStore(t)
	f.versioned = false
	st := devStage()
	tk := f.add(ticket.Ticket{Status: st.Ready})

	err := store.Claim(context.Background(), tk.ID, st, record.Claim{Host: "gpu-1", Role: st.Role})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	got, _ := f.get(tk.ID)
	if got.Status != st.Working {
		t.Errorf("the ticket is in %q, want the working column", got.Status)
	}
	if h := heldBy(t, f, tk.ID); h != st.Role+"@gpu-1" {
		t.Errorf("assignee = %q", h)
	}
}

// ARBITRATION IS PER ROLE even on the fallback. Comparing against every claim
// would mean losing to the stage before: the product manager's claim is older
// than the developer's and would always win, so a scoped ticket would never
// reach a developer.
func TestTheFallbackIgnoresAnEarlierStagesClaim(t *testing.T) {
	f, store := newFakeStore(t)
	f.versioned = false
	st := devStage()
	tk := f.add(ticket.Ticket{Status: st.Ready})

	scoped, _ := record.Render(record.Claim{Host: "gpu-1", Role: workflow.RoleScoping})
	f.appendComment(tk.ID, "gpu-1", scoped)

	err := store.Claim(context.Background(), tk.ID, st, record.Claim{Host: "gpu-1", Role: st.Role})
	if err != nil {
		t.Fatalf("the developer lost to the product manager's older claim: %v", err)
	}
}

// THE SECOND ATTEMPT MUST NOT LOSE TO THE FIRST.
//
// A claim comment outlives the attempt that wrote it, and the fallback picks the
// oldest claim for the role — so a retry read back its own dead claim as the
// winner and yielded. The ticket then sat in its queue growing one claim comment
// per poll: never worked again, never escalated, and nothing on the board saying
// why. Measured on the integration plane: one dev-agent failure, then thirty
// seconds of silence with three claims on the ticket.
//
// The test above covers an EARLIER STAGE's claim; this one covers this stage's
// earlier ATTEMPT, which is a different comment and was the one that stalled.
func TestTheFallbackDoesNotLoseToThisRolesPreviousAttempt(t *testing.T) {
	f, store := newFakeStore(t)
	f.versioned = false
	st := devStage()
	tk := f.add(ticket.Ticket{Status: st.Ready})
	ctx := context.Background()

	// A full first attempt: claim, then the round ends as the ticket goes back.
	if err := store.Claim(ctx, tk.ID, st, record.Claim{Host: "gpu-1", Role: st.Role}); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	closed, err := record.CloseClaim(st.Role)
	if err != nil {
		t.Fatalf("CloseClaim: %v", err)
	}
	f.appendComment(tk.ID, "gpu-1", closed)
	if err := store.MoveTo(ctx, tk.ID, st.Ready); err != nil {
		t.Fatalf("return the ticket to its queue: %v", err)
	}

	if err := store.Claim(ctx, tk.ID, st, record.Claim{Host: "gpu-1", Role: st.Role}); err != nil {
		t.Fatalf("the retry lost to its own previous attempt, so the ticket stalls "+
			"in its queue forever: %v", err)
	}
	got, _ := f.get(tk.ID)
	if got.Status != st.Working {
		t.Errorf("the ticket is in %q, want the working column: the second attempt "+
			"never started", got.Status)
	}
}

// A ROUND THAT IS STILL OPEN STILL ARBITRATES. The close is what ends a round,
// so without one an earlier claim for this role must still win — otherwise a
// host would take a ticket another host is working right now.
func TestTheFallbackStillYieldsToAnUnclosedClaimForThisRole(t *testing.T) {
	f, store := newFakeStore(t)
	f.versioned = false
	st := devStage()
	tk := f.add(ticket.Ticket{Status: st.Ready})

	live, _ := record.Render(record.Claim{Host: "gpu-other", Role: st.Role})
	f.appendComment(tk.ID, "gpu-other", live)

	err := store.Claim(context.Background(), tk.ID, st, record.Claim{Host: "gpu-1", Role: st.Role})
	if !transport.Conflict(err) {
		t.Fatalf("err = %v, want a conflict: gpu-other holds an OPEN claim and two "+
			"hosts would now work the same ticket", err)
	}
}

// A DEAD HOST MUST NOT WIN THE RACE FOREVER.
//
// A claim is written when work starts and nothing closes it if the process is
// killed. The ceiling already forgives those — PruneStale — but arbitration did
// not, so the dead claim went on winning: the ticket was ELIGIBLE AND
// UNCLAIMABLE at the same time, sitting in its queue with nothing on the board
// to say why. Forgiving the attempt was only half the fix, and the half that was
// missing had no test because no fake ever arbitrated.
func TestAClaimLeftByADeadHostStopsWinningOnceItIsStale(t *testing.T) {
	f, store := newFakeStore(t)
	f.versioned = false
	st := devStage()
	tk := f.add(ticket.Ticket{Status: st.Ready})

	dead, _ := record.Render(record.Claim{Host: "gpu-dead", Role: st.Role})
	f.appendComment(tk.ID, "gpu-dead", dead)

	// The process is gone and the window has passed.
	f.mu.Lock()
	f.now = f.now.Add(record.StaleAfter + time.Hour)
	f.mu.Unlock()

	if err := store.Claim(context.Background(), tk.ID, st, record.Claim{Host: "gpu-1", Role: st.Role}); err != nil {
		t.Fatalf("a ticket whose only claim had aged out was still unclaimable: %v", err)
	}
	got, _ := f.get(tk.ID)
	if got.Status != st.Working {
		t.Errorf("the ticket is in %q, want the working column", got.Status)
	}
}

// A claim inside the window is LIVE, and staleness must not release it early —
// that would put two hosts on one ticket, which is the unrecoverable direction.
func TestAClaimInsideTheWindowStillWins(t *testing.T) {
	f, store := newFakeStore(t)
	f.versioned = false
	st := devStage()
	tk := f.add(ticket.Ticket{Status: st.Ready})

	live, _ := record.Render(record.Claim{Host: "gpu-other", Role: st.Role})
	f.appendComment(tk.ID, "gpu-other", live)

	f.mu.Lock()
	f.now = f.now.Add(record.StaleAfter - time.Minute)
	f.mu.Unlock()

	err := store.Claim(context.Background(), tk.ID, st, record.Claim{Host: "gpu-1", Role: st.Role})
	if !transport.Conflict(err) {
		t.Fatalf("err = %v, want a conflict: gpu-other's claim is still inside the "+
			"staleness window and two hosts would now work the same ticket", err)
	}
}

// RECONCILE'S NOTE MUST RELEASE THE RACE, NOT JUST THE COUNTER.
//
// A host that restarts leaves a claim behind, and reconcile writes a note that
// forgives the attempt. It carries the returned marker, which attempt counting
// treats as a fresh start — but arbitration did not, so the stranded ticket
// stayed unclaimable anyway and the note fixed nothing anyone could see.
func TestReconcilesInterruptedNoteMakesTheTicketClaimableAgain(t *testing.T) {
	f, store := newFakeStore(t)
	f.versioned = false
	st := devStage()
	tk := f.add(ticket.Ticket{Status: st.Ready})

	stranded, _ := record.Render(record.Claim{Host: "gpu-1", Role: st.Role})
	f.appendComment(tk.ID, "gpu-1", stranded)
	// The body dispatch.InterruptedBody writes. Spelled out rather than imported:
	// dispatch depends on this package, so the test cannot depend on dispatch.
	f.appendComment(tk.ID, "gpu-1", "**Attempt interrupted.**\n\n"+record.ReturnedMarker)

	if err := store.Claim(context.Background(), tk.ID, st, record.Claim{Host: "gpu-1", Role: st.Role}); err != nil {
		t.Fatalf("the ticket reconcile released was still unclaimable: %v", err)
	}
}

// BOTH PATHS MUST WRITE THE SAME THING. They differ in whether the write is
// conditional — arbitration, not what a held ticket looks like — and a ticket
// claimed on one path and merely moved on the other reads as unheld on a board.
func TestBothClaimPathsLeaveTheTicketLookingTheSame(t *testing.T) {
	st := devStage()
	c := record.Claim{Host: "gpu-1", Role: st.Role}

	f1, s1 := newFakeStore(t)
	t1 := f1.add(ticket.Ticket{Status: st.Ready})
	if err := s1.Claim(context.Background(), t1.ID, st, c); err != nil {
		t.Fatalf("versioned claim: %v", err)
	}

	f2, s2 := newFakeStore(t)
	f2.versioned = false
	t2 := f2.add(ticket.Ticket{Status: st.Ready})
	if err := s2.Claim(context.Background(), t2.ID, st, c); err != nil {
		t.Fatalf("append claim: %v", err)
	}

	got1, _ := f1.get(t1.ID)
	got2, _ := f2.get(t2.ID)
	if got1.Status != got2.Status {
		t.Errorf("the two paths left the ticket in %q and %q", got1.Status, got2.Status)
	}
	if heldBy(t, f1, t1.ID) != heldBy(t, f2, t2.ID) {
		t.Errorf("the two paths assigned %q and %q", heldBy(t, f1, t1.ID), heldBy(t, f2, t2.ID))
	}
	if record.Attempts(got1, st.Role) != record.Attempts(got2, st.Role) {
		t.Error("the two paths recorded a different number of attempts")
	}
}

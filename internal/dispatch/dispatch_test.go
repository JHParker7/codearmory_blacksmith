package dispatch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/code-armory-app/blacksmith/internal/record"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/transport"
	"github.com/code-armory-app/blacksmith/internal/wake"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

// drainOnce runs one fill and waits for the work it started to report, which is
// what a test wants instead of racing the loop.
func drainOnce(t *testing.T, d *Dispatcher, n int) {
	t.Helper()
	done := make(chan string, 16)
	if err := d.fill(context.Background(), done); err != nil {
		t.Fatalf("fill: %v", err)
	}
	for i := 0; i < n; i++ {
		select {
		case id := <-done:
			d.finish(id)
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of %d tickets reported", i, n)
		}
	}
}

// startedNothing runs one fill and asserts that no work began.
//
// IT READS InFlight, NOT WHAT THE HANDLER DID. start records a ticket as in
// flight BEFORE it spawns the goroutine, so in-flight is decided by the time
// fill returns while the handler may not have run yet. Asserting on the handler
// is a race that passes whether or not the guard being tested exists — which is
// exactly how this whole family of tests first passed against a dispatcher with
// its ceiling and its in-flight guard removed.
func startedNothing(t *testing.T, d *Dispatcher) {
	t.Helper()
	done := make(chan string, 16)
	if err := d.fill(context.Background(), done); err != nil {
		t.Fatalf("fill: %v", err)
	}
	if got := d.InFlight(); got != 0 {
		t.Fatalf("%d tickets were started, want none", got)
	}
}

// A HANDLER WITH NO ROUTING ENTRY IS A WIRING MISTAKE. Left to run it would poll
// a column that does not exist and silently never work.
func TestAHandlerWithNoStageIsRefusedAtWiring(t *testing.T) {
	b := newBoard()
	_, err := New(b, &stubHandler{role: "no-such-agent"}, workflow.New(workflow.Options{}), Options{})
	if err == nil {
		t.Fatal("a dispatcher was built for a role the table does not route")
	}
	if !strings.Contains(err.Error(), "no-such-agent") {
		t.Errorf("err = %v, want it to name the role", err)
	}
}

// A stage this host does not run is the same wiring question, and must fail the
// same way rather than polling a column nothing fills.
func TestAStageThisHostDoesNotRunIsRefused(t *testing.T) {
	b := newBoard()
	_, err := New(b, &stubHandler{role: workflow.RoleCoverage}, workflow.New(workflow.Options{}), Options{})
	if err == nil {
		t.Fatal("a dispatcher was built for a stage this host does not run")
	}
}

func TestAClaimedTicketIsWorkedAndAdvanced(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	tk := b.add(ticket.Ticket{Status: st.Ready})

	h := &stubHandler{role: workflow.RoleDev}
	d := newDispatcher(t, b, h, Options{})
	drainOnce(t, d, 1)

	if got := h.worked(); len(got) != 1 || got[0] != tk.ID {
		t.Fatalf("the handler worked %v", got)
	}
	got := b.get(tk.ID)
	if got.Status != st.Success {
		t.Errorf("the ticket ended in %q, want %q", got.Status, st.Success)
	}
	// The claim is the audit trail AND the attempt counter.
	if n := record.Attempts(got, workflow.RoleDev); n != 1 {
		t.Errorf("the ticket records %d attempts, want 1", n)
	}
	// It must not still name the agent that finished with it.
	if got.AssigneeID != nil {
		t.Errorf("the ticket still names %q after moving on", *got.AssigneeID)
	}
}

// A TICKET IN FLIGHT IS NOT OFFERED AGAIN, even when the listing still shows it
// in the queue.
//
// The claim moves the ticket out of its column, so in the ordinary case a second
// poll simply does not see it — which is why a test built on the ordinary case
// passes with this guard deleted. The case that needs the guard is a STALE
// LISTING: a store replica that has not caught up, or a listing read before the
// claim landed. Then two goroutines on this host would work one ticket.
func TestATicketInFlightIsNotOfferedAgain(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	tk := b.add(ticket.Ticket{Status: st.Ready})

	h := &stubHandler{role: workflow.RoleDev, block: make(chan struct{})}
	d := newDispatcher(t, b, h, Options{Concurrency: 4})

	done := make(chan string, 4)
	if err := d.fill(context.Background(), done); err != nil {
		t.Fatalf("fill: %v", err)
	}
	if got := d.InFlight(); got != 1 {
		t.Fatalf("in flight = %d after the first fill", got)
	}

	// The listing goes stale: it still reports the ticket as queued while this
	// host is working it.
	b.mu.Lock()
	b.tickets[tk.ID].Status = st.Ready
	b.mu.Unlock()

	before := len(b.get(tk.ID).Comments)
	if err := d.fill(context.Background(), done); err != nil {
		t.Fatalf("fill: %v", err)
	}
	// COUNT THE CLAIM, not the in-flight total: the in-flight set is keyed by
	// ticket id, so a second start on the same ticket leaves the count at one
	// while two goroutines run against it. The claim is written synchronously and
	// is the thing that actually distinguishes the two.
	if got := len(b.get(tk.ID).Comments); got != before {
		t.Errorf("a second claim was written; a stale listing put two agents on one ticket")
	}

	close(h.block)
	<-done
	if got := h.worked(); len(got) != 1 {
		t.Errorf("the handler worked the ticket %d times: %v", len(got), got)
	}
}

// THE ATTEMPT CEILING IS THE LOOP BREAKER. A ticket an agent has already failed
// this many times is left alone rather than retried forever.
func TestATicketAtItsCeilingIsNotClaimed(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	tk := b.add(ticket.Ticket{Status: st.Ready})

	for i := 0; i < 3; i++ {
		body, _ := record.Render(record.Claim{Host: "gpu-1", Role: workflow.RoleDev})
		b.AddComment(context.Background(), tk.ID, body)
	}

	before := len(b.get(tk.ID).Comments)

	h := &stubHandler{role: workflow.RoleDev}
	d := newDispatcher(t, b, h, Options{MaxAttempts: 3})
	startedNothing(t, d)

	if got := b.get(tk.ID); got.Status != st.Ready {
		t.Errorf("the ticket moved to %q; a ticket at its ceiling must be left alone", got.Status)
	}
	// NO ATTEMPT MAY BE SPENT. Rejecting the ticket only after claiming it still
	// writes a claim, which is another attempt against a ticket that had none —
	// the loop breaker exists precisely to stop that, and a test that watched only
	// whether the handler ran would pass either way.
	if got := len(b.get(tk.ID).Comments); got != before {
		t.Errorf("the ticket gained %d comments; a ticket at its ceiling was claimed anyway", got-before)
	}
}

// STALE CLAIMS ARE FORGIVEN. A claim is written when work starts and nothing
// closes it if the process dies, so three interrupted runs would otherwise make
// a ticket permanently unclaimable with nothing on the board to say why.
func TestClaimsWhoseProcessIsGoneDoNotCountAgainstTheCeiling(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	tk := b.add(ticket.Ticket{Status: st.Ready})

	for i := 0; i < 3; i++ {
		body, _ := record.Render(record.Claim{Host: "gpu-1", Role: workflow.RoleDev})
		b.AddComment(context.Background(), tk.ID, body)
	}
	// Time moves past the staleness window with the process gone.
	b.now = b.now.Add(record.StaleAfter + time.Hour)

	h := &stubHandler{role: workflow.RoleDev}
	d := newDispatcher(t, b, h, Options{MaxAttempts: 3})
	drainOnce(t, d, 1)

	if got := h.worked(); len(got) != 1 {
		t.Errorf("a ticket whose claims had all aged out was not worked: %v", got)
	}
}

// DEPENDENCIES GATE THE STAGES THAT START WORK. A ticket whose prerequisites are
// unfinished is not late, it is not due.
func TestATicketWaitingOnAPrerequisiteIsNotClaimed(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	if !st.RequiresDependencies {
		t.Fatal("this test assumes the developer waits for its prerequisites")
	}
	b.add(ticket.Ticket{Status: st.Ready, DependsOn: []ticket.Dependency{
		{ID: "t-9", Status: workflow.ColInDev},
	}})

	h := &stubHandler{role: workflow.RoleDev}
	d := newDispatcher(t, b, h, Options{})
	startedNothing(t, d)
}

func TestATicketWhosePrerequisitesAreDoneIsClaimed(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	b.add(ticket.Ticket{Status: st.Ready, DependsOn: []ticket.Dependency{
		{ID: "t-9", Status: workflow.ColDone},
	}})

	h := &stubHandler{role: workflow.RoleDev}
	d := newDispatcher(t, b, h, Options{})
	drainOnce(t, d, 1)

	if got := h.worked(); len(got) != 1 {
		t.Errorf("a ready ticket was not worked: %v", got)
	}
}

// THE HANDLER GETS THE FINAL SAY, and it is given the FULL ticket to say it
// with — a listing carries no comments, so a predicate judged on one evaluates
// against nothing.
func TestAHandlerThatDoesNotWantATicketReleasesIt(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	tk := b.add(ticket.Ticket{Status: st.Ready})
	b.AddComment(context.Background(), tk.ID, "**Not for the developer.**")

	var sawComments bool
	h := &stubHandler{role: workflow.RoleDev, wants: func(t ticket.Ticket) bool {
		for _, c := range t.Comments {
			if strings.Contains(c.Body, "Not for the developer") {
				sawComments = true
				return false
			}
		}
		return true
	}}
	d := newDispatcher(t, b, h, Options{})
	startedNothing(t, d)

	if !sawComments {
		t.Error("the handler was asked about a ticket with no comments on it")
	}
}

// THE AUTHORSHIP GUARD APPLIES TO INTAKE ONLY. Applying it everywhere would do
// real harm now: the child tickets the product manager creates are precisely the
// work the developer exists to pick up.
func TestTheAuthorshipGuardAppliesToIntakeAndNotToLaterStages(t *testing.T) {
	t.Run("intake ignores its own output", func(t *testing.T) {
		b := newBoard()
		b.add(ticket.Ticket{Status: workflow.ColInbox, CreatedBy: "pm-agent"})

		h := &stubHandler{role: workflow.RoleScoping}
		d := newDispatcher(t, b, h, Options{AgentAuthors: []string{"pm-agent"}})
		startedNothing(t, d)
	})

	t.Run("a later stage takes agent-created work", func(t *testing.T) {
		b := newBoard()
		st := devStage(t)
		b.add(ticket.Ticket{Status: st.Ready, CreatedBy: "pm-agent"})

		h := &stubHandler{role: workflow.RoleDev}
		d := newDispatcher(t, b, h, Options{AgentAuthors: []string{"pm-agent"}})
		drainOnce(t, d, 1)

		if got := h.worked(); len(got) != 1 {
			t.Errorf("the developer refused a ticket the product manager created: %v", got)
		}
	})
}

// A PEER WINNING THE RACE IS EXPECTED, NOT AN ERROR. The poll simply moves on.
func TestALostClaimIsNotAFailure(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	b.add(ticket.Ticket{Status: st.Ready})
	b.failClaim = errors.New("wrapped: " + transport.ErrConflict.Error())
	b.failClaim = transport.ErrConflict

	h := &stubHandler{role: workflow.RoleDev}
	d := newDispatcher(t, b, h, Options{})

	done := make(chan string, 4)
	if err := d.fill(context.Background(), done); err != nil {
		t.Fatalf("fill reported a lost race as a failure: %v", err)
	}
	if got := d.InFlight(); got != 0 {
		t.Errorf("%d tickets were started after the claim was lost", got)
	}
}

// A TICKET THAT CANNOT BE RE-READ AFTER THE CLAIM IS PUT BACK. Working from data
// known to be partial is worse than waiting a poll.
func TestAnUnverifiableTicketIsReleasedNotWorked(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	tk := b.add(ticket.Ticket{Status: st.Ready})

	h := &stubHandler{role: workflow.RoleDev}
	d := newDispatcher(t, b, h, Options{})

	// The claim succeeds and the re-read then fails.
	b.beforeClaim = func(id string) { b.failGet[id] = true }

	done := make(chan string, 4)
	if err := d.fill(context.Background(), done); err != nil {
		t.Fatalf("fill: %v", err)
	}
	if got := d.InFlight(); got != 0 {
		t.Errorf("%d tickets were started from data known to be partial", got)
	}
	if got := b.get(tk.ID); got.Status != st.Ready {
		t.Errorf("the ticket was left in %q rather than put back", got.Status)
	}
}

// ONE UNREADABLE TICKET MUST NOT COST THE WHOLE POLL.
func TestOneUnreadableTicketDoesNotStopThePoll(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	bad := b.add(ticket.Ticket{Status: st.Ready})
	good := b.add(ticket.Ticket{Status: st.Ready})
	b.failGet[bad.ID] = true

	h := &stubHandler{role: workflow.RoleDev}
	d := newDispatcher(t, b, h, Options{Concurrency: 2})
	drainOnce(t, d, 1)

	if got := h.worked(); len(got) != 1 || got[0] != good.ID {
		t.Errorf("worked %v, want only the readable ticket %s", got, good.ID)
	}
}

// A HANDLER FAILURE IS A RETRY WHILE ATTEMPTS REMAIN, and goes in front of a
// person once they are gone — rather than cycling through a queue it cannot
// leave.
func TestAFailureRetriesThenEscalates(t *testing.T) {
	st := devStage(t)

	t.Run("attempts remaining", func(t *testing.T) {
		b := newBoard()
		tk := b.add(ticket.Ticket{Status: st.Ready})
		h := &stubHandler{role: workflow.RoleDev, status: workflow.OutcomeFailed}
		d := newDispatcher(t, b, h, Options{MaxAttempts: 3})
		drainOnce(t, d, 1)

		if got := b.get(tk.ID); got.Status != st.Ready {
			t.Errorf("a failure with attempts left went to %q, want the queue", got.Status)
		}
	})

	t.Run("last attempt", func(t *testing.T) {
		b := newBoard()
		tk := b.add(ticket.Ticket{Status: st.Ready})
		// Two spent, and the claim about to be made is the third.
		for i := 0; i < 2; i++ {
			body, _ := record.Render(record.Claim{Host: "gpu-1", Role: workflow.RoleDev})
			b.AddComment(context.Background(), tk.ID, body)
		}
		h := &stubHandler{role: workflow.RoleDev, status: workflow.OutcomeFailed}
		d := newDispatcher(t, b, h, Options{MaxAttempts: 3})
		drainOnce(t, d, 1)

		if got := b.get(tk.ID); got.Status != st.Exhausted {
			t.Errorf("the last attempt sent the ticket to %q, want %q", got.Status, st.Exhausted)
		}
	})
}

// A HANDLER THAT RETURNS AN ERROR IS A FAILURE, and the error becomes the detail
// rather than being swallowed.
func TestAHandlerErrorIsRecordedAsAFailure(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	tk := b.add(ticket.Ticket{Status: st.Ready})

	h := &stubHandler{role: workflow.RoleDev, err: errors.New("the sandbox never booted")}
	d := newDispatcher(t, b, h, Options{MaxAttempts: 3})
	drainOnce(t, d, 1)

	if got := b.get(tk.ID); got.Status != st.Ready {
		t.Errorf("an errored handler sent the ticket to %q", got.Status)
	}
}

// HANDLED MEANS THE STAGE PLACED THE TICKET ITSELF, and moving it again would
// undo that.
func TestAHandledOutcomeLeavesTheTicketWhereTheStagePutIt(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	tk := b.add(ticket.Ticket{Status: st.Ready})

	h := &stubHandler{role: workflow.RoleDev, status: workflow.OutcomeHandled}
	d := newDispatcher(t, b, h, Options{})
	drainOnce(t, d, 1)

	// The claim moved it to the working column and the handler "placed" it there;
	// nothing after the handler may move it again.
	if got := b.get(tk.ID); got.Status != st.Working {
		t.Errorf("the ticket ended in %q; the dispatcher moved a ticket the stage placed", got.Status)
	}
}

// RETURNED GOES BACK to the stage that can act on it, and does not count against
// the returning stage.
func TestAReturnedTicketGoesBackToTheEarlierQueue(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	tk := b.add(ticket.Ticket{Status: st.Ready})

	h := &stubHandler{role: workflow.RoleDev, status: workflow.OutcomeReturned}
	d := newDispatcher(t, b, h, Options{})
	drainOnce(t, d, 1)

	if got := b.get(tk.ID); got.Status != st.Returns {
		t.Errorf("a returned ticket went to %q, want %q", got.Status, st.Returns)
	}
}

// A MOVE THAT FAILS MUST NOT LOSE THE WORK SILENTLY: the ticket stays in the
// working column for the next reconcile to find.
func TestAFailedMoveLeavesTheTicketForReconcile(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	tk := b.add(ticket.Ticket{Status: st.Ready})

	h := &stubHandler{role: workflow.RoleDev}
	d := newDispatcher(t, b, h, Options{})

	done := make(chan string, 4)
	b.beforeClaim = func(string) { /* claim still works */ }
	if err := d.fill(context.Background(), done); err != nil {
		t.Fatalf("fill: %v", err)
	}
	b.mu.Lock()
	b.failMove = true
	b.mu.Unlock()
	<-done

	if got := b.get(tk.ID); got.Status != st.Working {
		t.Errorf("the ticket is in %q; a failed move must leave it held for reconcile", got.Status)
	}
}

// PRIORITY IS A REFILL RULE: the next ticket is the highest-priority available
// one, oldest first within a priority so nothing starves.
func TestPlanOrdersByPriorityThenAge(t *testing.T) {
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	candidates := []ticket.Ticket{
		{ID: "low", Priority: "low", CreatedAt: at},
		{ID: "high-late", Priority: "high", CreatedAt: at.Add(2 * time.Hour)},
		{ID: "high-early", Priority: "high", CreatedAt: at.Add(time.Hour)},
		{ID: "critical", Priority: "critical", CreatedAt: at.Add(3 * time.Hour)},
	}

	got := Plan(candidates, 0, 4)
	want := []string{"critical", "high-early", "high-late", "low"}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("Plan()[%d] = %q, want %q", i, got[i].ID, id)
		}
	}
}

func TestPlanFillsOnlyTheRoomThatIsLeft(t *testing.T) {
	candidates := []ticket.Ticket{{ID: "a"}, {ID: "b"}, {ID: "c"}}

	if got := Plan(candidates, 1, 3); len(got) != 2 {
		t.Errorf("Plan with one in flight of three returned %d", len(got))
	}
	if got := Plan(candidates, 3, 3); got != nil {
		t.Errorf("Plan at capacity returned %d tickets", len(got))
	}
	if got := Plan(nil, 0, 3); got != nil {
		t.Errorf("Plan with no candidates returned %d", len(got))
	}
	// It must not disturb the caller's slice: fill reuses it.
	candidates[0].Priority = "high"
	if got := Plan(candidates, 0, 1); got[0].ID != "a" {
		t.Errorf("Plan()[0] = %q", got[0].ID)
	}
	if candidates[0].ID != "a" {
		t.Error("Plan reordered the caller's slice")
	}
}

// CONCURRENCY BOUNDS WHAT ONE STAGE STARTS AT ONCE.
func TestFillRespectsConcurrency(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	for i := 0; i < 5; i++ {
		b.add(ticket.Ticket{Status: st.Ready})
	}

	h := &stubHandler{role: workflow.RoleDev, block: make(chan struct{})}
	d := newDispatcher(t, b, h, Options{Concurrency: 2})

	done := make(chan string, 8)
	if err := d.fill(context.Background(), done); err != nil {
		t.Fatalf("fill: %v", err)
	}
	if got := d.InFlight(); got != 2 {
		t.Errorf("in flight = %d, want the configured 2", got)
	}
	close(h.block)
}

// ADVANCING A TICKET WAKES THE OTHER STAGES, or the hand-off waits for a timer.
func TestFinishingATicketWakesTheOtherStages(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	b.add(ticket.Ticket{Status: st.Ready})

	tr := wake.New()
	woken := tr.Subscribe()
	h := &stubHandler{role: workflow.RoleDev}
	d := newDispatcher(t, b, h, Options{Wake: tr})
	drainOnce(t, d, 1)

	select {
	case <-woken:
	case <-time.After(time.Second):
		t.Error("finishing a ticket did not wake the other stages")
	}
}

// A ticket the handler PLACED does not wake anyone, because nothing moved.
func TestAPlacedTicketDoesNotWakeTheOtherStages(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	b.add(ticket.Ticket{Status: st.Ready})

	tr := wake.New()
	woken := tr.Subscribe()
	h := &stubHandler{role: workflow.RoleDev, status: workflow.OutcomeHandled}
	d := newDispatcher(t, b, h, Options{Wake: tr})
	drainOnce(t, d, 1)

	select {
	case <-woken:
		t.Error("a stage that placed its own ticket woke every other stage for nothing")
	case <-time.After(50 * time.Millisecond):
	}
}

// A TICKET GETS EVERY ATTEMPT IT WAS PROMISED.
//
// The two ceilings have to agree. Selection refuses a ticket once its spent
// attempts reach the maximum, so routing must escalate exactly when the NEXT
// attempt would be refused — no earlier and no later. Escalating early throws
// away an attempt the ticket was entitled to; escalating late puts it back in a
// queue that will never select it again, which is a stall with nothing on the
// board to explain it.
//
// This walks the whole budget rather than checking one boundary, because the
// off-by-one it exists to catch is invisible at both ends: with three attempts
// configured, a stage that escalates one early still looks correct on the first
// attempt and on the last.
func TestAFailingTicketGetsEveryAttemptBeforeEscalating(t *testing.T) {
	const max = 3
	b := newBoard()
	st := devStage(t)
	tk := b.add(ticket.Ticket{Status: st.Ready})

	h := &stubHandler{role: workflow.RoleDev, status: workflow.OutcomeFailed}
	d := newDispatcher(t, b, h, Options{MaxAttempts: max})

	for attempt := 1; attempt <= max; attempt++ {
		drainOnce(t, d, 1)

		got := b.get(tk.ID)
		if n := record.Attempts(got, workflow.RoleDev); n != attempt {
			t.Fatalf("attempt %d: the ticket records %d claims", attempt, n)
		}
		if attempt < max {
			if got.Status != st.Ready {
				t.Fatalf("attempt %d of %d sent the ticket to %q; it still had attempts left",
					attempt, max, got.Status)
			}
			continue
		}
		if got.Status != st.Exhausted {
			t.Errorf("the last attempt left the ticket in %q, want %q — a queue nothing will select from",
				got.Status, st.Exhausted)
		}
	}
	if got := len(h.worked()); got != max {
		t.Errorf("the handler ran %d times, want the %d attempts the ticket was promised", got, max)
	}
}

// noteOn returns the escalation note on a ticket, or "" if there is none.
func noteOn(b *board, id string) string {
	for _, c := range b.get(id).Comments {
		if strings.Contains(c.Body, record.EscalatedMarker) {
			return c.Body
		}
	}
	return ""
}

// spend writes n claims, so the next attempt is the ticket's last.
func spend(b *board, id, role string, n int) {
	for i := 0; i < n; i++ {
		body, _ := record.Render(record.Claim{Host: "gpu-1", Role: role})
		b.AddComment(context.Background(), id, body)
	}
}

// A TICKET ESCALATED TO A PERSON MUST SAY WHY, ON ITSELF.
//
// One carried nothing but its claim comments: the board read "BLOCKED — needs
// you" and the ticket gave no reason at all. The cause was in the service log,
// which is the one place a person reading the board is not looking — and on a
// host that has restarted since, is not keeping either.
func TestAnExhaustedTicketRecordsWhyItWasEscalated(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	tk := b.add(ticket.Ticket{Status: st.Ready})
	spend(b, tk.ID, workflow.RoleDev, 2)

	h := &stubHandler{role: workflow.RoleDev,
		err: errors.New("sandbox: create: POST /leases: 400 Bad Request")}
	d := newDispatcher(t, b, h, Options{MaxAttempts: 3})
	drainOnce(t, d, 1)

	if got := b.get(tk.ID); got.Status != st.Exhausted {
		t.Fatalf("status = %q, want the ticket escalated", got.Status)
	}
	note := noteOn(b, tk.ID)
	if note == "" {
		t.Fatal("the ticket was escalated with no explanation on it at all")
	}
	// NAME THE CAUSE, NOT THE SYMPTOM. "blocked after 3 attempts" sends a correct
	// reader to the model or the specification when the failure was a lease.
	if !strings.Contains(note, "POST /leases: 400") {
		t.Errorf("the note does not carry the actual failure:\n%s", note)
	}
	if !strings.Contains(note, workflow.RoleDev) {
		t.Errorf("the note does not say which stage gave up:\n%s", note)
	}
}

// A STAGE THAT FAILED WITHOUT SAYING ANYTHING is a different problem from one
// that failed with a reason, and the note must not read as though the reason was
// merely left out here.
func TestAnEscalationWithNoDetailSaysSo(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	tk := b.add(ticket.Ticket{Status: st.Ready})
	spend(b, tk.ID, workflow.RoleDev, 2)

	h := &stubHandler{role: workflow.RoleDev, status: workflow.OutcomeFailed}
	d := newDispatcher(t, b, h, Options{MaxAttempts: 3})
	drainOnce(t, d, 1)

	note := noteOn(b, tk.ID)
	if note == "" {
		t.Fatal("no escalation note was written")
	}
	if !strings.Contains(note, "reported no detail") {
		t.Errorf("an empty detail is itself the finding and is not named:\n%s", note)
	}
}

// A TICKET WITH ATTEMPTS LEFT IS NOT ESCALATED, so it must not be annotated as
// though it were — the marker is what a person and the window key on.
func TestATicketWithAttemptsLeftIsNotAnnotated(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	tk := b.add(ticket.Ticket{Status: st.Ready})

	h := &stubHandler{role: workflow.RoleDev, err: errors.New("transient")}
	d := newDispatcher(t, b, h, Options{MaxAttempts: 3})
	drainOnce(t, d, 1)

	if note := noteOn(b, tk.ID); note != "" {
		t.Fatalf("a retryable failure was recorded as an escalation:\n%s", note)
	}
}

// THE NOTE IS WRITTEN BEFORE THE MOVE, so a store that accepts the comment and
// then fails the move still leaves the reason where a person will find it.
// Written the other way round, the reason is lost exactly when it is needed.
func TestTheReasonSurvivesAFailedMove(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	tk := b.add(ticket.Ticket{Status: st.Ready})
	spend(b, tk.ID, workflow.RoleDev, 2)
	b.failMove = true

	h := &stubHandler{role: workflow.RoleDev, err: errors.New("sandbox never booted")}
	d := newDispatcher(t, b, h, Options{MaxAttempts: 3})
	drainOnce(t, d, 1)

	if note := noteOn(b, tk.ID); note == "" {
		t.Fatal("the move failed and took the reason with it")
	}
}

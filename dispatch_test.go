package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func tk(id, priority string, at int) Ticket {
	return Ticket{
		TicketID:  id,
		Priority:  priority,
		Status:    ColInbox,
		CreatedAt: time.Unix(int64(at), 0),
	}
}

func ids(ts []Ticket) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.TicketID
	}
	return out
}

// Refill to N, never drain-then-refetch: aggregate throughput holds only while
// every slot is busy.
func TestPlanDispatchFillsToConcurrency(t *testing.T) {
	c := []Ticket{tk("a", "medium", 1), tk("b", "medium", 2), tk("c", "medium", 3), tk("d", "medium", 4)}

	if got := ids(planDispatch(c, 0, 3)); len(got) != 3 {
		t.Errorf("planDispatch(empty host, max 3) = %v, want 3 tickets", got)
	}
	if got := ids(planDispatch(c, 2, 3)); len(got) != 1 {
		t.Errorf("planDispatch(2 in flight, max 3) = %v, want 1 ticket to top up", got)
	}
	if got := planDispatch(c, 3, 3); got != nil {
		t.Errorf("planDispatch(full) = %v, want nothing", ids(got))
	}
}

func TestPlanDispatchOrdersByPriorityThenAge(t *testing.T) {
	c := []Ticket{tk("old-low", "low", 1), tk("new-high", "high", 9), tk("old-med", "medium", 2), tk("new-med", "medium", 5)}
	got := ids(planDispatch(c, 0, 4))
	want := []string{"new-high", "old-med", "new-med", "old-low"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v (priority first, oldest first within a priority)", got, want)
		}
	}
}

// A critical ticket is ordered FIRST here and pins nothing. Exclusivity is the
// serving queue's job (TestQueueCriticalTakesTheWholeBox), because that is the
// only layer that can actually deliver it: a dispatcher is per stage, so pinning
// here stopped this stage's siblings while the other five stages kept hitting
// the same model — all of the lost throughput, none of the exclusive stream.
//
// The regression this guards is real and was measured: a parent ticket filed as
// critical propagated that priority to all seven of its children, and the whole
// board then ran one ticket at a time against a four-slot server.
func TestPlanDispatchCriticalOrdersFirstButDoesNotPinTheStage(t *testing.T) {
	c := []Ticket{tk("normal", "medium", 1), tk("urgent", "critical", 2), tk("other", "high", 3)}

	got := ids(planDispatch(c, 0, 4))
	if len(got) != 3 {
		t.Fatalf("planDispatch on an empty host = %v, want all 3: the stage still fills to concurrency", got)
	}
	if got[0] != "urgent" {
		t.Errorf("order = %v, want the critical ticket first", got)
	}
	// Room is still room, even with a critical ticket among the candidates.
	if got := ids(planDispatch(c, 2, 4)); len(got) != 2 {
		t.Errorf("planDispatch(2 in flight, max 4) = %v, want 2 to top up rather than a stage-wide stall", got)
	}
}

// Siblings that all inherit "critical" must still run in parallel: this is the
// exact board shape that serialised the pipeline before the rule moved.
func TestPlanDispatchAllCriticalSiblingsStillFillTheStage(t *testing.T) {
	c := []Ticket{tk("a", "critical", 1), tk("b", "critical", 2), tk("c", "critical", 3), tk("d", "critical", 4)}
	if got := ids(planDispatch(c, 0, 4)); len(got) != 4 {
		t.Fatalf("planDispatch(4 critical siblings, max 4) = %v, want all 4", got)
	}
}

func TestPlanDispatchEmpty(t *testing.T) {
	if got := planDispatch(nil, 0, 4); got != nil {
		t.Errorf("planDispatch(nil) = %v, want nil", ids(got))
	}
}

// The loop breaker applies to the PULL: without it the PM agent's own tickets
// come straight back to the department.
func TestEligibleExcludesAgentAuthoredTickets(t *testing.T) {
	d := NewDispatcher(nil, nil, &stubHandler{role: "pm-agent"}, DispatcherOpts{
		Host: "h", AgentAuthors: []string{"pm-agent", "dev-agent"},
	})
	if d.eligible(Ticket{TicketID: "x", CreatedBy: "pm-agent", Status: ColInbox}) {
		t.Error("a ticket authored by an agent is eligible; the department would feed itself")
	}
	if !d.eligible(Ticket{TicketID: "x", CreatedBy: "alice", Status: ColInbox}) {
		t.Error("a human-authored ticket is not eligible")
	}
}

func TestEligibleRespectsAttemptCeiling(t *testing.T) {
	d := NewDispatcher(nil, nil, &stubHandler{role: "pm-agent"}, DispatcherOpts{Host: "h", MaxAttempts: 2})

	var t2 Ticket
	t2.TicketID = "x"
	t2.CreatedBy = "alice"
	t2.Status = ColInbox
	for i := range 2 {
		payload, _ := json.Marshal(ClaimToken{Host: "h", Role: "pm-agent"})
		t2.Comments = append(t2.Comments, Comment{
			CommentID: fmt.Sprintf("c%d", i),
			Body:      claimMarker + string(payload) + " -->",
			CreatedAt: time.Unix(int64(i), 0),
		})
	}
	if d.eligible(t2) {
		t.Error("a ticket at the attempt ceiling is eligible; it would be retried forever")
	}
}

// Attempts are counted per role, so a developer failure does not exhaust the
// product manager's budget on the same ticket.
func TestAttemptsAreCountedPerRole(t *testing.T) {
	var ticket Ticket
	for i, role := range []string{"pm-agent", "dev-agent", "dev-agent"} {
		payload, _ := json.Marshal(ClaimToken{Host: "h", Role: role})
		ticket.Comments = append(ticket.Comments, Comment{
			CommentID: fmt.Sprintf("c%d", i),
			Body:      claimMarker + string(payload) + " -->",
			CreatedAt: time.Unix(int64(i), 0),
		})
	}
	if got := attempts(ticket, "pm-agent"); got != 1 {
		t.Errorf("attempts(pm-agent) = %d, want 1", got)
	}
	if got := attempts(ticket, "dev-agent"); got != 2 {
		t.Errorf("attempts(dev-agent) = %d, want 2", got)
	}
	if got := attempts(ticket, "qa-agent"); got != 0 {
		t.Errorf("attempts(qa-agent) = %d, want 0", got)
	}
}

// Reconcile must be scoped to THIS host: a peer's in-flight work is
// indistinguishable from a stranded claim when viewed from here.
func TestReconcileOnlyReleasesThisHostsClaims(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "mine", Status: ColScoping, CreatedBy: "alice"})
	f.addTicket(t, Ticket{TicketID: "theirs", Status: ColScoping, CreatedBy: "alice"})
	f.addClaim(t, "mine", "osiris", "pm-agent", time.Unix(10, 0))
	f.addClaim(t, "theirs", "other-host", "pm-agent", time.Unix(10, 0))

	d := NewDispatcher(api, nil, &stubHandler{role: "pm-agent"}, DispatcherOpts{Host: "osiris"})
	if err := d.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() = %v", err)
	}

	if got := f.get("mine").Status; got != ColInbox {
		t.Errorf("this host's stranded ticket status = %q, want %q", got, ColInbox)
	}
	if got := f.get("theirs").Status; got != ColScoping {
		t.Errorf("another host's ticket status = %q, want it untouched: reconcile stole live work", got)
	}
}

// End to end through the real client: claim, work, record, release.
func TestDispatcherWorksATicket(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "do a thing", CreatedBy: "alice", Priority: "medium"})

	rec, dir := newTestRecorder(t)
	h := &stubHandler{role: "pm-agent", status: OutcomeSuccess}
	d := NewDispatcher(api, rec, h, DispatcherOpts{Host: "osiris", Concurrency: 2, Poll: 20 * time.Millisecond})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go d.Run(ctx)

	// At least once, not exactly once. Releasing a ticket makes it eligible again,
	// and this stub Wants everything — a real handler is self-terminating, leaving
	// a marker (a triage comment, a pushed branch) that drops it from its own
	// predicate. attempts() is the backstop for one that is not.
	waitFor(t, "the ticket to be handled", func() bool { return h.count() >= 1 })
	waitFor(t, "the ticket to advance", func() bool { return f.get("t1").Status == ColTracking })

	// Attribution is read from the comment, which outlives the claim: ClaimedBy
	// answers "is it held NOW", and the ticket has just been released.
	cm, ok := oldestClaim(f.get("t1").Comments)
	if !ok {
		t.Fatal("no claim comment recorded; the run has no attribution")
	}
	tok, err := parseClaim(cm.Body)
	if err != nil || tok.Host != "osiris" || tok.Role != "pm-agent" {
		t.Errorf("claim = %+v (err=%v), want it to record host and role for attribution", tok, err)
	}
	if _, held := ClaimedBy(f.get("t1")); held {
		t.Error("ClaimedBy() = true on a released ticket; the next stage could never take it")
	}

	records := readRecords(t, dir)
	var sawStart, sawOutcome bool
	for _, r := range records {
		if r.TaskID != "t1" {
			continue
		}
		switch r.Kind {
		case KindStart:
			sawStart = true
		case KindOutcome:
			sawOutcome = true
			if r.Status != OutcomeSuccess {
				t.Errorf("outcome status = %q, want %q", r.Status, OutcomeSuccess)
			}
		}
	}
	if !sawStart || !sawOutcome {
		t.Errorf("transcript incomplete: start=%v outcome=%v", sawStart, sawOutcome)
	}
}

// A ticket already claimed by a peer must never be picked up.
func TestDispatcherSkipsPeerClaimedTickets(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	// In progress, not merely commented on: status is what makes a claim LIVE.
	// A claim comment on an open ticket is a finished stage's attribution, and
	// treating that as a hold is what used to lock every later stage out.
	f.addTicket(t, Ticket{TicketID: "t1", CreatedBy: "alice", Status: ColScoping})
	f.addClaim(t, "t1", "peer", "pm-agent", time.Unix(5, 0))

	rec, _ := newTestRecorder(t)
	h := &stubHandler{role: "pm-agent", status: OutcomeSuccess}
	d := NewDispatcher(api, rec, h, DispatcherOpts{Host: "osiris", Concurrency: 2, Poll: 10 * time.Millisecond})

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	d.Run(ctx)

	if h.count() != 0 {
		t.Errorf("handler ran %d times on a peer-claimed ticket, want 0", h.count())
	}
}

// A handler failure must not stall the loop or lose the ticket.
func TestDispatcherSurvivesHandlerFailure(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", CreatedBy: "alice"})

	rec, dir := newTestRecorder(t)
	h := &stubHandler{role: "pm-agent", err: fmt.Errorf("model exploded")}
	d := NewDispatcher(api, rec, h, DispatcherOpts{Host: "osiris", Concurrency: 1, Poll: 20 * time.Millisecond})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go d.Run(ctx)

	waitFor(t, "the failing handler to run", func() bool { return h.count() >= 1 })
	waitFor(t, "a failure outcome to be recorded", func() bool {
		for _, r := range readRecords(t, dir) {
			if r.Kind == KindOutcome && r.Status == OutcomeFailed {
				return true
			}
		}
		return false
	})
	// It goes back to its queue to be retried, and once the attempt ceiling is
	// spent it lands in the column a person watches rather than cycling through a
	// queue it can never leave. Either is acceptable here; being left in the
	// WORKING column is not, because that is a ticket stuck mid-stage forever.
	waitFor(t, "the ticket to leave the working column", func() bool {
		st := f.get("t1").Status
		return st == ColInbox || st == ColBlocked
	})
	waitFor(t, "a permanently failing ticket to end up where a person will see it", func() bool {
		return f.get("t1").Status == ColBlocked
	})
}

type stubHandler struct {
	role   string
	status string
	err    error
	// wants overrides the default "takes everything", so a test can mirror a real
	// self-terminating predicate.
	wants func(Ticket) bool

	mu sync.Mutex
	n  int
}

func (s *stubHandler) Role() string { return s.role }
func (s *stubHandler) Wants(t Ticket) bool {
	if s.wants != nil {
		return s.wants(t)
	}
	return true
}
func (s *stubHandler) Class() Class { return ClassSmall }
func (s *stubHandler) Handle(ctx context.Context, t Ticket) (string, string, error) {
	s.mu.Lock()
	s.n++
	s.mu.Unlock()
	if s.err != nil {
		return OutcomeFailed, "", s.err
	}
	return s.status, "ok", nil
}
func (s *stubHandler) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

// A ticket whose work has finished must be released even when the shutdown that
// ended the work also cancelled the context. Without this it stays claimed until
// some later run's reconcile notices — recoverable, but a ticket the department
// has finished with should not look like one it is still working.
//
// Found by e2e: the lifecycle test cancels the dispatcher the moment the triage
// comment lands, which is exactly the shutdown ordering.
func TestDispatcherReleasesTicketAfterContextCancellation(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", CreatedBy: "alice"})

	rec, _ := newTestRecorder(t)
	handled := make(chan struct{})
	h := &blockingHandler{role: "pm-agent", entered: handled}
	d := NewDispatcher(api, rec, h, DispatcherOpts{Host: "osiris", Concurrency: 1, Poll: 20 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	go d.Run(ctx)

	<-handled // the handler is running
	waitFor(t, "the ticket to be claimed", func() bool { return f.get("t1").Status == ColScoping })
	cancel()    // shutdown, mid-task
	h.release() // the handler then finishes

	// It ADVANCES rather than returning to the queue: the handler reported
	// success, and a cancelled context must not lose that. What matters is that
	// it does not stay in the working column, which is what "stuck in progress
	// until some later reconcile" looked like.
	waitFor(t, "the ticket to leave the working column despite the cancellation", func() bool {
		return f.get("t1").Status == ColTracking
	})
}

// blockingHandler signals when it starts and waits to be let go, so a test can
// interleave a cancellation with work that is genuinely in flight.
type blockingHandler struct {
	role     string
	entered  chan struct{}
	once     sync.Once
	unblock  chan struct{}
	initOnce sync.Once
}

func (b *blockingHandler) ch() chan struct{} {
	b.initOnce.Do(func() { b.unblock = make(chan struct{}) })
	return b.unblock
}
func (b *blockingHandler) Wants(Ticket) bool { return true }
func (b *blockingHandler) Role() string      { return b.role }
func (b *blockingHandler) Class() Class      { return ClassSmall }
func (b *blockingHandler) release()          { b.once.Do(func() { close(b.ch()) }) }
func (b *blockingHandler) Handle(ctx context.Context, t Ticket) (string, string, error) {
	c := b.ch()
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-c
	return OutcomeSuccess, "done", nil
}

// THE PIPELINE MUST HAND OFF. Comments are append-only and nothing deletes them,
// so a ticket carries every claim ever made against it. Reading "claimed" off
// that history made a ticket claimed forever after its first stage: the product
// manager triaged it, released it, and the developer agent was then locked out of
// it permanently — as was the reviewer after that. Each stage handed to a stage
// that could no longer take it, and the department silently stopped after triage.
func TestReleasedTicketIsEligibleForTheNextStage(t *testing.T) {
	dev := NewDevAgent(nil, nil, ClassLarge,
		RepoConfig{URL: "git://host/demo.git", TestCommand: "go test ./..."}, 6)
	d := NewDispatcher(nil, nil, dev, DispatcherOpts{Host: "osiris", AgentAuthors: defaultAgentAuthors})

	released := Ticket{
		TicketID: "t1", CreatedBy: "alice", Status: ColReadyForDev,
		Comments: []Comment{
			{CommentID: "c1", Body: `<!-- blacksmith:claim {"host":"osiris","role":"pm-agent","run_id":"r1"} -->`},
			{CommentID: "c2", Body: "**Triage** (automated)\n\nlooks reasonable"},
		},
	}
	if !dev.Wants(released) {
		t.Fatal("Wants() = false on a triaged, unbranched ticket; the test premise is wrong")
	}
	if !d.eligible(released) {
		t.Error("a released ticket is not eligible: the pipeline cannot hand off past its first stage")
	}

	// Still held: status, not the comment, is what says so.
	held := released
	held.Status = ColInDev
	if d.eligible(held) {
		t.Error("an in-progress ticket is eligible; two agents would work it at once")
	}
	if _, claimed := ClaimedBy(held); !claimed {
		t.Error("ClaimedBy() = false on an in-progress ticket with a claim comment")
	}
	if _, claimed := ClaimedBy(released); claimed {
		t.Error("ClaimedBy() = true on a released ticket; the claim is history, not a hold")
	}
}

// A ticket LISTING carries no comments, and every pipeline marker is a comment.
// Judged from the listing alone a stage re-claims work it has already finished:
// the product manager triages, releases, and immediately wants the ticket back
// because the triage comment it just wrote is invisible in the next listing.
func TestStartRereadsBeforeWorkingSoAStageDoesNotRepeatItself(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", CreatedBy: "alice", Status: ColInbox})
	f.seedComment(t, "t1", "**Triage** (automated)\n\nalready triaged")

	rec, _ := newTestRecorder(t)
	h := &stubHandler{role: "pm-agent", status: OutcomeSuccess}
	// Wants mirrors the real product manager: it takes only untriaged tickets.
	h.wants = func(tk Ticket) bool { return !hasTriage(tk) }
	d := NewDispatcher(api, rec, h, DispatcherOpts{Host: "osiris", Concurrency: 1, Poll: 10 * time.Millisecond})

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	d.Run(ctx)

	if h.count() != 0 {
		t.Errorf("handler ran %d times on an already-triaged ticket, want 0", h.count())
	}
	// And it must not be left holding the ticket it declined.
	if got := f.get("t1").Status; got != ColInbox {
		t.Errorf("status = %q after declining, want %q: the next stage could never take it", got, ColInbox)
	}
}

// SELECTION MUST SEE COMMENTS. A listing carries none, and the pipeline's stages
// are defined entirely by them, so judging eligibility from a listing breaks the
// department in two opposite ways at once — and the second one is silent.
//
// A stage whose predicate REQUIRES a marker (the developer wants a triaged
// ticket) matches nothing, ever, so it simply never works. A stage whose
// predicate requires a marker's ABSENCE (the product manager wants an untriaged
// one) matches everything, so it re-claims the same tickets on every poll. The
// pipeline stops after triage while looking busy.
func TestFillSelectsOnFullTicketsNotListings(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", CreatedBy: "alice", Status: ColReadyForDev})
	f.seedComment(t, "t1", "**Triage** (automated)\n\nready for a developer")

	rec, _ := newTestRecorder(t)
	// The developer agent's real predicate, without needing a model or a repo.
	h := &stubHandler{role: "dev-agent", status: OutcomeSuccess}
	h.wants = func(tk Ticket) bool { return hasTriage(tk) && !hasBranch(tk) }
	d := NewDispatcher(api, rec, h, DispatcherOpts{
		Host: "osiris", Concurrency: 1, Poll: 10 * time.Millisecond,
		AgentAuthors: defaultAgentAuthors,
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go d.Run(ctx)

	waitFor(t, "the triaged ticket to be picked up by the next stage", func() bool { return h.count() >= 1 })
}

// The other half of the same bug: a stage must not take a ticket it has already
// finished with. Without the full ticket the marker it wrote is invisible, so it
// claims the same work every poll forever.
func TestFillDoesNotReselectAFinishedStage(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", CreatedBy: "alice", Status: ColInbox})
	f.seedComment(t, "t1", "**Triage** (automated)\n\nalready done by this stage")

	rec, _ := newTestRecorder(t)
	h := &stubHandler{role: "pm-agent", status: OutcomeSuccess}
	h.wants = func(tk Ticket) bool { return !hasTriage(tk) }
	d := NewDispatcher(api, rec, h, DispatcherOpts{
		Host: "osiris", Concurrency: 1, Poll: 10 * time.Millisecond,
		AgentAuthors: defaultAgentAuthors,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	d.Run(ctx)

	if h.count() != 0 {
		t.Errorf("handler ran %d times on a ticket it had already triaged, want 0", h.count())
	}
	// Nor may it leave claim comments behind on every poll.
	claims := 0
	for _, c := range f.get("t1").Comments {
		if strings.HasPrefix(c.Body, claimMarker) {
			claims++
		}
	}
	if claims != 0 {
		t.Errorf("%d claim comments written for work never done; the store fills with churn", claims)
	}
}

// Reading every listed ticket in full is what selection now costs, so it must be
// bounded: one poll against a large board cannot become hundreds of requests.
func TestFillBoundsHowManyTicketsItReadsPerPoll(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	for i := range maxHydratePerPoll + 25 {
		id := fmt.Sprintf("t%03d", i)
		f.addTicket(t, Ticket{TicketID: id, CreatedBy: "alice", Status: ColInbox})
	}

	rec, _ := newTestRecorder(t)
	h := &stubHandler{role: "pm-agent", status: OutcomeSuccess}
	h.wants = func(Ticket) bool { return false } // select nothing; we are counting reads
	d := NewDispatcher(api, rec, h, DispatcherOpts{Host: "osiris", Concurrency: 1, Poll: time.Hour})

	if err := d.fill(context.Background(), make(chan string, 1)); err != nil {
		t.Fatalf("fill() = %v", err)
	}
	if got := f.ticketReads(); got > maxHydratePerPoll {
		t.Errorf("fill read %d tickets in one poll, want at most %d", got, maxHydratePerPoll)
	}
}

// A return sends work BACKWARDS, to the queue named by the routing table rather
// than to any column the agent chose. The reviewer hands a rejected change to
// the developer; the resolver hands a vanished conflict to the integrator.
func TestReturnedRoutesToTheStagesReturnsColumn(t *testing.T) {
	for _, tc := range []struct {
		role string
		want string
	}{
		{roleReview, ColReadyForDev},
		{roleResolve, ColReadyForIntegration},
	} {
		d := NewDispatcher(nil, nil, &stubHandler{role: tc.role}, DispatcherOpts{Host: "h"})
		if got := d.destination(Ticket{TicketID: "x"}, OutcomeReturned); got != tc.want {
			t.Errorf("%s returned → %q, want %q", tc.role, got, tc.want)
		}
	}
}

// A stage with nowhere to return work must not silently keep the ticket. An
// empty destination means "do not move it", which for a ticket sitting in a
// working column is a ticket nothing will ever collect.
// Scoping, not development, is the example here: the developer GAINED a Returns
// column when a specification that does not compile turned out to need sending
// back to its author. Scoping is the first stage and structurally has nowhere to
// return work to, so it is the honest case for this guard.
func TestReturnedFromAStageThatCannotReturnGoesToAPerson(t *testing.T) {
	if stages[roleScoping].Returns != "" {
		t.Fatalf("roleScoping gained a Returns column (%q); this test needs a stage that has none",
			stages[roleScoping].Returns)
	}
	d := NewDispatcher(nil, nil, &stubHandler{role: roleScoping}, DispatcherOpts{Host: "h"})
	got := d.destination(Ticket{TicketID: "x"}, OutcomeReturned)
	if got == "" {
		t.Fatal("a return from a stage with no Returns column left the ticket unmoved; it would sit in the working column forever")
	}
	if got != stages[roleScoping].Exhausted {
		t.Errorf("destination = %q, want %q", got, stages[roleScoping].Exhausted)
	}
}

// The developer's own return path, which is what that column was added for.
//
// TO THE RECONCILER, not the section author. A developer works on a TASK, and a
// task in the section author's queue is a failure this repository has already
// paid for: a blocked task revived into ready_for_spec was claimed by the
// section author, read a hundred times and killed. The reconciler is the stage
// that may edit this task's tests, so it is the one that can act on a hand-back.
func TestDeveloperReturnsABrokenSpecificationToTheStageThatCanFixIt(t *testing.T) {
	d := NewDispatcher(nil, nil, &stubHandler{role: roleDev}, DispatcherOpts{Host: "h"})
	got := d.destination(Ticket{TicketID: "x"}, OutcomeReturned)
	if got != ColReadyForSpecMerge {
		t.Errorf("a returned task went to %q, want %q — it must reach the agent allowed to edit tests",
			got, ColReadyForSpecMerge)
	}
	// And something must actually take from there, or the hand-back is a ticket
	// sent to a column nothing polls.
	if merge, ok := stageFor(roleSpecMerge); !ok || merge.Ready != got {
		t.Errorf("nothing takes from %q; the returned task would sit there", got)
	}
}

// THE STRANDING GUARD. A returned ticket goes back to a queue that has already
// spent attempts on it. If those still counted, the stage asked to fix the work
// would refuse to pick it up, and the ticket would sit in a queue nothing ever
// selects from — the exact failure that stranded tickets in ready_for_dev.
func TestAttemptsResetAfterAReturn(t *testing.T) {
	claim := func(i int, role string) Comment {
		payload, _ := json.Marshal(ClaimToken{Host: "h", Role: role})
		return Comment{
			CommentID: fmt.Sprintf("c%d", i),
			Body:      claimMarker + string(payload) + " -->",
			CreatedAt: time.Unix(int64(i), 0),
		}
	}
	var ticket Ticket
	ticket.Comments = []Comment{
		claim(0, roleDev),
		claim(1, roleDev),
		claim(2, roleDev),
		{CommentID: "c3", Body: "review says no " + returnedMarker, CreatedAt: time.Unix(3, 0)},
		claim(4, roleDev),
	}
	if got := attempts(ticket, roleDev); got != 1 {
		t.Errorf("attempts = %d after a return, want 1 — the round restarts or the fix can never be picked up", got)
	}

	d := NewDispatcher(nil, nil, &stubHandler{role: roleDev}, DispatcherOpts{Host: "h", MaxAttempts: 3})
	ticket.TicketID = "x"
	ticket.CreatedBy = "alice"
	ticket.Status = ColReadyForDev
	if !d.eligible(ticket) {
		t.Error("a returned ticket is not eligible for the stage asked to fix it; that is the stranding bug")
	}
}

// The reset is positional, so comments arriving out of order must not decide it.
// Claims BEFORE the return are spent; claims after it are the current round.
func TestAttemptsIgnoreCommentOrderWhenCountingARound(t *testing.T) {
	claim := func(id string, sec int) Comment {
		payload, _ := json.Marshal(ClaimToken{Host: "h", Role: roleDev})
		return Comment{CommentID: id, Body: claimMarker + string(payload) + " -->", CreatedAt: time.Unix(int64(sec), 0)}
	}
	var ticket Ticket
	// Deliberately shuffled: the return at t=3 sits last in the slice.
	ticket.Comments = []Comment{
		claim("c4", 4),
		claim("c0", 0),
		claim("c1", 1),
		{CommentID: "c3", Body: "returned " + returnedMarker, CreatedAt: time.Unix(3, 0)},
	}
	if got := attempts(ticket, roleDev); got != 1 {
		t.Errorf("attempts = %d, want 1 — the count must follow timestamps, not slice order", got)
	}
}

// Without a return marker the count is unchanged, so this stays additive to
// every ticket that never goes backwards.
func TestAttemptsUnchangedWithoutAReturn(t *testing.T) {
	claim := func(i int) Comment {
		payload, _ := json.Marshal(ClaimToken{Host: "h", Role: roleDev})
		return Comment{CommentID: fmt.Sprintf("c%d", i), Body: claimMarker + string(payload) + " -->", CreatedAt: time.Unix(int64(i), 0)}
	}
	var ticket Ticket
	ticket.Comments = []Comment{claim(0), claim(1), claim(2)}
	if got := attempts(ticket, roleDev); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
}

// The cross-stage trigger. Without it a hand-off between two goroutines in one
// process waits for a timer — three times per ticket, which was around 22
// seconds of idle GPU on the default interval.
func TestWakeSignalsEverySubscriber(t *testing.T) {
	w := NewWake()
	a, b := w.Subscribe(), w.Subscribe()
	w.Signal()
	for i, ch := range []<-chan struct{}{a, b} {
		select {
		case <-ch:
		default:
			t.Errorf("subscriber %d was not woken; its stage would wait for the poll timer", i)
		}
	}
}

// Signalling must never block the stage that just finished a ticket. The buffer
// is one deep and a full one is dropped, because a wake already pending is as
// good as two.
func TestWakeNeverBlocksAndCoalesces(t *testing.T) {
	w := NewWake()
	ch := w.Subscribe()
	for range 100 {
		w.Signal() // would deadlock if Signal blocked on a full buffer
	}
	if len(ch) != 1 {
		t.Errorf("buffered %d wakes, want exactly 1 — they must coalesce", len(ch))
	}
	<-ch
	select {
	case <-ch:
		t.Error("a second wake was delivered; 100 signals collapsed to more than one")
	default:
	}
}

// A nil Wake is the "poll only" configuration, and must be safe rather than a
// panic: it is what every test and any single-stage host gets.
func TestNilWakeIsSafe(t *testing.T) {
	var w *Wake
	w.Signal() // must not panic
	if ch := w.Subscribe(); ch != nil {
		t.Error("a nil Wake handed out a channel; a nil receive is the correct no-op in a select")
	}
}

// The dispatcher must actually select on the wake, not merely hold one.
func TestDispatcherAdvancingATicketWakesTheOtherStages(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x", CreatedBy: "alice", Status: ColInbox})

	w := NewWake()
	listener := w.Subscribe() // stands in for another stage's dispatcher
	d := NewDispatcher(api, nil, &stubHandler{role: "pm-agent", status: OutcomeSuccess},
		DispatcherOpts{Host: "h", Wake: w})

	d.work(context.Background(), f.get("t1"))

	select {
	case <-listener:
	default:
		t.Error("advancing a ticket woke nothing; the next stage would wait a full poll interval")
	}
}

// An unmet-but-LIVE dependency must still hold: releasing those would start work
// against a prerequisite that is merely unfinished, which is the case waiting
// exists for.
func TestALiveDependencyStillHoldsTheTicket(t *testing.T) {
	waiting := Ticket{
		Status:    ColReadyForDev,
		DependsOn: []TicketDependency{{TicketID: "root", Status: ColInDev}},
	}
	if dependenciesMet(waiting) {
		t.Error("a prerequisite still being worked no longer holds the ticket back")
	}
	if _, dead := dependencyDeadlocked(waiting); dead {
		t.Error("an in-progress prerequisite was judged dead")
	}
}

// A PREREQUISITE IS NEEDED BY DEFINITION, so its failure is the one worth
// retrying — the tickets resting on it are exactly what make it worth another
// attempt. Two worse answers came first: stranding the dependents turned one
// failure into six untouched tickets, and letting them run without the
// prerequisite produced six failures that were not their fault.
func TestABlockedPrerequisiteIsSentBackForAnotherAttempt(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "root", Title: "foundation", CreatedBy: "alice", Status: ColBlocked})
	waiting := Ticket{
		TicketID: "child", Title: "rests on it", CreatedBy: "alice", Status: ColReadyForDev,
		DependsOn: []TicketDependency{{TicketID: "root", Title: "foundation", Status: ColBlocked}},
	}
	f.addTicket(t, waiting)

	d := NewDispatcher(api, nil, &stubHandler{role: roleDev}, DispatcherOpts{Host: "h"})
	d.handleDeadPrerequisite(context.Background(), f.get("child"), TicketDependency{TicketID: "root", Title: "foundation", Status: ColBlocked})

	root := f.get("root")
	if root.Status != ColReadyForDev {
		t.Errorf("the blocked prerequisite is in %q, want it back in %q for another attempt",
			root.Status, ColReadyForDev)
	}
	joined := strings.Join(f.commentBodies("root"), "\n")
	if !strings.Contains(joined, revivedMarker) {
		t.Errorf("nothing records why the prerequisite came back:\n%s", joined)
	}
	// It must carry a return marker, or the fresh budget it was promised is not
	// actually fresh — see attempts().
	if !strings.Contains(joined, returnedMarker) {
		t.Error("the revived prerequisite has no return marker, so its attempts never reset")
	}
	// The DEPENDENT must not have been touched: it is not the ticket at fault.
	if got := f.get("child").Status; got != ColReadyForDev {
		t.Errorf("the dependent moved to %q; only the prerequisite should be acted on", got)
	}
}

// A foundation that has failed repeatedly is a real answer. At the ceiling it
// stays blocked and a person decides, rather than cycling forever.
func TestPrerequisiteRevivalIsBounded(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	root := Ticket{TicketID: "root", Title: "foundation", CreatedBy: "alice", Status: ColBlocked}
	for i := 0; i < maxRevivals; i++ {
		root.Comments = append(root.Comments, Comment{
			CommentID: fmt.Sprintf("r%d", i), Body: revivedMarker + " again",
		})
	}
	f.addTicket(t, root)
	f.addTicket(t, Ticket{
		TicketID: "child", Title: "rests on it", CreatedBy: "alice", Status: ColReadyForDev,
		DependsOn: []TicketDependency{{TicketID: "root", Status: ColBlocked}},
	})

	d := NewDispatcher(api, nil, &stubHandler{role: roleDev}, DispatcherOpts{Host: "h"})
	d.handleDeadPrerequisite(context.Background(), f.get("child"), TicketDependency{TicketID: "root", Title: "foundation", Status: ColBlocked})

	if got := f.get("root").Status; got != ColBlocked {
		t.Errorf("a prerequisite past the revival ceiling was requeued to %q; it must stay blocked", got)
	}
	// And the dependent must be moved out of the queue rather than left looking
	// queued while nothing is coming for it — the failure this path circles.
	if got := f.get("child").Status; got != ColBlocked {
		t.Errorf("the dependent is still in %q after revival was exhausted; nothing will ever collect it", got)
	}
}

// A prerequisite that is merely unfinished must be left alone — waiting is what
// the dependency gate is for.
func TestALivePrerequisiteIsNotRevived(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "root", Title: "foundation", CreatedBy: "alice", Status: ColInDev})
	f.addTicket(t, Ticket{
		TicketID: "child", Title: "rests on it", CreatedBy: "alice", Status: ColReadyForDev,
		DependsOn: []TicketDependency{{TicketID: "root", Status: ColInDev}},
	})

	d := NewDispatcher(api, nil, &stubHandler{role: roleDev}, DispatcherOpts{Host: "h"})
	d.handleDeadPrerequisite(context.Background(), f.get("child"), TicketDependency{TicketID: "root", Title: "foundation", Status: ColBlocked})

	if got := f.get("root").Status; got != ColInDev {
		t.Errorf("an in-progress prerequisite was moved to %q", got)
	}
}

// THE TEST AUTHOR WAITS, and this reverses the earlier contract. Tests do come
// from the ticket rather than from code — but a specification NAMES things, and
// an author writing against Task and Store before the foundation ticket has
// created them invents their shape. The developer, which waits and merges, then
// has to satisfy an invented interface against the real one: a contradiction, not
// a failing test.
//
// The cost is deliberate and was the argument against it: this was the only
// parallel stage, and siblings of a slow foundation now wait rather than being
// specified at once.
func TestTheTestAuthorWaitsForDependencies(t *testing.T) {
	if !stages[roleTest].RequiresDependencies {
		t.Error("the test author starts before the code its tests must name exists")
	}
	if !stages[roleDev].RequiresDependencies {
		t.Error("the developer no longer waits for dependencies; it would build against absent code")
	}
}

// A REVIVED PREREQUISITE GOES BACK TO ITS OWN QUEUE, not to the queue of
// whichever dispatcher noticed it. That was harmless while every stage worked
// the same kind of ticket and wrong the moment they did not.
//
// Measured: a blocked TASK was revived by a section dispatcher into
// ready_for_spec, where the section author claimed it, read a hundred times and
// was killed — a task ticket being worked by the stage that writes sections.
func TestARevivedPrerequisiteReturnsToItsOwnStage(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "task0000", Title: "the store", Status: ColBlocked})
	f.addTicket(t, Ticket{TicketID: "sect0000", Title: "a section", Status: ColBlocked})
	// Their histories say which stage owns each. Written directly rather than
	// through Claim, which requires the ticket to be sitting in that stage's
	// Ready column — these are blocked, which is the situation under test.
	history := func(id, role string) {
		t.Helper()
		if _, err := api.AddComment(context.Background(), id,
			fmt.Sprintf(`%s{"host":"h","role":%q,"run_id":"r@h"} -->`, claimMarker, role)); err != nil {
			t.Fatalf("record a claim on %s: %v", id, err)
		}
	}
	history("task0000", roleDev)
	history("sect0000", roleSpec)

	// The reviver is the SECTION dispatcher in both cases — the bug was that its
	// own Ready column was used regardless of what it was reviving.
	d := NewDispatcher(api, nil, NewSpecAgent(nil, nil, ClassLarge, RepoConfig{}, 10), DispatcherOpts{Host: "h"})

	if got := d.reviveQueue(context.Background(), "task0000"); got != ColReadyForDev {
		t.Errorf("a blocked task is revived into %q, want %q", got, ColReadyForDev)
	}
	if got := d.reviveQueue(context.Background(), "sect0000"); got != ColReadyForSpec {
		t.Errorf("a blocked section is revived into %q, want %q", got, ColReadyForSpec)
	}
	// A ticket no stage has touched has no history to read, so the reviver's own
	// queue is the honest fallback.
	f.addTicket(t, Ticket{TicketID: "fresh000", Title: "untouched", Status: ColBlocked})
	if got := d.reviveQueue(context.Background(), "fresh000"); got != ColReadyForSpec {
		t.Errorf("an untouched ticket went to %q, want the reviver's queue", got)
	}
}

// A SPECIFICATION HAND-BACK MUST RESTART THE ATTEMPT COUNT, exactly as a review
// rejection does. Both send the ticket back a stage and bring it forward again,
// and the attempts spent on the version that was sent back should not be charged
// against the corrected one.
//
// Measured on r68: "status parameter filtering" round-tripped between developer
// and specification author until it held three claims for EACH role, and the
// dispatcher then refused to give it to either. It sat in ready_for_dev that no
// developer would ever claim, and no counter said why. The hand-back loop was
// eating the budget it needed to converge.
func TestASpecHandBackRestartsTheAttemptCount(t *testing.T) {
	// RECENT, deliberately: a claim older than staleClaimAfter is treated as
	// abandoned, so a fixed past date would make these fixtures orphans and test
	// the wrong rule.
	at := func(n int) time.Time { return time.Now().Add(-time.Duration(10-n) * time.Minute) }
	claim := func(id string, min int, role string) Comment {
		return Comment{
			CommentID: id, CreatedAt: at(min),
			Body: claimMarker + `{"host":"osiris","role":"` + role + `","run_id":"r"}` + " -->",
		}
	}
	tk := Ticket{Comments: []Comment{
		claim("c1", 1, "dev-agent"),
		claim("c2", 2, "dev-agent"),
		{CommentID: "c3", CreatedAt: at(3), Body: specRepairMarker + "\n\nthe tests panic in their own setup"},
		claim("c4", 4, "dev-agent"),
	}}

	if got := attempts(tk, "dev-agent"); got != 1 {
		t.Errorf("attempts = %d, want 1 — the hand-back did not restart the count", got)
	}
	// And the reviewer's send-back must keep working the same way.
	tk2 := Ticket{Comments: []Comment{
		claim("c1", 1, "dev-agent"),
		{CommentID: "c2", CreatedAt: at(2), Body: returnedMarker + "\n\nreviewer found a bug"},
		claim("c3", 3, "dev-agent"),
	}}
	if got := attempts(tk2, "dev-agent"); got != 1 {
		t.Errorf("reviewer reset regressed: attempts = %d, want 1", got)
	}
}

// Without any return marker the ceiling must still bite, or the loop breaker is
// gone entirely.
func TestPlainClaimsStillReachTheCeiling(t *testing.T) {
	at := func(n int) time.Time { return time.Now().Add(-time.Duration(10-n) * time.Minute) }
	var cs []Comment
	for i := 1; i <= 3; i++ {
		cs = append(cs, Comment{
			CommentID: string(rune('a' + i)), CreatedAt: at(i),
			Body: claimMarker + `{"host":"osiris","role":"dev-agent","run_id":"r"}` + " -->",
		})
	}
	if got := attempts(Ticket{Comments: cs}, "dev-agent"); got != defaultMaxAttempts {
		t.Errorf("attempts = %d, want %d — the loop breaker no longer trips", got, defaultMaxAttempts)
	}
}

// A CLAIM IS WRITTEN WHEN WORK STARTS, so a process that dies mid-attempt leaves
// one behind that no outcome will ever close — and the ceiling counts it as an
// attempt spent. Measured, and self-inflicted: restarting blacksmith three times
// to install fixes left three orphaned claims on one ticket, which hit the
// ceiling of 3 and became permanently unclaimable, sitting in ready_for_dev that
// no developer would take.
func TestOrphanedClaimsDoNotCountTowardTheCeiling(t *testing.T) {
	claim := func(id string, age time.Duration) Comment {
		return Comment{
			CommentID: id, CreatedAt: time.Now().Add(-age),
			Body: claimMarker + `{"host":"osiris","role":"dev-agent","run_id":"r"}` + " -->",
		}
	}
	// Three claims, all older than any attempt could run for.
	old := Ticket{Comments: []Comment{
		claim("c1", 3*time.Hour), claim("c2", 2*time.Hour), claim("c3", 90*time.Minute),
	}}
	if got := attempts(pruneStaleClaims(old), "dev-agent"); got != 0 {
		t.Errorf("attempts = %d, want 0 — orphaned claims still strand the ticket", got)
	}

	// A live attempt must still count, or the loop breaker stops working.
	live := Ticket{Comments: []Comment{claim("c1", time.Minute), claim("c2", 2*time.Minute)}}
	if got := attempts(pruneStaleClaims(live), "dev-agent"); got != 2 {
		t.Errorf("attempts = %d, want 2 — recent claims must still count", got)
	}

	// And the ceiling must still be reachable within the window.
	var recent []Comment
	for i := 0; i < defaultMaxAttempts; i++ {
		recent = append(recent, claim(string(rune('a'+i)), time.Duration(i)*time.Minute))
	}
	if got := attempts(pruneStaleClaims(Ticket{Comments: recent}), "dev-agent"); got != defaultMaxAttempts {
		t.Errorf("attempts = %d, want %d — the loop breaker no longer trips", got, defaultMaxAttempts)
	}
}

// A restart must not charge the ticket it interrupted.
//
// The ceiling counts claims, and a claim is written when work STARTS — nothing
// closes it when the process is killed. Measured: six restarts in one morning put
// three claims on a single ticket, which then sat in ready_for_dev with nothing
// wrong with it and was collected at 09:45:02, four seconds after the oldest claim
// aged past the 45-minute stale window. Eighteen idle minutes, self-inflicted.
func TestReconcileClearsTheAttemptItInterrupted(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "mine", Status: ColScoping, CreatedBy: "alice"})
	f.addClaim(t, "mine", "osiris", "pm-agent", time.Unix(10, 0))

	d := NewDispatcher(api, nil, &stubHandler{role: "pm-agent"}, DispatcherOpts{Host: "osiris"})
	if err := d.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() = %v", err)
	}

	got := f.get("mine")
	if got.Status != ColInbox {
		t.Fatalf("status = %q, want it released to %q", got.Status, ColInbox)
	}
	// The release alone is not enough: without the marker the interrupted claim
	// still counts, and enough restarts make the ticket unclaimable.
	if n := attempts(got, "pm-agent"); n != 0 {
		t.Errorf("attempts() = %d after an interrupted run, want 0: a restart is charging the ticket", n)
	}
}

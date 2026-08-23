package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Task dispatch: the pull loop.
//
// blacksmith is a client, so it PULLS work — conductor cannot reach an
// intermittent workstation behind NAT. The loop is edge-triggered on completion
// for latency and level-triggered on a timer for correctness. The timer is not a
// stopgap to remove once completion notification works: it is what makes the
// loop self-healing when a task dies without reporting, or when a ticket arrives
// while the queue is empty.

// defaultPollInterval is the level trigger. Fixed, with no backoff: a backoff
// would only add latency to the case that matters.
//
// THE INTERVAL IS THE LATENCY. It is the whole of the gap between a person filing
// a ticket and an agent picking it up, and it compounds down the pipeline: with a
// three-stage department, one ticket waits through it three times before a branch
// exists. At five minutes — the old default — that was a quarter of an hour of
// nothing visibly happening, which reads as broken rather than idle and made the
// department unusable to watch. Fifteen seconds is short enough that a person who
// files a ticket sees it move.
//
// COST PER TICK is a listing plus one read per candidate it has not already
// rejected, bounded by maxHydratePerPoll — not the single call this once was, and
// the reason that ceiling exists. On a small board that is a handful of requests
// a minute. On a large one a short interval multiplies against the cap, and the
// answer there is not a longer timer: it is to stop polling. See the ticket on
// event-driven dispatch — the platform already solves this for outposts, which
// long-poll an outbound connection rather than waking on a clock.
const defaultPollInterval = 15 * time.Second

// defaultMaxAttempts is the loop breaker's ceiling. A ticket an agent has
// already failed this many times is left alone rather than retried forever.
const defaultMaxAttempts = 3

// maxHydratePerPoll bounds how many tickets one poll will read in full.
//
// Selection needs comments and a listing has none, so each candidate costs a
// request. The cap is what keeps that from turning a large board into hundreds
// of requests every poll: past it, the poll works with what it has and picks the
// rest up next time. Deliberately well above any realistic number of OPEN
// tickets a department is choosing between at once.
const maxHydratePerPoll = 50

// Handler works one ticket. The agents themselves implement it.
type Handler interface {
	// Role is the agent account this handler acts as, e.g. "pm-agent".
	Role() string
	// Class is the model class the handler needs.
	Class() Class
	// Wants reports whether this handler should work a ticket.
	//
	// Without it two agents on one host race for the same tickets, and the claim
	// makes that race silently destructive: whichever wins, the other never sees
	// the ticket again. The predicate is what turns one queue into a pipeline —
	// the product manager takes untriaged tickets, the developer takes triaged
	// ones — using only what is already on the ticket, with no extra state.
	Wants(t Ticket) bool

	// Handle does the work and returns an outcome status (OutcomeSuccess /
	// OutcomeFailed) plus a short detail for the transcript.
	Handle(ctx context.Context, t Ticket) (status, detail string, err error)
}

// Dispatcher runs one handler against a stream of tickets.
type Dispatcher struct {
	api     *CodeArmory
	rec     *Recorder
	handler Handler

	// stage is the routing entry for this handler's role: the column it takes
	// from, the one it parks held work in, and where a ticket goes when it
	// finishes or runs out of attempts.
	stage Stage

	host        string
	boardID     string
	concurrency int
	poll        time.Duration
	maxAttempts int

	// agentAuthors are the usernames of agent accounts. Tickets they authored
	// are excluded from selection — without this the PM agent's own output feeds
	// straight back into the department.
	agentAuthors map[string]bool
	// tracer reports under this role's own service name, so the five stages are
	// five services rather than one averaged together.
	tracer trace.Tracer

	// wake is the shared cross-stage trigger. Nil is valid and means "poll only".
	wake *Wake

	mu       sync.Mutex
	inFlight map[string]bool
}

// DispatcherOpts configures a Dispatcher.
type DispatcherOpts struct {
	Tracers      *agentTracers
	Host         string
	BoardID      string
	Concurrency  int
	Poll         time.Duration
	MaxAttempts  int
	AgentAuthors []string
	// Wake is the shared cross-stage trigger. Optional: a nil Wake leaves the
	// dispatcher on its poll timer alone, which is what every test wants and what
	// a single-stage host would get.
	Wake *Wake
}

func NewDispatcher(api *CodeArmory, rec *Recorder, h Handler, opts DispatcherOpts) *Dispatcher {
	if opts.Concurrency < 1 {
		opts.Concurrency = 1
	}
	if opts.Poll <= 0 {
		opts.Poll = defaultPollInterval
	}
	if opts.MaxAttempts < 1 {
		opts.MaxAttempts = defaultMaxAttempts
	}
	authors := map[string]bool{}
	for _, a := range opts.AgentAuthors {
		authors[a] = true
	}
	// A handler with no routing entry would poll a column that does not exist and
	// silently never work: better to fail where it is configured.
	st, ok := stageFor(h.Role())
	if !ok {
		panic("no workflow stage for role " + h.Role())
	}
	return &Dispatcher{
		api: api, rec: rec, handler: h,
		stage:        st,
		tracer:       opts.Tracers.Tracer(h.Role()),
		host:         opts.Host,
		boardID:      opts.BoardID,
		concurrency:  opts.Concurrency,
		poll:         opts.Poll,
		maxAttempts:  opts.MaxAttempts,
		agentAuthors: authors,
		wake:         opts.Wake,
		inFlight:     map[string]bool{},
	}
}

// Run drives the loop until ctx ends.
func (d *Dispatcher) Run(ctx context.Context) error {
	if err := d.Reconcile(ctx); err != nil {
		// A failed reconcile is not fatal — it leaves some tickets stranded until
		// the next restart, which is better than refusing to start at all.
		slog.WarnContext(ctx, "startup reconcile failed; some tickets may stay claimed", "error", err)
	}

	done := make(chan string, d.concurrency)
	ticker := time.NewTicker(d.poll)
	defer ticker.Stop()
	woken := d.wake.Subscribe()

	for {
		if err := d.fill(ctx, done); err != nil && !errors.Is(err, context.Canceled) {
			slog.WarnContext(ctx, "fill failed; will retry on the next tick", "error", err)
		}
		select {
		case <-ctx.Done():
			d.drain(done)
			return ctx.Err()
		case id := <-done:
			// Edge trigger: refill the moment a slot frees, rather than waiting
			// for the batch to drain. Aggregate throughput holds only while every
			// slot is busy.
			d.finish(id)
		case <-woken:
			// Edge trigger ACROSS stages: some stage moved a ticket, so this one
			// may have work now. Without it a hand-off waits for the timer.
		case <-ticker.C:
			// Level trigger. Still required: it is what finds tickets that appear
			// from outside this process, which no in-process signal can know about.
			//
			// The unstranding sweep belongs HERE and not on the wake channel: a
			// blocker reaching done does wake the stages, but the sweep costs a
			// read per blocked ticket per stage, and a ticket that has waited for
			// its blocker can wait one more poll.
			d.unstrand(ctx)
			d.retryBlocked(ctx)
			d.closeFinishedRequests(ctx)
		}
	}
}

// closeFinishedRequests moves a broken-down request out of tracking once every
// ticket under it is done.
//
// NOTHING EVER CLAIMED A TRACKING TICKET. ColTracking is only ever a
// destination — the product manager's Success — and no stage names it as Ready,
// so a request that has been broken down stays there for good. The intent was
// that the parent remain visible; the effect was that a request can never be
// finished, however much of it is.
//
// Reported from the window on r76, and it was right: five tasks and eleven
// sections all done, the delivered code building and its 37 tests passing, and
// the request still showing as waiting. There was no signal anywhere that the
// job was complete.
//
// Done rather than deleted, so the parent stays visible and the original request
// can still be followed to the tickets that answered it — the reason it was
// parked in tracking in the first place.
//
// ONLY WHEN EVERY CHILD IS DONE. A blocked or failed task means the request is
// not finished, and saying it is would hide exactly the case an operator needs to
// see. A request with no children is left alone: it has not been broken down, so
// there is nothing to conclude from.
//
// Exactly one stage sweeps this, the one whose Success IS tracking, for the same
// reason unstrand matches on the stranding stage: seven dispatchers polling one
// column would race to move the same ticket.
func (d *Dispatcher) closeFinishedRequests(ctx context.Context) {
	if d.stage.Success != ColTracking {
		return
	}
	tracking, err := d.api.ListTickets(ctx, ListOpts{BoardID: d.boardID, Status: ColTracking})
	if err != nil {
		slog.WarnContext(ctx, "could not list tracking requests to close", "error", err)
		return
	}
	if len(tracking) == 0 {
		return
	}
	all, err := d.api.ListTickets(ctx, ListOpts{BoardID: d.boardID})
	if err != nil {
		slog.WarnContext(ctx, "could not list the board to close finished requests", "error", err)
		return
	}
	for _, req := range tracking {
		kids := 0
		finished := 0
		for _, t := range all {
			if t.ParentID == nil || *t.ParentID != req.TicketID {
				continue
			}
			kids++
			if t.Status == ColDone {
				finished++
			}
		}
		if kids == 0 || finished != kids {
			continue
		}
		body := fmt.Sprintf("**Request complete.** All %d tasks opened for this request are done, "+
			"so it leaves `%s`. The work it was broken into is linked below it.", kids, ColTracking)
		if _, cerr := d.api.AddComment(ctx, req.TicketID, body); cerr != nil {
			slog.WarnContext(ctx, "could not explain a completed request",
				"ticket_id", req.TicketID, "error", cerr)
		}
		if merr := d.api.MoveTo(ctx, req.TicketID, ColDone); merr != nil {
			slog.WarnContext(ctx, "could not close a finished request",
				"ticket_id", req.TicketID, "error", merr)
			continue
		}
		slog.InfoContext(ctx, "closed a request whose tasks are all done",
			"ticket_id", req.TicketID, "tasks", kids)
	}
}

// reconcileReturnBody resets the attempt count for work this host was killed in
// the middle of. It carries returnedMarker, which attempts() treats as a fresh
// start — the same mechanism a reviewer's send-back uses.
const reconcileReturnBody = "**Attempt interrupted.** The agent host restarted while this ticket was being " +
	"worked, so the claim it left behind is not an attempt anyone spent. It goes back to its queue with " +
	"a fresh budget.\n\n" + returnedMarker

// Reconcile releases tickets THIS host claimed that have no live work.
//
// Scoped to this host deliberately: another host's in-flight work is
// indistinguishable from a stranded claim when viewed from here, so an unscoped
// sweep would steal live work from a peer.
func (d *Dispatcher) Reconcile(ctx context.Context) error {
	tickets, err := d.api.ListTickets(ctx, ListOpts{BoardID: d.boardID, Status: d.stage.Working})
	if err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}
	var released int
	for _, t := range tickets {
		// Re-read: whose claim this is lives in a COMMENT, and the listing does not
		// carry comments. Judged from the listing alone no ticket ever looks like
		// this host's, so nothing is ever released and stranded claims accumulate
		// silently — the exact failure Reconcile exists to prevent.
		full, ferr := d.api.GetTicket(ctx, t.TicketID)
		if ferr != nil {
			slog.WarnContext(ctx, "reconcile: could not re-read ticket", "ticket_id", t.TicketID, "error", ferr)
			continue
		}
		tok, ok := ClaimedBy(full)
		if !ok || tok.Host != d.host {
			continue
		}
		d.mu.Lock()
		live := d.inFlight[t.TicketID]
		d.mu.Unlock()
		if live {
			continue
		}
		// AN INTERRUPTED ATTEMPT IS NOT A SPENT ONE, and saying so here is the whole
		// difference between a ticket that resumes and one that sits unclaimable.
		//
		// The ceiling counts claims. A claim is written when work starts and nothing
		// closes it when the process is killed, so every restart to install a fix
		// charges the in-flight ticket one attempt. pruneStaleClaims forgives those,
		// but only after 45 minutes — and this host KNOWS, right now, that it is the
		// one that died, so there is nothing to wait for.
		//
		// Measured: six restarts in a morning put three claims on one ticket at
		// 08:59, 09:06 and 09:10. It then sat in ready_for_dev with nothing wrong
		// with it, and was picked up at 09:45:02 — four seconds after the first claim
		// aged out. Eighteen minutes of an idle department and a wasted lease,
		// entirely self-inflicted.
		if _, cerr := d.api.AddComment(ctx, t.TicketID, reconcileReturnBody); cerr != nil {
			// Not fatal: releasing the ticket still beats leaving it held. It costs the
			// ticket an attempt, which is the old behaviour.
			slog.WarnContext(ctx, "reconcile: could not clear the interrupted attempt",
				"ticket_id", t.TicketID, "error", cerr)
		}
		if err := d.api.MoveTo(ctx, t.TicketID, d.stage.Ready); err != nil {
			slog.WarnContext(ctx, "reconcile: could not release ticket", "ticket_id", t.TicketID, "error", err)
			continue
		}
		released++
	}
	if released > 0 {
		slog.InfoContext(ctx, "reconcile released stranded claims", "count", released, "host", d.host)
	}
	return nil
}

// fill claims and starts work until the concurrency target is met.
func (d *Dispatcher) fill(ctx context.Context, done chan<- string) error {
	// THE COLUMN NARROWS; HYDRATION STILL HAPPENS.
	//
	// Asking the server for this stage's column replaces the marker predicates
	// that routing used to need, and it is what makes the read below cheap: the
	// listing is now a handful of tickets waiting on one stage rather than every
	// open ticket on the board.
	//
	// But it does NOT remove the read. A listing carries no comments, and two
	// things that decide eligibility are still comments: the attempt ceiling,
	// counted from claim records, and the refinements a couple of stages make
	// within their column. Judged from a listing those evaluate against nothing —
	// silently, and in the worst direction, since a predicate requiring a marker's
	// ABSENCE is satisfied by every ticket and re-claims the column every poll.
	// (Removing this read is exactly that regression; it was caught by the test
	// written for the first occurrence.)
	tickets, err := d.api.ListTickets(ctx, ListOpts{BoardID: d.boardID, Status: d.stage.Ready})
	if err != nil {
		return err
	}

	d.mu.Lock()
	inFlight := len(d.inFlight)
	busy := make(map[string]bool, len(d.inFlight))
	for id := range d.inFlight {
		busy[id] = true
	}
	d.mu.Unlock()

	candidates := make([]Ticket, 0, len(tickets))
	var looked int
	for _, t := range tickets {
		if busy[t.TicketID] {
			continue
		}
		// A ticket waiting on a prerequisite that can never finish is not waiting,
		// so it is released to run rather than skipped every poll forever. Done
		// here, in the stage that would otherwise have worked it, because this is
		// the only place that both sees the ticket and knows its dependencies.
		if d.stage.RequiresDependencies {
			if blocker, dead := dependencyDeadlocked(t); dead {
				d.handleDeadPrerequisite(ctx, t, blocker)
				continue
			}
		}
		// Dependencies and authorship travel ON the listing, so a ticket that is
		// not ready costs nothing to reject.
		if !d.readyOnListing(t) {
			continue
		}
		if looked >= maxHydratePerPoll {
			slog.DebugContext(ctx, "stopped hydrating candidates at the per-poll cap",
				"cap", maxHydratePerPoll, "listed", len(tickets), "role", d.handler.Role())
			break
		}
		looked++
		full, err := d.api.GetTicket(ctx, t.TicketID)
		if err != nil {
			// One unreadable ticket must not cost the whole poll.
			slog.DebugContext(ctx, "skipping ticket that could not be read",
				"ticket_id", t.TicketID, "error", err)
			continue
		}
		if d.eligible(full) {
			candidates = append(candidates, full)
		}
	}

	for _, t := range planDispatch(candidates, inFlight, d.concurrency) {
		if err := d.start(ctx, t, done); err != nil {
			if errors.Is(err, ErrConflict) {
				// A peer won the race. Expected, not an error worth logging loudly.
				slog.DebugContext(ctx, "ticket claimed by another host", "ticket_id", t.TicketID)
				continue
			}
			slog.WarnContext(ctx, "could not start ticket", "ticket_id", t.TicketID, "error", err)
			continue
		}
		inFlight++
	}
	return nil
}

// eligible decides whether this stage may take a ticket sitting in its queue.
// The column has already answered "is this mine"; what is left is whether the
// work is READY and whether this stage has attempts remaining.
func (d *Dispatcher) eligible(t Ticket) bool {
	if !d.readyOnListing(t) {
		return false
	}
	if attempts(pruneStaleClaims(t), d.handler.Role()) >= d.maxAttempts {
		return false
	}
	// The handler gets the final say. Most stages accept everything in their
	// column — the column IS the predicate now — but one that wants a subset of
	// it still says so here, and it is given the FULL ticket to say it with.
	return d.handler.Wants(t)
}

// readyOnListing is the half of eligibility answerable from a listing: the
// column, the authorship guard and the dependency gate all travel on one.
// Separated so a poll can reject work without paying for a read.
func (d *Dispatcher) readyOnListing(t Ticket) bool {
	// THE COLUMN, RE-CHECKED. fill only passes tickets the server returned for
	// this stage's queue, so this is belt-and-braces there — but eligibility is
	// also the answer to "may this stage work this ticket", asked of a ticket from
	// anywhere, and a stage that said yes to work another already holds would put
	// two agents on it.
	if t.Status != d.stage.Ready {
		return false
	}
	// The authorship guard applies to SCOPING ONLY.
	//
	// It exists so the department does not feed its own output back to itself,
	// and under marker routing it had to apply everywhere: a ticket the product
	// manager created carried no triage marker, so the product manager wanted it
	// again. Under column routing that loop is structurally impossible — scoping
	// takes from inbox and writes elsewhere — and applying the guard to every
	// stage would now do real harm, because the child tickets the product manager
	// creates are precisely the work the developer agent exists to pick up.
	if d.stage.Ready == ColInbox && d.agentAuthors[t.CreatedBy] {
		return false
	}
	if attempts(pruneStaleClaims(t), d.handler.Role()) >= d.maxAttempts {
		return false
	}
	// Dependencies gate the stages that START work. A ticket whose prerequisites
	// are unfinished is not late, it is not due: writing code against something
	// that does not exist yet produces a branch nobody can usefully review.
	if d.stage.RequiresDependencies && !dependenciesMet(t) {
		return false
	}
	return true
}

// start claims a ticket and launches the handler.
func (d *Dispatcher) start(ctx context.Context, t Ticket, done chan<- string) error {
	tok := ClaimToken{Host: d.host, Role: d.handler.Role(), RunID: t.TicketID + "@" + d.host}
	// Claim moves the ticket out of its queue column itself — as one conditional
	// write where the server supports it, so the move and the race are decided
	// together rather than in two steps with a window between them.
	if err := d.api.Claim(ctx, t.TicketID, d.stage, tok); err != nil {
		return err
	}

	// RE-READ BEFORE WORKING. Routing no longer needs comments, but the WORK
	// still does: the developer agent's brief is the scoping comment, the
	// reviewer's branch name is the developer's, and a listing carries none of
	// them. The attempt ceiling is also counted from claim comments.
	full, err := d.api.GetTicket(ctx, t.TicketID)
	if err != nil {
		// Unverifiable. Yield rather than work from data known to be partial; the
		// release puts the ticket back for the next poll.
		d.release(ctx, t.TicketID)
		return fmt.Errorf("re-read %s after claim: %w", t.TicketID, err)
	}
	if !d.handler.Wants(full) || attempts(pruneStaleClaims(full), d.handler.Role()) > d.maxAttempts {
		slog.DebugContext(ctx, "released after re-read: not this stage's ticket",
			"ticket_id", t.TicketID, "role", d.handler.Role())
		d.release(ctx, t.TicketID)
		return nil
	}

	d.mu.Lock()
	d.inFlight[t.TicketID] = true
	d.mu.Unlock()

	go func() {
		defer func() { done <- t.TicketID }()
		d.work(ctx, full)
	}()
	return nil
}

// release puts a ticket back, on a detached context for the same reason the
// post-work release uses one: a cancelled context must not strand it in
// progress until some later run's reconcile notices.
func (d *Dispatcher) release(ctx context.Context, ticketID string) {
	releaseCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer stop()
	if err := d.api.MoveTo(releaseCtx, ticketID, d.stage.Ready); err != nil {
		slog.WarnContext(ctx, "could not release ticket; the next reconcile will",
			"ticket_id", ticketID, "error", err)
	}
}

// work runs the handler for one ticket and records the outcome.
//
// ONE SPAN PER TICKET, here rather than in each agent: this is the only place
// every stage passes through, so instrumenting it means a new agent is traced the
// day it is written rather than the day someone remembers. The span carries the
// ticket and the role because "which stage, on which ticket, on which host" is
// the question every investigation starts from — and on a shared host serving
// several developers it is the only way to tell one department's work from
// another's.
func (d *Dispatcher) work(ctx context.Context, t Ticket) {
	ctx, span := d.tracer.Start(ctx, "stage."+d.handler.Role())
	defer span.End()
	span.SetAttributes(
		attribute.String("ticket.id", t.TicketID),
		attribute.String("agent.role", d.handler.Role()),
		attribute.String("agent.host", d.host),
		attribute.String("ticket.priority", t.Priority),
	)

	tctx := d.rec.Start(ctx, t.TicketID+"@"+d.host, t.TicketID, d.handler.Role())

	status, detail, err := d.handler.Handle(tctx, t)
	if err != nil {
		status, detail = OutcomeFailed, err.Error()
		span.RecordError(err)
		span.SetStatus(codes.Error, "handler failed")
		slog.ErrorContext(ctx, "handler failed", "ticket_id", t.TicketID, "role", d.handler.Role(), "error", err)
	}
	span.SetAttributes(attribute.String("outcome.status", status))
	// The DETAIL is what distinguishes "merged" from "conflict" from "no usable
	// resolution" — outcomes a stage reports as success because they are not
	// failures of the stage, only of the attempt. Without it on the span, three
	// very different endings look identical in a trace.
	if detail != "" {
		span.SetAttributes(attribute.String("outcome.detail", clip(detail, 200)))
	}
	slog.InfoContext(ctx, "stage finished",
		"ticket_id", t.TicketID, "role", d.handler.Role(), "status", status, "detail", clip(detail, 200))
	d.rec.Finish(tctx, status, detail)

	// Release on a DETACHED context. The work is finished either way, and if the
	// release inherits a cancelled ctx — which is exactly what happens on
	// shutdown, and on any handler that returns because ctx ended — the ticket is
	// left stuck in progress until some later run's reconcile notices. That is
	// recoverable but wrong: a ticket the department has finished with should not
	// look claimed. (Found by an e2e run that cancelled the dispatcher as soon as
	// the triage comment landed, which is precisely the shutdown ordering.)
	//
	// The ticket ADVANCES rather than being released. Under the old model every
	// stage handed the ticket back to one shared "open" pool and the next stage
	// worked out from comments whether it was its turn; now finishing a stage
	// moves the work to the next column, which is both the handover and the
	// thing a person sees on the board.
	releaseCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer stop()
	next := d.destination(t, status)
	if next == "" {
		// The handler placed the ticket. Moving it again would undo that.
		span.SetAttributes(attribute.Bool("ticket.placed_by_handler", true))
		return
	}
	span.SetAttributes(attribute.String("ticket.next_column", next))
	if uerr := d.api.MoveTo(releaseCtx, t.TicketID, next); uerr != nil {
		slog.WarnContext(ctx, "could not advance ticket after work; the next reconcile will return it to its queue",
			"ticket_id", t.TicketID, "next", next, "error", uerr)
		return
	}
	// The ticket is now in another stage's queue. Tell everyone rather than
	// leaving it to be discovered a poll interval later.
	d.wake.Signal()
}

// destination maps an outcome onto the column the ticket moves to.
//
// THE ONLY PLACE THAT DECIDES. Every stage reports what happened and this reads
// the routing table, so adding a column is one edit and no agent has to be
// taught about it.
func (d *Dispatcher) destination(t Ticket, status string) string {
	switch status {
	case OutcomeSuccess:
		return d.stage.Success
	case OutcomeHandled:
		// The stage placed the ticket itself. An empty destination means "do not
		// move it", which the caller checks for.
		return ""
	case OutcomeConflicted:
		return ColConflicted
	case OutcomeBlocked:
		return d.stage.Exhausted
	case OutcomeReturned:
		// Handed back to an earlier queue. A stage with no Returns column cannot
		// do this, and falling through to "do not move it" would strand the
		// ticket in the working column with nothing coming to collect it — so
		// that case goes where every other dead end goes, in front of a person.
		if d.stage.Returns == "" {
			return d.stage.Exhausted
		}
		return d.stage.Returns
	case OutcomeAbandoned:
		// The host went away mid-task. That is not the agent getting it wrong, so
		// the work goes back to its queue to be picked up again rather than
		// counting against the ticket.
		return d.stage.Ready
	}
	// Failed. Retry while attempts remain — the claim just made is not yet in the
	// copy of the ticket read before the work, hence the +1 — and otherwise put it
	// where a person will see it, rather than cycling it through a queue it cannot
	// leave.
	if attempts(pruneStaleClaims(t), d.handler.Role())+1 >= d.maxAttempts {
		return d.stage.Exhausted
	}
	return d.stage.Ready
}

func (d *Dispatcher) finish(id string) {
	d.mu.Lock()
	delete(d.inFlight, id)
	d.mu.Unlock()
}

// drain waits briefly for in-flight work to report so shutdown logging is honest.
func (d *Dispatcher) drain(done <-chan string) {
	d.mu.Lock()
	n := len(d.inFlight)
	d.mu.Unlock()
	for range n {
		select {
		case id := <-done:
			d.finish(id)
		case <-time.After(5 * time.Second):
			return
		}
	}
}

// InFlight reports how many tickets are being worked.
func (d *Dispatcher) InFlight() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.inFlight)
}

// planDispatch decides which tickets to start right now.
//
// Priority is a REFILL RULE rather than a separate scheduler: the next ticket is
// always the highest-priority available one. It is only an ORDERING here — how
// many run at once is this stage's concurrency and nothing else.
//
// CRITICAL EXCLUSIVITY BELONGS TO THE MODEL, NOT TO THIS STAGE. Queue.Acquire
// already gives a critical request the whole serving class: nothing else is
// admitted while one runs, and a waiting one holds the door shut until the box
// drains (queue.go, admission rules 1-3). That is the thing worth protecting,
// because the point of running a critical ticket alone is that one stream is
// FASTER than a share of four — ~160 tok/s against ~317 aggregate split between
// them.
//
// Pinning the stage as well used to look like the same rule, and was strictly
// worse. A dispatcher is per stage, so it could only stop ITS OWN queue from
// claiming; the other five stages carried on hitting the same llama-server. The
// critical ticket therefore never actually got an exclusive stream — it merely
// stopped its siblings from being worked. Measured on a seven-ticket board that
// inherited "critical" from its parent: two agents running against four slots,
// one of them idle, and every stage serialised end to end.
//
// So: order by priority, fill to concurrency, and let the queue decide who gets
// the GPU. Preemption is still never on the table anywhere — the tokens already
// spent on in-flight work would be thrown away.
func planDispatch(candidates []Ticket, inFlight, max int) []Ticket {
	if len(candidates) == 0 || inFlight >= max {
		return nil
	}
	ordered := make([]Ticket, len(candidates))
	copy(ordered, candidates)
	sort.SliceStable(ordered, func(i, j int) bool {
		pi, pj := ParsePriority(ordered[i].Priority), ParsePriority(ordered[j].Priority)
		if pi != pj {
			return pi > pj
		}
		return ordered[i].CreatedAt.Before(ordered[j].CreatedAt)
	})

	room := max - inFlight
	if room > len(ordered) {
		room = len(ordered)
	}
	return ordered[:room]
}

// returnedMarker records that a stage handed a ticket BACK to an earlier queue.
// An HTML comment for the same reason the claim record is one: it has to be
// matched exactly by code and it should not be something a person reads.
const returnedMarker = "<!-- blacksmith:returned -->"

// attempts counts how many times this role has claimed a ticket IN THE CURRENT
// ROUND. The claim comments are the attempt log, so no extra state is needed.
//
// The round matters because a ticket can now come backwards. When the reviewer
// rejects a change the ticket returns to the developer's queue, and the work
// waiting there is not the work that was already tried — it is a fix for a
// finding that did not exist when those attempts were spent. Counting across the
// return would let a ticket be sent back and then refused by the very stage
// asked to fix it, which is the stranding failure this pipeline has already been
// bitten by once: a queue nothing will ever select from.
//
// Comments are sorted rather than trusted in file order. The ticket service does
// return them in order today, but the whole meaning of the count depends on
// which side of the marker a claim falls, and that is too much to rest on an
// undocumented guarantee — the claim arbitration in codearmory.go sorts for the
// same reason.
func attempts(t Ticket, role string) int {
	ordered := make([]Comment, len(t.Comments))
	copy(ordered, t.Comments)
	sort.SliceStable(ordered, func(i, j int) bool {
		if !ordered[i].CreatedAt.Equal(ordered[j].CreatedAt) {
			return ordered[i].CreatedAt.Before(ordered[j].CreatedAt)
		}
		return ordered[i].CommentID < ordered[j].CommentID
	})

	n := 0
	for _, c := range ordered {
		// A RETURN RESTARTS THE COUNT, whichever direction it came from.
		//
		// The reviewer's send-back has always reset it: the ticket goes back a stage
		// and comes forward again, and the attempts it spent getting rejected should
		// not be charged against the corrected version. A specification handed back
		// by the developer is the same journey and was not resetting anything, so
		// every round trip cost one dev attempt AND one spec attempt out of three.
		//
		// Measured on r68: "status parameter filtering" reached three claims for
		// each role, and the dispatcher then refused to give it to either — a ticket
		// sitting in ready_for_dev that no developer would ever claim, with nothing
		// in any counter to say why. The hand-back loop was consuming the budget it
		// needed to converge.
		//
		// Safe to reset here because the round trips are bounded already, by
		// maxSpecRepairs in endOnBrokenSpec — this cannot yield more resets than
		// that ceiling allows.
		if strings.Contains(c.Body, returnedMarker) || strings.Contains(c.Body, specRepairMarker) {
			n = 0
			continue
		}
		if !strings.HasPrefix(c.Body, claimMarker) {
			continue
		}
		if tok, err := parseClaim(c.Body); err == nil && tok.Role == role {
			n++
		}
	}
	return n
}

// revivePrerequisite gives a dead prerequisite another attempt, because the
// tickets waiting on it are what make its failure worth retrying.
//
// TWO WRONG ANSWERS WERE TRIED FIRST, and naming them is the point of this
// comment. Stranding the dependents turned one real failure into a batch that
// never ran — measured at one failure and six untouched tickets. Releasing the
// dependency and letting them run anyway was worse in a subtler way: a
// prerequisite is BY DEFINITION needed, so the dependents then fail for a reason
// that is not theirs, and the run produces six misleading failures instead of
// one honest one.
//
// The thing that actually wants retrying is the prerequisite. Its failure is
// more consequential than an ordinary ticket's precisely because other work
// rests on it, and it is the only ticket whose success unblocks the rest. So it
// goes back to a queue with a fresh round of attempts, using the same mechanism
// a review rejection uses — see attempts(), where a return marker restarts the
// count.
//
// Bounded, because a prerequisite that cannot be built is a real answer too. At
// the ceiling it is left blocked and the dependents strand behind it, which is
// the correct end state: a person is needed.
func (d *Dispatcher) handleDeadPrerequisite(ctx context.Context, t Ticket, dep TicketDependency) {
	blockerID := dep.TicketID
	blocker, err := d.api.GetTicket(ctx, blockerID)
	if err != nil {
		slog.WarnContext(ctx, "could not read a blocked prerequisite", "ticket_id", blockerID, "error", err)
		return
	}
	if blocker.Status != ColBlocked {
		return // it moved on its own; nothing to revive
	}
	if n := revivalsSoFar(blocker); n >= maxRevivals {
		// Tried enough. The prerequisite stays blocked and a person decides — but
		// the ticket waiting on it must NOT be left in a queue nothing will ever
		// collect. That was the original failure: candidates skipped every poll
		// forever, looking queued while nothing was coming for them.
		d.strandBehind(ctx, t, blockerName(dep))
		return
	}
	body := fmt.Sprintf("%s %q depends on this ticket and cannot proceed while it is blocked, so it is "+
		"going back for another attempt with a fresh budget.\n\nA prerequisite is needed by definition: "+
		"letting the work that rests on it run without it would only produce failures that are not its "+
		"fault. %s", revivedMarker, t.Title, returnedMarker)
	if _, err := d.api.AddComment(ctx, blockerID, body); err != nil {
		slog.WarnContext(ctx, "could not explain a revived prerequisite", "ticket_id", blockerID, "error", err)
		return
	}
	// BACK TO ITS OWN QUEUE, NOT THE REVIVER'S. This sent the blocker to
	// d.stage.Ready — the column of whichever dispatcher happened to notice it —
	// which was harmless while every stage worked the same kind of ticket and
	// wrong the moment they did not.
	//
	// Measured: a blocked TASK was revived by a section dispatcher into
	// ready_for_spec, where the section author claimed it, read a hundred times
	// and was killed. A task belongs to the stage that develops it; a section to
	// the stage that writes it.
	queue := d.reviveQueue(ctx, blockerID)
	if err := d.api.MoveTo(ctx, blockerID, queue); err != nil {
		slog.ErrorContext(ctx, "could not requeue a blocked prerequisite",
			"ticket_id", blockerID, "error", err)
		return
	}
	slog.InfoContext(ctx, "revived a blocked prerequisite because work depends on it",
		"ticket_id", blockerID, "dependent", t.TicketID, "queue", queue)
	d.wake.Signal()
}

// maxBlockedRetries bounds how many times a blocked ticket is put back on its
// own account, with nothing depending on it to force the issue.
//
// Three, because most blocks are not decisions — they are a stage having been
// wrong once. Measured on r79: a reviewer answered that a specification could not
// be implemented while its own reason said every assertion was satisfiable, and
// five tasks blocked behind that one misread verdict. Nothing was wrong with any
// of them and no retry existed to find that out; the board sat still until a
// person looked.
//
// It is not unbounded, because a ticket that fails the same way three times is
// telling you something a fourth will not, and a queue that never gives up looks
// identical to one that is working.
const maxBlockedRetries = 3

// retryMarker records a blocked ticket put back for another go.
const retryMarker = "**Retried after blocking.**"

func blockedRetriesSoFar(t Ticket) int {
	n := 0
	for _, c := range t.Comments {
		if strings.Contains(c.Body, retryMarker) {
			n++
		}
	}
	return n
}

// retryBlocked puts blocked tickets back for another attempt.
//
// A BLOCK IS USUALLY A STAGE HAVING BEEN WRONG, NOT AN ANSWER. handleDeadPrerequisite
// already revives a blocked ticket that something else is waiting on, because the
// dependents make it urgent — but a ticket nobody depends on, or the last one in
// a chain, had no way back at all. It stayed blocked until a person noticed,
// however transient the cause.
//
// The retry RESETS THE PATH rather than just re-queueing it. returnedMarker gives
// the stage a fresh budget, and specRepairsSoFar treats the same comment as a
// clean slate, so a ticket that exhausted its hand-backs to the specification
// author can use them again. Putting it back with its counters spent would buy
// one turn and block again.
//
// Each stage takes only its own, decided by the claim history rather than by the
// column, for the same reason handleDeadPrerequisite revives into reviveQueue: a
// task and a section look alike, and sending a task to the section author's queue
// had it read a hundred times and killed.
func (d *Dispatcher) retryBlocked(ctx context.Context) {
	blocked, err := d.api.ListTickets(ctx, ListOpts{BoardID: d.boardID, Status: ColBlocked})
	if err != nil {
		slog.WarnContext(ctx, "could not list blocked tickets to retry", "error", err)
		return
	}
	for _, t := range blocked {
		// Re-read: the listing carries no comments, and every decision below is
		// made from them. The same trap unstrand documents.
		full, ferr := d.api.GetTicket(ctx, t.TicketID)
		if ferr != nil {
			continue
		}
		// A ticket stranded behind a blocker is unstrand's, not this sweep's:
		// putting it back before its blocker recovers only fails it again.
		if _, ok := strandedFrom(full); ok {
			continue
		}
		if _, still := dependencyDeadlocked(full); still {
			continue
		}
		queue := d.reviveQueue(ctx, full.TicketID)
		if queue != d.stage.Ready {
			continue // another stage owns this one
		}
		if n := blockedRetriesSoFar(full); n >= maxBlockedRetries {
			continue // it has had its three; a person decides now
		}

		n := blockedRetriesSoFar(full) + 1
		body := fmt.Sprintf("%s Nothing depends on this and it has been sitting blocked, so it goes back "+
			"to `%s` with a fresh budget and its hand-backs reset. Attempt %d of %d.\n\nMost blocks are a "+
			"stage having been wrong once rather than a decision; if this one is real it will block again "+
			"and stay blocked. %s", retryMarker, queue, n, maxBlockedRetries, returnedMarker)
		if _, cerr := d.api.AddComment(ctx, full.TicketID, body); cerr != nil {
			slog.WarnContext(ctx, "could not explain a retry", "ticket_id", full.TicketID, "error", cerr)
			continue
		}
		if merr := d.api.MoveTo(ctx, full.TicketID, queue); merr != nil {
			slog.WarnContext(ctx, "could not retry a blocked ticket", "ticket_id", full.TicketID, "error", merr)
			continue
		}
		slog.InfoContext(ctx, "retried a blocked ticket", "ticket_id", full.TicketID,
			"queue", queue, "attempt", n, "of", maxBlockedRetries)
		d.wake.Signal()
	}
}

// maxRevivals bounds how many times a prerequisite is sent back. Small on
// purpose: each revival costs a full attempt at every stage, and a foundation
// that has failed three rounds is telling you something a fourth will not.
const maxRevivals = 2

// revivedMarker records a prerequisite that was sent back because something
// depends on it.
const revivedMarker = "**Prerequisite revived.**"

func revivalsSoFar(t Ticket) int {
	n := 0
	for _, c := range t.Comments {
		if strings.Contains(c.Body, revivedMarker) {
			n++
		}
	}
	return n
}

// strandBehind is the end of the line: a prerequisite that could not be built
// even after being sent back, and the work resting on it.
//
// Reached only once revival is exhausted, and it exists so a dependent is never
// left in a queue nothing will collect. That silent state — a ticket that looks
// queued while nothing is coming for it — is the failure this whole path has
// been circling, and it is worse than an honest move to the column a person
// watches.
func (d *Dispatcher) strandBehind(ctx context.Context, t Ticket, blocker string) {
	body := fmt.Sprintf("**Stranded.** This ticket waits on %q, which stayed blocked after being sent back "+
		"for another attempt.\n\nBuild that ticket, or drop the dependency if it is no longer needed, and "+
		"move this back to `%s`.\n\n%s%s -->", blocker, d.stage.Ready, strandedMarker, d.stage.Ready)
	if _, err := d.api.AddComment(ctx, t.TicketID, body); err != nil {
		slog.WarnContext(ctx, "could not explain why a ticket was stranded", "ticket_id", t.TicketID, "error", err)
	}
	if err := d.api.MoveTo(ctx, t.TicketID, d.stage.Exhausted); err != nil {
		slog.ErrorContext(ctx, "could not move a stranded ticket out of the queue",
			"ticket_id", t.TicketID, "blocker", blocker, "error", err)
		return
	}
	slog.WarnContext(ctx, "stranded a ticket whose prerequisite could not be built",
		"ticket_id", t.TicketID, "blocker", blocker, "role", d.handler.Role())
}

// strandedMarker records, in the stranding comment itself, the column the ticket
// came out of.
//
// THE COLUMN HAS TO BE WRITTEN DOWN, because a stranded ticket usually has no
// claim history to infer it from — it was stranded precisely because nothing ever
// got to work on it. reviveQueue can read a blocker's history; this one has none.
// Measured on r70: all twelve stranded tickets had zero claims between them, so
// any scheme that derived the queue from history would have had to guess for
// every single one.
const strandedMarker = "<!-- blacksmith:stranded "

// strandedFrom returns the column a ticket was stranded out of.
//
// Only the LAST word counts. A ticket can be stranded, revived, worked and
// stranded again, and anything that moves it forward — a claim, a return —
// settles the question more recently than an older stranding note did.
func strandedFrom(t Ticket) (string, bool) {
	ordered := make([]Comment, len(t.Comments))
	copy(ordered, t.Comments)
	sort.SliceStable(ordered, func(i, j int) bool {
		if !ordered[i].CreatedAt.Equal(ordered[j].CreatedAt) {
			return ordered[i].CreatedAt.Before(ordered[j].CreatedAt)
		}
		return ordered[i].CommentID < ordered[j].CommentID
	})
	col, ok := "", false
	for _, c := range ordered {
		if _, err := parseClaim(c.Body); err == nil {
			col, ok = "", false
			continue
		}
		if strings.Contains(c.Body, returnedMarker) {
			col, ok = "", false
			continue
		}
		if i := strings.Index(c.Body, strandedMarker); i >= 0 {
			rest := c.Body[i+len(strandedMarker):]
			if j := strings.Index(rest, " -->"); j >= 0 {
				col, ok = strings.TrimSpace(rest[:j]), true
			}
		}
	}
	return col, ok && col != ""
}

// unstrand puts back the tickets that were stranded behind a blocker which has
// since recovered.
//
// THE CASCADE HEALS IN ONE DIRECTION ONLY, and that is the bug this exists to
// close. handleDeadPrerequisite revives a dependent while the BLOCKER is blocked;
// once it strands the dependent, the dependent is itself in blocked, which no
// dispatcher polls. So if the blocker later succeeds — which is the ordinary
// outcome of reviving it — nothing is left watching, and the tickets behind it
// sit in blocked for the rest of the run looking like a human decision.
//
// Measured on r70: two dev failures from a broken settings file stranded twelve
// tickets, the blocker then reached done on a later attempt, and the board sat at
// blocked=12 done=6 with nothing running and nothing wrong. It had to be put back
// by hand.
//
// Each stage takes only its own, which is what keeps seven dispatchers from
// fighting over one ticket: the column in the marker was written by the stage
// that stranded it, so exactly one stage matches.
func (d *Dispatcher) unstrand(ctx context.Context) {
	blocked, err := d.api.ListTickets(ctx, ListOpts{BoardID: d.boardID, Status: ColBlocked})
	if err != nil {
		slog.WarnContext(ctx, "could not list blocked tickets to unstrand", "error", err)
		return
	}
	for _, t := range blocked {
		// Re-read: the listing carries no comments, and the marker is a comment.
		// The same trap Reconcile documents — judged from the listing alone,
		// nothing is ever stranded and the sweep is a silent no-op.
		full, ferr := d.api.GetTicket(ctx, t.TicketID)
		if ferr != nil {
			continue
		}
		col, ok := strandedFrom(full)
		if !ok || col != d.stage.Ready {
			continue
		}
		if _, still := dependencyDeadlocked(full); still {
			continue // its blocker is still blocked; stranding is still the truth
		}
		body := "**Unstranded.** The ticket this was waiting on is no longer blocked, so this goes back to `" +
			d.stage.Ready + "` with a fresh budget. It was never attempted.\n\n" + returnedMarker
		if _, cerr := d.api.AddComment(ctx, t.TicketID, body); cerr != nil {
			slog.WarnContext(ctx, "could not explain an unstranding", "ticket_id", t.TicketID, "error", cerr)
		}
		if merr := d.api.MoveTo(ctx, t.TicketID, d.stage.Ready); merr != nil {
			slog.WarnContext(ctx, "could not unstrand a ticket", "ticket_id", t.TicketID, "error", merr)
			continue
		}
		slog.InfoContext(ctx, "unstranded a ticket whose blocker recovered",
			"ticket_id", t.TicketID, "column", d.stage.Ready, "role", d.handler.Role())
	}
}

// reviveQueue is the column a blocked prerequisite belongs back in.
//
// The ticket's own CLAIM HISTORY answers this: every stage records the role that
// took it, so the most recent claim names the stage that owns this kind of
// ticket. That is more reliable than inferring from shape — a task and a section
// look alike, and only their history says which is which.
//
// Falls back to the reviver's own queue when there is no history to read, which
// is the old behaviour and is right for a ticket no stage has touched.
func (d *Dispatcher) reviveQueue(ctx context.Context, blockerID string) string {
	full, err := d.api.GetTicket(ctx, blockerID)
	if err != nil {
		return d.stage.Ready
	}
	for i := len(full.Comments) - 1; i >= 0; i-- {
		tok, err := parseClaim(full.Comments[i].Body)
		if err != nil {
			continue
		}
		if st, ok := stageFor(tok.Role); ok {
			return st.Ready
		}
	}
	return d.stage.Ready
}

// staleClaimAfter is how long a claim may stand before it is treated as
// abandoned rather than as an attempt spent.
//
// Comfortably longer than any real attempt — a lease and an iteration budget
// both expire well inside it — so this only ever forgives claims whose process
// is gone.
const staleClaimAfter = 45 * time.Minute

// pruneStaleClaims drops claims whose process is gone, so they do not count as
// attempts spent.
//
// A CLAIM IS WRITTEN WHEN WORK STARTS, and nothing closes it if the process dies
// mid-attempt. The ceiling counts claims, so an interrupted run silently costs
// the ticket an attempt — and three of them make it permanently unclaimable, with
// nothing on the board to say why. Measured and self-inflicted: restarting
// blacksmith three times to install fixes stranded a ticket in ready_for_dev that
// no developer would take.
//
// Kept OUT of attempts() on purpose. That function answers "how many claims are
// recorded", which is a counting question its tests pin precisely; this answers
// "which claims are still real", which is a liveness question and belongs where
// the ceiling is applied.
func pruneStaleClaims(t Ticket) Ticket {
	kept := make([]Comment, 0, len(t.Comments))
	for _, c := range t.Comments {
		// A TIMESTAMP THAT IS NOT A REAL TIME CARRIES NO LIVENESS INFORMATION, and
		// must not be read as "very old". Claims come back from the API with real
		// times; a zero or epoch value means the field was never populated, and
		// treating that as abandoned would forgive every claim ever made.
		if strings.HasPrefix(c.Body, claimMarker) && c.CreatedAt.Year() >= 2000 &&
			time.Since(c.CreatedAt) > staleClaimAfter {
			continue
		}
		kept = append(kept, c)
	}
	t.Comments = kept
	return t
}

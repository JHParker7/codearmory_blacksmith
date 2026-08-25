// Package dispatch runs one handler against a stream of tickets.
//
// It is the only place every stage passes through, which is what makes it the
// right home for three things no agent should have to repeat: claiming work
// race-safely, deciding where a finished ticket goes, and recording that it
// happened. An agent written tomorrow is traced, counted and routed the day it
// is written rather than the day someone remembers.
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/platform"
	"github.com/code-armory-app/blacksmith/internal/queue"
	"github.com/code-armory-app/blacksmith/internal/record"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/transcript"
	"github.com/code-armory-app/blacksmith/internal/transport"
	"github.com/code-armory-app/blacksmith/internal/wake"
	"github.com/code-armory-app/blacksmith/internal/workflow"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// DefaultPoll is how often a stage looks for work it has not been told about.
//
// The interval is a floor on hand-off latency and a multiplier on read volume,
// which is why the wake trigger exists beside it. On a large board a short
// interval multiplies against the hydration cap, and the answer there is not a
// longer timer: it is to stop polling and have the platform push.
const DefaultPoll = 15 * time.Second

// DefaultMaxAttempts is the loop breaker's ceiling. A ticket an agent has
// already failed this many times is left alone rather than retried forever.
const DefaultMaxAttempts = 3

// MaxHydratePerPoll bounds how many tickets one poll reads in full.
//
// Selection needs comments and a listing has none, so each candidate costs a
// request. This is what keeps a large board from becoming hundreds of requests
// every poll: past the cap the poll works with what it has and picks the rest up
// next time. Deliberately well above any realistic number of tickets a
// department is choosing between at once.
const MaxHydratePerPoll = 50

// Handler works one ticket. The agents implement it.
type Handler interface {
	// Role is the agent account this handler acts as.
	Role() string

	// Class is the model class the handler needs.
	Class() model.Class

	// Wants reports whether this handler should work a ticket.
	//
	// Without it two agents on one host race for the same tickets, and the claim
	// makes that race silently destructive: whichever wins, the other never sees
	// the ticket again. Under column routing most stages accept everything in
	// their queue — the column IS the predicate — but one that wants a subset of
	// it says so here, given the FULL ticket to say it with.
	Wants(t ticket.Ticket) bool

	// Handle does the work and returns an outcome plus a short detail for the
	// transcript.
	Handle(ctx context.Context, t ticket.Ticket) (status workflow.Outcome, detail string, err error)
}

// Store is the ticket operations a dispatcher needs.
//
// AN INTERFACE, so the loop can be driven against an in-memory board. What is
// being tested here is a claim-and-route protocol, and a test that has to stand
// up a real store to check the ordering of two moves is a test nobody writes.
type Store interface {
	List(ctx context.Context, opts ticket.ListOpts) ([]ticket.Ticket, error)
	Get(ctx context.Context, id string) (ticket.Ticket, error)
	AddComment(ctx context.Context, id, body string) (ticket.Comment, error)
	MoveTo(ctx context.Context, id, column string) error
	Claim(ctx context.Context, id string, st workflow.Stage, c record.Claim) error
}

var _ Store = (*platform.Store)(nil)

// Options configures a Dispatcher.
type Options struct {
	Host    string
	BoardID string

	// Concurrency is how many tickets this stage works at once. It is an ordering
	// input only — how many model requests actually run is the serving class's
	// admission queue, not this.
	Concurrency int

	Poll        time.Duration
	MaxAttempts int

	// AgentAuthors are the usernames of agent accounts. See readyOnListing.
	AgentAuthors []string

	// Wake is the shared cross-stage trigger. Optional: nil leaves the dispatcher
	// on its poll timer alone.
	Wake *wake.Trigger

	// Tracer reports under this role's own service name, so the stages are
	// several services rather than one averaged together. Nil is allowed.
	Tracer trace.Tracer

	// Recorder captures the transcript. Nil is allowed and records nothing.
	Recorder *transcript.Recorder
}

// Dispatcher runs one handler against a stream of tickets.
type Dispatcher struct {
	store   Store
	handler Handler
	rec     *transcript.Recorder

	// stage is the routing entry for this handler's role: the column it takes
	// from, the one it parks held work in, and where a ticket goes when it
	// finishes or runs out of attempts.
	stage workflow.Stage

	host        string
	boardID     string
	concurrency int
	poll        time.Duration
	maxAttempts int

	agentAuthors map[string]bool
	tracer       trace.Tracer
	wake         *wake.Trigger

	// now is the clock staleness is judged against. Injectable so a test can put
	// a claim in the past without waiting for it.
	now func() time.Time

	mu       sync.Mutex
	inFlight map[string]bool
}

// New builds a dispatcher for a handler.
//
// A HANDLER WITH NO ROUTING ENTRY IS A WIRING MISTAKE and fails here, where the
// message can name the role. Left to run, it would poll a column that does not
// exist and silently never work — the failure mode this whole package is written
// to make impossible.
func New(store Store, h Handler, tb workflow.Table, opts Options) (*Dispatcher, error) {
	st, ok := tb.For(h.Role())
	if !ok {
		return nil, fmt.Errorf("no workflow stage for role %q on this host", h.Role())
	}
	if opts.Concurrency < 1 {
		opts.Concurrency = 1
	}
	if opts.Poll <= 0 {
		opts.Poll = DefaultPoll
	}
	if opts.MaxAttempts < 1 {
		opts.MaxAttempts = DefaultMaxAttempts
	}

	authors := make(map[string]bool, len(opts.AgentAuthors))
	for _, a := range opts.AgentAuthors {
		authors[a] = true
	}

	return &Dispatcher{
		store: store, handler: h, rec: opts.Recorder,
		stage:        st,
		host:         opts.Host,
		boardID:      opts.BoardID,
		concurrency:  opts.Concurrency,
		poll:         opts.Poll,
		maxAttempts:  opts.MaxAttempts,
		agentAuthors: authors,
		tracer:       opts.Tracer,
		wake:         opts.Wake,
		now:          time.Now,
		inFlight:     map[string]bool{},
	}, nil
}

// Stage reports the routing entry this dispatcher works.
func (d *Dispatcher) Stage() workflow.Stage { return d.stage }

// InFlight reports how many tickets are being worked.
func (d *Dispatcher) InFlight() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.inFlight)
}

// Run drives the loop until ctx ends.
func (d *Dispatcher) Run(ctx context.Context) error {
	if err := d.Reconcile(ctx); err != nil {
		// NOT FATAL. A failed reconcile leaves some tickets claimed until the next
		// restart, which is better than refusing to start at all.
		slog.WarnContext(ctx, "startup reconcile failed; some tickets may stay claimed",
			"role", d.handler.Role(), "error", err)
	}

	done := make(chan string, d.concurrency)
	ticker := time.NewTicker(d.poll)
	defer ticker.Stop()
	woken := d.wake.Subscribe()

	for {
		if err := d.fill(ctx, done); err != nil && !errors.Is(err, context.Canceled) {
			slog.WarnContext(ctx, "fill failed; will retry on the next tick",
				"role", d.handler.Role(), "error", err)
		}

		select {
		case <-ctx.Done():
			d.drain(done)
			return ctx.Err()

		case id := <-done:
			// EDGE TRIGGER: refill the moment a slot frees rather than waiting for
			// the batch to drain. Aggregate throughput holds only while every slot
			// is busy.
			d.finish(id)

		case <-woken:
			// Edge trigger ACROSS stages: some stage moved a ticket, so this one may
			// have work now. Without it a hand-off waits for the timer.

		case <-ticker.C:
			// LEVEL TRIGGER. Still required: it is what finds tickets that appear
			// from outside this process, which no in-process signal can know about.
		}
	}
}

// Reconcile releases tickets THIS HOST claimed that have no live work.
//
// Scoped to this host deliberately: another host's in-flight work is
// indistinguishable from a stranded claim when viewed from here, so an unscoped
// sweep would steal live work from a peer.
func (d *Dispatcher) Reconcile(ctx context.Context) error {
	held, err := d.store.List(ctx, ticket.ListOpts{BoardID: d.boardID, Status: d.stage.Working})
	if err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}

	var released int
	for _, t := range held {
		// RE-READ. Whose claim this is lives in a COMMENT and a listing carries
		// none, so judged from the listing alone no ticket ever looks like this
		// host's — nothing is released and stranded claims accumulate silently,
		// which is the exact failure this exists to prevent.
		full, ferr := d.store.Get(ctx, t.ID)
		if ferr != nil {
			slog.WarnContext(ctx, "reconcile: could not re-read ticket", "ticket_id", t.ID, "error", ferr)
			continue
		}
		c, ok := record.HeldBy(full, true)
		if !ok || c.Host != d.host {
			continue
		}

		d.mu.Lock()
		live := d.inFlight[t.ID]
		d.mu.Unlock()
		if live {
			continue
		}

		// AN INTERRUPTED ATTEMPT IS NOT A SPENT ONE, and saying so here is the whole
		// difference between a ticket that resumes and one that sits unclaimable.
		//
		// The ceiling counts claims. A claim is written when work starts and nothing
		// closes it when the process is killed, so every restart to install a fix
		// charges the in-flight ticket one attempt. Stale-claim pruning forgives
		// those, but only after its window — and this host KNOWS, right now, that it
		// is the one that died, so there is nothing to wait for.
		//
		// Measured: six restarts in a morning put three claims on one ticket at
		// 08:59, 09:06 and 09:10. It then sat in its queue with nothing wrong with
		// it and was picked up at 09:45:02, four seconds after the first claim aged
		// out. Eighteen minutes of an idle department, entirely self-inflicted.
		if _, cerr := d.store.AddComment(ctx, t.ID, InterruptedBody); cerr != nil {
			// Not fatal: releasing still beats leaving it held. It costs the ticket
			// an attempt, which is the behaviour this comment replaces.
			slog.WarnContext(ctx, "reconcile: could not clear the interrupted attempt",
				"ticket_id", t.ID, "error", cerr)
		}
		if err := d.store.MoveTo(ctx, t.ID, d.stage.Ready); err != nil {
			slog.WarnContext(ctx, "reconcile: could not release ticket", "ticket_id", t.ID, "error", err)
			continue
		}
		released++
	}

	if released > 0 {
		slog.InfoContext(ctx, "reconcile released stranded claims",
			"count", released, "host", d.host, "role", d.handler.Role())
	}
	return nil
}

// InterruptedBody resets the attempt count for work this host was killed in the
// middle of. It carries the returned marker, which attempt counting treats as a
// fresh start — the same mechanism a reviewer's send-back uses.
const InterruptedBody = "**Attempt interrupted.** The agent host restarted while this ticket was being " +
	"worked, so the claim it left behind is not an attempt anyone spent. It goes back to its queue with " +
	"a fresh budget.\n\n" + record.ReturnedMarker

// fill claims and starts work until the concurrency target is met.
func (d *Dispatcher) fill(ctx context.Context, done chan<- string) error {
	// THE COLUMN NARROWS; HYDRATION STILL HAPPENS.
	//
	// Asking the store for this stage's column replaces the marker predicates that
	// routing used to need, and it makes the read below cheap: the listing is a
	// handful of tickets waiting on one stage rather than every open ticket.
	//
	// But it does NOT remove the read. A listing carries no comments, and the
	// attempt ceiling is counted from claim records. Judged from a listing that
	// evaluates against nothing — silently, and in the worst direction, since a
	// predicate requiring a marker's ABSENCE is satisfied by every ticket and
	// re-claims the column every poll.
	listed, err := d.store.List(ctx, ticket.ListOpts{BoardID: d.boardID, Status: d.stage.Ready})
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

	candidates := make([]ticket.Ticket, 0, len(listed))
	var looked int
	for _, t := range listed {
		if busy[t.ID] {
			continue
		}
		// Dependencies and authorship travel ON the listing, so a ticket that is
		// not ready costs nothing to reject.
		if !d.readyOnListing(t) {
			continue
		}
		if looked >= MaxHydratePerPoll {
			slog.DebugContext(ctx, "stopped hydrating candidates at the per-poll cap",
				"cap", MaxHydratePerPoll, "listed", len(listed), "role", d.handler.Role())
			break
		}
		looked++

		full, err := d.store.Get(ctx, t.ID)
		if err != nil {
			// ONE UNREADABLE TICKET MUST NOT COST THE WHOLE POLL.
			slog.DebugContext(ctx, "skipping ticket that could not be read",
				"ticket_id", t.ID, "error", err)
			continue
		}
		if d.Eligible(full) {
			candidates = append(candidates, full)
		}
	}

	for _, t := range Plan(candidates, inFlight, d.concurrency) {
		if err := d.start(ctx, t, done); err != nil {
			if transport.Conflict(err) {
				// A PEER WON THE RACE. Expected, not an error worth logging loudly.
				slog.DebugContext(ctx, "ticket claimed by another host", "ticket_id", t.ID)
				continue
			}
			slog.WarnContext(ctx, "could not start ticket", "ticket_id", t.ID, "error", err)
			continue
		}
		inFlight++
	}
	return nil
}

// Eligible decides whether this stage may take a ticket sitting in its queue.
//
// The column has already answered "is this mine"; what is left is whether the
// work is READY and whether this stage has attempts remaining.
func (d *Dispatcher) Eligible(t ticket.Ticket) bool {
	if !d.readyOnListing(t) {
		return false
	}
	if d.spent(t) >= d.maxAttempts {
		return false
	}
	// The handler gets the final say, with the full ticket to say it with.
	return d.handler.Wants(t)
}

// spent counts the attempts this role has really used on a ticket: claims minus
// the ones whose process is gone.
func (d *Dispatcher) spent(t ticket.Ticket) int {
	return record.Attempts(record.PruneStale(t, d.now()), d.handler.Role())
}

// readyOnListing is the half of eligibility answerable FROM A LISTING: the
// column, the authorship guard and the dependency gate all travel on one.
// Separated so a poll can reject work without paying for a read.
func (d *Dispatcher) readyOnListing(t ticket.Ticket) bool {
	// THE COLUMN, RE-CHECKED. fill only passes tickets the store returned for this
	// stage's queue, so it is belt-and-braces there — but this also answers "may
	// this stage work this ticket" asked of a ticket from anywhere, and a stage
	// that said yes to work another already holds would put two agents on it.
	if t.Status != d.stage.Ready {
		return false
	}

	// THE AUTHORSHIP GUARD APPLIES TO INTAKE ONLY.
	//
	// It exists so the department does not feed its own output back to itself, and
	// under marker routing it had to apply everywhere: a ticket the product
	// manager created carried no triage marker, so the product manager wanted it
	// again. Under column routing that loop is structurally impossible — intake
	// takes from the inbox and writes elsewhere — and applying the guard to every
	// stage would now do real harm, because the child tickets the product manager
	// creates are precisely the work the developer exists to pick up.
	if d.stage.Ready == workflow.ColInbox && d.agentAuthors[t.CreatedBy] {
		return false
	}

	if d.spent(t) >= d.maxAttempts {
		return false
	}

	// Dependencies gate the stages that START work. A ticket whose prerequisites
	// are unfinished is not late, it is NOT DUE: writing code against something
	// that does not exist yet produces a branch nobody can usefully review.
	if d.stage.RequiresDependencies && !workflow.Ready(t) {
		return false
	}
	return true
}

// start claims a ticket and launches the handler.
func (d *Dispatcher) start(ctx context.Context, t ticket.Ticket, done chan<- string) error {
	c := record.Claim{Host: d.host, Role: d.handler.Role(), RunID: t.ID + "@" + d.host}

	// Claim moves the ticket out of its queue itself — as one conditional write
	// where the store supports it, so the move and the race are decided together
	// rather than in two steps with a window between them.
	if err := d.store.Claim(ctx, t.ID, d.stage, c); err != nil {
		return err
	}

	// RE-READ BEFORE WORKING. Routing no longer needs comments, but the WORK does:
	// the developer's brief is the scoping comment, the reviewer's branch name is
	// the developer's, and a listing carries none of them.
	full, err := d.store.Get(ctx, t.ID)
	if err != nil {
		// Unverifiable. Yield rather than work from data known to be partial; the
		// release puts the ticket back for the next poll.
		d.release(ctx, t.ID)
		return fmt.Errorf("re-read %s after claim: %w", t.ID, err)
	}
	if !d.handler.Wants(full) || d.spent(full) > d.maxAttempts {
		slog.DebugContext(ctx, "released after re-read: not this stage's ticket",
			"ticket_id", t.ID, "role", d.handler.Role())
		d.release(ctx, t.ID)
		return nil
	}

	d.mu.Lock()
	d.inFlight[t.ID] = true
	d.mu.Unlock()

	go func() {
		defer func() { done <- t.ID }()
		d.work(ctx, full)
	}()
	return nil
}

// release puts a ticket back, on a DETACHED context for the same reason the
// post-work move uses one: a cancelled context must not strand it in progress
// until some later run's reconcile notices.
func (d *Dispatcher) release(ctx context.Context, id string) {
	releaseCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer stop()
	if err := d.store.MoveTo(releaseCtx, id, d.stage.Ready); err != nil {
		slog.WarnContext(ctx, "could not release ticket; the next reconcile will",
			"ticket_id", id, "error", err)
	}
}

// work runs the handler for one ticket and records the outcome.
//
// ONE SPAN PER TICKET, here rather than in each agent: this is the only place
// every stage passes through, so instrumenting it means a new agent is traced the
// day it is written. The span carries the ticket, the role and the host because
// "which stage, on which ticket, on which host" is the question every
// investigation starts from — and on a shared host it is the only way to tell one
// department's work from another's.
func (d *Dispatcher) work(ctx context.Context, t ticket.Ticket) {
	var span trace.Span
	if d.tracer != nil {
		ctx, span = d.tracer.Start(ctx, "stage."+d.handler.Role())
		defer span.End()
		span.SetAttributes(
			attribute.String("ticket.id", t.ID),
			attribute.String("agent.role", d.handler.Role()),
			attribute.String("agent.host", d.host),
			attribute.String("ticket.priority", t.Priority),
		)
	}

	tctx := d.rec.Start(ctx, t.ID+"@"+d.host, t.ID, d.handler.Role())

	status, detail, err := d.handler.Handle(tctx, t)
	if err != nil {
		status, detail = workflow.OutcomeFailed, err.Error()
		if span != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "handler failed")
		}
		slog.ErrorContext(ctx, "handler failed",
			"ticket_id", t.ID, "role", d.handler.Role(), "error", err)
	}

	if span != nil {
		span.SetAttributes(attribute.String("outcome.status", status))
		// The DETAIL distinguishes "merged" from "conflict" from "no usable
		// resolution" — outcomes a stage reports as success because they are not
		// failures of the stage, only of the attempt. Without it on the span, three
		// very different endings look identical in a trace.
		if detail != "" {
			span.SetAttributes(attribute.String("outcome.detail", clip(detail, 200)))
		}
	}
	slog.InfoContext(ctx, "stage finished",
		"ticket_id", t.ID, "role", d.handler.Role(), "status", status, "detail", clip(detail, 200))
	d.rec.Finish(tctx, status, detail)

	// Move on a DETACHED context. The work is finished either way, and if the move
	// inherits a cancelled ctx — exactly what happens on shutdown, and on any
	// handler that returns because ctx ended — the ticket is left stuck in progress
	// until some later reconcile notices. Recoverable but wrong: a ticket the
	// department has finished with should not look claimed.
	moveCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer stop()

	// THE TICKET ADVANCES rather than being released. Finishing a stage moves the
	// work to the next column, which is both the hand-over and the thing a person
	// sees on the board.
	next := d.Destination(t, status)
	if next == "" {
		// The handler placed the ticket. Moving it again would undo that.
		if span != nil {
			span.SetAttributes(attribute.Bool("ticket.placed_by_handler", true))
		}
		return
	}
	if span != nil {
		span.SetAttributes(attribute.String("ticket.next_column", next))
	}

	// SAY WHY, ON THE TICKET, BEFORE MOVING IT OUT OF REACH.
	//
	// A ticket escalated to a person carried nothing but its claim comments: the
	// board said "BLOCKED — needs you" and the ticket itself gave no reason at
	// all. The cause was in the service log, which is the one place a person
	// reading the board is not looking — and on a host that has since restarted,
	// is not keeping either.
	//
	// Measured: a spec-agent that could not create a sandbox failed three times in
	// the same second, exhausted the ticket and blocked it. Diagnosing that took
	// reading the process log by hand; the ticket had recorded nothing. Written
	// BEFORE the move so a failure to comment cannot leave the ticket escalated
	// and silent — the reverse order loses the reason exactly when it is needed.
	if next == d.stage.Exhausted {
		if _, err := d.store.AddComment(moveCtx, t.ID, d.escalation(status, detail)); err != nil {
			slog.WarnContext(ctx, "could not record why the ticket was escalated",
				"ticket_id", t.ID, "role", d.handler.Role(), "error", err)
		}
	}

	if err := d.store.MoveTo(moveCtx, t.ID, next); err != nil {
		slog.WarnContext(ctx, "could not advance ticket; the next reconcile will return it to its queue",
			"ticket_id", t.ID, "next", next, "error", err)
		return
	}

	// The ticket is now in another stage's queue. Tell everyone rather than
	// leaving it to be discovered a poll interval later.
	d.wake.Signal()
}

// escalation is what a ticket says about itself once the department has given
// up on it.
//
// NAME THE CAUSE, NOT THE SYMPTOM. "blocked after 3 attempts" sends a correct
// reader to the wrong place — to the model, or to the specification — when the
// actual failure was a lease that could not be created. The last detail is the
// only thing that distinguishes the two, so it is quoted verbatim rather than
// summarised.
func (d *Dispatcher) escalation(status workflow.Outcome, detail string) string {
	var b strings.Builder
	b.WriteString(record.EscalatedMarker + "\n\n")
	fmt.Fprintf(&b, "`%s` stopped after %d attempts on this host (%s), and the "+
		"pipeline has no further move.\n\n", d.handler.Role(), d.maxAttempts, d.host)

	if detail = strings.TrimSpace(detail); detail != "" {
		fmt.Fprintf(&b, "The last attempt ended `%s`:\n\n```\n%s\n```\n",
			status, clip(detail, 1500))
	} else {
		// AN EMPTY DETAIL IS ITSELF THE FINDING. A stage that failed without saying
		// anything is a different problem from one that failed with a reason, and
		// the note must not read as though a reason was simply omitted here.
		fmt.Fprintf(&b, "The last attempt ended `%s` and reported no detail, which "+
			"is itself worth looking at — the stage failed without saying why.\n", status)
	}
	return b.String()
}

// Destination is the column an outcome sends this stage's ticket to.
//
// The routing itself belongs to the table; what this adds is the one question
// the table cannot answer — whether the ticket has attempts left.
//
// THE TWO CEILINGS MUST AGREE, and getting that wrong is invisible in both
// directions. Selection refuses a ticket once its spent attempts REACH the
// maximum, so this must escalate exactly when the next attempt would be refused:
// escalate early and the ticket loses an attempt it was entitled to; escalate
// late and it goes back to a queue that will never select it again — a stall
// with nothing on the board to explain it.
//
// The ticket counted here is the one read AFTER the claim, so the attempt about
// to end is already in it. An earlier version added one on the belief that it
// was not, and the arithmetic that follows from that belief spends a ticket's
// last attempt discovering it had none: with three configured, every ticket got
// two. Caught by walking the whole budget rather than checking a boundary.
func (d *Dispatcher) Destination(t ticket.Ticket, status workflow.Outcome) string {
	return d.stage.Destination(status, d.spent(t) < d.maxAttempts)
}

func (d *Dispatcher) finish(id string) {
	d.mu.Lock()
	delete(d.inFlight, id)
	d.mu.Unlock()
}

// drain waits briefly for in-flight work to report, so shutdown logging is
// honest about what was still running.
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

// Plan decides which tickets to start right now.
//
// Priority is a REFILL RULE rather than a separate scheduler: the next ticket is
// always the highest-priority available one. It is only an ORDERING here — how
// many run at once is this stage's concurrency and nothing else.
//
// CRITICAL EXCLUSIVITY BELONGS TO THE MODEL, NOT TO THIS STAGE. The admission
// queue already gives a critical request the whole serving class, and that is
// the thing worth protecting: the point of running a critical ticket alone is
// that one stream is FASTER than a share of four — about 160 tokens/second
// against 317 aggregate split between them.
//
// Pinning the stage as well used to look like the same rule and was strictly
// worse. A dispatcher is per stage, so it could only stop its OWN queue from
// claiming while the other stages carried on hitting the same server. The
// critical ticket never got an exclusive stream — it merely lost its siblings.
// Measured on a seven-ticket board that inherited "critical" from its parent:
// two agents against four slots, one of them idle, every stage serialised.
func Plan(candidates []ticket.Ticket, inFlight, max int) []ticket.Ticket {
	if len(candidates) == 0 || inFlight >= max {
		return nil
	}

	ordered := make([]ticket.Ticket, len(candidates))
	copy(ordered, candidates)
	sort.SliceStable(ordered, func(i, j int) bool {
		pi, pj := queue.ParsePriority(ordered[i].Priority), queue.ParsePriority(ordered[j].Priority)
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

// clip bounds a string for a log line or a span attribute, cutting on a rune
// boundary — a byte cut leaves half a character, and the department's prose is
// full of em dashes.
func clip(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// Package scope is the product manager: it reads a request and turns it into
// the tickets that answer it.
//
// TWO HALVES, AND THE SECOND ONE IS WHY THIS PACKAGE IS NOT CONTAINED. The
// first half is judgement — a priority from an allowlist, a summary, and a
// breakdown, all bounded by internal/plan. The second half CREATES WORK: it is
// the one place in the department that opens tickets.
//
// The original containment note said the breakdown was written into a comment
// as a PROPOSAL rather than created, and that a prompt-injected model could
// therefore write a misleading comment and set a wrong priority, and nothing
// else. That is no longer true and the reasoning is worth keeping rather than
// deleting, because it names exactly what widening this stage cost:
//
//   - every ticket lands on THIS BOARD rather than in the inbox, so nothing
//     created here reaches a queue an operator is not already watching;
//   - every ticket carries ParentID, so an operator who dislikes what was
//     created can find all of it from the request;
//   - the count and the size are capped before anything is created, by
//     internal/plan, which is a different package precisely so the bounds are
//     testable without a board.
//
// What a prompt-injected model can do here is open up to MaxSubtasks tickets
// with misleading titles under one parent. It cannot reach another board, touch
// code, or run anything.
package scope

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/code-armory-app/blacksmith/internal/agent/plan"
	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/queue"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

// Marker is what makes a triage comment recognisable later. It is part of the
// rendered comment, so changing one without the other silently re-scopes every
// ticket in the system.
const Marker = "**Triage**"

// DescriptionRunes bounds how much of a request is quoted into the tickets this
// stage opens, and RequestRunes how much reaches the model.
//
// The model's copy is larger because it is deciding what the request contains;
// a task ticket only needs enough context to place its criteria.
const (
	RequestRunes     = 4000
	DescriptionRunes = 2000
	SectionRunes     = 1500
)

// SystemPrompt briefs the triage stage.
//
// IT MUST NOT INVENT A REQUIREMENT AND IT MUST NOT LOSE ONE, and for a long
// time it was told only the first. Four separate rules here forbid adding
// something the request does not say; none forbade dropping something it does.
//
// The asymmetry had teeth because the caps push the same way: 2-5 criteria per
// subtask, and MaxCriteriaBeforeSplit sends anything past three back to be
// broken down. A request naming five edge cases alongside its happy path does
// not fit, so the cheapest way to satisfy the instructions was to drop some.
//
// Measured on r80. The request said "handle the edges as well as the happy
// path: empty and whitespace-only titles, oversized input, a body that is not
// an object, a duplicate or missing field, and a path that does not exist".
// Three of those five became criteria. The other two — oversized input and a
// non-object body — produced no criterion, no test, and no code: a 5MB title
// was accepted with a 200 and echoed back, and the branch meant to reject a
// non-object body was unreachable. The delivered suite passed 23 of 23.
//
// THAT IS THE FAILURE THIS STAGE IS UNIQUELY ABLE TO CAUSE. Every stage after
// it works from the criteria, so a requirement dropped here is not wrong
// anywhere — it is absent everywhere, and the run reports success.
const SystemPrompt = `You are the product manager for a software team. You triage incoming tickets.

Reply with ONLY a JSON object, no prose and no code fences, in exactly this shape:
{
  "summary": "one sentence restating the request",
  "priority": "low" | "medium" | "high" | "critical",
  "labels": ["short-kebab-case", ...],
  "subtasks": [
    {"title": "one concrete unit of work", "file": "store.go", "depends_on": [0, 1], "acceptance": ["a specific, checkable statement of what this must do"]}
  ],
  "needs_detail": ["question to ask the reporter", ...],
  "rationale": "one or two sentences justifying the priority"
}

Guidance:
- "critical" means the platform is broken or data is at risk. Use it sparingly; it stops all other work.
- Prefer 2-6 subtasks. If the ticket is already one unit of work, return an empty list.
- "depends_on" lists the INDEXES of subtasks that must be FINISHED FIRST, counting from 0 in the order you listed them. Omit it, or use [], when a subtask can start immediately.
- Only list a dependency that genuinely blocks. Two subtasks touching different functions do not block each other and must both be startable at once; a test depends on the code it tests. Inventing an order makes independent work run one piece at a time for no reason.
- DO NOT CHAIN. The most common mistake is listing the work in the order you would do it and making each subtask depend on the previous one. That is sequencing, not blocking, and it means nothing ever runs beside anything else. Aim for a few FOUNDATIONS that depend on nothing, and everything else depending only on those.
  Wrong (a chain):  A:[]  B:[0]  C:[1]  D:[2]  E:[3]
  Right (fan-out):  A:[]  B:[0]  C:[0]  D:[0]  E:[0]
  Ask of each subtask: "which pieces must EXIST before I can write a single line of this?" — usually that is one type or one file, not everything listed before it.
- NO DOCUMENTATION SUBTASKS. The project's README and architecture notes are written before you see this request and are already in the repository. Never emit a subtask whose job is to write, update or document anything — every subtask must change code. A subtask asking for a README cannot be built by this team: the stage that would receive it is only allowed to write test files.
- NO "write the tests" SUBTASK either. Tests are written for every subtask automatically, before its code, by a stage of its own.
- EVERY SUBTASK NEEDS "acceptance": 2-5 short, checkable statements of what it must do. These become the tests, so be concrete — "List(Filter{Done:true}) returns only completed tasks" rather than "filtering works".
- COVER EVERY CASE THE REQUEST NAMES. When the request lists specific situations — "handle empty and whitespace-only titles, oversized input, a body that is not an object, a duplicate or missing field, and a path that does not exist" — each one is a requirement someone wrote down, and each needs a criterion somewhere in your subtasks. If they do not fit in one subtask, that is what more subtasks are for. Dropping one is the one mistake nothing downstream can catch: the developer satisfies every test it is given and reports success, and the requirement is simply gone.
- DERIVE EVERY CRITERION FROM THE REQUEST TEXT. If the request does not say it, DO NOT invent it. Do not decide, on your own, that a list must be empty rather than nil, that a search is case-insensitive, that ids must be sequential, or that an error must have a particular wording. Those are decisions the person asking has not made, and each one becomes a test the developer is forced to satisfy.
- If a detail genuinely matters and the request is silent, put the QUESTION in needs_detail instead of answering it yourself in a criterion.
- GIVE EVERY SUBTASK ITS OWN "file", and never the same one twice. Subtasks are built AT THE SAME TIME, on separate branches, by agents that cannot see each other's work; two of them editing one file produces a merge conflict that a person has to resolve, and it is the single most common way parallel work is lost here. Name the source file this subtask's code belongs in — "store.go", "handlers.go", "filter.go" — one file per subtask, all different. Use the language's normal layout: a new file in the same package is free, so prefer one over adding to a file another subtask already owns. Do not name a test file; those are written by another stage.
- If the request is too vague to act on, say so in needs_detail rather than inventing requirements.`

// RefinePrompt briefs the second pass, on one unit of work at a time.
//
// WHY A SECOND PASS RATHER THAN A BETTER FIRST ONE. Asking for smaller pieces
// up front was tried through the acceptance-criteria change and did not hold:
// the model breaks a request into the parts it naturally has, and "the REST
// API" is one part however large it is. Splitting is a different question from
// scoping, asked of one piece at a time with its own criteria in front of it.
const RefinePrompt = `You are the product manager for a software team. A unit of work has come back too large and you are splitting it.

Reply with ONLY a JSON object, no prose and no code fences:
{"subtasks":[{"title":"one concrete unit of work","file":"handlers_read.go","acceptance":["a specific, checkable statement"]}]}

Rules:
- Return 2 or 3 subtasks. If the work genuinely cannot be split, return an empty list and it will be left alone.
- THEY MUST BE INDEPENDENT. Every one has to be buildable at the same time as the others, with no ordering between them. If you can only split it into steps that must happen in order, return an empty list instead — a chain is worse than one large ticket, because it makes each piece wait.
- Divide the ORIGINAL criteria between them. Do not invent new requirements, and do not drop any: every criterion you were given must appear in exactly one subtask.
- At most 3 criteria each.
- Give each one its own "file", all different, and different from the file you were given.`

// Store is the board operations this stage needs.
type Store interface {
	AddComment(ctx context.Context, id, body string) (ticket.Comment, error)
	Update(ctx context.Context, id string, up ticket.Update) (ticket.Ticket, error)
	Create(ctx context.Context, t ticket.Ticket) (ticket.Ticket, error)
	AddDependency(ctx context.Context, id, dependsOn string) error
	MoveTo(ctx context.Context, id, column string) error
}

// Gateway is the model call this needs.
type Gateway interface {
	Chat(ctx context.Context, class model.Class, req model.ChatRequest) (model.ChatResult, error)
}

// Options are the two shapes this stage can be run in.
//
// BOTH ARE MEASUREMENTS IN PROGRESS, not settled design, which is why they are
// options rather than rewrites: each keeps the other shape runnable on the same
// seed, so the answer is a number instead of an argument.
type Options struct {
	// MergeTasksFirst sends the whole plan to the ticket-merge stage instead of
	// straight to specification.
	//
	// THE PIPELINE IS STRICTLY SERIAL, SO EVERY SPLIT IS A FIXED COST PAID IN
	// FULL. Tasks are chained — every task waits for every task before it — and
	// the model serves one request at a time whatever the slot count says, so
	// splitting buys parallelism that neither the graph nor the hardware can
	// deliver. What it does buy is a spec-merge attempt, a security pass, an
	// integrator run and a fresh developer's startup, per task.
	//
	// Measured on r85 one level down, at the section level: collapsing several
	// section authors into one per task took a run from 3.6 minutes per task to
	// 2.3, on a bigger board. This is the same argument one level up.
	//
	// THE PLAN IS KEPT, NOT DISCARDED. Every unit of work the breakdown found is
	// still a real ticket on the board; a later stage folds them into one.
	// Collapsing the plan before creating anything was tried and produced a board
	// carrying a single generic ticket, which loses what the board is for.
	MergeTasksFirst bool

	// OneSpecAuthorPerTask has one agent write a task's whole specification
	// instead of one per slice.
	//
	// The split exists because a weaker model could not hold an assembled task:
	// section specifications took 3, 5 and 11 turns where the assembled task took
	// 10 once and failed at 49 and 80 twice. THAT CEILING HAS MOVED ONCE ALREADY
	// — it is why the developer side collapsed to one per task — and whether it
	// has moved for the AUTHOR too is a question about the current model, not
	// about the old one. Default off, which is the shape every run so far used.
	OneSpecAuthorPerTask bool
}

// Agent scopes one request.
type Agent struct {
	gw    Gateway
	store Store
	class model.Class
	opts  Options
}

// New builds the product manager.
func New(gw Gateway, store Store, class model.Class, opts Options) *Agent {
	return &Agent{gw: gw, store: store, class: class, opts: opts}
}

func (a *Agent) Role() string       { return workflow.RoleScoping }
func (a *Agent) Class() model.Class { return a.class }

// Wants accepts everything in its queue. THE COLUMN IS THE PREDICATE: a request
// sitting in the inbox is by definition unscoped, where this used to have to
// prove it by the absence of a triage comment.
func (a *Agent) Wants(ticket.Ticket) bool { return true }

// EntryColumn is where a freshly planned task waits.
func (a *Agent) EntryColumn() string {
	if a.opts.MergeTasksFirst {
		return workflow.ColReadyForTicketMerge
	}
	return workflow.ColReadyForSpecMerge
}

// Handle scopes one request.
func (a *Agent) Handle(ctx context.Context, t ticket.Ticket) (workflow.Outcome, string, error) {
	res, err := a.gw.Chat(ctx, a.class, model.ChatRequest{
		Messages: []model.Message{
			{Role: "system", Content: SystemPrompt},
			// THE REQUEST GOES IN A USER TURN. A ticket description is
			// attacker-influenced text in the general case, and the system prompt is
			// the one place it must not be able to reach.
			{Role: "user", Content: RenderTicket(t)},
		},
		Temperature: 0,
		// THE DEVELOPER'S CEILING, because this stage runs on the same class and a
		// smaller number buys nothing.
		//
		// max_tokens is a stop condition, not a reservation. A low ceiling frees no
		// capacity — how many stages run at once is the class's slot count — and
		// what it does instead is cut the reply off mid-object, which loses the
		// WHOLE breakdown rather than shortening it. Truncation is not a smaller
		// answer, it is no answer.
		//
		// Measured twice at 3700, both exactly at the ceiling and both "decode
		// triage: unexpected EOF", with the request blocking after two attempts and
		// no stage having disagreed with it. The room it needs is not fixed: the
		// architect's document grew and the breakdown grew with it, and a thinking
		// model spends part of the same allowance before it writes anything.
		MaxTokens: model.MaxReplyTokens,
		Priority:  queue.ParsePriority(t.Priority),
	})
	if err != nil {
		return workflow.OutcomeFailed, "", fmt.Errorf("scope %s: %w", t.ID, err)
	}

	tri, err := ParseTriage(res.Content)
	if err != nil {
		// A model that cannot produce parseable output is a failure worth
		// surfacing ON THE TICKET, not a silent no-op: otherwise a broken prompt
		// looks exactly like a quiet department.
		if _, cerr := a.store.AddComment(ctx, t.ID, TriageFailed(res)); cerr != nil {
			return workflow.OutcomeFailed, "", fmt.Errorf(
				"scope %s: unparseable output and the comment failed too: %w", t.ID, cerr)
		}
		return workflow.OutcomeFailed, "unparseable model output", fmt.Errorf("scope %s: %w", t.ID, err)
	}
	tri = plan.Sanitise(tri)

	if _, err := a.store.AddComment(ctx, t.ID, RenderTriage(tri, res)); err != nil {
		return workflow.OutcomeFailed, "", fmt.Errorf("scope %s: comment: %w", t.ID, err)
	}

	if tri.Priority != "" && tri.Priority != t.Priority {
		if _, err := a.store.Update(ctx, t.ID, ticket.Update{Priority: tri.Priority}); err != nil {
			// The triage comment already landed, so this is a PARTIAL SUCCESS rather
			// than a failure: re-running would duplicate the comment.
			return workflow.OutcomeSuccess, "scoped; priority update failed: " + err.Error(), nil
		}
	}

	// A REQUEST THAT IS ONE PIECE OF WORK BECOMES THE TICKET. Creating a single
	// child for it would add a level of indirection that says nothing — and the
	// parent would then sit in tracking forever waiting on a child that is just
	// itself with a different id.
	if len(tri.Subtasks) < 2 {
		// It goes to the TEST AUTHOR, the first work stage: a ticket reaching
		// development without tests would be verified by a gate with nothing in it.
		if err := a.store.MoveTo(ctx, t.ID, workflow.ColReadyForTests); err != nil {
			return workflow.OutcomeFailed, "", fmt.Errorf("scope %s: hand to the test author: %w", t.ID, err)
		}
		// HANDLED, not success: success would advance it to tracking and undo the
		// move just made.
		return workflow.OutcomeHandled, "scoped as one unit of work", nil
	}

	// A SECOND PASS, ON THE TASKS THE FIRST ONE LEFT TOO BIG. Refine keeps both
	// levels rather than flattening them.
	tasks, sections, err := a.Open(ctx, t, a.Refine(ctx, t, tri))
	if err != nil {
		return workflow.OutcomeFailed, "", fmt.Errorf("scope %s: %w", t.ID, err)
	}
	return workflow.OutcomeSuccess,
		fmt.Sprintf("opened %d tasks and %d specification sections", tasks, sections), nil
}

// Refine splits the tasks the first pass left too large, KEEPING BOTH LEVELS.
//
// The outer level is a TASK: a unit of work, developed on one branch. The inner
// level is its SECTIONS: slices of that task's specification, each written by
// its own agent so that each slice actually gets written. Flattening them was
// the mistake — it turned a specification device into a work-partitioning one,
// and gave every slice its own branch, developer, merge and place in a
// dependency queue.
//
// INDEPENDENCE IS STILL THE CONSTRAINT on the inner level: sections of one task
// never depend on each other, so all of them can be specified at once.
func (a *Agent) Refine(ctx context.Context, t ticket.Ticket, tri plan.Triage) []plan.Task {
	out := make([]plan.Task, 0, len(tri.Subtasks))
	for _, st := range tri.Subtasks {
		task := plan.Task{Task: st, Sections: []plan.Subtask{st}}
		if plan.NeedsSplit(st) {
			if children := a.SplitOne(ctx, t, st); len(children) > 0 {
				for i := range children {
					children[i].GroupTitle = st.Title
				}
				task.Sections = children
				slog.InfoContext(ctx, "split a task into specification sections",
					"ticket_id", t.ID, "title", st.Title,
					"criteria", len(st.Acceptance), "sections", len(children))
			}
		}
		out = append(out, task)
	}
	return out
}

// SplitOne asks for one unit of work's breakdown.
//
// A FAILURE RETURNS NOTHING, which leaves the unit exactly as it was: this pass
// may improve a breakdown and must never lose one. That is why every branch
// here warns and returns nil rather than propagating an error — there is no
// outcome in which a failed split should fail the scoping.
func (a *Agent) SplitOne(ctx context.Context, t ticket.Ticket, st plan.Subtask) []plan.Subtask {
	res, err := a.gw.Chat(ctx, a.class, model.ChatRequest{
		Messages: []model.Message{
			{Role: "system", Content: RefinePrompt},
			{Role: "user", Content: SplitRequest(st)},
		},
		Temperature: 0,
		MaxTokens:   model.MaxReplyTokens,
		Priority:    queue.ParsePriority(t.Priority),
	})
	if err != nil {
		slog.WarnContext(ctx, "could not split a unit of work; leaving it whole",
			"ticket_id", t.ID, "title", st.Title, "error", err)
		return nil
	}

	var split struct {
		Subtasks []plan.Subtask `json:"subtasks"`
	}
	if err := model.DecodeObject(res.Content, &split); err != nil {
		slog.WarnContext(ctx, "a split came back unparseable; leaving the unit whole",
			"ticket_id", t.ID, "title", st.Title, "error", err)
		return nil
	}
	if !plan.AcceptSplit(split.Subtasks) {
		return nil
	}

	// THE CHILDREN GO THROUGH THE SAME SANITISERS as the first pass. They are
	// model output reaching a ticket by exactly the same route, and a split that
	// proposed a test file or a second claim on one source file would otherwise
	// bypass every bound the first pass enforces.
	//
	// Edges are dropped rather than translated: children of one unit are
	// independent by construction, and the prompt asks for that explicitly.
	children := plan.SanitiseSubtasks(split.Subtasks)
	for i := range children {
		children[i].DependsOn = nil
	}
	if !plan.AcceptSplit(children) {
		return nil
	}

	// AND IT MUST STILL ASK FOR EVERYTHING THE UNIT ASKED FOR. The children
	// REPLACE the parent outright, so a criterion appearing in none of them is
	// not wrong somewhere — it is absent everywhere, and the run reports success.
	// Leaving the unit whole costs nothing; taking a lossy split cannot be undone.
	if !plan.SplitCovers(st, children) {
		slog.WarnContext(ctx, "a split dropped or reworded a criterion; leaving the unit whole",
			"ticket_id", t.ID, "title", st.Title,
			"criteria", len(st.Acceptance), "children", len(children))
		return nil
	}
	return children
}

// SplitRequest is what the second pass is shown: one unit of work and its own
// criteria, and nothing about the rest of the request.
func SplitRequest(st plan.Subtask) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Unit of work: %s\n\n", st.Title)
	if st.File != "" {
		fmt.Fprintf(&b, "It was assigned the file %s.\n\n", st.File)
	}
	b.WriteString("It is done when ALL of these are true:\n")
	for _, c := range st.Acceptance {
		fmt.Fprintf(&b, "  - %s\n", c)
	}
	return b.String()
}

// Open creates the two levels the plan describes: a TASK per unit of work, and
// a SECTION per slice of that task's specification.
//
// The shape, and why each part is load-bearing:
//
//   - A TASK starts at the entry column and DEPENDS ON ITS SECTIONS. That is
//     what holds it until its specification is complete: the developer stage
//     already refuses a ticket whose dependencies are unfinished, so no new
//     mechanism is needed.
//   - A SECTION starts in ready_for_spec and ends at done. It is never
//     developed; it exists so its slice of the specification actually gets
//     written, and its tests land on the TASK's branch.
//   - Tasks keep the dependencies BETWEEN them, so the API task still waits for
//     the store task — which the developer then merges.
func (a *Agent) Open(ctx context.Context, t ticket.Ticket, tasks []plan.Task) (int, int, error) {
	parent := t.ID
	board := t.Board()

	ids := make([]string, len(tasks))
	var opened, sections int

	for i, pt := range tasks {
		task, err := a.store.Create(ctx, ticket.Ticket{
			Title:       pt.Task.Title,
			Description: TaskDescription(t, pt),
			Status:      a.EntryColumn(),
			Priority:    t.Priority,
			ParentID:    &parent,
			BoardID:     boardPtr(board),
		})
		if err != nil {
			// PARTIAL CREATION IS REPORTED, NOT ROLLED BACK: the tickets already
			// opened are real work, and the parent keeps the full breakdown in its
			// comment so the gap is visible.
			return opened, sections, fmt.Errorf("create task %d of %d: %w", i+1, len(tasks), err)
		}
		ids[i] = task.ID
		opened++

		// EVERY TASK WAITS FOR EVERY TASK BEFORE IT, not only for what the model
		// declared. The declared graph is a fan-out BY INSTRUCTION — the prompt
		// demands it, because chaining once destroyed the pipeline's parallelism —
		// and a fan-out is optimistic: it says the API, the validation and the UI
		// each need only the store, when in fact they name each other's types.
		//
		// Measured on the run that produced this: all three ran at once, each
		// merging an integration branch that held only the store, and the
		// developers logged 78 "undefined:" errors compiling against code their
		// siblings had not merged yet. One task merged; three stalled.
		//
		// THE PARALLELISM THIS COSTS IS MOSTLY IMAGINARY. Tasks that cannot compile
		// without each other were never independent — they were queued by failure
		// instead of by order. What remains is the parallelism that is real: the
		// SECTIONS inside a task, which are independent by construction and are
		// where most of the concurrency was anyway.
		a.chainTo(ctx, task.ID, ids[:i], "a task may be worked before what it needs exists")

		// NO SECTIONS YET WHEN THE TASKS ARE BEING MERGED. A section written now
		// describes ONE task's slice, and the merged ticket carries all of them —
		// so the author would be briefed on a fifth of the work it is about to be
		// asked for, and the other tasks' sections would be left queued against
		// tickets that no longer exist.
		//
		// Measured on r88, which is what the merge stage was added for: four
		// sections sat in ready_for_spec against absorbed tasks, and the surviving
		// one briefed its author on "Define the Ticket type and status constants"
		// while its ticket asked for the store, the API, the board and the server
		// as well.
		if a.opts.MergeTasksFirst {
			continue
		}

		n, err := a.openSections(ctx, t, pt, task.ID, board, ids[:i])
		sections += n
		if err != nil {
			return opened, sections, fmt.Errorf("task %d of %d: %w", i+1, len(tasks), err)
		}
	}
	return opened, sections, nil
}

// openSections writes one task's specification brief onto the board.
//
// ONE AUTHOR OR ONE PER SLICE. The routing is identical either way — a section
// is claimed, written and ends at done, and its task waits for it — so the only
// thing that varies is how many tickets carry the work. That is what makes the
// two shapes comparable on one seed.
func (a *Agent) openSections(
	ctx context.Context, req ticket.Ticket, pt plan.Task, taskID, board string, priorTasks []string,
) (int, error) {
	planned := pt.Sections
	describe := func(i int) string { return SectionDescription(req, pt, i) }
	if a.opts.OneSpecAuthorPerTask && len(pt.Sections) > 1 {
		planned = []plan.Subtask{{Title: "Specification for " + pt.Task.Title}}
		describe = func(int) string { return WholeSpecDescription(req, pt) }
	}

	ids := make([]string, len(planned))
	var opened int

	for i, sec := range planned {
		sub, err := a.store.Create(ctx, ticket.Ticket{
			Title:       sec.Title,
			Description: describe(i),
			Status:      workflow.ColReadyForSpec,
			Priority:    req.Priority,
			ParentID:    &taskID,
			BoardID:     boardPtr(board),
		})
		if err != nil {
			return opened, fmt.Errorf("create section %d of %d: %w", i+1, len(planned), err)
		}
		opened++

		// THE TASK WAITS FOR ITS OWN SPECIFICATION. No promotion mechanism is
		// needed: the developer stage refuses a ticket whose dependencies are
		// unfinished, and a section ends at done.
		a.dependOn(ctx, taskID, sub.ID, "a task does not wait for its specification; it may develop early")

		// A SECTION WAITS FOR EVERYTHING ITS TASK WAITS FOR. Writing tests against
		// types that do not exist yet is the same failure one stage earlier: a UI
		// section ran while the API task was still in development, and specified an
		// interface that did not exist.
		a.chainTo(ctx, sub.ID, priorTasks, "a specification section does not wait for what its task waits for")

		// AND ON THE SECTION BEFORE IT, WITHIN THE TASK. Sections were independent
		// while they only wrote tests; now each one is also implemented, and
		// implementations are not independent — "basic task operations" needs the
		// types that "task data structure" creates. They share one branch, so
		// ordering them is what lets each author see what the section before it
		// built.
		if i > 0 && ids[i-1] != "" {
			a.dependOn(ctx, sub.ID, ids[i-1],
				"a section does not wait for the one before it; it may build against absent code")
		}
		ids[i] = sub.ID
	}
	return opened, nil
}

// chainTo records that one ticket waits for every id given.
func (a *Agent) chainTo(ctx context.Context, id string, on []string, why string) {
	for _, prior := range on {
		if prior == "" {
			continue
		}
		a.dependOn(ctx, id, prior, why)
	}
}

// dependOn records one prerequisite.
//
// A FAILURE HERE WARNS RATHER THAN FAILING THE STAGE. The tickets are already
// created and are real work; abandoning the scoping over a missing edge would
// leave the board holding a half-built plan and no comment explaining it. The
// cost of the missing edge is that a ticket may be worked early, which the
// developer's own verification catches.
func (a *Agent) dependOn(ctx context.Context, id, on, why string) {
	if err := a.store.AddDependency(ctx, id, on); err != nil {
		slog.WarnContext(ctx, why, "ticket_id", id, "depends_on", on, "error", err)
	}
}

// TaskDescription is what the DEVELOPER reads: everything its task must
// satisfy, gathered from the sections that describe it.
//
// THE CRITERIA GO FIRST, above the original request. They are the specific
// thing this task must satisfy; the request is context. Put the other way
// round, the developer reads two thousand words about a whole application and
// then has to decide for itself which part is this task's.
func TaskDescription(t ticket.Ticket, pt plan.Task) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", pt.Task.Title)
	b.WriteString("This task is done when ALL of these are true:\n\n")
	for _, sec := range pt.Sections {
		for _, c := range sec.Acceptance {
			fmt.Fprintf(&b, "  - %s\n", c)
		}
	}
	b.WriteString("\nIts tests are already written and are on this branch. " +
		"These come from the request and are the whole of what this task asks for. " +
		"Anything the request does not state is NOT a requirement of this task.\n\n")
	if pt.Task.File != "" {
		fmt.Fprintf(&b, plan.TaskFileIntro+"%s`, creating it if it does not exist.\n\n", pt.Task.File)
	}
	fmt.Fprintf(&b, "Original request:\n\n%s\n", clip(t.Description, DescriptionRunes))
	return b.String()
}

// SectionDescription is what ONE section's author reads: one slice of a task's
// specification, and nothing about the other slices.
//
// Naming the file is what keeps several authors writing onto one branch from
// colliding — they are working the same tree at the same time.
func SectionDescription(t ticket.Ticket, pt plan.Task, i int) string {
	sec := pt.Sections[i]
	var b strings.Builder
	fmt.Fprintf(&b, "Write ONE slice of the specification for %q.\n\n", pt.Task.Title)
	fmt.Fprintf(&b, "This slice: %s\n\n", sec.Title)
	if len(sec.Acceptance) > 0 {
		b.WriteString("Write a test for each of these and STOP:\n\n")
		for _, c := range sec.Acceptance {
			fmt.Fprintf(&b, "  - %s\n", c)
		}
		b.WriteString("\nThese come from the request and are the whole of what this slice asks for. " +
			"Anything the request does not state is NOT a requirement.\n\n")
	}
	fmt.Fprintf(&b, "Put these tests in `%s`, and touch no other test file: "+
		"the other slices of this task are being written AT THE SAME TIME onto this same branch, "+
		"and editing theirs loses their work.\n\n", SpecFile(sec))
	fmt.Fprintf(&b, "Original request:\n\n%s\n", clip(t.Description, SectionRunes))
	return b.String()
}

// WholeSpecDescription briefs ONE author on every slice of a task.
//
// It names a file per slice exactly as the split brief does, because the file
// layout is what keeps the developer's work from colliding later — what changes
// is who writes them, not what gets written.
func WholeSpecDescription(t ticket.Ticket, pt plan.Task) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Write the WHOLE specification for %q.\n\n", pt.Task.Title)
	b.WriteString("It has these slices, and you are writing ALL of them:\n\n")
	for i, sec := range pt.Sections {
		fmt.Fprintf(&b, "%d. %s — put these tests in `%s`\n", i+1, sec.Title, SpecFile(sec))
		for _, c := range sec.Acceptance {
			fmt.Fprintf(&b, "     - %s\n", c)
		}
	}
	b.WriteString("\nThese come from the request and are the whole of what this task asks for. " +
		"Anything the request does not state is NOT a requirement.\n\n")
	b.WriteString("Write one test file per slice, named above, and no others. " +
		"They share one package, so a name declared in one file may not be declared again in another.\n\n")
	fmt.Fprintf(&b, "Original request:\n\n%s\n", clip(t.Description, SectionRunes))
	return b.String()
}

// DefaultSpecFile is where a section's tests go when the breakdown assigned it
// no source file.
//
// A NAME IS ALWAYS GIVEN. An author told nothing about where to write picks a
// name per run, and two authors on one branch then pick different ones for the
// same slice — or the same one for different slices.
const DefaultSpecFile = "spec_test.go"

// SpecFile names the test file a section's author is told to write.
func SpecFile(sec plan.Subtask) string {
	if sec.File == "" {
		return DefaultSpecFile
	}
	return plan.SpecFileFor(sec.File)
}

// RenderTicket is the user turn. Bounded because a ticket description is
// attacker-influenced text in the general case and context is scarce.
func RenderTicket(t ticket.Ticket) string {
	desc := t.Description
	if len([]rune(desc)) > RequestRunes {
		desc = string([]rune(desc)[:RequestRunes]) + "\n…(truncated)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Title: %s\n", t.Title)
	fmt.Fprintf(&b, "Current priority: %s\n", t.Priority)
	fmt.Fprintf(&b, "Reported by: %s\n\n", t.CreatedBy)
	fmt.Fprintf(&b, "Description:\n%s\n", desc)
	return b.String()
}

// ParseTriage decodes the model's reply.
func ParseTriage(raw string) (plan.Triage, error) {
	var tri plan.Triage
	if err := model.DecodeObject(raw, &tri); err != nil {
		return plan.Triage{}, fmt.Errorf("decode triage: %w", err)
	}
	return tri, nil
}

// TriageFailed is what goes on the ticket when the reply could not be read.
//
// TRUNCATION AND MALFORMEDNESS READ THE SAME AND ARE NOT THE SAME. One is a
// length problem and one is the model's, and they need opposite fixes: "your
// reply was rejected" makes a model try the same thing again, where "you were
// cut off" makes it change what it does. The reply's own finish reason is the
// only thing that distinguishes them, so it is reported rather than guessed at.
func TriageFailed(res model.ChatResult) string {
	var b strings.Builder
	b.WriteString(Marker + " (automated)\n\n")
	b.WriteString("Triage failed: the model did not return usable JSON.\n\n")
	if res.Truncated(model.MaxReplyTokens) {
		fmt.Fprintf(&b, "The reply was CUT OFF at the token ceiling after %d tokens, "+
			"so it is a length problem rather than a malformed answer.\n\n", res.CompletionTokens)
	}
	fmt.Fprintf(&b, "What it said:\n\n%s\n", clip(res.Content, MaxSaidRunes))
	return b.String()
}

// MaxSaidRunes bounds how much of an unusable reply is quoted onto the ticket.
// THE REPLY ITSELF GOES ON THE TICKET: without the text there is nothing to
// reason from but the timing.
const MaxSaidRunes = 1000

// RenderTriage formats the comment.
//
// The breakdown is rendered here as well as created, because the comment is the
// only place the WHOLE plan appears: the tickets carry a slice each, and a
// partial creation leaves a gap that only this comment makes visible.
func RenderTriage(t plan.Triage, res model.ChatResult) string {
	var b strings.Builder
	b.WriteString(Marker + " (automated)\n\n")
	if t.Summary != "" {
		fmt.Fprintf(&b, "%s\n\n", t.Summary)
	}
	if t.Priority != "" {
		fmt.Fprintf(&b, "- **Priority:** %s", t.Priority)
		if t.Rationale != "" {
			fmt.Fprintf(&b, " — %s", t.Rationale)
		}
		b.WriteString("\n")
	}
	if len(t.Labels) > 0 {
		fmt.Fprintf(&b, "- **Labels:** %s\n", strings.Join(t.Labels, ", "))
	}
	if len(t.Subtasks) > 0 {
		b.WriteString("\n**Breakdown**\n")
		for _, st := range t.Subtasks {
			fmt.Fprintf(&b, "  - [ ] %s", st.Title)
			if st.File != "" {
				fmt.Fprintf(&b, " `%s`", st.File)
			}
			// INDEXES ARE RESOLVED TO TITLES HERE, and only here, because the
			// sanitisers guarantee every surviving edge points inside the list. A
			// renderer that indexed a raw reply would panic on the model's first
			// out-of-range dependency.
			if len(st.DependsOn) > 0 {
				names := make([]string, 0, len(st.DependsOn))
				for _, d := range st.DependsOn {
					if d >= 0 && d < len(t.Subtasks) {
						names = append(names, t.Subtasks[d].Title)
					}
				}
				if len(names) > 0 {
					fmt.Fprintf(&b, "  _(after: %s)_", strings.Join(names, "; "))
				}
			}
			b.WriteString("\n")
		}
	}
	if len(t.NeedsDetail) > 0 {
		b.WriteString("\n**Needs detail before this can be worked:**\n")
		for _, q := range t.NeedsDetail {
			fmt.Fprintf(&b, "  - %s\n", q)
		}
	}
	fmt.Fprintf(&b, "\n<sub>%s · %d/%d tokens · %dms</sub>\n",
		res.Model, res.PromptTokens, res.CompletionTokens, res.Latency.Milliseconds())
	return b.String()
}

func boardPtr(id string) *string {
	if id == "" {
		return nil
	}
	return &id
}

// clip shortens a string to max runes, adding an ellipsis when it had to cut.
//
// A NON-POSITIVE MAX IS A CALLER'S ARITHMETIC, NOT A REQUEST FOR NOTHING, and
// it must not panic. Returning empty is the honest answer — there is no room
// for any of it — and it keeps an arithmetic slip out of the panic path.
func clip(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/code-armory-app/blacksmith/internal/model"
	"log/slog"
	"os"
	"path"
	"slices"
	"sort"
	"strings"
)

// The product-manager agent: triage. It reads a ticket and proposes a priority,
// labels, a short summary and a decomposition into subtasks.
//
// It is first for a reason — triage quality is cheap to demonstrate, a small
// model does it well, and it produces the cleanest supervised signal in the
// system for later fine-tuning.
//
// CONTAINMENT. This agent's entire effect on the world is: one ticket comment,
// and a priority drawn from a fixed allowlist. Model output is never passed
// through to an API as-is — the priority is validated against known values, the
// text is bounded, and subtasks are written into the comment as a PROPOSAL
// rather than created as tickets. A prompt-injected model can therefore write a
// misleading comment and set a wrong priority, and nothing else. That bound is
// deliberate and should survive any extension of this agent: widening what
// triage may do widens what an injection may do, by exactly the same amount.

// pmSystemPrompt briefs the triage stage.
//
// IT MUST NOT INVENT A REQUIREMENT AND IT MUST NOT LOSE ONE, and for a long time
// it was told only the first. Four separate rules here forbid adding something
// the request does not say; none forbade dropping something it does.
//
// The asymmetry had teeth because the caps push the same way: 2-5 criteria per
// subtask, and maxCriteriaBeforeSplit sends anything past three back to be broken
// down. A request naming five edge cases alongside its happy path does not fit,
// so the cheapest way to satisfy the instructions was to drop some.
//
// Measured on r80. The request said "handle the edges as well as the happy path:
// empty and whitespace-only titles, oversized input, a body that is not an
// object, a duplicate or missing field, and a path that does not exist". Three of
// those five became criteria. The other two — oversized input and a non-object
// body — produced no criterion, no test, and no code: a 5MB title was accepted
// with a 200 and echoed back, and the branch meant to reject a non-object body
// was unreachable. The delivered suite passed 23 of 23.
//
// That is the failure this stage is uniquely able to cause. Every stage after it
// works from the criteria, so a requirement dropped here is not wrong anywhere —
// it is absent everywhere, and the run reports success.
const pmSystemPrompt = `You are the product manager for a software team. You triage incoming tickets.

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

// triageLimits bound what the model can put into a comment. A model that emits
// a very long or very large response must not turn into a very long ticket
// comment, and these caps are cheaper than trusting max_tokens alone.
const (
	maxLabels   = 8
	maxSubtasks = 12
	// maxCriteriaBeforeSplit is how much a single subtask may ask for before the
	// product manager is asked to break it down again.
	//
	// THE ONLY TICKET THAT EVER MERGES IS THE SMALLEST ONE. Across a full day of
	// runs the foundation ticket — no dependencies, tightest scope — merged in
	// every configuration, while the API, UI and validation tickets merged in
	// none of them. Three criteria is roughly where the foundation ticket sat.
	maxCriteriaBeforeSplit = 3
	maxNeedsDetail         = 6
	maxItemRunes           = 200
	// maxAcceptance bounds the criteria per subtask. Five is enough to state what
	// a unit of work must do; a list longer than that is a subtask that should
	// have been two, and it becomes a test file the developer cannot finish.
	maxAcceptance   = 5
	maxSummaryRunes = 500
)

// validPriorities is the allowlist. Model output outside it is discarded rather
// than forwarded to the API.
var validPriorities = []string{"low", "medium", "high", "critical"}

// subtask is one unit of work and what genuinely blocks it.
//
// DependsOn holds indexes into the same list, because the model has no ticket
// ids to refer to — the tickets do not exist until after it answers.
type subtask struct {
	Title string `json:"title"`
	// File is the source file this subtask's code belongs in, and it exists to
	// keep parallel work from colliding.
	//
	// Subtasks fan out from one request and are built SIMULTANEOUSLY on separate
	// branches by agents that cannot see each other. When each one writes to the
	// obvious place — main.go, in a single-package Go repository — the integrator
	// merges the first and every branch after it conflicts. Measured: a run where
	// the store ticket and the data-structure ticket both appended to main.go, the
	// second reaching `conflicted` sixty seconds after the first merged.
	//
	// One file each turns that from a conflict into a non-event, because a new
	// file in the same package costs nothing in Go and most languages. It is a
	// HINT, not a constraint the harness enforces: a ticket that genuinely must
	// touch shared code still can, and still conflicts, which is what the resolver
	// is for.
	File string `json:"file,omitempty"`
	// GroupTitle names the subtask a split child came from. It is set by refine,
	// never by the model, and it exists so the breakdown stays legible: without
	// it twelve leaves all hang off the original request and nothing shows which
	// three came from "the REST API".
	GroupTitle string `json:"-"`
	DependsOn  []int  `json:"depends_on,omitempty"`
	// Acceptance is what this subtask must do, DRAWN FROM THE REQUEST.
	//
	// It exists because the stage after this one works from almost nothing: a
	// child ticket carries a one-line title and the original request, so the spec
	// author has to INVENT the detail — and every invention becomes an assertion
	// the developer is bound to and cannot edit. Measured across four runs: the
	// requirements that consumed whole budgets (case-insensitive title matching,
	// an empty slice rather than nil, sequential ids) appear nowhere in the
	// request. They are an agent filling a blank page.
	//
	// A criterion is a sentence a person could check. The point is not ceremony:
	// it is that "test exactly these" is a rule with a referent, where "cover the
	// error and boundary cases" has none and so always argues for more.
	Acceptance []string `json:"acceptance,omitempty"`
}

type triage struct {
	Summary     string    `json:"summary"`
	Priority    string    `json:"priority"`
	Labels      []string  `json:"labels"`
	Subtasks    []subtask `json:"subtasks"`
	NeedsDetail []string  `json:"needs_detail"`
	Rationale   string    `json:"rationale"`
}

// PMAgent implements Handler.
type PMAgent struct {
	gw    *Gateway
	api   *CodeArmory
	class Class
}

func NewPMAgent(gw *Gateway, api *CodeArmory, class Class) *PMAgent {
	return &PMAgent{gw: gw, api: api, class: class}
}

func (a *PMAgent) Role() string { return "pm-agent" }
func (a *PMAgent) Class() Class { return a.class }

// Wants takes tickets nobody has triaged yet. The triage comment is the marker,
// so the pipeline stage lives on the ticket rather than in a side table that
// could drift from it.
// Wants accepts everything in the inbox. The COLUMN is the predicate now: a
// request sitting in inbox is by definition unscoped, where this used to have to
// prove it by the absence of a triage comment.
func (a *PMAgent) Wants(Ticket) bool { return true }

// triageMarker is what makes a triage comment recognisable later. It is part of
// the rendered comment, so changing one without the other silently re-triages
// every ticket in the system.
const triageMarker = "**Triage**"

func hasTriage(t Ticket) bool {
	for _, c := range t.Comments {
		if strings.Contains(c.Body, triageMarker) {
			return true
		}
	}
	return false
}

// Handle triages one ticket.
func (a *PMAgent) Handle(ctx context.Context, t Ticket) (string, string, error) {
	res, err := a.gw.Chat(ctx, a.class, ChatRequest{
		Messages: []Message{
			{Role: "system", Content: pmSystemPrompt},
			{Role: "user", Content: renderTicket(t)},
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
		MaxTokens: maxDevReplyTokens,
		Priority:  ParsePriority(t.Priority),
	})
	if err != nil {
		return OutcomeFailed, "", fmt.Errorf("triage %s: %w", t.TicketID, err)
	}

	tri, err := parseTriage(res.Content)
	if err != nil {
		// A model that cannot produce parseable output is a failure worth
		// surfacing on the ticket, not a silent no-op: otherwise a broken prompt
		// looks exactly like a quiet department.
		if _, cerr := a.api.AddComment(ctx, t.TicketID, "Triage failed: the model did not return usable JSON."); cerr != nil {
			return OutcomeFailed, "", fmt.Errorf("triage %s: unparseable output and comment failed: %w", t.TicketID, cerr)
		}
		return OutcomeFailed, "unparseable model output", fmt.Errorf("triage %s: %w", t.TicketID, err)
	}
	tri = sanitiseTriage(tri)

	if _, err := a.api.AddComment(ctx, t.TicketID, renderTriage(tri, res)); err != nil {
		return OutcomeFailed, "", fmt.Errorf("triage %s: comment: %w", t.TicketID, err)
	}

	if tri.Priority != "" && tri.Priority != t.Priority {
		if _, err := a.api.UpdateTicket(ctx, t.TicketID, TicketUpdate{Priority: tri.Priority}); err != nil {
			// The triage comment already landed, so this is a partial success
			// rather than a failure: re-running would duplicate the comment.
			return OutcomeSuccess, "scoped; priority update failed: " + err.Error(), nil
		}
	}

	// A request that is ONE piece of work becomes the ticket. Creating a single
	// child for it would add a level of indirection that says nothing — and the
	// parent would then sit in tracking forever waiting on a child that is just
	// itself with a different id.
	if len(tri.Subtasks) < 2 {
		// One unit of work: the request IS the ticket. It goes to the test author,
		// the first work stage — a ticket reaching development without tests would
		// be verified by a gate with nothing in it.
		if err := a.api.MoveTo(ctx, t.TicketID, ColReadyForTests); err != nil {
			return OutcomeFailed, "", fmt.Errorf("scope %s: hand to the test author: %w", t.TicketID, err)
		}
		// HANDLED, not success: success would advance it to tracking and undo the
		// move just made.
		return OutcomeHandled, "scoped as one unit of work", nil
	}

	// A SECOND PASS, ON THE TASKS THE FIRST ONE LEFT TOO BIG. See refine: it
	// keeps both levels rather than flattening them.
	plan := a.refine(ctx, t, tri)
	tasks, sections, err := a.open(ctx, t, plan)
	if err != nil {
		return OutcomeFailed, "", fmt.Errorf("scope %s: %w", t.TicketID, err)
	}
	return OutcomeSuccess, fmt.Sprintf("opened %d tasks and %d specification sections", tasks, sections), nil
}

// specPlan renders the breakdown as the specification's table of contents,
// ahead of the original request.
//
// THE PLAN COMES FIRST because renderTicket truncates a long description, and
// the thing the author must not lose is what to write. The request is context
// and survives being clipped; the plan is the instruction.
func specPlan(t Ticket, tri triage) string {
	var b strings.Builder
	b.WriteString("SPECIFICATION PLAN. Write ONE test file per section below, named after it, ")
	b.WriteString("covering exactly the criteria it lists and nothing else.\n\n")
	for i, st := range tri.Subtasks {
		fmt.Fprintf(&b, "%d. %s\n", i+1, st.Title)
		if st.File != "" {
			fmt.Fprintf(&b, "   Tests go in %s\n", specFileFor(st.File))
		}
		for _, c := range st.Acceptance {
			fmt.Fprintf(&b, "   - %s\n", c)
		}
		b.WriteString("\n")
	}
	b.WriteString("These sections are the whole of what this request asks for. ")
	b.WriteString("Anything the request does not state is not part of it.\n\n")
	fmt.Fprintf(&b, "Original request:\n\n%s\n", clip(t.Description, 2000))
	return b.String()
}

// taskFileIntro introduces the source file a task owns.
//
// It is a CONSTANT because two stages depend on the exact words: the product
// manager writes the sentence, and the ticket merge stage reads it back to name
// the matching test file. Coupling one stage to another's prose by retyping it
// is a bug waiting for someone to reword a sentence; naming it is not.
const taskFileIntro = "Put this task's code in `"

// specFileFor names the test file for a section, from the source file the
// product manager assigned it.
func specFileFor(source string) string {
	base := strings.TrimSuffix(source, ".go")
	if base == source {
		return source + "_test.go"
	}
	return base + "_test.go"
}

// onePlanTask says whether the whole plan becomes ONE task rather than one per
// unit of work.
//
// THE PIPELINE IS STRICTLY SERIAL, SO EVERY SPLIT IS A FIXED COST PAID IN FULL.
// Tasks are chained — every task waits for every task before it — and the model
// serves one request at a time whatever the slot count says, so splitting buys
// parallelism that neither the graph nor the hardware can deliver. What it does
// buy is a spec-merge attempt, a security pass, an integrator run and a fresh
// developer's startup, per task.
//
// Measured on r85 at the section level: collapsing several section authors into
// one per task took a run from 3.6 minutes per task to 2.3, on a bigger board.
// This is the same argument one level up.
//
// The plan is KEPT, not discarded. Each unit of work the product manager found
// becomes a slice of the single task, with its file and its criteria intact, so
// the specification author is briefed on all of them at once and the developer
// implements them together. What is removed is the coordination between them,
// not the planning.
func onePlanTask() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("AGENTS_PM_ONE_TASK")), "true")
}

// taskEntryColumn is where a freshly planned task waits.
func taskEntryColumn() string {
	if onePlanTask() {
		return ColReadyForTicketMerge
	}
	return ColReadyForSpecMerge
}

// open creates the two levels the plan describes: a TASK per unit of work, and
// a SUB-TASK per slice of that task's specification.
//
// THIS IS THE ONE PLACE THE DEPARTMENT CREATES WORK, and the bounds matter:
// subtasks are already capped and clipped by sanitiseTriage, every ticket lands
// on THIS board rather than in the inbox, and each carries ParentID so an
// operator who dislikes what was created can find all of it from the request.
//
// The shape, and why each part is load-bearing:
//
//   - A TASK starts in ready_for_dev and DEPENDS ON ITS SUB-TASKS. That is what
//     holds it until its specification is complete: the dev stage already refuses
//     a ticket whose dependencies are unfinished, so no new mechanism is needed.
//   - A SUB-TASK starts in ready_for_spec and ends at done. It is never
//     developed; it exists so its slice of the specification actually gets
//     written, and its tests land on the TASK's branch.
//   - Tasks keep the dependencies BETWEEN them, so the API task still waits for
//     the store task — which the developer then merges.
func (a *PMAgent) open(ctx context.Context, t Ticket, plan []planTask) (int, int, error) {
	board := ""
	if t.BoardID != nil {
		board = *t.BoardID
	}
	parent := t.TicketID

	taskIDs := make([]string, len(plan))
	var tasks, sections int

	for i, pt := range plan {
		task, err := a.api.CreateTicket(ctx, Ticket{
			Title:       pt.task.Title,
			Description: taskDescription(t, pt),
			// Into the RECONCILER, not straight to development: its sections are
			// written by authors that cannot see each other, and what they produce
			// has to compile as one package before anyone is asked to satisfy it.
			//
			// UNLESS THE TASKS ARE BEING MERGED FIRST, in which case they wait for
			// that stage. The tickets are still created — every unit of work the
			// plan found is a real ticket on the board — and a stage folds them
			// into one afterwards. Collapsing the plan before creating anything was
			// tried and produced a board carrying a single generic ticket, which
			// loses what the board is for.
			Status:   taskEntryColumn(),
			Priority: t.Priority,
			ParentID: &parent,
			BoardID:  boardPtr(board),
		})
		if err != nil {
			// Partial creation is reported, not rolled back: the tickets already
			// opened are real work, and the parent keeps the full breakdown in its
			// comment so the gap is visible.
			return tasks, sections, fmt.Errorf("create task %d of %d: %w", i+1, len(plan), err)
		}
		taskIDs[i] = task.TicketID
		tasks++

		// EVERY TASK WAITS FOR EVERY TASK BEFORE IT, not only for what the model
		// declared. The declared graph is a fan-out by instruction — the scoping
		// prompt demands it, because chaining once destroyed the pipeline's
		// parallelism — and a fan-out is optimistic: it says the API, the
		// validation and the UI each need only the store, when in fact they name
		// each other's types.
		//
		// Measured on the run that produced this change: all three ran at once,
		// each merging an integration branch that held only the store, and the
		// developers logged 78 "undefined:" errors compiling against code their
		// siblings had not merged yet. One task merged; three stalled.
		//
		// The parallelism this costs is mostly imaginary. Tasks that cannot
		// compile without each other were never independent — they were queued by
		// failure instead of by order. What remains is the parallelism that is
		// real: the three SECTIONS inside a task, which are independent by
		// construction and are where most of the concurrency was anyway.
		for prior := range i {
			if taskIDs[prior] == "" {
				continue
			}
			if err := a.api.AddDependency(ctx, task.TicketID, taskIDs[prior]); err != nil {
				slog.WarnContext(ctx, "could not record what a task waits for; it may be worked early",
					"ticket_id", task.TicketID, "depends_on", taskIDs[prior], "error", err)
			}
		}

		// NO SECTIONS YET WHEN THE TASKS ARE BEING MERGED. A section written now
		// describes ONE task's slice, and the merged ticket carries all of them —
		// so the author would be briefed on a fifth of the work it is about to be
		// asked for, and the other tasks' sections would be left queued against
		// tickets that no longer exist.
		//
		// Measured on r88, which is what this stage was added for: four sections sat
		// in ready_for_spec against absorbed tasks, and the surviving one briefed
		// its author on "Define the Ticket type and status constants" while its
		// ticket asked for the store, the API, the board and the server as well.
		//
		// The merge stage writes the section instead, once it knows what the merged
		// ticket actually contains.
		if onePlanTask() {
			continue
		}

		// ONE AUTHOR OR ONE PER SLICE. The routing is identical either way — a
		// section is claimed, written and ends at done, and its task waits for it —
		// so the only thing that varies is how many tickets carry the work. That is
		// what makes the two shapes comparable on one seed.
		planned := pt.sections
		describe := func(j int) string { return sectionDescription(t, pt, j) }
		if oneSpecAuthorPerTask() && len(pt.sections) > 1 {
			planned = []subtask{{Title: "Specification for " + pt.task.Title}}
			describe = func(int) string { return wholeSpecDescription(t, pt) }
		}

		sectionIDs := make([]string, len(planned))
		for j, sec := range planned {
			sub, err := a.api.CreateTicket(ctx, Ticket{
				Title:       sec.Title,
				Description: describe(j),
				Status:      ColReadyForSpec,
				Priority:    t.Priority,
				ParentID:    &task.TicketID,
				BoardID:     boardPtr(board),
			})
			if err != nil {
				return tasks, sections, fmt.Errorf("create section %d of task %d: %w", j+1, i+1, err)
			}
			sections++

			// THE TASK WAITS FOR ITS OWN SPECIFICATION. No promotion mechanism is
			// needed: the dev stage refuses a ticket whose dependencies are
			// unfinished, and a sub-task ends at done.
			if err := a.api.AddDependency(ctx, task.TicketID, sub.TicketID); err != nil {
				slog.WarnContext(ctx, "a task does not wait for its specification section; it may develop early",
					"ticket_id", task.TicketID, "section", sub.TicketID, "error", err)
			}
			// A section waits for everything its task waits for. Writing tests
			// against types that do not exist yet is the same failure one stage
			// earlier: a UI section ran while the API task was still in
			// development, and specified an interface that did not exist.
			for prior := range i {
				if taskIDs[prior] == "" {
					continue
				}
				if err := a.api.AddDependency(ctx, sub.TicketID, taskIDs[prior]); err != nil {
					slog.WarnContext(ctx, "a specification section does not wait for what its task waits for",
						"ticket_id", sub.TicketID, "depends_on", taskIDs[prior], "error", err)
				}
			}

			// AND ON THE SECTION BEFORE IT, WITHIN THE TASK. Sections were
			// independent while they only wrote tests; now each one is also
			// implemented, and implementations are not independent — "basic task
			// operations" needs the types that "task data structure" creates. They
			// share one branch, so ordering them is what lets each developer see
			// what the section before it built.
			if j > 0 && sectionIDs[j-1] != "" {
				if err := a.api.AddDependency(ctx, sub.TicketID, sectionIDs[j-1]); err != nil {
					slog.WarnContext(ctx, "a section does not wait for the one before it; it may build against absent code",
						"ticket_id", sub.TicketID, "depends_on", sectionIDs[j-1], "error", err)
				}
			}
			sectionIDs[j] = sub.TicketID
		}
	}
	return tasks, sections, nil
}

func boardPtr(id string) *string {
	if id == "" {
		return nil
	}
	return &id
}

// taskDescription is what the DEVELOPER reads: everything its task must satisfy,
// gathered from the sections that describe it.
//
// THE CRITERIA GO FIRST, above the original request. They are the specific thing
// this task must satisfy; the request is context. Put the other way round, the
// developer reads two thousand words about a whole application and then has to
// decide for itself which part is this task's.
func taskDescription(t Ticket, pt planTask) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", pt.task.Title)
	b.WriteString("This task is done when ALL of these are true:\n\n")
	for _, sec := range pt.sections {
		for _, c := range sec.Acceptance {
			fmt.Fprintf(&b, "  - %s\n", c)
		}
	}
	b.WriteString("\nIts tests are already written and are on this branch. " +
		"These come from the request and are the whole of what this task asks for. " +
		"Anything the request does not state is NOT a requirement of this task.\n\n")
	if pt.task.File != "" {
		fmt.Fprintf(&b, taskFileIntro+"%s`, creating it if it does not exist.\n\n", pt.task.File)
	}
	fmt.Fprintf(&b, "Original request:\n\n%s\n", clip(t.Description, 2000))
	return b.String()
}

// sectionDescription is what a SUB-TASK author reads: one slice of a task's
// specification, and nothing about the other slices.
//
// Naming the file is what keeps three authors writing onto one branch from
// colliding — they are working the same tree at the same time.
// oneSpecAuthorPerTask says whether a task's whole specification is written by
// ONE author instead of one author per slice.
//
// The split exists because a weaker model could not hold an assembled task:
// section specs took 3, 5 and 11 turns where the assembled task took 10 once and
// failed at 49 and 80 twice. That ceiling has moved once already — it is why the
// developer side collapsed to one per task — and whether it has moved for the
// AUTHOR too is a question about the current model, not about the old one.
//
// So it is a flag rather than a rewrite: both shapes stay runnable on the same
// seed, and the answer is a measurement instead of an argument. Default off,
// which is the shape every run so far has used.
func oneSpecAuthorPerTask() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("AGENTS_SPEC_ONE_AUTHOR")), "true")
}

// wholeSpecDescription briefs ONE author on every slice of a task.
//
// It names a file per slice exactly as the split brief does, because the file
// layout is what keeps the developer's work from colliding later — what changes
// is who writes them, not what gets written.
func wholeSpecDescription(t Ticket, pt planTask) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Write the WHOLE specification for %q.\n\n", pt.task.Title)
	b.WriteString("It has these slices, and you are writing ALL of them:\n\n")
	for i, sec := range pt.sections {
		file := "spec_test.go"
		if sec.File != "" {
			file = specFileFor(sec.File)
		}
		fmt.Fprintf(&b, "%d. %s — put these tests in `%s`\n", i+1, sec.Title, file)
		for _, c := range sec.Acceptance {
			fmt.Fprintf(&b, "     - %s\n", c)
		}
	}
	b.WriteString("\nThese come from the request and are the whole of what this task asks for. " +
		"Anything the request does not state is NOT a requirement.\n\n")
	b.WriteString("Write one test file per slice, named above, and no others. " +
		"They share one package, so a name declared in one file may not be declared again in another.\n\n")
	fmt.Fprintf(&b, "Original request:\n\n%s\n", clip(t.Description, 1500))
	return b.String()
}

func sectionDescription(t Ticket, pt planTask, i int) string {
	sec := pt.sections[i]
	var b strings.Builder
	fmt.Fprintf(&b, "Write ONE slice of the specification for %q.\n\n", pt.task.Title)
	fmt.Fprintf(&b, "This slice: %s\n\n", sec.Title)
	if len(sec.Acceptance) > 0 {
		b.WriteString("Write a test for each of these and STOP:\n\n")
		for _, c := range sec.Acceptance {
			fmt.Fprintf(&b, "  - %s\n", c)
		}
		b.WriteString("\nThese come from the request and are the whole of what this slice asks for. " +
			"Anything the request does not state is NOT a requirement.\n\n")
	}
	file := "spec_test.go"
	if sec.File != "" {
		file = specFileFor(sec.File)
	}
	fmt.Fprintf(&b, "Put these tests in `%s`, and touch no other test file: "+
		"the other slices of this task are being written AT THE SAME TIME onto this same branch, "+
		"and editing theirs loses their work.\n\n", file)
	fmt.Fprintf(&b, "Original request:\n\n%s\n", clip(t.Description, 1500))
	return b.String()
}

// renderTicket is the user turn. Bounded because a ticket description is
// attacker-influenced text in the general case and context is scarce.
func renderTicket(t Ticket) string {
	desc := t.Description
	if len([]rune(desc)) > 4000 {
		desc = string([]rune(desc)[:4000]) + "\n…(truncated)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Title: %s\n", t.Title)
	fmt.Fprintf(&b, "Current priority: %s\n", t.Priority)
	fmt.Fprintf(&b, "Reported by: %s\n\n", t.CreatedBy)
	fmt.Fprintf(&b, "Description:\n%s\n", desc)
	return b.String()
}

// parseTriage decodes the model's JSON, tolerating the code fences models add
// despite being told not to.
func parseTriage(raw string) (triage, error) {
	s := strings.TrimSpace(raw)
	// Not fence-extracted when it is already an object; see decodeJSONObject for
	// why taking the first ``` out of a reply that contains one destroys it.
	if !strings.HasPrefix(s, "{") {
		if fenced := extractFenced(s); fenced != "" {
			s = fenced
		}
	}
	// Trim anything either side of the outermost object.
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start == -1 || end == -1 || end < start {
		return triage{}, errors.New("no JSON object in model output")
	}
	var tri triage
	if err := model.DecodeJSON(s[start:end+1], &tri); err != nil {
		return triage{}, fmt.Errorf("decode triage: %w", err)
	}
	return tri, nil
}

// extractFenced unwraps a reply the model put inside a code fence.
//
// THE CLOSING FENCE IS THE LAST ONE, NOT THE NEXT ONE. A payload that itself
// contains fences is the normal case here, not an exotic one: the architect asks
// for markdown files, and a README with a ```bash block in it is ordinary. Taking
// the NEXT ``` as the close cuts the payload at its first inner fence and returns
// a fragment with no closing brace, which then reads as "no JSON object in the
// model output" against a reply that was perfectly well formed.
//
// Measured on the dense 14B, which wraps its answers in ```json where the MoE
// emitted them bare — so the same harness bug was invisible on one model and
// fatal on the other, and would have been scored as the model being worse.
//
// Trailing prose after the last fence is harmless: callers extract the outermost
// { … } from whatever this returns.
func extractFenced(s string) string {
	i := strings.Index(s, "```")
	if i == -1 {
		return ""
	}
	rest := s[i+3:]
	if nl := strings.IndexByte(rest, '\n'); nl != -1 {
		rest = rest[nl+1:] // drop a language tag
	}
	if j := strings.LastIndex(rest, "```"); j != -1 {
		return rest[:j]
	}
	return rest
}

// sanitiseTriage enforces the limits and the priority allowlist. Everything the
// model produced passes through here before it can reach the API.
func sanitiseTriage(t triage) triage {
	if !slices.Contains(validPriorities, t.Priority) {
		t.Priority = "" // unknown priority: leave the ticket's own value alone
	}
	t.Summary = clip(t.Summary, maxSummaryRunes)
	t.Rationale = clip(t.Rationale, maxSummaryRunes)
	t.Labels = clipList(t.Labels, maxLabels)
	t.Subtasks = sanitiseSubtasks(t.Subtasks)
	t.NeedsDetail = clipList(t.NeedsDetail, maxNeedsDetail)
	return t
}

// sanitiseSubtasks bounds the breakdown and validates its edges.
//
// IT FAILS OPEN. An edge that cannot be trusted is DROPPED, never replaced with
// a guess — and if the graph contains a cycle, every edge goes. That asymmetry
// is deliberate: a missing dependency costs at most a merge conflict, which the
// resolver already handles, while a wrong one stalls work that was ready and
// says on the board that it is waiting for something. One is recoverable and
// visible; the other is neither.
//
// An earlier version avoided the judgement entirely by chaining subtasks in the
// order proposed. That is worse than it looks: it serialises work that has no
// relationship, so a request for two independent functions ran as four
// sequential agent cycles instead of two parallel ones.
func sanitiseSubtasks(in []subtask) []subtask {
	out := make([]subtask, 0, min(len(in), maxSubtasks))
	// remap turns an index into the MODEL's list into an index into the kept
	// list, or -1 for a subtask that was dropped. Dropping shifts every later
	// position, so edges must be translated rather than reused: without this a
	// dependency on "the store" silently becomes a dependency on whatever moved
	// into slot 0. The old code only ever truncated the tail, where the
	// distinction did not show up.
	remap := make([]int, len(in))
	for i := range remap {
		remap[i] = -1
	}
	// A file claimed twice is worse than a file claimed once, because the whole
	// point of the assignment is that no two subtasks share one. The FIRST claim
	// wins and later ones are dropped rather than renamed: a wrong guess at a name
	// would send an agent to a file nobody meant.
	claimed := map[string]bool{}
	for i, st := range in {
		st.Title = clip(st.Title, maxItemRunes)
		if st.Title == "" {
			continue
		}
		if notCodeWork(st.Title) {
			continue
		}
		st.Acceptance = clipList(st.Acceptance, maxAcceptance)
		st.File = sanitiseSubtaskFile(st.File, claimed)
		if st.File != "" {
			claimed[st.File] = true
		}
		if len(out) == maxSubtasks {
			break
		}
		remap[i] = len(out)
		out = append(out, st)
	}
	// Edges are resolved through the remap, so an index into a subtask that was
	// dropped is itself dropped rather than silently pointing at whatever now
	// occupies that position.
	for i := range out {
		kept := make([]int, 0, len(out[i].DependsOn))
		seen := map[int]bool{}
		for _, d := range out[i].DependsOn {
			if d < 0 || d >= len(remap) {
				continue
			}
			n := remap[d]
			if n == -1 || n == i || seen[n] {
				continue
			}
			seen[n] = true
			kept = append(kept, n)
		}
		out[i].DependsOn = kept
	}
	if hasSubtaskCycle(out) {
		for i := range out {
			out[i].DependsOn = nil
		}
	}
	return flattenToRoots(out)
}

// notCodeWork reports whether a proposed subtask is one this department cannot
// build: documentation, or "write the tests".
//
// BOTH ARE ALREADY DONE BY A STAGE OF THEIR OWN, and the pipeline has nowhere to
// put a ticket asking for them. The architect writes the documentation into the
// base branch before the breakdown; the spec author writes tests for every
// ticket before its code. A ticket asking for either is not merely redundant, it
// is UNSATISFIABLE: the spec author may write only *_test.go, so a "write the
// README" ticket has no permitted action that reaches its finish condition. It
// held a large-class slot for 642 seconds, produced a test file that tests a
// README, and handed the developer a gate it could not pass.
//
// The prompt already forbids both, in capitals, and qwen3-coder emitted them
// anyway — twice on the same request. That is the reason this is code: a rule
// the model may decline to follow is not a rule.
//
// DELIBERATELY NARROW. Dropping real work is far worse than letting one odd
// ticket through, so the match anchors on the LEADING VERB rather than searching
// the whole title: "Implement filtering and document the query parameters" is
// implementation work that merely mentions documenting, and must survive.
func notCodeWork(title string) bool {
	words := strings.Fields(strings.ToLower(title))
	if len(words) == 0 {
		return false
	}
	// Only titles that OPEN with a producing verb are candidates. Anything
	// beginning "implement", "build", "fix", … is code work whatever it mentions.
	switch words[0] {
	case "create", "write", "add", "update", "produce", "document":
	default:
		return false
	}
	if words[0] == "document" {
		return true
	}
	head := words
	if len(head) > 5 {
		head = head[:5] // the object of the verb, not the rest of the sentence
	}
	clean := func(s string) string { return strings.Trim(s, ".,:;()") }
	for i, w := range head {
		switch clean(w) {
		case "readme", "readme.md", "changelog", "docs", "documentation":
			return true
		// PLURAL ONLY. "tests" is the deliverable; a singular "test" is almost
		// always a modifier on something real — a test harness, a test command, a
		// test endpoint — and dropping those would be silent lost scope. Caught by
		// the false-positive test, which is the one that matters here.
		case "tests":
			return true
		case "test":
			if i+1 < len(head) {
				switch clean(head[i+1]) {
				case "suite", "coverage":
					return true
				}
			}
		}
	}
	return false
}

// flattenToRoots re-points every dependency at the foundations a subtask
// ultimately rests on, collapsing chains into fan-out.
//
// The prompt already asks for dependencies that genuinely block, and models
// ignore it: asked to break down a REST API, one returned store → handlers →
// validation → filtering → tests → README, a six-deep chain in which nothing can
// ever run beside anything else. Sequencing is the natural way to describe work,
// so it is what gets emitted, and the cost is paid twice over — no parallelism at
// all, and a single failure anywhere strands every ticket behind it. Measured on
// a real batch: five minutes of work, thirteen minutes of deadlock.
//
// So the graph is normalised rather than trusted. Each subtask keeps only the
// ROOTS of its ancestor closure — the subtasks that depend on nothing — which
// leaves the foundations first and everything else able to start together.
//
// The trade is deliberate and worth naming: a subtask whose real prerequisite is
// mid-chain may now start before that work has merged, and fail. That is
// recoverable — it is retried, and the attempt is cheap now that a sandbox is
// held per ticket. A chain is not recoverable: one failure silently strands the
// rest, which is the strictly worse outcome and the one actually observed.
func flattenToRoots(in []subtask) []subtask {
	roots := func(start int) []int {
		seen := map[int]bool{start: true}
		stack := append([]int(nil), in[start].DependsOn...)
		var out []int
		for len(stack) > 0 {
			n := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[n] {
				continue
			}
			seen[n] = true
			if len(in[n].DependsOn) == 0 {
				out = append(out, n)
				continue
			}
			stack = append(stack, in[n].DependsOn...)
		}
		sort.Ints(out)
		return out
	}
	for i := range in {
		if len(in[i].DependsOn) == 0 {
			continue
		}
		in[i].DependsOn = roots(i)
	}
	return in
}

// hasSubtaskCycle reports whether the breakdown contains a cycle, which would
// leave every subtask in it waiting on another and none of them workable.
func hasSubtaskCycle(in []subtask) bool {
	const (
		unvisited = 0
		open      = 1
		closed    = 2
	)
	state := make([]int, len(in))
	var walk func(int) bool
	walk = func(i int) bool {
		if state[i] == open {
			return true
		}
		if state[i] == closed {
			return false
		}
		state[i] = open
		for _, d := range in[i].DependsOn {
			if walk(d) {
				return true
			}
		}
		state[i] = closed
		return false
	}
	for i := range in {
		if walk(i) {
			return true
		}
	}
	return false
}

func clipList(in []string, max int) []string {
	out := make([]string, 0, min(len(in), max))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		out = append(out, clip(s, maxItemRunes))
		if len(out) == max {
			break
		}
	}
	return out
}

// clipEnds keeps the BEGINNING and the END of a long output, dropping the
// middle and saying how much it dropped.
//
// clip() keeps the head, which is right for a document and wrong for a test run.
// `go test` puts a COMPILE error at the top — "# tracker [tracker.test]" and the
// file:line — and an ASSERTION failure at the bottom, after the module
// downloads and the passing tests. Keeping only the head therefore feeds the
// model "it failed" plus a list of downloads on exactly the runs where it most
// needs the message; keeping only the tail loses the compile errors. Both ends
// are the signal and the middle rarely is.
func clipEnds(s string, head, tail int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= head+tail {
		return s
	}
	dropped := len(r) - head - tail
	return string(r[:head]) +
		fmt.Sprintf("\n\n… %d characters omitted from the middle …\n\n", dropped) +
		string(r[len(r)-tail:])
}

// clip shortens a string to max runes, adding an ellipsis when it had to cut.
//
// A NON-POSITIVE MAX IS A CALLER'S ARITHMETIC, NOT A REQUEST FOR NOTHING, and it
// must not panic. Every caller computes max by subtracting what else is on the
// line from the width available, and any of those subtractions can go negative
// on a narrow terminal or a long prefix: the window crashed on
// `slice bounds out of range [:-1]` when a third project made the row carry a
// 16-character "[ab-qwen38-r71] " tag that did not fit.
//
// Returning empty is the honest answer — there is no room for any of it — and it
// keeps a display arithmetic slip out of the panic path.
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

// renderTriage formats the comment. Subtasks are a PROPOSAL, not created
// tickets — see the containment note at the top of this file.
func renderTriage(t triage, res ChatResult) string {
	var b strings.Builder
	b.WriteString(triageMarker + " (automated)\n\n")
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
		b.WriteString("\n**Proposed breakdown** (not created automatically):\n")
		for _, st := range t.Subtasks {
			fmt.Fprintf(&b, "  - [ ] %s", st.Title)
			if len(st.DependsOn) > 0 {
				names := make([]string, 0, len(st.DependsOn))
				for _, d := range st.DependsOn {
					names = append(names, t.Subtasks[d].Title)
				}
				fmt.Fprintf(&b, "  _(after: %s)_", strings.Join(names, "; "))
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

// sanitiseSubtaskFile accepts a plain repo-relative source path and rejects
// everything else.
//
// The value is written into a ticket that an agent then acts on, so the checks
// are the same ones applyEdits makes for a path it is about to write: no
// absolute paths, no traversal, nothing outside the repository. A rejected
// assignment becomes NO assignment — the subtask is still perfectly buildable
// without one, and inventing a replacement name would point an agent at a file
// nobody chose.
//
// A TEST FILE IS REFUSED. Those belong to the spec author, which is a different
// stage with its own rules, and a developer told its work belongs in
// store_test.go has been told to do the one thing it is forbidden to do.
func sanitiseSubtaskFile(p string, claimed map[string]bool) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = strings.TrimPrefix(p, "./")
	if len(p) > maxItemRunes || path.IsAbs(p) || strings.HasPrefix(p, "~") {
		return ""
	}
	if p != path.Clean(p) || strings.HasPrefix(p, "../") || p == ".." {
		return ""
	}
	if isTestFile(p) || claimed[p] {
		return ""
	}
	return p
}

// refineSystemPrompt asks for one subtask to be broken into independent pieces.
const refineSystemPrompt = `You are the product manager for a software team. A unit of work has come back too large and you are splitting it.

Reply with ONLY a JSON object, no prose and no code fences:
{"subtasks":[{"title":"one concrete unit of work","file":"handlers_read.go","acceptance":["a specific, checkable statement"]}]}

Rules:
- Return 2 or 3 subtasks. If the work genuinely cannot be split, return an empty list and it will be left alone.
- THEY MUST BE INDEPENDENT. Every one has to be buildable at the same time as the others, with no ordering between them. If you can only split it into steps that must happen in order, return an empty list instead — a chain is worse than one large ticket, because it makes each piece wait.
- Divide the ORIGINAL criteria between them. Do not invent new requirements, and do not drop any: every criterion you were given must appear in exactly one subtask.
- At most 3 criteria each.
- Give each one its own "file", all different, and different from the file you were given.`

// refine splits the subtasks the first pass left too large.
//
// WHY A SECOND PASS RATHER THAN A BETTER FIRST ONE. Asking for smaller pieces up
// front was tried through the acceptance-criteria change and did not hold: the
// model breaks a request into the parts it naturally has, and "the REST API" is
// one part however large it is. Splitting is a different question from scoping,
// asked of one piece at a time with its own criteria in front of it.
//
// INDEPENDENCE IS THE WHOLE CONSTRAINT. Twelve tickets in a chain is worse than
// four, because the failure that dominated every run was a ticket waiting on a
// dependency. A split that produces steps rather than pieces is refused by the
// prompt and, if it arrives anyway, flattened: children inherit only the
// PARENT's edges and never depend on each other.
// refine splits the tasks the first pass left too large, KEEPING BOTH LEVELS.
//
// The outer level is a TASK: a unit of work, developed on one branch. The inner
// level is its SUB-TASKS: slices of that task's specification, each written by
// its own agent so that each slice actually gets written. Flattening them was
// the mistake — it turned a specification device into a work-partitioning one,
// and gave every slice its own branch, developer, merge and place in a
// dependency queue.
//
// INDEPENDENCE IS STILL THE CONSTRAINT on the inner level: sub-tasks of one task
// never depend on each other, so all three can be specified at once.
func (a *PMAgent) refine(ctx context.Context, t Ticket, tri triage) []planTask {
	out := make([]planTask, 0, len(tri.Subtasks))
	for _, st := range tri.Subtasks {
		task := planTask{task: st, sections: []subtask{st}}
		if len(st.Acceptance) > maxCriteriaBeforeSplit {
			if children := a.splitOne(ctx, t, st); len(children) > 0 {
				task.sections = children
				slog.InfoContext(ctx, "split a task into specification sections",
					"ticket_id", t.TicketID, "title", st.Title, "criteria", len(st.Acceptance),
					"sections", len(children))
			}
		}
		out = append(out, task)
	}
	return out
}

// planTask is one unit of work and the slices of specification that describe it.
type planTask struct {
	task     subtask
	sections []subtask
}

// splitOne asks for one subtask's breakdown. A failure returns nothing, which
// leaves the subtask exactly as it was: this stage may improve a breakdown and
// must never lose one.
func (a *PMAgent) splitOne(ctx context.Context, t Ticket, st subtask) []subtask {
	var b strings.Builder
	fmt.Fprintf(&b, "Unit of work: %s\n\n", st.Title)
	if st.File != "" {
		fmt.Fprintf(&b, "It was assigned the file %s.\n\n", st.File)
	}
	b.WriteString("It is done when ALL of these are true:\n")
	for _, c := range st.Acceptance {
		fmt.Fprintf(&b, "  - %s\n", c)
	}
	res, err := a.gw.Chat(ctx, a.class, ChatRequest{
		Messages: []Message{
			{Role: "system", Content: refineSystemPrompt},
			{Role: "user", Content: b.String()},
		},
		Temperature: 0,
		MaxTokens:   maxDevReplyTokens,
		Priority:    ParsePriority(t.Priority),
	})
	if err != nil {
		slog.WarnContext(ctx, "could not split a subtask; leaving it whole",
			"ticket_id", t.TicketID, "title", st.Title, "error", err)
		return nil
	}
	var split struct {
		Subtasks []subtask `json:"subtasks"`
	}
	if err := decodeJSONObject(res.Content, &split); err != nil {
		slog.WarnContext(ctx, "a split came back unparseable; leaving the subtask whole",
			"ticket_id", t.TicketID, "title", st.Title, "error", err)
		return nil
	}
	// A split into one is not a split, and anything past three is the model
	// ignoring the brief rather than finding structure.
	if len(split.Subtasks) < 2 || len(split.Subtasks) > 3 {
		return nil
	}
	return split.Subtasks
}

// Package ticketmerge folds a plan's tasks into one ticket.
//
// IT CALLS NO MODEL, and that is the point. Merging is CONCATENATION: every unit
// of work the plan found is carried into one brief with its criteria and its file
// intact. A model asked to do this can paraphrase, reorder or drop — and dropping
// a requirement is the one failure nothing downstream can catch, because the
// developer satisfies every test it is given and reports success while the
// requirement is simply gone.
//
// THE TICKETS ARE CREATED FIRST AND MERGED SECOND. Collapsing the plan before
// anything is written was tried and produced a board with one generic ticket on
// it, which loses what the board is for: what was planned, what is done, and what
// each piece cost. The originals stay, each marked with where its work went.
package ticketmerge

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/code-armory-app/blacksmith/internal/agent/plan"
	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/record"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

// Store is the board operations this stage needs.
type Store interface {
	List(ctx context.Context, opts ticket.ListOpts) ([]ticket.Ticket, error)
	Update(ctx context.Context, id string, up ticket.Update) (ticket.Ticket, error)
	Create(ctx context.Context, t ticket.Ticket) (ticket.Ticket, error)
	AddComment(ctx context.Context, id, body string) (ticket.Comment, error)
	AddDependency(ctx context.Context, id, dependsOn string) error
	MoveTo(ctx context.Context, id, column string) error
}

// MergedFromMarker records the ticket that absorbed the others.
const MergedFromMarker = "**Merged from the tasks below.**"

// Agent folds a plan's tasks into one.
type Agent struct {
	store Store
}

func New(store Store) *Agent { return &Agent{store: store} }

func (a *Agent) Role() string { return workflow.RoleTicketMerge }

// Class is required by the dispatcher and unused: this stage calls no model. It
// names the small class so a host serving only that one can still merge.
// Class is ClassNone: this stage calls no model, and saying "small" instead
// would leave it unassembled on a host that does not serve small — stranding
// every ticket in its column.
func (a *Agent) Class() model.Class { return model.ClassNone }

// Wants takes any task waiting to be merged.
func (a *Agent) Wants(ticket.Ticket) bool { return true }

// Handle folds every sibling task into this one.
//
// THE FIRST TICKET ABSORBS THE REST rather than a new one being created, because
// a merged ticket that IS one of the originals keeps its history: its claim
// record, its place among the request's children, and the id anything already
// referring to it used.
func (a *Agent) Handle(ctx context.Context, t ticket.Ticket) (workflow.Outcome, string, error) {
	if t.Parent() == "" {
		// Nothing to merge with: a request scoped as one unit of work is already
		// what this stage would produce.
		return workflow.OutcomeSuccess, "nothing to merge; it is already one task", nil
	}

	all, err := a.store.List(ctx, ticket.ListOpts{BoardID: t.Board()})
	if err != nil {
		return workflow.OutcomeFailed, "", fmt.Errorf("ticket merge: list: %w", err)
	}

	siblings := a.siblingsOf(t, all)
	if len(siblings) == 0 {
		return workflow.OutcomeSuccess, "nothing left to merge", nil
	}

	ordered := append([]ticket.Ticket{t}, siblings...)

	if _, err := a.store.Update(ctx, t.ID, ticket.Update{Description: Brief(ordered)}); err != nil {
		return workflow.OutcomeFailed, "", fmt.Errorf("ticket merge: write the merged brief: %w", err)
	}
	a.comment(ctx, t.ID, fmt.Sprintf(
		"%s\n\nThis ticket now carries the work of %d tasks, listed in it in the order they were "+
			"planned. They stay on the board, each marked with where its work went.",
		MergedFromMarker, len(ordered)))

	merged := a.absorb(ctx, t, siblings, all)

	opened, err := a.openSections(ctx, t, ordered)
	if err != nil {
		return workflow.OutcomeFailed, "", err
	}

	slog.InfoContext(ctx, "merged the plan's tasks into one",
		"ticket_id", t.ID, "absorbed", merged, "sections", len(opened))
	return workflow.OutcomeSuccess, fmt.Sprintf("merged %d tasks into one", len(ordered)), nil
}

// siblingsOf finds the tasks this one absorbs, OLDEST FIRST AND SORTED — not
// merely assumed.
//
// This ticket is the oldest by construction, since the dispatcher takes them in
// creation order, but the siblings arrive in whatever order the listing
// returned. That order decides how the brief reads AND, since each section waits
// for the one before it, the order the specification is written in. Leaving it
// to the platform made both vary run to run; the test for the chain caught it by
// failing twice and then passing on the same command.
func (a *Agent) siblingsOf(t ticket.Ticket, all []ticket.Ticket) []ticket.Ticket {
	out := make([]ticket.Ticket, 0, len(all))
	for _, s := range all {
		if s.ID == t.ID || s.Parent() != t.Parent() {
			continue
		}
		// ONLY WHAT IS STILL WAITING. A sibling already merged, or already past this
		// stage, is not this ticket's to absorb.
		if s.Status != workflow.ColReadyForTicketMerge {
			continue
		}
		out = append(out, s)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// absorb closes each sibling and everything under it, and reports how many were
// taken.
func (a *Agent) absorb(ctx context.Context, into ticket.Ticket, siblings, all []ticket.Ticket) int {
	var merged int
	for _, s := range siblings {
		a.comment(ctx, s.ID, fmt.Sprintf(
			"%s\n\nIts requirements were carried into `%s` unchanged, and are specified and built "+
				"there. Nothing here was dropped.",
			record.MergedIntoMarker, shortID(into.ID)))

		// ANYTHING UNDER IT GOES TOO. A section left queued against an absorbed
		// ticket is written anyway, by an author briefed on a fifth of the work,
		// onto a branch nobody develops.
		for _, child := range all {
			if child.Parent() != s.ID || child.Status == workflow.ColDone {
				continue
			}
			a.comment(ctx, child.ID, fmt.Sprintf(
				"%s\n\nThe ticket this belonged to was merged into `%s`, which is specified in one "+
					"pass. There is nothing separate to write here.",
				record.MergedIntoMarker, shortID(into.ID)))
			if err := a.store.MoveTo(ctx, child.ID, workflow.ColDone); err != nil {
				slog.WarnContext(ctx, "could not close a merged ticket's section",
					"ticket_id", child.ID, "error", err)
			}
		}

		if err := a.store.MoveTo(ctx, s.ID, workflow.ColDone); err != nil {
			slog.WarnContext(ctx, "could not close a merged ticket",
				"ticket_id", s.ID, "merged_into", into.ID, "error", err)
			continue
		}
		merged++
	}
	return merged
}

// openSections opens ONE SECTION PER UNIT OF WORK, not one covering all of them.
//
// AN AUTHOR IS FINISHED WHEN ITS GATE PASSES, and its gate passes as soon as its
// first file compiles and fails correctly. Give it five units and it stops after
// one — measured on a run that delivered a fifth of the system and reported
// success. Letting it end its own stage instead was measured worse still: the
// same author ran 49 turns and wrote one file 41 times.
//
// So THE BRIEF IS WHAT GETS SIZED, not the stopping rule. One unit per author
// means the gate and the job end at the same moment.
func (a *Agent) openSections(ctx context.Context, t ticket.Ticket, ordered []ticket.Ticket) ([]string, error) {
	opened := make([]string, 0, len(ordered))

	for i, unit := range ordered {
		sec, err := a.store.Create(ctx, ticket.Ticket{
			Title:       "Specification for " + unit.Title,
			Description: SectionBrief(unit, i+1, len(ordered)),
			Status:      workflow.ColReadyForSpec,
			Priority:    t.Priority,
			ParentID:    &t.ID,
			BoardID:     ticket.Ptr(t.Board()),
		})
		if err != nil {
			return opened, fmt.Errorf("ticket merge: open a specification section: %w", err)
		}

		// THE TASK WAITS FOR EVERY ONE OF THEM. Development refuses a ticket whose
		// prerequisites are unfinished, so the developer starts once the whole
		// specification exists rather than partway through it.
		if err := a.store.AddDependency(ctx, t.ID, sec.ID); err != nil {
			slog.WarnContext(ctx, "the merged task does not wait for one of its sections; it may develop early",
				"ticket_id", t.ID, "section", sec.ID, "error", err)
		}

		// AND EACH ON THE ONE BEFORE IT. They share a branch, and a sandbox begins
		// by resetting hard to that branch — so two authors writing at once wipe
		// each other's work, and NEITHER CAN TELL: the file is still in the losing
		// agent's tree, so its next edit reads as a no-op, while the gate it runs
		// against reports that no test files were written.
		//
		// Measured: five sections claimed at once, four of them looping on that
		// exact pair of messages until the run was stopped.
		if i > 0 {
			if err := a.store.AddDependency(ctx, sec.ID, opened[i-1]); err != nil {
				slog.WarnContext(ctx, "a section does not wait for the one before it; they will race on the branch",
					"ticket_id", sec.ID, "depends_on", opened[i-1], "error", err)
			}
		}
		opened = append(opened, sec.ID)
	}
	return opened, nil
}

// Brief is every task's description, in order, under one heading.
//
// CONCATENATION AND NOTHING ELSE. Each original is quoted WHOLE rather than
// summarised, because a summary is where a requirement goes missing and there is
// no reader downstream who could notice.
func Brief(tickets []ticket.Ticket) string {
	var b strings.Builder
	b.WriteString("This ticket is several units of work, merged. Every one of them is below, " +
		"in the order they were planned. All of them are part of this ticket.\n\n")
	for i, t := range tickets {
		fmt.Fprintf(&b, "─── %d of %d: %s ───\n\n%s\n\n", i+1, len(tickets),
			strings.TrimSpace(t.Title), strings.TrimSpace(t.Description))
	}
	return strings.TrimSpace(b.String())
}

// SectionBrief is one unit of work's own brief.
//
// It says which slice this is and how many there are, because the authors share
// a branch and each needs to know the others exist — but it carries only its OWN
// requirements, so its gate passes exactly when its job is done.
func SectionBrief(unit ticket.Ticket, n, total int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Write the tests for ONE slice of this task: %s\n\n", strings.TrimSpace(unit.Title))

	// NAME THE FILE. Measured: a store author put its tests into another section's
	// file, because nothing had told it its own existed. Appending to a file it
	// had not written meant EDITING rather than creating, and it spent three turns
	// failing to quote a function back exactly before one landed. The delivered
	// repository had no store test file at all. An author writing a fresh file it
	// owns cannot make that mistake.
	file := "spec_test.go"
	if src := SourceFileFromBrief(unit.Description); src != "" {
		file = plan.SpecFileFor(src)
	}
	fmt.Fprintf(&b, "Put these tests in `%s`, creating it, and touch no other test file. "+
		"It is slice %d of %d; the others are written by other authors onto this same branch, "+
		"before and after yours, and editing theirs loses their work.\n\n", file, n, total)

	b.WriteString(strings.TrimSpace(unit.Description))
	return b.String()
}

// SourceFileFromBrief reads back the source file a plan assigned to a task, so
// the matching test file can be named. Empty when the plan named no file, which
// is why the caller keeps a default.
func SourceFileFromBrief(desc string) string {
	_, rest, ok := strings.Cut(desc, plan.TaskFileIntro)
	if !ok {
		return ""
	}
	file, _, ok := strings.Cut(rest, "`")
	if !ok {
		return ""
	}
	return strings.TrimSpace(file)
}

func (a *Agent) comment(ctx context.Context, id, body string) {
	if _, err := a.store.AddComment(ctx, id, body); err != nil {
		slog.WarnContext(ctx, "could not record a ticket merge", "ticket_id", id, "error", err)
	}
}

// shortID is how a ticket is named to a person in a comment.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

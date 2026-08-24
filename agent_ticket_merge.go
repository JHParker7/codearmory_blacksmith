package main

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

// TicketMergeAgent folds the product manager's tasks into one.
//
// IT CALLS NO MODEL, and that is the point. Merging is concatenation: every unit
// of work the product manager found is carried into one brief with its criteria
// and its file intact. A model asked to do this can paraphrase, reorder or drop,
// and dropping a requirement is the one failure nothing downstream can catch —
// the developer satisfies every test it is given and reports success while the
// requirement is simply gone. That is measured, on r80, and it is why the
// coverage rule exists in the product manager's brief one stage earlier.
//
// THE TICKETS ARE CREATED FIRST AND MERGED SECOND. Collapsing the plan before
// anything is written was tried and produced a board with one generic ticket on
// it, which loses what the board is for: what was planned, what is done, and what
// each piece cost. The originals stay, each marked with what it was merged into.
type TicketMergeAgent struct {
	api *CodeArmory
}

func NewTicketMergeAgent(api *CodeArmory) *TicketMergeAgent {
	return &TicketMergeAgent{api: api}
}

func (a *TicketMergeAgent) Role() string { return roleTicketMerge }

// Class is required by Handler and unused: this stage calls no model. It names
// the small class so a host serving only that one can still merge.
func (a *TicketMergeAgent) Class() Class { return ClassSmall }

// Wants takes any task waiting to be merged.
func (a *TicketMergeAgent) Wants(Ticket) bool { return true }

// mergedIntoMarker records a ticket folded into another, so the board says where
// its work went rather than leaving it looking abandoned.
const mergedIntoMarker = "**Merged into another ticket.**"

// mergedFromMarker records the ticket that absorbed the others.
const mergedFromMarker = "**Merged from the tasks below.**"

// Handle folds every sibling task into this one.
//
// THE FIRST TICKET ABSORBS THE REST rather than a new one being created, because
// a merged ticket that is one of the originals keeps its history: its claim
// record, its place in the request's children, and the id anything already
// referring to it used.
//
// Ordered by creation so the brief reads in the order the work was planned, and
// so two hosts racing this reach the same answer.
func (a *TicketMergeAgent) Handle(ctx context.Context, t Ticket) (string, string, error) {
	if t.ParentID == nil || *t.ParentID == "" {
		// Nothing to merge with: a request scoped as one unit of work is already
		// what this stage would produce.
		return OutcomeSuccess, "nothing to merge; it is already one task", nil
	}
	board := ""
	if t.BoardID != nil {
		board = *t.BoardID
	}
	all, err := a.api.ListTickets(ctx, ListOpts{BoardID: board})
	if err != nil {
		return OutcomeFailed, "", fmt.Errorf("ticket merge: list: %w", err)
	}

	siblings := make([]Ticket, 0, len(all))
	for _, s := range all {
		if s.TicketID == t.TicketID {
			continue
		}
		if s.ParentID == nil || *s.ParentID != *t.ParentID {
			continue
		}
		// Only what is still waiting to be merged. A sibling already merged, or
		// already past this stage, is not this ticket's to absorb.
		if s.Status != ColReadyForTicketMerge {
			continue
		}
		siblings = append(siblings, s)
	}
	if len(siblings) == 0 {
		return OutcomeSuccess, "nothing left to merge", nil
	}

	// OLDEST FIRST, SORTED — not merely assumed. This ticket is the oldest by
	// construction, since the dispatcher takes them in creation order, but the
	// siblings arrive in whatever order the listing returned. That order decides
	// how the brief reads AND, since each section waits for the one before it, the
	// order the specification is written in. Leaving it to the platform made both
	// vary run to run; the test for the chain caught it by failing twice and then
	// passing on the same command.
	sort.SliceStable(siblings, func(i, j int) bool {
		if !siblings[i].CreatedAt.Equal(siblings[j].CreatedAt) {
			return siblings[i].CreatedAt.Before(siblings[j].CreatedAt)
		}
		return siblings[i].TicketID < siblings[j].TicketID
	})
	ordered := append([]Ticket{t}, siblings...)
	brief := mergedBrief(ordered)

	if _, err := a.api.UpdateTicket(ctx, t.TicketID, TicketUpdate{Description: brief}); err != nil {
		return OutcomeFailed, "", fmt.Errorf("ticket merge: write the merged brief: %w", err)
	}
	a.comment(ctx, t, fmt.Sprintf("%s\n\nThis ticket now carries the work of %d tasks, listed in it "+
		"in the order they were planned. They stay on the board, each marked with where its work went.",
		mergedFromMarker, len(ordered)))

	merged := 0
	for _, s := range siblings {
		a.comment(ctx, s, fmt.Sprintf("%s\n\nIts requirements were carried into `%s` unchanged, and are "+
			"specified and built there. Nothing here was dropped.", mergedIntoMarker, shortID(t.TicketID)))
		// ANYTHING UNDER IT GOES TOO. A section left queued against an absorbed
		// ticket is written anyway, by an author briefed on a fifth of the work,
		// onto a branch nobody develops.
		for _, child := range all {
			if child.ParentID == nil || *child.ParentID != s.TicketID || child.Status == ColDone {
				continue
			}
			a.comment(ctx, child, fmt.Sprintf("%s\n\nThe ticket this belonged to was merged into `%s`, "+
				"which is specified in one pass. There is nothing separate to write here.",
				mergedIntoMarker, shortID(t.TicketID)))
			if err := a.api.MoveTo(ctx, child.TicketID, ColDone); err != nil {
				slog.WarnContext(ctx, "could not close a merged ticket's section",
					"ticket_id", child.TicketID, "error", err)
			}
		}
		if err := a.api.MoveTo(ctx, s.TicketID, ColDone); err != nil {
			slog.WarnContext(ctx, "could not close a merged ticket",
				"ticket_id", s.TicketID, "merged_into", t.TicketID, "error", err)
			continue
		}
		merged++
	}
	// ONE SECTION PER UNIT OF WORK, not one covering all of them.
	//
	// AN AUTHOR IS FINISHED WHEN ITS GATE PASSES, and its gate passes as soon as
	// its first file compiles and fails correctly. Give it five units and it stops
	// after one — measured on r89, which delivered a fifth of the system and
	// reported success. Letting it end its own stage instead was measured worse
	// still: on r90 the same author ran 49 turns and wrote one file 41 times.
	//
	// So the brief is what gets sized, not the stopping rule. One unit per author
	// means the gate and the job end at the same moment, which is the arrangement
	// that has always worked here.
	//
	// They all write to THIS ticket's branch, which is the whole point of merging:
	// several focused authors produce one specification, and one developer
	// implements it. The developer's own gate is honest — green means every test
	// passes — so nothing downstream needs changing.
	opened := make([]string, 0, len(ordered))
	for i, unit := range ordered {
		sec, err := a.api.CreateTicket(ctx, Ticket{
			Title:       "Specification for " + unit.Title,
			Description: unitSpecBrief(unit, i+1, len(ordered)),
			Status:      ColReadyForSpec,
			Priority:    t.Priority,
			ParentID:    &t.TicketID,
			BoardID:     boardPtr(board),
		})
		if err != nil {
			return OutcomeFailed, "", fmt.Errorf("ticket merge: open a specification section: %w", err)
		}
		// THE TASK WAITS FOR EVERY ONE OF THEM. Development refuses a ticket whose
		// dependencies are unfinished, so the developer starts once the whole
		// specification exists rather than partway through it.
		if err := a.api.AddDependency(ctx, t.TicketID, sec.TicketID); err != nil {
			slog.WarnContext(ctx, "the merged task does not wait for one of its sections; it may develop early",
				"ticket_id", t.TicketID, "section", sec.TicketID, "error", err)
		}

		// AND EACH ON THE ONE BEFORE IT. They share a branch, and a sandbox begins
		// by resetting hard to that branch — so two authors writing at once wipe
		// each other's work, and neither can tell: the file is still in the losing
		// agent's tree, so its next edit reads as a no-op, while the gate it runs
		// against reports "no test files were written".
		//
		// Measured on r91, which is what this stage produced without the ordering:
		// five sections claimed at once, four of them looping on that exact pair of
		// messages until the run was stopped. The product manager chains its own
		// sections for the same reason and always has.
		if i > 0 {
			if err := a.api.AddDependency(ctx, sec.TicketID, opened[i-1]); err != nil {
				slog.WarnContext(ctx, "a section does not wait for the one before it; they will race on the branch",
					"ticket_id", sec.TicketID, "depends_on", opened[i-1], "error", err)
			}
		}
		opened = append(opened, sec.TicketID)
	}

	slog.InfoContext(ctx, "merged the product manager's tasks into one",
		"ticket_id", t.TicketID, "absorbed", merged, "sections", len(opened))
	return OutcomeSuccess, fmt.Sprintf("merged %d tasks into one", len(ordered)), nil
}

// mergedBrief is every task's description, in order, under one heading.
//
// Concatenation and nothing else. Each original is quoted whole rather than
// summarised, because a summary is where a requirement goes missing and there is
// no reader downstream who could notice.
func mergedBrief(tickets []Ticket) string {
	var b strings.Builder
	b.WriteString("This ticket is several units of work, merged. Every one of them is below, " +
		"in the order they were planned. All of them are part of this ticket.\n\n")
	for i, t := range tickets {
		fmt.Fprintf(&b, "─── %d of %d: %s ───\n\n%s\n\n", i+1, len(tickets),
			strings.TrimSpace(t.Title), strings.TrimSpace(t.Description))
	}
	return strings.TrimSpace(b.String())
}

// unitSpecBrief is one unit of work's own brief.
//
// It says which slice this is and how many there are, because the authors share a
// branch and each needs to know the others exist — but it carries only its OWN
// requirements, so its gate passes exactly when its job is done.
func unitSpecBrief(unit Ticket, n, total int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Write the tests for ONE slice of this task: %s\n\n", strings.TrimSpace(unit.Title))

	// NAME THE FILE. The product manager's own section brief has always done this
	// (see sectionDescription), and this one did not: it passed the task
	// description, which names the SOURCE file, and left the author to choose
	// where its tests went.
	//
	// Measured on r92. The store author put its tests into the ticket author's
	// ticket_test.go, because nothing had told it store_test.go existed. Appending
	// to a file it had not written meant editing rather than creating, and it
	// spent three turns and roughly 38 seconds failing to quote a function back
	// exactly before one landed. The delivered repository had no store_test.go at
	// all. An author writing a fresh file it owns cannot make that mistake.
	file := "spec_test.go"
	if src := sourceFileFromBrief(unit.Description); src != "" {
		file = specFileFor(src)
	}
	fmt.Fprintf(&b, "Put these tests in `%s`, creating it, and touch no other test file. "+
		"It is slice %d of %d; the others are written by other authors onto this same branch, "+
		"before and after yours, and editing theirs loses their work.\n\n", file, n, total)

	b.WriteString(strings.TrimSpace(unit.Description))
	return b.String()
}

// sourceFileFromBrief reads back the source file the product manager assigned to
// a task, so the matching test file can be named. Empty when the plan named no
// file, which is why the caller keeps a default.
func sourceFileFromBrief(desc string) string {
	_, rest, ok := strings.Cut(desc, taskFileIntro)
	if !ok {
		return ""
	}
	file, _, ok := strings.Cut(rest, "`")
	if !ok {
		return ""
	}
	return strings.TrimSpace(file)
}

// mergedSpecBrief asks for the whole specification of a merged ticket.
//
// It quotes the merged brief rather than pointing at the parent, because the
// author is shown its OWN ticket's description and nothing else — a brief that
// says "see the task above" is a brief with the requirements missing.
func mergedSpecBrief(brief string) string {
	var b strings.Builder
	b.WriteString("Write the WHOLE specification for this ticket, in one pass.\n\n")
	b.WriteString("It is several units of work merged into one, listed below in the order they were " +
		"planned. Write tests for EVERY one of them — each names the file its tests belong in. " +
		"A unit with no tests is a requirement nobody will build.\n\n")
	b.WriteString("These come from the request and are the whole of what this ticket asks for. " +
		"Anything the request does not state is NOT a requirement.\n\n")
	b.WriteString(brief)
	return b.String()
}

func (a *TicketMergeAgent) comment(ctx context.Context, t Ticket, body string) {
	if _, err := a.api.AddComment(ctx, t.TicketID, body); err != nil {
		slog.WarnContext(ctx, "could not record a ticket merge", "ticket_id", t.TicketID, "error", err)
	}
}

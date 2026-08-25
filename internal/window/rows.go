package window

import (
	"fmt"
	"sort"
	"time"

	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

// NeedsAPerson reports a ticket the department has given up on.
func NeedsAPerson(status string) bool {
	return status == workflow.ColBlocked || status == workflow.ColConflicted
}

// Done reports work that reached the end of the pipeline.
func Done(status string) bool { return status == workflow.ColDone }

// Label is what a column means, in words a person reading the board can act on.
//
// PROSE, NOT THE COLUMN NAME. "ready_for_dev" tells the reader what the schema
// calls it; "has tests, waiting for a developer" tells them whether anything is
// wrong. The two states that need a person are shouted, because they are the
// only rows in the list that are asking for something.
func Label(status string) string {
	switch status {
	case workflow.ColInbox:
		return "waiting to be designed"
	case workflow.ColDesigning:
		return "having its documentation written"
	case workflow.ColReadyForScoping:
		return "documented, waiting to be scoped"
	case workflow.ColScoping:
		return "being scoped"
	case workflow.ColReadyForTicketMerge:
		return "waiting to be folded into one task"
	case workflow.ColMergingTickets:
		return "being folded into one task"
	case workflow.ColTracking:
		return "broken down — tracking its children"
	case workflow.ColReadyForTests:
		return "waiting for its tests to be written"
	case workflow.ColWritingTests:
		return "having its tests written"
	case workflow.ColReadyForSpec:
		return "waiting for its specification"
	case workflow.ColWritingSpec:
		return "having its specification written"
	case workflow.ColReadyForSpecMerge:
		return "waiting for its sections to be reconciled"
	case workflow.ColMergingSpecs:
		return "having its sections reconciled"
	case workflow.ColReadyForDev:
		return "has tests, waiting for a developer"
	case workflow.ColInDev:
		return "being written"
	case workflow.ColReadyForMaintenance:
		return "waiting for its lint findings to be fixed"
	case workflow.ColMaintaining:
		return "having its lint findings fixed"
	case workflow.ColReadyForCoverage:
		return "waiting for its edge cases to be covered"
	case workflow.ColCovering:
		return "having its edge cases covered"
	case workflow.ColReadyForReview:
		return "waiting for review"
	case workflow.ColInReview:
		return "being reviewed"
	case workflow.ColReadyForIntegration:
		return "waiting to merge"
	case workflow.ColIntegrating:
		return "merging"
	case workflow.ColConflicted:
		return "MERGE CONFLICT"
	case workflow.ColBlocked:
		return "BLOCKED — needs you"
	case workflow.ColDone:
		return "merged to the integration branch"
	case "":
		return "no status"
	}
	// A COLUMN THE DEPARTMENT DOES NOT KNOW ABOUT IS SOMEONE ELSE'S, and saying
	// so is more useful than pretending it is a stage.
	return status + " (not a department column)"
}

// Urgency orders rows by what a person opening the window needs to see.
//
// The question this window is opened to answer is "is anything wrong", so the
// rows that are asking for something come first and the finished ones last.
const (
	UrgencyNeedsAPerson = iota
	UrgencyHeld
	UrgencyQueued
	UrgencyDone
)

// Rank is one ticket's own urgency.
func Rank(tb workflow.Table, status string) int {
	switch {
	case NeedsAPerson(status):
		return UrgencyNeedsAPerson
	case Done(status):
		return UrgencyDone
	case HeldByAStage(tb, status):
		return UrgencyHeld
	default:
		return UrgencyQueued
	}
}

// Progress is how many of a ticket's children have finished.
type Progress struct{ Done, Total int }

// Row is one line of the board: a ticket, how deep it sits under its run, and
// what its children have done.
type Row struct {
	Ticket   ticket.Ticket
	Depth    int
	Progress Progress
}

// Board is the ordered rows plus what was left out.
//
// THE HIDDEN COUNT GOES BACK TO THE CALLER rather than being dropped. A board
// that silently omits rows is worse than a busy one: the reader cannot tell the
// difference between "nothing else is happening" and "the window is not showing
// it to me".
type Board struct {
	Rows   []Row
	Hidden int
}

// RunState is how a whole run — a root and everything under it — is getting on.
//
// THREE STATES, NOT TWO. "Finished" once meant both "done" and "gave up", which
// sorted a run waiting for a person in among the completed ones where nobody
// would look at it again. A run the department has abandoned is over, but it is
// over in a way that is asking for something.
type RunState int

const (
	// RunLive has work still moving in it.
	RunLive RunState = iota
	// RunNeedsAPerson is over and asking for someone.
	RunNeedsAPerson
	// RunDone is over and finished.
	RunDone
)

// StateOf reports how a run is getting on.
//
// TRACKING COUNTS AS OVER for a root: a broken-down request sits there for the
// life of its children, so a run whose children are all done is finished even
// though its root never leaves that column.
func StateOf(root ticket.Ticket, children map[string][]ticket.Ticket) RunState {
	state := RunDone
	seen := map[string]bool{}

	var walk func(t ticket.Ticket)
	walk = func(t ticket.Ticket) {
		if state == RunLive || seen[t.ID] {
			return
		}
		seen[t.ID] = true

		switch {
		case NeedsAPerson(t.Status):
			state = RunNeedsAPerson
		case !Done(t.Status) && t.Status != workflow.ColTracking:
			state = RunLive
			return
		}
		for _, c := range children[t.ID] {
			walk(c)
		}
	}
	walk(root)
	return state
}

// Rows groups tickets into runs and orders them for reading.
//
// A LIVE RUN OUTRANKS A FINISHED ONE WHATEVER IS INSIDE IT. Rank takes the most
// urgent descendant, which is right WITHIN a run and wrong BETWEEN them: one
// blocked section in a run that ended days ago would rank the entire dead run
// above today's work, because a blocked ticket is the most urgent thing there
// is. That is what made the board muddy once there were several runs on it — the
// question a person opens this to ask is "what is happening now", and the answer
// must not be sorted underneath what happened last week.
func Rows(ts []ticket.Ticket, tb workflow.Table, showFinished bool) Board {
	present := make(map[string]bool, len(ts))
	for _, t := range ts {
		present[t.ID] = true
	}

	children := map[string][]ticket.Ticket{}
	roots := make([]ticket.Ticket, 0, len(ts))
	for _, t := range ts {
		// A PARENT THIS BOARD CANNOT SEE MAKES ITS CHILD A ROOT. Nesting a row
		// under something absent would hide it entirely.
		if p := t.Parent(); p != "" && present[p] && p != t.ID {
			children[p] = append(children[p], t)
			continue
		}
		roots = append(roots, t)
	}

	// best is a row's own rank or its most urgent descendant's, whichever is more
	// urgent. Memoised because a deep board would otherwise walk each subtree
	// once per comparison — and the memo is seeded before the walk, which is what
	// stops a cycle in the parent links from recursing forever.
	best := map[string]int{}
	var rank func(t ticket.Ticket) int
	rank = func(t ticket.Ticket) int {
		if r, ok := best[t.ID]; ok {
			return r
		}
		own := Rank(tb, t.Status)
		best[t.ID] = own
		r := own
		for _, c := range children[t.ID] {
			if cr := rank(c); cr < r {
				r = cr
			}
		}
		best[t.ID] = r
		return r
	}

	byRank := func(rows []ticket.Ticket) {
		sort.SliceStable(rows, func(i, j int) bool {
			if ri, rj := rank(rows[i]), rank(rows[j]); ri != rj {
				return ri < rj
			}
			return rows[i].CreatedAt.After(rows[j].CreatedAt)
		})
	}
	sort.SliceStable(roots, func(i, j int) bool {
		// LIVE RUNS FIRST, WHATEVER IS INSIDE THE DEAD ONES. That is the r-run
		// lesson: rank takes the most urgent descendant, so one blocked section in
		// a run that ended days ago would otherwise outrank today's work.
		//
		// Below that, RANK ALONE IS ENOUGH to put an abandoned run above a
		// completed one — a row needing a person ranks most urgent and a done row
		// least, so a three-way state comparison here would be a second way of
		// saying the same thing. The three-way distinction earns its place in the
		// HIDING, where it decides what the reader never sees.
		li, lj := StateOf(roots[i], children) == RunLive, StateOf(roots[j], children) == RunLive
		if li != lj {
			return li
		}
		if ri, rj := rank(roots[i]), rank(roots[j]); ri != rj {
			return ri < rj
		}
		return roots[i].CreatedAt.After(roots[j].CreatedAt)
	})

	var out Board
	var walk func(t ticket.Ticket, depth int)
	walk = func(t ticket.Ticket, depth int) {
		kids := children[t.ID]
		var p Progress
		for _, c := range kids {
			p.Total++
			if Done(c.Status) {
				p.Done++
			}
		}
		out.Rows = append(out.Rows, Row{Ticket: t, Depth: depth, Progress: p})

		byRank(kids)
		for _, c := range kids {
			walk(c, depth+1)
		}
	}

	for _, root := range roots {
		// ONLY WHAT IS *DONE* IS EVER HIDDEN. A run the department gave up on has
		// also stopped moving, but it is the one thing the reader opened this
		// window to find — hiding it by default puts the only rows asking for a
		// human behind a keypress nobody knew to make.
		if !showFinished && StateOf(root, children) == RunDone {
			out.Hidden++
			continue
		}
		walk(root, 0)
	}
	return out
}

// RuntimeText renders how long a ticket took, WITH SECONDS KEPT.
//
// Rounding to whole minutes is right for "how stale is this" and useless for
// "which of these was slow". Measured on r77: two sections took 5m44s and 5m00s
// and both rendered as "5m"; two more took 1m50s and 1m12s and both rendered as
// "1m". The whole reason to show a runtime is to COMPARE runtimes, and half of
// r77's tickets collided with another under the coarser clock.
func RuntimeText(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

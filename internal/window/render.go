package window

import (
	"fmt"
	"strings"
	"time"

	"github.com/code-armory-app/blacksmith/internal/agent/review"
	"github.com/code-armory-app/blacksmith/internal/record"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

// Tone says how a cell should be coloured. The view owns the palette; this
// package owns the decision, so the rule that a blocked ticket is red is tested
// rather than buried in a render function.
type Tone int

const (
	ToneWaiting Tone = iota
	ToneLive
	ToneDone
	ToneAlert
	// ToneQuiet is for a parent reporting on its children. A PARENT IS NOT AN
	// AGENT: it shows what is happening beneath it dimly, so the eye goes to the
	// row actually being worked rather than to every ancestor of it.
	ToneQuiet
)

// Cell is one rendered piece of a row, with the tone it should carry.
type Cell struct {
	Text string
	Tone Tone
}

// StateCells is the right-hand side of a row: what is happening, why it is not
// moving, and how long it has been that way.
//
// EVERY CLAUSE HERE ANSWERS A QUESTION THAT WAS ASKED OF A REAL BOARD and could
// not be answered from the row. They are separate cells rather than one string
// so the tones can differ within the state — a live role beside a dim turn count
// reads at a glance, and one uniform colour does not.
func StateCells(t ticket.Ticket, act Activity, now time.Time) []Cell {
	var out []Cell

	switch {
	case act.Live(now) && act.Role != "":
		out = append(out, Cell{act.Role + " · " + act.What, ToneLive})
		out = append(out, Cell{fmt.Sprintf(" · turn %d", act.Turns), ToneQuiet})

	case act.Working > 0:
		out = append(out, Cell{act.What, ToneQuiet})
		out = append(out, Cell{fmt.Sprintf(" · turn %d", act.Turns), ToneQuiet})

	case MergedAway(t):
		// MERGED IS NOT BUILT. The ticket-merge stage folds several tickets into
		// one and closes the rest, so four of them reach done within seconds of the
		// product manager finishing — which on a board of "done" labels reads as
		// four pieces of work completed instantly. The distinction was in a comment
		// and nowhere on the row, which is where it gets read.
		into := MergedInto(t)
		if into == "" {
			into = "another ticket"
		}
		out = append(out, Cell{"merged into " + into, ToneQuiet})

	case NeedsAPerson(t.Status):
		out = append(out, Cell{Label(t.Status), ToneAlert})

	case Done(t.Status):
		out = append(out, Cell{Label(t.Status), ToneDone})

	default:
		out = append(out, Cell{Label(t.Status), ToneWaiting})
	}

	// HOW LONG IT HAS BEEN THERE. A board is a set of rows that all look equally
	// alive; the age is what separates "working on it" from "wedged an hour ago",
	// and it is the first thing a person wants at a glance. Total age lives in the
	// detail view — on the row the question is whether THIS stage is progressing.
	if since := StageSince(t); !since.IsZero() {
		out = append(out, Cell{" · " + ShortDuration(now.Sub(since)), ToneQuiet})
	}

	// WHY IT IS NOT MOVING. A queued ticket and one blocked behind a prerequisite
	// look identical otherwise — both simply sit there — and the difference is the
	// most asked question about this board. Watching four tickets wait on one
	// foundation reads as a broken pipeline until you can see that is exactly what
	// they are doing.
	if u := UnmetDependencies(t); len(u) > 0 && !act.Live(now) && !Done(t.Status) {
		if len(u) == 1 {
			out = append(out, Cell{" · waits on " + ShortID(u[0].ID), ToneQuiet})
		} else {
			out = append(out, Cell{fmt.Sprintf(" · waits on %d", len(u)), ToneQuiet})
		}
	}

	// Once a branch exists it is the only thing on the row you can act on, so it
	// is shown rather than left to be dug out of a comment.
	if br := record.BranchOf(t); br != "" && !act.Live(now) {
		out = append(out, Cell{" · " + br, ToneQuiet})
	}
	return out
}

// Counts is the board summarised the way a person actually asks about it: how
// much is finished, how much is moving, and how much is stuck.
//
// Finished work used to be legible only by reading every row and noticing which
// said "merged" — the rows were there, but "what has this thing actually done?"
// took a scan rather than a glance, which on a board of forty is not an answer.
type Counts struct{ Done, Moving, Stuck, Waiting int }

func (c Counts) Total() int { return c.Done + c.Moving + c.Stuck + c.Waiting }

// Tally counts a board. It takes the rows rather than the raw tickets so it
// counts exactly what is on screen.
func Tally(rows []Row, acts map[string]Activity, now time.Time) Counts {
	var c Counts
	for _, r := range rows {
		switch {
		case Done(r.Ticket.Status):
			c.Done++
		case NeedsAPerson(r.Ticket.Status):
			c.Stuck++
		case acts[r.Ticket.ID].Live(now):
			c.Moving++
		default:
			c.Waiting++
		}
	}
	return c
}

// ReturnCeiling names the tickets the reviewer bounced until it ran out of
// rounds.
//
// THIS IS THE ONE THING IN THE WINDOW THAT ASKS FOR A PERSON BY NAME rather than
// just colouring a row. A ticket at the ceiling has had ten developer runs spent
// on it and is still not passing review, which means either the change is harder
// than the pipeline can handle or the reviewer is wrong — and both are
// judgements no further agent round will make.
//
// Deliberately NOT derived from the blocked column: plenty of things land there,
// and this alert is specifically the loop that had to be stopped.
func ReturnCeiling(tickets []ticket.Ticket) []string {
	var hit []string
	for _, t := range tickets {
		for _, c := range t.Comments {
			if strings.Contains(c.Body, review.ReturnCeilingMarker) {
				hit = append(hit, ShortID(t.ID))
				break
			}
		}
	}
	return hit
}

// NextStep is the command to run when a ticket has become a person's problem.
//
// Empty unless there is BOTH something wrong and something to act on: an alert
// with no branch is a statement, and this is meant to be a next move. The two
// reasons a person is needed read very differently, so they are named
// separately — one is a disagreement to settle, the other a bug to fix.
func NextStep(t ticket.Ticket, repoURL string) (why, branch, fetch string) {
	branch = record.BranchOf(t)
	if branch == "" || !NeedsAPerson(t.Status) {
		return "", "", ""
	}
	why = "the department is out of options on this one"
	if t.Status == workflow.ColConflicted {
		why = "this one cannot be merged mechanically"
	}
	if repoURL != "" {
		fetch = fmt.Sprintf("git fetch %s %s && git switch -c %s FETCH_HEAD",
			repoURL, branch, branch)
	}
	return why, branch, fetch
}

// EmptyBoardReason explains a board with no rows on it.
//
// AN EMPTY BOARD AND A FINISHED ONE LOOK THE SAME, and saying the wrong one is
// worse than saying nothing. Finished runs are hidden by default, so a board
// where everything is done shows no rows at all — and the message for an empty
// board sent the reader off to file a ticket when seven had just been built.
// Reported from the window after a run finished: every ticket done, the board
// blank, and it read as stuck.
//
// "no tickets" while the first read is still in flight is also a lie, and it is
// the frame every launch opens on.
func EmptyBoardReason(loading bool, hidden int) string {
	switch {
	case loading:
		return "reading the store…"
	case hidden > 0:
		them, are := "them", "are"
		if hidden == 1 {
			them, are = "it", "is"
		}
		return fmt.Sprintf("nothing running — %s %s hidden. Press h to see %s.",
			pluralRuns(hidden), are, them)
	default:
		return "no tickets on this board yet — press n to file one"
	}
}

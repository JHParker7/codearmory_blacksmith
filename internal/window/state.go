// Package window derives what a person reading the board actually needs to
// know: where a ticket is, how long it has taken, and whether anything is wrong.
//
// THE DERIVATIONS LIVE HERE AND THE DRAWING LIVES ELSEWHERE. This window is
// opened to answer "is the department stuck or busy", and every rule below
// exists because the obvious answer was misleading in a way that sent someone to
// the wrong place. A clock that keeps climbing on abandoned work, a queue time
// counted as run time, a merged ticket shown as finished — each looked right and
// each cost an investigation.
package window

import (
	"strings"
	"time"

	"github.com/code-armory-app/blacksmith/internal/record"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

// HeldByAStage reports whether a stage currently HAS this ticket, as opposed to
// the ticket sitting in a queue waiting for one.
//
// DERIVED FROM THE ROUTING TABLE rather than listed here, so a stage cannot be
// added without its working column being counted — a new stage whose column
// nothing recognised would show every ticket it holds as waiting.
func HeldByAStage(tb workflow.Table, status string) bool {
	return tb.IsWorking(status)
}

// Stopped reports whether a ticket has stopped moving, so its clock should stop
// with it.
//
// DONE AND BLOCKED BOTH STOP. Done is obvious. Blocked is the one that matters
// more in practice: it means the department has given up and is waiting for a
// person, so nothing is being spent on it — and a counter that keeps climbing
// says the opposite, exactly when someone is trying to work out how long the
// board has been STUCK rather than BUSY. On r79 five tasks blocked at once and
// every one went on reporting time as though it were still being worked.
func Stopped(status string) bool {
	return status == workflow.ColDone || status == workflow.ColBlocked
}

// Runtime is how long a ticket has taken, whether that number is final, and
// whether there is a number to show at all.
//
// THERE ARE THREE STATES AND ONLY ONE OF THEM TICKS:
//
//	held by a stage     climbing, from when it entered that stage
//	waiting in a queue  nothing — the row shows no runtime at all
//	finished            frozen at what it took, end to end
//
// A WAITING TICKET IS NOT RUNNING, AND ITS CLOCK MUST NOT SAY IT IS. This
// counted from creation for anything unfinished, so a ticket queued behind its
// dependencies climbed while nothing whatever was being spent on it. On a board
// where tasks are chained — every task waits for every task before it — that is
// most of the board most of the time, and the number shown was the age of the
// run rather than the cost of the work. Reported from the window on r85: four
// tickets sat at fifteen minutes each while one developer worked, and the
// obvious reading was that the run had got slower. It had not; they had not
// started.
//
// NOTHING IS SHOWN rather than a frozen figure for a waiting ticket, because a
// number that has stopped moving looks exactly like a number nobody is updating
// — and this window is read to find out whether anything is wrong.
//
// A HELD TICKET COUNTS FROM WHEN THE STAGE TOOK IT, not from creation: the queue
// time before it is not that stage's doing, and adding it hides how long the
// stage is actually taking.
func Runtime(tb workflow.Table, t ticket.Ticket, now time.Time) (d time.Duration, final, show bool) {
	if t.CreatedAt.IsZero() {
		return 0, false, false
	}

	if Stopped(t.Status) {
		// A finished ticket whose update time is missing or older than its
		// creation has no honest total to report, so it reports none rather than a
		// negative or a fabricated one.
		if t.UpdatedAt.IsZero() || !t.UpdatedAt.After(t.CreatedAt) {
			return 0, true, false
		}
		return t.UpdatedAt.Sub(t.CreatedAt), true, true
	}

	if HeldByAStage(tb, t.Status) {
		since := t.UpdatedAt
		if since.IsZero() || since.Before(t.CreatedAt) {
			since = t.CreatedAt
		}
		return now.Sub(since), false, true
	}

	return 0, false, false
}

// MergedAway reports whether this ticket was folded into another rather than
// worked. Its requirements live on, in the ticket MergedInto names.
//
// SHOWN AS MERGED RATHER THAN DONE, because "done" invites the reader to look
// for the work on this ticket's branch — and there is none. It was carried
// somewhere else.
func MergedAway(t ticket.Ticket) bool {
	for _, c := range t.Comments {
		if strings.Contains(c.Body, record.MergedIntoMarker) {
			return true
		}
	}
	return false
}

// MergedInto is the id the merge comment names, or "" when the comment cannot be
// read.
//
// AN UNREADABLE COMMENT STILL MEANS MERGED. The row says merged either way,
// which is the part that matters; only the pointer to where is lost.
func MergedInto(t ticket.Ticket) string {
	for _, c := range t.Comments {
		i := strings.Index(c.Body, record.MergedIntoMarker)
		if i < 0 {
			continue
		}
		// The comment carries the id in backticks: "carried into `abc12345`".
		rest := c.Body[i:]
		if j := strings.Index(rest, "`"); j >= 0 {
			rest = rest[j+1:]
			if k := strings.Index(rest, "`"); k > 0 {
				return rest[:k]
			}
		}
	}
	return ""
}

// UnmetDependencies are the blockers this ticket is still waiting for.
//
// A BLOCKER WHOSE STATUS IS UNKNOWN COUNTS AS UNMET. The store leaves it empty
// when the blocker is not visible to this account, and a window that read that
// as "finished" would show a ticket as ready when nothing can start it — sending
// the reader to look for a stuck dispatcher rather than a permissions problem.
func UnmetDependencies(t ticket.Ticket) []ticket.Dependency {
	var out []ticket.Dependency
	for _, d := range t.DependsOn {
		if d.Status != workflow.ColDone {
			out = append(out, d)
		}
	}
	return out
}

// Waiting reports whether a ticket is held up by its blockers rather than by the
// department.
//
// THE DISTINCTION IS THE WHOLE POINT OF THE WINDOW. A ticket waiting for a
// dependency and a ticket nothing has claimed look identical in a column
// listing, and they need opposite responses: one is the pipeline working
// correctly, the other is a stage that is not running.
func Waiting(t ticket.Ticket) bool {
	return !Stopped(t.Status) && len(UnmetDependencies(t)) > 0
}

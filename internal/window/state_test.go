package window

import (
	"testing"
	"time"

	"github.com/code-armory-app/blacksmith/internal/record"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

func table() workflow.Table {
	return workflow.New(workflow.Options{Architect: true, Coverage: true})
}

var now = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

func at(minutes int) time.Time { return now.Add(-time.Duration(minutes) * time.Minute) }

// THREE STATES AND ONLY ONE OF THEM TICKS.
func TestOnlyAHeldTicketHasARunningClock(t *testing.T) {
	tb := table()

	cases := []struct {
		name  string
		t     ticket.Ticket
		want  time.Duration
		final bool
		show  bool
	}{
		{
			// HELD: climbing, from when the STAGE took it — the queue time before
			// is not that stage's doing, and adding it hides how long the stage is
			// actually taking.
			name: "held by a stage",
			t:    ticket.Ticket{Status: workflow.ColInDev, CreatedAt: at(60), UpdatedAt: at(5)},
			want: 5 * time.Minute, final: false, show: true,
		},
		{
			// WAITING: nothing at all. A number that has stopped moving looks
			// exactly like a number nobody is updating, and this window is read to
			// find out whether anything is wrong. r85: four tickets showed fifteen
			// minutes each while one developer worked, and the obvious reading was
			// that the run had got slower. They had not started.
			name: "waiting in a queue",
			t:    ticket.Ticket{Status: workflow.ColReadyForDev, CreatedAt: at(60), UpdatedAt: at(50)},
			want: 0, final: false, show: false,
		},
		{
			// FINISHED: frozen at what it took, end to end.
			name: "done",
			t:    ticket.Ticket{Status: workflow.ColDone, CreatedAt: at(60), UpdatedAt: at(56)},
			want: 4 * time.Minute, final: true, show: true,
		},
		{
			// BLOCKED STOPS TOO, and it is the one that matters in practice: the
			// department has given up and is waiting for a person, so nothing is
			// being spent. On r79 five tasks blocked at once and every one went on
			// reporting time as though it were still being worked.
			name: "blocked",
			t:    ticket.Ticket{Status: workflow.ColBlocked, CreatedAt: at(60), UpdatedAt: at(30)},
			want: 30 * time.Minute, final: true, show: true,
		},
	}

	for _, c := range cases {
		got, final, show := Runtime(tb, c.t, now)
		if got != c.want || final != c.final || show != c.show {
			t.Errorf("%s: Runtime = (%v, final=%v, show=%v), want (%v, %v, %v)",
				c.name, got, final, show, c.want, c.final, c.show)
		}
	}
}

// A HELD TICKET WITH NO UPDATE TIME falls back to creation rather than showing
// nothing: it IS running, and the only honest clock available is the older one.
func TestAHeldTicketWithNoUpdateTimeStillTicks(t *testing.T) {
	tb := table()

	for _, tk := range []ticket.Ticket{
		{Status: workflow.ColInDev, CreatedAt: at(10)},
		// An update time BEFORE creation is a clock skew, not a fact.
		{Status: workflow.ColInDev, CreatedAt: at(10), UpdatedAt: at(90)},
	} {
		got, final, show := Runtime(tb, tk, now)
		if !show || final {
			t.Errorf("a held ticket reported (show=%v final=%v)", show, final)
		}
		if got != 10*time.Minute {
			t.Errorf("Runtime = %v, want it to fall back to creation", got)
		}
	}
}

// A TICKET WITH NO CREATION TIME HAS NO CLOCK, and inventing one would put a
// fabricated number on a row someone is reading to make a decision.
func TestATicketWithNoCreationTimeShowsNothing(t *testing.T) {
	got, _, show := Runtime(table(), ticket.Ticket{Status: workflow.ColInDev}, now)
	if show || got != 0 {
		t.Errorf("Runtime = (%v, show=%v) for a ticket with no clock", got, show)
	}
}

// A FINISHED TICKET WITH NO HONEST TOTAL reports none rather than a negative or
// a fabricated one.
func TestAFinishedTicketWithABadClockShowsNothing(t *testing.T) {
	tb := table()
	for _, tk := range []ticket.Ticket{
		{Status: workflow.ColDone, CreatedAt: at(10)},
		{Status: workflow.ColDone, CreatedAt: at(10), UpdatedAt: at(20)},
		{Status: workflow.ColDone, CreatedAt: at(10), UpdatedAt: at(10)},
	} {
		got, final, show := Runtime(tb, tk, now)
		if show {
			t.Errorf("a finished ticket with an unusable clock showed %v", got)
		}
		if !final {
			t.Error("a finished ticket was not reported as final")
		}
		if got < 0 {
			t.Errorf("Runtime = %v", got)
		}
	}
}

// DERIVED FROM THE ROUTING TABLE, so a stage cannot be added without its working
// column being counted — a new stage whose column nothing recognised would show
// every ticket it holds as waiting, which reads as a stalled department.
func TestEveryStagesWorkingColumnCountsAsHeld(t *testing.T) {
	tb := table()
	for _, st := range tb.Stages() {
		if !HeldByAStage(tb, st.Working) {
			t.Errorf("%s holds tickets in %q and the window calls that waiting", st.Role, st.Working)
		}
		// And its READY column is not held: that is a queue.
		if HeldByAStage(tb, st.Ready) {
			t.Errorf("%s queues tickets in %q and the window calls that held", st.Role, st.Ready)
		}
	}
}

// A BLOCKER WHOSE STATUS IS UNKNOWN COUNTS AS UNMET. The store leaves it empty
// when the blocker is not visible to this account, and reading that as
// "finished" would show a ticket as ready when nothing can start it — sending
// the reader to look for a stuck dispatcher rather than a permissions problem.
func TestAnInvisibleBlockerCountsAsUnmet(t *testing.T) {
	tk := ticket.Ticket{
		Status: workflow.ColReadyForDev,
		DependsOn: []ticket.Dependency{
			{ID: "a", Status: workflow.ColDone},
			{ID: "b", Status: ""}, // not visible to this account
			{ID: "c", Status: workflow.ColInDev},
		},
	}

	got := UnmetDependencies(tk)
	if len(got) != 2 {
		t.Fatalf("%d unmet, want the invisible one and the unfinished one: %+v", len(got), got)
	}
	if !Waiting(tk) {
		t.Error("a ticket with unmet blockers was not reported as waiting")
	}
}

// THE DISTINCTION IS THE WHOLE POINT OF THE WINDOW. A ticket waiting for a
// dependency and a ticket nothing has claimed look identical in a column
// listing, and they need opposite responses: one is the pipeline working
// correctly, the other is a stage that is not running.
func TestATicketNothingHasClaimedIsNotReportedAsWaiting(t *testing.T) {
	unclaimed := ticket.Ticket{Status: workflow.ColReadyForDev}
	if Waiting(unclaimed) {
		t.Error("a ticket with no blockers was reported as waiting for one")
	}

	// A FINISHED TICKET IS NOT WAITING, whatever its blockers say — a done ticket
	// listed as waiting sends the reader looking for work that is over.
	done := ticket.Ticket{
		Status:    workflow.ColDone,
		DependsOn: []ticket.Dependency{{ID: "a", Status: workflow.ColInDev}},
	}
	if Waiting(done) {
		t.Error("a finished ticket was reported as waiting")
	}
	blocked := done
	blocked.Status = workflow.ColBlocked
	if Waiting(blocked) {
		t.Error("a blocked ticket was reported as waiting for a dependency")
	}
}

// SHOWN AS MERGED RATHER THAN DONE, because "done" invites the reader to look
// for the work on this ticket's branch — and there is none. It was carried
// somewhere else.
func TestAMergedTicketSaysWhereItsWorkWent(t *testing.T) {
	tk := ticket.Ticket{Comments: []ticket.Comment{
		{Body: "an ordinary note"},
		{Body: record.MergedIntoMarker + " carried into `abc12345`, which now holds its criteria."},
	}}

	if !MergedAway(tk) {
		t.Error("a merged ticket was not recognised")
	}
	if got := MergedInto(tk); got != "abc12345" {
		t.Errorf("MergedInto = %q", got)
	}

	// AN UNREADABLE COMMENT STILL MEANS MERGED: only the pointer is lost, and the
	// row saying "merged" is the part that matters.
	vague := ticket.Ticket{Comments: []ticket.Comment{{Body: record.MergedIntoMarker}}}
	if !MergedAway(vague) {
		t.Error("a merge comment with no id was not recognised as merged")
	}
	if got := MergedInto(vague); got != "" {
		t.Errorf("MergedInto = %q for a comment with no id", got)
	}

	// And an ordinary ticket is neither.
	plain := ticket.Ticket{Comments: []ticket.Comment{{Body: "just a note"}}}
	if MergedAway(plain) || MergedInto(plain) != "" {
		t.Error("an ordinary ticket was reported as merged")
	}
}

// A MERGE COMMENT WITH AN UNCLOSED BACKTICK must not return the rest of the
// comment as an id: a row showing three sentences where an id belongs is worse
// than one showing none.
func TestAMalformedMergeCommentYieldsNoId(t *testing.T) {
	tk := ticket.Ticket{Comments: []ticket.Comment{
		{Body: record.MergedIntoMarker + " carried into `abc12345 and then some more prose"},
	}}
	if got := MergedInto(tk); got != "" {
		t.Errorf("MergedInto = %q from an unclosed quote", got)
	}
	if !MergedAway(tk) {
		t.Error("the ticket is still merged even though its id is unreadable")
	}
}

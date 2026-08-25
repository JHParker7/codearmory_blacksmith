package window

import (
	"strings"
	"testing"
	"time"

	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

func tk(id, parent, status string, createdMinutesAgo int) ticket.Ticket {
	t := ticket.Ticket{ID: id, Title: id, Status: status, CreatedAt: at(createdMinutesAgo)}
	if parent != "" {
		p := parent
		t.ParentID = &p
	}
	return t
}

func ids(b Board) []string {
	out := make([]string, 0, len(b.Rows))
	for _, r := range b.Rows {
		out = append(out, r.Ticket.ID)
	}
	return out
}

func depthOf(b Board, id string) int {
	for _, r := range b.Rows {
		if r.Ticket.ID == id {
			return r.Depth
		}
	}
	return -1
}

// A run is a root and everything under it, nested.
func TestChildrenAreNestedUnderTheirRun(t *testing.T) {
	got := Rows([]ticket.Ticket{
		tk("task", "req", workflow.ColInDev, 10),
		tk("req", "", workflow.ColTracking, 20),
		tk("section", "task", workflow.ColDone, 5),
	}, table(), true)

	if len(got.Rows) != 3 {
		t.Fatalf("%d rows, want 3: %v", len(got.Rows), ids(got))
	}
	if got.Rows[0].Ticket.ID != "req" {
		t.Errorf("the run's root is not first: %v", ids(got))
	}
	if depthOf(got, "req") != 0 || depthOf(got, "task") != 1 || depthOf(got, "section") != 2 {
		t.Errorf("the nesting is wrong: %v", got.Rows)
	}
}

// A PARENT THIS BOARD CANNOT SEE MAKES ITS CHILD A ROOT. Nesting a row under
// something absent would hide it entirely — and a row nobody can see is the one
// failure this window exists to prevent.
func TestAChildWhoseParentIsNotOnTheBoardIsStillShown(t *testing.T) {
	got := Rows([]ticket.Ticket{
		tk("orphan", "a-request-on-another-board", workflow.ColInDev, 5),
	}, table(), true)

	if len(got.Rows) != 1 {
		t.Fatalf("the orphan was lost: %v", ids(got))
	}
	if got.Rows[0].Depth != 0 {
		t.Errorf("the orphan is nested under something absent: depth %d", got.Rows[0].Depth)
	}
}

// A ticket naming itself as its parent must not recurse forever.
func TestATicketThatIsItsOwnParentIsTreatedAsARoot(t *testing.T) {
	got := Rows([]ticket.Ticket{tk("loop", "loop", workflow.ColInDev, 5)}, table(), true)
	if len(got.Rows) != 1 || got.Rows[0].Depth != 0 {
		t.Errorf("a self-parented ticket rendered as %v", got.Rows)
	}
}

// A CYCLE IN THE PARENT LINKS MUST NOT HANG THE WINDOW. Two tickets each naming
// the other are not a run, but the board still has to draw.
func TestACycleDoesNotHangTheBoard(t *testing.T) {
	done := make(chan Board, 1)
	go func() {
		done <- Rows([]ticket.Ticket{
			tk("a", "b", workflow.ColInDev, 5),
			tk("b", "a", workflow.ColInDev, 5),
		}, table(), true)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("a cycle in the parent links hung the board")
	}
}

// LIVE FIRST, THEN WHAT IS ASKING FOR SOMEONE, THEN WHAT IS DONE.
//
// THREE STATES, NOT TWO. "Finished" once meant both "done" and "gave up", which
// sorted a run waiting for a person in among the completed ones where nobody
// would look at it again. It still sits BELOW live work — one blocked section in
// a run that ended days ago must not outrank today's work — but above the runs
// that are simply over.
func TestAnAbandonedRunSortsUnderLiveWorkAndOverFinishedWork(t *testing.T) {
	got := Rows([]ticket.Ticket{
		tk("done", "", workflow.ColDone, 40),
		tk("blocked", "", workflow.ColBlocked, 30),
		tk("queued", "", workflow.ColReadyForDev, 20),
		tk("held", "", workflow.ColInDev, 10),
	}, table(), true)

	want := []string{"held", "queued", "blocked", "done"}
	for i, id := range want {
		if got.Rows[i].Ticket.ID != id {
			t.Fatalf("order = %v, want %v", ids(got), want)
		}
	}
	// A conflict needs a person just as much as a block does.
	if !NeedsAPerson(workflow.ColConflicted) || !NeedsAPerson(workflow.ColBlocked) {
		t.Error("a row that needs a person was not recognised")
	}
}

// AND A RUN ASKING FOR SOMEONE IS NOT FILED AS FINISHED WORK. That is the whole
// point of the middle rank: the rows that need a human are the ones a person
// opened this window to find.
func TestARunThatGaveUpIsNotSortedInWithTheCompletedOnes(t *testing.T) {
	children := map[string][]ticket.Ticket{
		"abandoned": {tk("stuck", "abandoned", workflow.ColBlocked, 5)},
		"finished":  {tk("shipped", "finished", workflow.ColDone, 5)},
		"working":   {tk("busy", "working", workflow.ColInDev, 5)},
	}
	cases := map[string]struct {
		root ticket.Ticket
		want RunState
	}{
		"abandoned": {tk("abandoned", "", workflow.ColTracking, 10), RunNeedsAPerson},
		"finished":  {tk("finished", "", workflow.ColTracking, 10), RunDone},
		"working":   {tk("working", "", workflow.ColTracking, 10), RunLive},
	}
	for name, c := range cases {
		if got := StateOf(c.root, children); got != c.want {
			t.Errorf("%s: StateOf = %v, want %v", name, got, c.want)
		}
	}

	// A run with BOTH a blocked child and live work is LIVE: something is still
	// moving, so it is not waiting on a person yet.
	mixed := map[string][]ticket.Ticket{
		"run": {
			tk("stuck", "run", workflow.ColBlocked, 5),
			tk("busy", "run", workflow.ColInDev, 5),
		},
	}
	if got := StateOf(tk("run", "", workflow.ColTracking, 10), mixed); got != RunLive {
		t.Errorf("a run with work still moving reported %v", got)
	}
}

// A LIVE RUN OUTRANKS A FINISHED ONE WHATEVER IS INSIDE IT.
//
// Rank takes the most urgent descendant, which is right within a run and wrong
// between them: one blocked section in a run that ended days ago would rank the
// entire dead run above today's work, because a blocked ticket is the most
// urgent thing there is. The question is "what is happening now", and the answer
// must not be sorted underneath what happened last week.
func TestALiveRunSortsAboveADeadOneThatContainsABlockedTicket(t *testing.T) {
	got := Rows([]ticket.Ticket{
		// A run that is over, with a blocked ticket in it.
		tk("old-run", "", workflow.ColTracking, 5000),
		tk("old-child", "old-run", workflow.ColBlocked, 5000),
		// Today's run, quietly working.
		tk("new-run", "", workflow.ColTracking, 10),
		tk("new-child", "new-run", workflow.ColInDev, 10),
	}, table(), true)

	if got.Rows[0].Ticket.ID != "new-run" {
		t.Errorf("a dead run outranked today's work: %v", ids(got))
	}
}

// WITHIN a run the most urgent descendant still wins, which is what makes a
// blocked section findable under the run it belongs to.
func TestWithinARunTheMostUrgentChildLiftsItsBranch(t *testing.T) {
	got := Rows([]ticket.Ticket{
		tk("run", "", workflow.ColTracking, 10),
		tk("quiet", "run", workflow.ColReadyForDev, 9),
		tk("stuck", "run", workflow.ColBlocked, 8),
	}, table(), true)

	if got.Rows[1].Ticket.ID != "stuck" {
		t.Errorf("the blocked child is not first under its run: %v", ids(got))
	}
}

// A BOARD THAT SILENTLY OMITS ROWS IS WORSE THAN A BUSY ONE: the reader cannot
// tell "nothing else is happening" from "the window is not showing it to me".
func TestHidingFinishedRunsReportsHowManyWereHidden(t *testing.T) {
	all := []ticket.Ticket{
		tk("live", "", workflow.ColInDev, 10),
		tk("over", "", workflow.ColDone, 20),
		tk("also-over", "", workflow.ColDone, 30),
	}

	shown := Rows(all, table(), false)
	if len(shown.Rows) != 1 || shown.Rows[0].Ticket.ID != "live" {
		t.Errorf("rows = %v, want only the live one", ids(shown))
	}
	if shown.Hidden != 2 {
		t.Errorf("Hidden = %d, want 2", shown.Hidden)
	}

	everything := Rows(all, table(), true)
	if len(everything.Rows) != 3 || everything.Hidden != 0 {
		t.Errorf("showing everything gave %v hidden=%d", ids(everything), everything.Hidden)
	}
}

// TRACKING COUNTS AS OVER FOR A ROOT: a broken-down request sits there for the
// life of its children, so a run whose children are all done IS finished even
// though its root never leaves that column.
func TestARunIsOverWhenItsChildrenAreDoneEvenThoughItsRootTracks(t *testing.T) {
	over := Rows([]ticket.Ticket{
		tk("req", "", workflow.ColTracking, 20),
		tk("task", "req", workflow.ColDone, 10),
	}, table(), false)
	if over.Hidden != 1 {
		t.Errorf("a finished run was not recognised: %d hidden", over.Hidden)
	}

	// And one live child keeps the whole run live.
	live := Rows([]ticket.Ticket{
		tk("req", "", workflow.ColTracking, 20),
		tk("task", "req", workflow.ColDone, 10),
		tk("other", "req", workflow.ColInDev, 10),
	}, table(), false)
	if live.Hidden != 0 {
		t.Errorf("a run with work in it was hidden: %d", live.Hidden)
	}
}

// A row carries what its children have done, so a run reads as "3/5" rather
// than requiring the reader to count the lines under it.
func TestARowCountsWhatItsChildrenHaveFinished(t *testing.T) {
	got := Rows([]ticket.Ticket{
		tk("run", "", workflow.ColTracking, 20),
		tk("a", "run", workflow.ColDone, 10),
		tk("b", "run", workflow.ColDone, 10),
		tk("c", "run", workflow.ColInDev, 10),
	}, table(), true)

	if p := got.Rows[0].Progress; p.Done != 2 || p.Total != 3 {
		t.Errorf("Progress = %+v, want 2 of 3", p)
	}
	// A leaf has no children and says so rather than reporting 0/0 as progress.
	for _, r := range got.Rows[1:] {
		if r.Progress.Total != 0 {
			t.Errorf("%s reports children it does not have: %+v", r.Ticket.ID, r.Progress)
		}
	}
}

// PROSE, NOT THE COLUMN NAME. "ready_for_dev" tells the reader what the schema
// calls it; "has tests, waiting for a developer" tells them whether anything is
// wrong.
func TestEveryColumnTheTableRoutesHasWordsOfItsOwn(t *testing.T) {
	tb := table()
	seen := map[string]bool{}

	for _, st := range tb.Stages() {
		for _, col := range []string{st.Ready, st.Working, st.Success, st.Exhausted} {
			if col == "" || seen[col] {
				continue
			}
			seen[col] = true
			got := Label(col)
			if strings.Contains(got, "not a department column") {
				t.Errorf("%q is routed by the table and the window has no words for it", col)
			}
			if got == col {
				t.Errorf("%q renders as its own column name", col)
			}
		}
	}
	if len(seen) < 10 {
		t.Fatalf("only %d columns were checked; the table looks empty", len(seen))
	}
}

// THE TWO STATES THAT NEED A PERSON ARE SHOUTED, because they are the only rows
// in the list that are asking for something.
func TestTheRowsThatNeedAPersonSaySoLoudly(t *testing.T) {
	for _, col := range []string{workflow.ColBlocked, workflow.ColConflicted} {
		got := Label(col)
		if got != strings.ToUpper(got[:1])+got[1:] || !strings.ContainsAny(got, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") {
			t.Errorf("%q renders as %q, which does not stand out", col, got)
		}
	}
	if !strings.Contains(Label(workflow.ColBlocked), "needs you") {
		t.Errorf("a blocked row does not say it needs the reader: %q", Label(workflow.ColBlocked))
	}
}

// A COLUMN THE DEPARTMENT DOES NOT KNOW ABOUT IS SOMEONE ELSE'S, and saying so
// beats pretending it is a stage.
func TestAnUnknownColumnSaysItIsNotOurs(t *testing.T) {
	if got := Label("someone_elses_column"); !strings.Contains(got, "not a department column") {
		t.Errorf("Label = %q", got)
	}
	if got := Label(""); got != "no status" {
		t.Errorf("an empty status rendered as %q", got)
	}
}

// READ OFF THE ROUTING TABLE rather than restated, so a stage added to the table
// shows up in the window the day it is written and cannot disagree with it.
func TestTheNextAgentIsReadFromTheTable(t *testing.T) {
	tb := table()
	if got := NextRole(tb, workflow.ColReadyForDev); got != workflow.RoleDev {
		t.Errorf("NextRole(ready_for_dev) = %q", got)
	}
	if got := NextRole(tb, workflow.ColInDev); got != "" {
		t.Errorf("a working column named a next agent: %q", got)
	}
	if got := NextRole(tb, "not_a_column"); got != "" {
		t.Errorf("an unknown column named %q", got)
	}
}

// THE WHOLE REASON TO SHOW A RUNTIME IS TO COMPARE RUNTIMES. Rounding to whole
// minutes collided half of r77's tickets with another: 5m44s and 5m00s both read
// as "5m", 1m50s and 1m12s both as "1m".
func TestARuntimeKeepsEnoughPrecisionToCompareTwo(t *testing.T) {
	a := RuntimeText(5*time.Minute + 44*time.Second)
	b := RuntimeText(5 * time.Minute)
	if a == b {
		t.Errorf("5m44s and 5m00s both render as %q", a)
	}
	if got := RuntimeText(44 * time.Second); got != "44s" {
		t.Errorf("RuntimeText(44s) = %q", got)
	}
	if got := RuntimeText(5*time.Minute + 4*time.Second); got != "5m04s" {
		t.Errorf("RuntimeText(5m4s) = %q", got)
	}
	if got := RuntimeText(2*time.Hour + 7*time.Minute); got != "2h07m" {
		t.Errorf("RuntimeText(2h7m) = %q", got)
	}
	// A negative duration is a clock problem, not something to render as "-3s".
	if got := RuntimeText(-3 * time.Second); got != "0s" {
		t.Errorf("RuntimeText(-3s) = %q", got)
	}
}

func TestAnEmptyBoardDrawsNothingRatherThanFailing(t *testing.T) {
	got := Rows(nil, table(), true)
	if len(got.Rows) != 0 || got.Hidden != 0 {
		t.Errorf("an empty board produced %v hidden=%d", ids(got), got.Hidden)
	}
}

// ONLY WHAT IS DONE IS EVER HIDDEN. A run the department gave up on has also
// stopped moving, but it is the one thing the reader opened this window to find
// — hiding it puts the only rows asking for a human behind a keypress nobody
// knew to make.
func TestARunThatNeedsAPersonIsNeverHidden(t *testing.T) {
	got := Rows([]ticket.Ticket{
		tk("stuck", "", workflow.ColBlocked, 20),
		tk("conflicted", "", workflow.ColConflicted, 20),
		tk("over", "", workflow.ColDone, 20),
	}, table(), false)

	shown := ids(got)
	for _, want := range []string{"stuck", "conflicted"} {
		var found bool
		for _, id := range shown {
			if id == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%q needs a person and was hidden: %v", want, shown)
		}
	}
	if got.Hidden != 1 {
		t.Errorf("Hidden = %d, want only the finished run", got.Hidden)
	}

	// And a whole run whose work is abandoned stays visible too.
	nested := Rows([]ticket.Ticket{
		tk("run", "", workflow.ColTracking, 20),
		tk("child", "run", workflow.ColBlocked, 10),
	}, table(), false)
	if nested.Hidden != 0 || len(nested.Rows) != 2 {
		t.Errorf("an abandoned run was hidden: %v hidden=%d", ids(nested), nested.Hidden)
	}
}

// THE MOST URGENT DESCENDANT LIFTS ITS WHOLE BRANCH, however deep it is. A
// blocked grandchild has to pull the task it belongs to up the list, or the
// reader has to expand every branch to find what is asking for them.
func TestAnUrgentGrandchildLiftsTheBranchAboveIt(t *testing.T) {
	got := Rows([]ticket.Ticket{
		tk("run", "", workflow.ColTracking, 100),

		// A task whose own status is unremarkable, with a blocked child under it.
		tk("has-trouble", "run", workflow.ColTracking, 50),
		tk("stuck", "has-trouble", workflow.ColBlocked, 40),

		// A task that is quietly queued, created more recently — so recency alone
		// would put it first.
		tk("quiet", "run", workflow.ColTracking, 10),
		tk("waiting", "quiet", workflow.ColReadyForDev, 5),
	}, table(), true)

	order := ids(got)
	trouble, quiet := indexOf(order, "has-trouble"), indexOf(order, "quiet")
	if trouble < 0 || quiet < 0 {
		t.Fatalf("rows = %v", order)
	}
	if trouble > quiet {
		t.Errorf("the branch holding a blocked ticket sorted below the quiet one: %v", order)
	}
}

func indexOf(ss []string, want string) int {
	for i, s := range ss {
		if s == want {
			return i
		}
	}
	return -1
}

// StateOf WALKS WHATEVER IT IS GIVEN, and a caller can hand it a children map
// with a cycle in it — Rows cannot, because a cycle leaves no roots to start
// from, but StateOf is exported and the guard is what keeps a bad map from
// hanging the window rather than drawing it.
func TestStateOfSurvivesACycleInTheChildren(t *testing.T) {
	children := map[string][]ticket.Ticket{
		"a": {tk("b", "a", workflow.ColTracking, 5)},
		"b": {tk("a", "b", workflow.ColTracking, 5)},
	}

	done := make(chan RunState, 1)
	go func() { done <- StateOf(tk("a", "", workflow.ColTracking, 10), children) }()

	select {
	case got := <-done:
		// Nothing in the cycle is doing work, so the run is over.
		if got != RunDone {
			t.Errorf("StateOf = %v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a cycle in the children hung StateOf")
	}
}

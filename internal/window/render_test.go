package window

import (
	"strings"
	"testing"
	"time"

	"github.com/code-armory-app/blacksmith/internal/agent/review"
	"github.com/code-armory-app/blacksmith/internal/record"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

// stateText flattens the cells so a test can assert on what a reader sees.
func stateText(cells []Cell) string {
	var b strings.Builder
	for _, c := range cells {
		b.WriteString(c.Text)
	}
	return b.String()
}

func toneOf(cells []Cell, substr string) (Tone, bool) {
	for _, c := range cells {
		if strings.Contains(c.Text, substr) {
			return c.Tone, true
		}
	}
	return 0, false
}

func TestStateCellsLiveAgentNamesTheRoleAndTheTurn(t *testing.T) {
	now := time.Now()
	tk := ticket.Ticket{ID: "t1", Status: workflow.ColInDev, UpdatedAt: now.Add(-2 * time.Minute)}
	act := Activity{Role: "dev-agent", What: "editing", At: now, Turns: 7}

	cells := StateCells(tk, act, now)
	got := stateText(cells)
	if !strings.Contains(got, "dev-agent · editing") {
		t.Fatalf("state = %q, want the role and what it is doing", got)
	}
	if !strings.Contains(got, "turn 7") {
		t.Fatalf("state = %q, want the turn count — it is how you tell progress "+
			"from a loop", got)
	}
	if tone, _ := toneOf(cells, "dev-agent"); tone != ToneLive {
		t.Fatalf("tone = %v, want ToneLive", tone)
	}
	// The turn count is dim so the eye lands on the role, not the number.
	if tone, _ := toneOf(cells, "turn 7"); tone != ToneQuiet {
		t.Fatalf("turn count tone = %v, want ToneQuiet", tone)
	}
}

// A PARENT IS NOT AN AGENT. It reports dimly so the eye goes to the row actually
// being worked rather than to every ancestor of it.
func TestStateCellsParentReportsQuietly(t *testing.T) {
	now := time.Now()
	tk := ticket.Ticket{ID: "root", Status: workflow.ColTracking, UpdatedAt: now}
	act := Activity{What: "2 working below", Working: 2, Turns: 11, At: now}

	cells := StateCells(tk, act, now)
	if got := stateText(cells); !strings.Contains(got, "2 working below") {
		t.Fatalf("state = %q, want the count of busy children", got)
	}
	if tone, _ := toneOf(cells, "2 working below"); tone != ToneQuiet {
		t.Fatalf("tone = %v, want ToneQuiet — a parent must not compete with the "+
			"row doing the work", tone)
	}
}

// MERGED IS NOT BUILT. Four tickets reaching "done" seconds after the product
// manager finishes reads as four pieces of work completed instantly.
func TestStateCellsMergedIsNotDone(t *testing.T) {
	now := time.Now()
	tk := ticket.Ticket{
		ID: "t1", Status: workflow.ColDone, UpdatedAt: now,
		Comments: []ticket.Comment{{Body: record.MergedIntoMarker + "\nSee ticket abcd1234."}},
	}

	got := stateText(StateCells(tk, Activity{}, now))
	if !strings.Contains(got, "merged") {
		t.Fatalf("state = %q, want it named as merged rather than done", got)
	}
	if strings.Contains(got, Label(workflow.ColDone)) {
		t.Fatalf("state = %q, must not also claim to be done", got)
	}
}

func TestStateCellsMergedWithoutANamedTarget(t *testing.T) {
	now := time.Now()
	tk := ticket.Ticket{
		ID: "t1", Status: workflow.ColDone, UpdatedAt: now,
		Comments: []ticket.Comment{{Body: record.MergedIntoMarker}},
	}
	got := stateText(StateCells(tk, Activity{}, now))
	if !strings.Contains(got, "merged into another ticket") {
		t.Fatalf("state = %q, want a readable fallback rather than a dangling "+
			"'merged into'", got)
	}
}

func TestStateCellsToneByStatus(t *testing.T) {
	now := time.Now()
	cases := []struct {
		status string
		want   Tone
	}{
		{workflow.ColBlocked, ToneAlert},
		{workflow.ColConflicted, ToneAlert},
		{workflow.ColDone, ToneDone},
		{workflow.ColScoping, ToneWaiting},
	}
	for _, c := range cases {
		t.Run(c.status, func(t *testing.T) {
			tk := ticket.Ticket{ID: "t1", Status: c.status, UpdatedAt: now}
			cells := StateCells(tk, Activity{}, now)
			if cells[0].Tone != c.want {
				t.Fatalf("tone for %q = %v, want %v", c.status, cells[0].Tone, c.want)
			}
		})
	}
}

// THE AGE IS WHAT SEPARATES "working on it" FROM "wedged an hour ago".
func TestStateCellsCarriesTheStageAge(t *testing.T) {
	now := time.Now()
	tk := ticket.Ticket{ID: "t1", Status: workflow.ColInDev, UpdatedAt: now.Add(-42 * time.Minute)}

	if got := stateText(StateCells(tk, Activity{}, now)); !strings.Contains(got, "42m") {
		t.Fatalf("state = %q, want the age of the current stage", got)
	}
}

// A QUEUED TICKET AND A BLOCKED-BEHIND-A-PREREQUISITE ONE LOOK IDENTICAL
// otherwise, and the difference is the most asked question about this board.
func TestStateCellsNamesASingleUnmetDependency(t *testing.T) {
	now := time.Now()
	tk := ticket.Ticket{
		ID: "t1", Status: workflow.ColScoping, UpdatedAt: now,
		DependsOn: []ticket.Dependency{{ID: "cb583cbd-aaaa", Status: workflow.ColInDev}},
	}

	got := stateText(StateCells(tk, Activity{}, now))
	if !strings.Contains(got, "waits on cb583cbd") {
		t.Fatalf("state = %q, want the prerequisite named — one id is worth more "+
			"than a count, because you can go and look at it", got)
	}
}

func TestStateCellsCountsSeveralUnmetDependencies(t *testing.T) {
	now := time.Now()
	tk := ticket.Ticket{
		ID: "t1", Status: workflow.ColScoping, UpdatedAt: now,
		DependsOn: []ticket.Dependency{
			{ID: "aaaaaaaa", Status: workflow.ColInDev},
			{ID: "bbbbbbbb", Status: workflow.ColDone},
			{ID: "cccccccc", Status: workflow.ColScoping},
		},
	}

	got := stateText(StateCells(tk, Activity{}, now))
	if !strings.Contains(got, "waits on 2") {
		t.Fatalf("state = %q, want 2 unmet — the met one must not be counted", got)
	}
}

// A WORKING TICKET IS NOT WAITING. Showing its dependencies while an agent has
// it says the opposite of what is happening.
func TestStateCellsHidesDependenciesWhileWorking(t *testing.T) {
	now := time.Now()
	tk := ticket.Ticket{
		ID: "t1", Status: workflow.ColInDev, UpdatedAt: now,
		DependsOn: []ticket.Dependency{{ID: "aaaaaaaa", Status: workflow.ColScoping}},
	}
	act := Activity{Role: "dev-agent", What: "editing", At: now}

	if got := stateText(StateCells(tk, act, now)); strings.Contains(got, "waits on") {
		t.Fatalf("state = %q, must not say it is waiting while an agent has it", got)
	}
}

func TestStateCellsHidesDependenciesOnceDone(t *testing.T) {
	now := time.Now()
	tk := ticket.Ticket{
		ID: "t1", Status: workflow.ColDone, UpdatedAt: now,
		DependsOn: []ticket.Dependency{{ID: "aaaaaaaa", Status: workflow.ColScoping}},
	}
	if got := stateText(StateCells(tk, Activity{}, now)); strings.Contains(got, "waits on") {
		t.Fatalf("state = %q, a finished ticket waits for nothing", got)
	}
}

// ONCE A BRANCH EXISTS IT IS THE ONLY THING ON THE ROW YOU CAN ACT ON.
func TestStateCellsShowsTheBranch(t *testing.T) {
	now := time.Now()
	tk := ticket.Ticket{
		ID: "t1", Status: workflow.ColInReview, UpdatedAt: now,
		Comments: []ticket.Comment{{Body: record.PublishBranch(record.BranchMarker, "bs/t1-store")}},
	}

	if got := stateText(StateCells(tk, Activity{}, now)); !strings.Contains(got, "bs/t1-store") {
		t.Fatalf("state = %q, want the branch on the row rather than dug out of a "+
			"comment", got)
	}
}

// While an agent is working, what it is doing matters more than where the last
// push went — and the row has a fixed width.
func TestStateCellsHidesTheBranchWhileWorking(t *testing.T) {
	now := time.Now()
	tk := ticket.Ticket{
		ID: "t1", Status: workflow.ColInDev, UpdatedAt: now,
		Comments: []ticket.Comment{{Body: record.PublishBranch(record.BranchMarker, "bs/t1-store")}},
	}
	act := Activity{Role: "dev-agent", What: "editing", At: now}

	if got := stateText(StateCells(tk, act, now)); strings.Contains(got, "bs/t1-store") {
		t.Fatalf("state = %q, want the live work to have the row", got)
	}
}

// ---- Tally ----

func TestTally(t *testing.T) {
	now := time.Now()
	rows := []Row{
		{Ticket: ticket.Ticket{ID: "done", Status: workflow.ColDone}},
		{Ticket: ticket.Ticket{ID: "blocked", Status: workflow.ColBlocked}},
		{Ticket: ticket.Ticket{ID: "conflict", Status: workflow.ColConflicted}},
		{Ticket: ticket.Ticket{ID: "working", Status: workflow.ColInDev}},
		{Ticket: ticket.Ticket{ID: "queued", Status: workflow.ColScoping}},
	}
	acts := map[string]Activity{"working": {Role: "dev-agent", At: now}}

	got := Tally(rows, acts, now)
	want := Counts{Done: 1, Moving: 1, Stuck: 2, Waiting: 1}
	if got != want {
		t.Fatalf("Tally = %+v, want %+v", got, want)
	}
	if got.Total() != 5 {
		t.Fatalf("Total = %d, want 5", got.Total())
	}
}

// A CLAIMED TICKET WHOSE HOST DIED IS NOT MOVING, and counting it as such is the
// exact reading this window exists to prevent.
func TestTallyCountsAStaleClaimAsWaiting(t *testing.T) {
	now := time.Now()
	rows := []Row{{Ticket: ticket.Ticket{ID: "t1", Status: workflow.ColInDev}}}
	acts := map[string]Activity{"t1": {Role: "dev-agent", At: now.Add(-StaleAfter - time.Minute)}}

	if got := Tally(rows, acts, now); got.Moving != 0 || got.Waiting != 1 {
		t.Fatalf("Tally = %+v, want the stale claim counted as waiting", got)
	}
}

func TestTallyEmpty(t *testing.T) {
	if got := Tally(nil, nil, time.Now()); got.Total() != 0 {
		t.Fatalf("Tally = %+v, want zero", got)
	}
}

// ---- ReturnCeiling ----

func TestReturnCeiling(t *testing.T) {
	tickets := []ticket.Ticket{
		{ID: "aaaaaaaa-1111", Comments: []ticket.Comment{
			{Body: "some review"},
			{Body: review.ReturnCeilingMarker + " after 10 returns"},
		}},
		{ID: "bbbbbbbb-2222", Comments: []ticket.Comment{{Body: "fine"}}},
		{ID: "cccccccc-3333", Status: workflow.ColBlocked},
	}

	got := ReturnCeiling(tickets)
	if len(got) != 1 || got[0] != "aaaaaaaa" {
		t.Fatalf("ReturnCeiling = %v, want only the ticket carrying the marker", got)
	}
}

// DELIBERATELY NOT THE BLOCKED COLUMN. Plenty of things land there; this alert
// is specifically the review loop that had to be stopped, and widening it would
// make it noise.
func TestReturnCeilingIsNotJustTheBlockedColumn(t *testing.T) {
	tickets := []ticket.Ticket{{ID: "aaaaaaaa", Status: workflow.ColBlocked,
		Comments: []ticket.Comment{{Body: "could not compile"}}}}

	if got := ReturnCeiling(tickets); len(got) != 0 {
		t.Fatalf("ReturnCeiling = %v, want none: blocked for another reason is not "+
			"the review ceiling", got)
	}
}

func TestReturnCeilingCountsATicketOnce(t *testing.T) {
	tickets := []ticket.Ticket{{ID: "aaaaaaaa", Comments: []ticket.Comment{
		{Body: review.ReturnCeilingMarker},
		{Body: review.ReturnCeilingMarker},
	}}}
	if got := ReturnCeiling(tickets); len(got) != 1 {
		t.Fatalf("ReturnCeiling = %v, want the ticket named once", got)
	}
}

// ---- NextStep ----

func TestNextStepGivesTheCommand(t *testing.T) {
	tk := ticket.Ticket{
		ID: "t1", Status: workflow.ColBlocked,
		Comments: []ticket.Comment{{Body: record.PublishBranch(record.BranchMarker, "bs/t1")}},
	}

	why, branch, fetch := NextStep(tk, "http://git/repo.git")
	if branch != "bs/t1" {
		t.Fatalf("branch = %q, want bs/t1", branch)
	}
	if !strings.Contains(why, "out of options") {
		t.Fatalf("why = %q, want the department's failure named", why)
	}
	if !strings.Contains(fetch, "git fetch http://git/repo.git bs/t1") {
		t.Fatalf("fetch = %q, want a command that can be pasted", fetch)
	}
	if !strings.Contains(fetch, "switch -c bs/t1 FETCH_HEAD") {
		t.Fatalf("fetch = %q, want it to leave you ON the branch", fetch)
	}
}

// THE TWO REASONS READ VERY DIFFERENTLY: one is a disagreement to settle, the
// other a bug to fix.
func TestNextStepNamesAConflictAsItsOwnThing(t *testing.T) {
	tk := ticket.Ticket{
		ID: "t1", Status: workflow.ColConflicted,
		Comments: []ticket.Comment{{Body: record.PublishBranch(record.BranchMarker, "bs/t1")}},
	}
	why, _, _ := NextStep(tk, "")
	if !strings.Contains(why, "merged mechanically") {
		t.Fatalf("why = %q, want the conflict named rather than a generic failure", why)
	}
}

// AN ALERT WITH NO BRANCH IS A STATEMENT, NOT A NEXT MOVE.
func TestNextStepSilentWithoutSomethingToActOn(t *testing.T) {
	noBranch := ticket.Ticket{ID: "t1", Status: workflow.ColBlocked}
	if why, br, _ := NextStep(noBranch, "http://git/repo.git"); why != "" || br != "" {
		t.Fatal("a blocked ticket with no branch has no command to offer")
	}

	healthy := ticket.Ticket{ID: "t1", Status: workflow.ColInDev,
		Comments: []ticket.Comment{{Body: record.PublishBranch(record.BranchMarker, "bs/t1")}}}
	if why, _, _ := NextStep(healthy, "http://git/repo.git"); why != "" {
		t.Fatal("a ticket the department is still working is not yours yet")
	}
}

func TestNextStepWithoutARepoURL(t *testing.T) {
	tk := ticket.Ticket{ID: "t1", Status: workflow.ColBlocked,
		Comments: []ticket.Comment{{Body: record.PublishBranch(record.BranchMarker, "bs/t1")}}}

	why, branch, fetch := NextStep(tk, "")
	if why == "" || branch == "" {
		t.Fatal("the branch is still worth naming without a repo URL")
	}
	if fetch != "" {
		t.Fatalf("fetch = %q, want none: a command with an empty remote does not run", fetch)
	}
}

// ---- EmptyBoardReason ----

// AN EMPTY BOARD AND A FINISHED ONE LOOK THE SAME, and the message for an empty
// board sent the reader to file a ticket when seven had just been built.
func TestEmptyBoardReason(t *testing.T) {
	if got := EmptyBoardReason(true, 0); !strings.Contains(got, "reading") {
		t.Fatalf("got %q, want the first read acknowledged — 'no tickets' while the "+
			"read is in flight is a lie, and it is the frame every launch opens on", got)
	}
	got := EmptyBoardReason(false, 7)
	if !strings.Contains(got, "7 finished runs") || !strings.Contains(got, "Press h") {
		t.Fatalf("got %q, want the finished runs counted and the key to see them", got)
	}
	if strings.Contains(got, "no tickets") {
		t.Fatalf("got %q, must not call a finished board an empty one", got)
	}
	if got := EmptyBoardReason(false, 1); !strings.Contains(got, "see it") {
		t.Fatalf("got %q, want singular phrasing for one run", got)
	}
	if got := EmptyBoardReason(false, 0); !strings.Contains(got, "press n") {
		t.Fatalf("got %q, want a genuinely empty board to say how to file work", got)
	}
}

// Loading wins: during the first read the hidden count is from no data at all.
func TestEmptyBoardReasonLoadingWins(t *testing.T) {
	if got := EmptyBoardReason(true, 5); !strings.Contains(got, "reading") {
		t.Fatalf("got %q, want the in-flight read reported first", got)
	}
}

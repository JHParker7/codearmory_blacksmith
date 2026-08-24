package workflow

import (
	"strings"
	"testing"
)

// configs is every shape of pipeline a host can run. The invariants below hold
// for ALL of them, which is the point of building the table as a value: the
// mutable version could only ever be examined in whichever state startup had
// left it in.
func configs() map[string]Table {
	return map[string]Table{
		"bare":               New(Options{}),
		"architect":          New(Options{Architect: true}),
		"coverage":           New(Options{Coverage: true}),
		"architect+coverage": New(Options{Architect: true, Coverage: true}),
	}
}

// EVERY STAGE MUST ROUTE SOMEWHERE. A row missing Ready, Working, Success or
// Exhausted is a stage that takes work and cannot put it down, and it shows up
// as a ticket sitting in a column nothing polls.
func TestEveryStageIsFullyRouted(t *testing.T) {
	for name, tb := range configs() {
		for _, role := range tb.Roles() {
			st, _ := tb.For(role)
			if st.Role != role {
				t.Errorf("%s: row keyed %q names itself %q", name, role, st.Role)
			}
			for field, col := range map[string]string{
				"Ready": st.Ready, "Working": st.Working,
				"Success": st.Success, "Exhausted": st.Exhausted,
			} {
				if col == "" {
					t.Errorf("%s: %s has no %s column; work would stop there with nothing to collect it",
						name, role, field)
				}
			}
		}
	}
}

// EVERY ROUTED COLUMN MUST BE A REAL ONE. A typo sends work to a column no
// dispatcher polls and no window renders, and nothing reports it.
func TestEveryRoutedColumnIsOnTheBoard(t *testing.T) {
	onBoard := map[string]bool{}
	for _, c := range Columns() {
		onBoard[c.Value] = true
	}
	for name, tb := range configs() {
		for _, st := range tb.Stages() {
			for field, col := range map[string]string{
				"Ready": st.Ready, "Working": st.Working, "Success": st.Success,
				"Exhausted": st.Exhausted, "Returns": st.Returns,
			} {
				if col != "" && !onBoard[col] {
					t.Errorf("%s: %s routes %s to %q, which is not a column", name, st.Role, field, col)
				}
			}
		}
	}
}

// TWO STAGES MUST NOT SHARE A QUEUE. Both would claim from it and which one won
// would depend on poll timing — the developer and upkeep are judged by different
// gates, so one taking the other's work is a silent mis-verification.
//
// This is also the invariant the old mutable table could not hold: with the
// architect left inert in the map, the inbox had two owners and which one a
// caller got depended on map iteration order.
func TestNoTwoStagesShareAQueue(t *testing.T) {
	for name, tb := range configs() {
		owner := map[string]string{}
		for _, st := range tb.Stages() {
			if prev, clash := owner[st.Ready]; clash {
				t.Errorf("%s: %s and %s both take from %q; which gets the work is a race",
					name, prev, st.Role, st.Ready)
			}
			owner[st.Ready] = st.Role
		}
	}
}

// A stage must not take from the column it writes to: that is a stage handing
// work to itself, which polls forever and looks busy.
func TestNoStageFeedsItself(t *testing.T) {
	for name, tb := range configs() {
		for _, st := range tb.Stages() {
			if st.Ready == st.Success {
				t.Errorf("%s: %s succeeds into its own queue %q", name, st.Role, st.Ready)
			}
			if st.Returns != "" && st.Returns == st.Ready {
				t.Errorf("%s: %s returns work to its own queue; a hand-back must reach an earlier stage",
					name, st.Role)
			}
		}
	}
}

// A HAND-BACK MUST REACH A STAGE THAT CAN ACT ON IT. Returns naming a column no
// stage collects from sends the ticket backwards into nothing — which is how a
// developer's correct diagnosis of a broken specification gets lost.
func TestEveryReturnReachesAStage(t *testing.T) {
	for name, tb := range configs() {
		queues := map[string]bool{}
		for _, st := range tb.Stages() {
			queues[st.Ready] = true
		}
		for _, st := range tb.Stages() {
			if st.Returns != "" && !queues[st.Returns] {
				t.Errorf("%s: %s returns work to %q, which no stage collects from", name, st.Role, st.Returns)
			}
		}
	}
}

// EVERY STAGE MUST BE REACHABLE. A queue nothing writes into is a stage that
// never runs, and it fails silently: the board simply never shows that column.
//
// The entry columns are the exceptions — the inbox is written by a person, the
// section, task and upkeep queues by an agent creating tickets rather than by a
// stage's Success.
func TestEveryStageIsReachable(t *testing.T) {
	entry := map[string]bool{
		ColInbox:               true, // a person files a request
		ColReadyForTests:       true, // the product manager, for a one-piece request
		ColReadyForSpec:        true, // the product manager, opening sections
		ColReadyForTicketMerge: true, // the product manager, when tasks are folded
		ColReadyForSpecMerge:   true, // the product manager, opening tasks
		ColReadyForMaintenance: true, // upkeep, harvested from earlier runs
		ColConflicted:          true, // an integrator's outcome, not its Success
	}
	for name, tb := range configs() {
		written := map[string]bool{}
		for _, st := range tb.Stages() {
			written[st.Success] = true
			written[st.Exhausted] = true
			if st.Returns != "" {
				written[st.Returns] = true
			}
		}
		for _, st := range tb.Stages() {
			if !written[st.Ready] && !entry[st.Ready] {
				t.Errorf("%s: nothing routes work into %q, so %s never runs", name, st.Ready, st.Role)
			}
		}
	}
}

// The developer hands back to the RECONCILER, the one stage that may edit the
// tests it cannot. Sending a task to the section author's queue instead is a
// failure already paid for: the task was claimed by the section author, read a
// hundred times and killed.
func TestTheDeveloperHandsBackToTheStageThatMayEditTests(t *testing.T) {
	dev, ok := New(Options{}).For(RoleDev)
	if !ok {
		t.Fatal("no developer stage")
	}
	if dev.Returns != ColReadyForSpecMerge {
		t.Errorf("the developer returns work to %q, want the reconciler's queue %q",
			dev.Returns, ColReadyForSpecMerge)
	}
}

// Upkeep must not share the developer's queue: the two are judged by different
// gates, and a ticket taken by the wrong dispatcher is verified against the
// wrong bar.
func TestUpkeepHasItsOwnQueueAndStillMerges(t *testing.T) {
	tb := New(Options{})
	up, ok := tb.For(RoleMaintain)
	if !ok {
		t.Fatal("no upkeep stage")
	}
	dev, _ := tb.For(RoleDev)
	if up.Ready == dev.Ready {
		t.Error("upkeep and development share a queue, so one gate would judge both")
	}
	if up.Success != ColReadyForIntegration {
		t.Errorf("upkeep succeeds into %q; work that is never merged is work thrown away", up.Success)
	}
}

// THE ARCHITECT IS INSERTED, NOT SWITCHED ON. A host without one must leave the
// product manager on the inbox, or every request sits there with nothing coming
// to collect it.
func TestWithoutAnArchitectTheProductManagerOwnsTheInbox(t *testing.T) {
	tb := New(Options{})
	if _, ok := tb.For(RoleArchitect); ok {
		t.Error("the architect is in a table built without one; an inert stage gives the inbox two owners")
	}
	pm, _ := tb.For(RoleScoping)
	if pm.Ready != ColInbox {
		t.Errorf("the product manager takes from %q with no architect running; requests would never be collected",
			pm.Ready)
	}
}

func TestWithAnArchitectTheProductManagerSitsBehindIt(t *testing.T) {
	tb := New(Options{Architect: true})
	arch, ok := tb.For(RoleArchitect)
	if !ok {
		t.Fatal("no architect stage in a table built with one")
	}
	if arch.Ready != ColInbox {
		t.Errorf("the architect takes from %q, want the inbox", arch.Ready)
	}
	pm, _ := tb.For(RoleScoping)
	if pm.Ready != arch.Success {
		t.Errorf("the product manager takes from %q but the architect hands off to %q", pm.Ready, arch.Success)
	}
}

// A REQUEST THAT COULD NOT BE DESIGNED IS STILL A REQUEST WORTH BREAKING DOWN.
// Documentation is context, not a gate, so blocking here would make a weak or
// unreachable architect fatal to work that never needed one.
func TestAnExhaustedArchitectDoesNotBlockTheRequest(t *testing.T) {
	arch, _ := New(Options{Architect: true}).For(RoleArchitect)
	if arch.Exhausted == ColBlocked {
		t.Error("an architect that gave up blocks the request; the breakdown never needed its output")
	}
	if arch.Exhausted != arch.Success {
		t.Errorf("an exhausted architect sends the request to %q, want the same queue as success (%q)",
			arch.Exhausted, arch.Success)
	}
}

// Turning coverage off must rewire the DEVELOPER too, or finished work goes to a
// column nothing polls.
func TestCoverageOffSendsTheDeveloperStraightToReview(t *testing.T) {
	tb := New(Options{})
	if _, ok := tb.For(RoleCoverage); ok {
		t.Error("the coverage stage is present in a table built without it")
	}
	dev, _ := tb.For(RoleDev)
	if dev.Success != ColReadyForReview {
		t.Errorf("with no coverage stage the developer succeeds into %q, which nothing collects from",
			dev.Success)
	}
}

func TestCoverageOnSitsBetweenDevelopmentAndReview(t *testing.T) {
	tb := New(Options{Coverage: true})
	cov, ok := tb.For(RoleCoverage)
	if !ok {
		t.Fatal("no coverage stage in a table built with one")
	}
	dev, _ := tb.For(RoleDev)
	if dev.Success != cov.Ready {
		t.Errorf("the developer succeeds into %q but coverage takes from %q", dev.Success, cov.Ready)
	}
	if cov.Success != ColReadyForReview {
		t.Errorf("coverage succeeds into %q, want review", cov.Success)
	}
	// Falling short of a percentage must not blackhole code that already
	// satisfies its specification.
	if cov.Exhausted != ColReadyForReview {
		t.Errorf("an exhausted coverage stage sends work to %q; a quality goal is not a correctness gate",
			cov.Exhausted)
	}
	if cov.Returns != ColReadyForDev {
		t.Errorf("coverage returns work to %q; a branch it cannot cover is the developer's to answer",
			cov.Returns)
	}
}

// Building a table must not disturb one already built. The mutable version this
// replaces could be rewired under a running pipeline, which moves tickets into a
// column nothing is polling.
func TestBuildingOneTableDoesNotRewireAnother(t *testing.T) {
	bare := New(Options{})
	before, _ := bare.For(RoleDev)

	_ = New(Options{Architect: true, Coverage: true})

	after, _ := bare.For(RoleDev)
	if after != before {
		t.Errorf("the developer stage changed to %+v after another table was built; it was %+v", after, before)
	}
	if _, ok := bare.For(RoleCoverage); ok {
		t.Error("building a coverage table added the stage to a table built without it")
	}
}

// Roles must come back in board order, so anything listing stages lists them the
// same way twice running — a map's order is not.
func TestRolesComeBackInBoardOrder(t *testing.T) {
	tb := New(Options{Architect: true, Coverage: true})
	first := strings.Join(tb.Roles(), ",")
	for i := 0; i < 5; i++ {
		if got := strings.Join(tb.Roles(), ","); got != first {
			t.Fatalf("Roles() varies between calls:\n%s\n%s", first, got)
		}
	}
	roles := tb.Roles()
	if roles[0] != RoleArchitect {
		t.Errorf("Roles() starts at %q, want the stage owning the inbox", roles[0])
	}
	for i, want := range []string{RoleArchitect, RoleScoping} {
		if roles[i] != want {
			t.Errorf("Roles()[%d] = %q, want %q", i, roles[i], want)
		}
	}
	if got := len(tb.Stages()); got != len(roles) {
		t.Errorf("Stages() returned %d entries for %d roles", got, len(roles))
	}
}

func TestForReportsARoleThisHostDoesNotRun(t *testing.T) {
	if _, ok := New(Options{}).For("no-such-agent"); ok {
		t.Error("For() claimed a stage for a role the table does not route")
	}
}

func TestTerminalColumnsAreTheOnesWorkStopsIn(t *testing.T) {
	for _, c := range []string{ColDone, ColBlocked} {
		if !Terminal(c) {
			t.Errorf("Terminal(%q) = false", c)
		}
	}
	// Conflicted is deliberately NOT terminal: it is the resolver's queue, so a
	// ticket in it is still on its way to done.
	for _, c := range []string{ColInbox, ColInDev, ColTracking, ColConflicted} {
		if Terminal(c) {
			t.Errorf("Terminal(%q) = true; work in it is still someone's to do", c)
		}
	}
}

// The board a host creates must not be reorderable by whoever last read it.
func TestColumnsHandsOutACopy(t *testing.T) {
	got := Columns()
	if len(got) == 0 {
		t.Fatal("the board has no columns")
	}
	first := got[0]
	got[0] = Column{Value: "scribbled-on"}
	if Columns()[0] != first {
		t.Error("writing to the slice from Columns() changed the board for the next caller")
	}
}

// Every column on the board should be one some stage actually uses, or the board
// shows a column that is silently never worked.
func TestEveryBoardColumnIsUsedBySomeStage(t *testing.T) {
	used := map[string]bool{}
	for _, st := range New(Options{Architect: true, Coverage: true}).Stages() {
		used[st.Ready], used[st.Working] = true, true
		used[st.Success], used[st.Exhausted] = true, true
		if st.Returns != "" {
			used[st.Returns] = true
		}
	}
	// Tracking holds broken-down parents and no stage takes from it: the parent
	// is not work, its children are.
	used[ColTracking] = true
	for _, c := range Columns() {
		if !used[c.Value] {
			t.Errorf("the board shows %q but no stage routes through it", c.Value)
		}
	}
}

func TestColumnValuesAndLabelsAreUnique(t *testing.T) {
	seenValue, seenLabel := map[string]bool{}, map[string]bool{}
	for _, c := range Columns() {
		if seenValue[c.Value] {
			t.Errorf("two columns share the value %q; the platform keys on it", c.Value)
		}
		if seenLabel[c.Label] {
			t.Errorf("two columns share the label %q; a person could not tell them apart", c.Label)
		}
		seenValue[c.Value], seenLabel[c.Label] = true, true
	}
}

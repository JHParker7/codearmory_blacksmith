package main

import "testing"

// A ticket behind a BLOCKED prerequisite can never become eligible: the gate
// requires done, and blocked is terminal without being done. Observed live — a
// six-ticket chain lost its second and the remaining four sat in ready_for_dev
// for a quarter of an hour looking queued.
func TestDependencyDeadlockedDetectsABlockedPrerequisite(t *testing.T) {
	tk := Ticket{DependsOn: []TicketDependency{
		{TicketID: "a", Title: "store", Status: ColDone},
		{TicketID: "b", Title: "handlers", Status: ColBlocked},
	}}
	dep, dead := dependencyDeadlocked(tk)
	if !dead {
		t.Fatal("a blocked prerequisite must be reported; otherwise the ticket waits forever")
	}
	// The DEPENDENCY comes back, not just a label. Returning the title alone was
	// useless to the caller that has to act on the blocker: it passed that title
	// to GetTicket as an id, failed on every ticket, and left the board deadlocked
	// while logging a warning nobody read.
	if dep.TicketID != "b" {
		t.Errorf("blocker id = %q, want the blocked one", dep.TicketID)
	}
	if blockerName(dep) != "handlers" {
		t.Errorf("blocker name = %q, want the title for a person to read", blockerName(dep))
	}
}

// Conflicted is the RESOLVER's queue, not a dead end — a ticket behind one is
// still on its way to done and must keep waiting.
func TestDependencyDeadlockedIgnoresConflicted(t *testing.T) {
	tk := Ticket{DependsOn: []TicketDependency{{TicketID: "a", Status: ColConflicted}}}
	if _, dead := dependencyDeadlocked(tk); dead {
		t.Error("a conflicted prerequisite was treated as a dead end; the resolver would have finished it")
	}
}

func TestDependencyDeadlockedIgnoresLiveWork(t *testing.T) {
	for _, st := range []string{ColDone, ColReadyForDev, ColInDev, ColInReview, ColIntegrating} {
		tk := Ticket{DependsOn: []TicketDependency{{TicketID: "a", Status: st}}}
		if _, dead := dependencyDeadlocked(tk); dead {
			t.Errorf("status %q was treated as a dead end", st)
		}
	}
}

// Falls back to the id when the blocker has no title, so the message still names
// something a person can look up.
func TestDependencyDeadlockedNamesTheIDWhenTitleIsMissing(t *testing.T) {
	tk := Ticket{DependsOn: []TicketDependency{{TicketID: "abc123", Status: ColBlocked}}}
	dep, dead := dependencyDeadlocked(tk)
	if !dead || blockerName(dep) != "abc123" {
		t.Errorf("got (%q,%v), want the ticket id", blockerName(dep), dead)
	}
}

// ORDER IS THE POINT. The test author must run BEFORE the developer: an author
// that can read the implementation writes tests describing what the code does
// rather than what the ticket asked for, which is the failure this stage exists
// to prevent, merely moved one column later.
func TestTestAuthorRunsBeforeTheDeveloper(t *testing.T) {
	author, ok := stageFor(roleTest)
	if !ok {
		t.Fatal("no routing entry for the test author")
	}
	dev, ok := stageFor(roleDev)
	if !ok {
		t.Fatal("no routing entry for the developer")
	}
	if author.Success != dev.Ready {
		t.Errorf("the test author hands work to %q but the developer takes from %q; the pipeline is broken between them",
			author.Success, dev.Ready)
	}
	// The developer's hand-off depends on whether the coverage stage is running.
	applyCoverageSetting(true)
	defer applyCoverageSetting(false)
	dev, _ = stageFor(roleDev)
	if dev.Success != ColReadyForCoverage {
		t.Errorf("with coverage on, the developer hands work to %q, want %q", dev.Success, ColReadyForCoverage)
	}
	cov, ok := stageFor(roleCoverage)
	if !ok {
		t.Fatal("no routing entry for the coverage author")
	}
	if cov.Ready != dev.Success {
		t.Errorf("the coverage author takes from %q but the developer hands to %q", cov.Ready, dev.Success)
	}
	if cov.Success != ColReadyForReview {
		t.Errorf("the coverage author hands work to %q, want %q", cov.Success, ColReadyForReview)
	}
	// BOTH WORKING STAGES WAIT. The author was exempt on the reasoning that tests
	// come from the ticket rather than from code — true, but a specification
	// NAMES things, and the names belong to the tickets it waits for. See
	// TestTheSpecStageWaitsForItsDependencies for the failure that reversed it.
	if !author.RequiresDependencies {
		t.Error("the test author starts before the code its tests must name exists")
	}
	if !dev.RequiresDependencies {
		t.Error("the developer no longer waits, and would build against code that does not exist")
	}
	// Columns are rendered in board order, so the author's must precede the
	// developer's or the board reads backwards.
	pos := map[string]int{}
	for i, c := range workflowColumns {
		pos[c.Value] = i
	}
	if pos[ColReadyForTests] > pos[ColReadyForDev] {
		t.Error("the board shows the test columns after the developer's; the pipeline reads backwards")
	}
}

// Every column a stage can route to must exist on the board, or a ticket lands
// in a status nothing renders and nothing polls.
func TestEveryRoutedColumnIsOnTheBoard(t *testing.T) {
	known := map[string]bool{}
	for _, c := range workflowColumns {
		known[c.Value] = true
	}
	for role, s := range stages {
		for name, col := range map[string]string{
			"Ready": s.Ready, "Working": s.Working, "Success": s.Success,
			"Exhausted": s.Exhausted, "Returns": s.Returns,
		} {
			if col == "" {
				continue
			}
			if !known[col] {
				t.Errorf("%s.%s routes to %q, which is not a board column", role, name, col)
			}
		}
	}
}

// COVERAGE IS A QUALITY GOAL, NOT A CORRECTNESS GATE. By the time work reaches
// this stage it has already satisfied the specification the developer was held
// to, so falling short of a percentage must not blackhole finished, working
// code — the shortfall is recorded and the work moves on.
//
// It also decides what a weak model in this stage costs: blocking would make one
// that cannot write tests actively harmful, where passing through makes it
// merely unhelpful.
func TestFallingShortOfCoverageDoesNotBlockTheWork(t *testing.T) {
	cov, ok := stageFor(roleCoverage)
	if !ok {
		t.Fatal("no routing entry for the coverage author")
	}
	if cov.Exhausted == ColBlocked {
		t.Error("exhausting the coverage stage blocks the ticket; working code would be lost to a percentage")
	}
	if cov.Exhausted != ColReadyForReview {
		t.Errorf("coverage exhaustion goes to %q, want it carried on to %q", cov.Exhausted, ColReadyForReview)
	}

	// Every OTHER stage must still block when it exhausts. There are exactly two
	// exceptions and they share a reason: their output is ADDITIVE. Coverage adds
	// tests to code that already satisfies its specification; the architect adds
	// documentation to a request that can still be built without it. Carrying on
	// costs those tickets nothing, where blocking would strand finished or
	// perfectly buildable work behind something optional.
	//
	// A stage that GATES must never be added to this list. Everything else here
	// decides whether the change is correct, and a correctness stage that gave up
	// and waved the ticket through would be worse than one that never ran.
	additive := map[string]bool{roleCoverage: true, roleArchitect: true}
	for role, st := range stages {
		if additive[role] {
			continue
		}
		if st.Exhausted != ColBlocked {
			t.Errorf("%s exhausts to %q; only a stage whose output is additive may carry on", role, st.Exhausted)
		}
	}
}

// COVERAGE IS OFF BY DEFAULT and the pipeline must be whole without it. It works,
// but in the wrong place: measured live, the developer pushed in 90 seconds and
// coverage then held the ticket over seven minutes with five siblings idle behind
// it, waiting only for the code to exist. It is parked until it runs after the
// merge, as part of the janitor work.
func TestCoverageIsOptionalAndThePipelineIsWholeWithoutIt(t *testing.T) {
	applyCoverageSetting(false)
	dev, ok := stageFor(roleDev)
	if !ok {
		t.Fatal("no routing entry for the developer")
	}
	if dev.Success != ColReadyForReview {
		t.Errorf("with coverage off the developer hands work to %q, want %q — the ticket would land in a "+
			"column nothing polls", dev.Success, ColReadyForReview)
	}

	// And back on, it routes through coverage again.
	applyCoverageSetting(true)
	dev, _ = stageFor(roleDev)
	if dev.Success != ColReadyForCoverage {
		t.Errorf("with coverage on the developer hands work to %q, want %q", dev.Success, ColReadyForCoverage)
	}
	applyCoverageSetting(false)

	// Whichever way it is set, every stage's destination must be a real column.
	known := map[string]bool{}
	for _, c := range workflowColumns {
		known[c.Value] = true
	}
	for role, st := range stages {
		for _, col := range []string{st.Ready, st.Working, st.Success, st.Exhausted, st.Returns} {
			if col != "" && !known[col] {
				t.Errorf("%s routes to %q, which is not a board column", role, col)
			}
		}
	}
}

// A SPECIFICATION NAMES THINGS, so the stage that writes it must wait for the
// tickets that create them. An API ticket writes tests against Task and Store,
// which the foundation ticket creates; an author that cannot see them invents
// their shape, and the developer — which does wait — then has to satisfy an
// invented interface against the real one already on the integration branch.
//
// The cost is real and deliberate: this was the pipeline's only parallel stage,
// and siblings of a slow foundation now wait rather than being specified at once.
func TestTheSpecStageWaitsForItsDependencies(t *testing.T) {
	spec, ok := stages[roleTest]
	if !ok {
		t.Fatal("no test stage is registered")
	}
	if !spec.RequiresDependencies {
		t.Error("the spec author starts before the code its tests must name exists")
	}
	// The developer still waits too — that was never in question.
	if dev := stages[roleDev]; !dev.RequiresDependencies {
		t.Error("the developer no longer waits for its dependencies")
	}
	// Scoping and design must NOT wait: they are what create the dependencies.
	for _, role := range []string{roleScoping, roleArchitect} {
		if st, ok := stages[role]; ok && st.RequiresDependencies {
			t.Errorf("%s waits for dependencies, but it is what produces them", role)
		}
	}
}

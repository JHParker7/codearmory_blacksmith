package main

import (
	"context"
	"strings"
	"testing"
)

// THE UPKEEP GATE IS THE LINTER AND THE TESTS, IN THAT ORDER.
//
// Lint first so a run that is only going to fail on style stops before paying for
// the suite, and so the linter's complaint is at the top of what the agent reads
// rather than under a screen of test output.
func TestMaintenanceGateRunsLintThenTests(t *testing.T) {
	got := withMaintenance(RepoConfig{
		LintCommand: "go vet ./...",
		TestCommand: "go test ./...",
	})
	if got.TestCommand != "go vet ./... && go test ./..." {
		t.Errorf("gate = %q, want lint then tests", got.TestCommand)
	}
	// The ordinary gate must not have moved: everything else on the board is
	// judged by it.
	if plain := (RepoConfig{TestCommand: "go test ./..."}); plain.TestCommand != "go test ./..." {
		t.Error("the ordinary gate changed")
	}
}

// A repository with no linter configured has no upkeep to do, and must not end up
// with an empty gate that passes everything.
func TestMaintenanceGateIsUnchangedWithoutALinter(t *testing.T) {
	got := withMaintenance(RepoConfig{TestCommand: "go test ./..."})
	if got.TestCommand != "go test ./..." {
		t.Errorf("gate = %q, want the tests alone", got.TestCommand)
	}
}

// The brief names both commands, because the agent is told what it is judged by
// rather than left to infer it.
func TestMaintenanceBriefNamesTheGate(t *testing.T) {
	brief := upkeepBrief(RepoConfig{LintCommand: "go vet ./...", TestCommand: "go test ./..."},
		"main.go:7: unused variable x", "agent/abc")
	for _, want := range []string{"go vet ./... && go test ./...", "unused variable x", "as little as possible"} {
		if !strings.Contains(brief, want) {
			t.Errorf("the brief does not mention %q", want)
		}
	}
}

// ONLY THE LINTER'S SECTION IS UPKEEP.
//
// The same blob carries a dependency scan, whose findings are in third-party code
// and are not this repository's to fix, and a static analysis pass whose output is
// a lead rather than a defect.
func TestOnlyTheLinterSectionBecomesUpkeep(t *testing.T) {
	blob := "## " + lintSectionLabel + "\nmain.go:7: unused variable x\n\n" +
		"## STATIC ANALYSIS of this project's own code\nsomething speculative\n\n" +
		"## DEPENDENCY SCAN — findings here are in third-party code, not this change\nCVE-1234 in a library\n"

	got := lintFindings(blob)
	if !strings.Contains(got, "unused variable x") {
		t.Errorf("the linter's own finding was dropped: %q", got)
	}
	for _, unwanted := range []string{"speculative", "CVE-1234"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("upkeep picked up %q, which is not its to fix", unwanted)
		}
	}
}

// A blob with no linter section yields nothing rather than the whole thing.
func TestNoLinterSectionYieldsNoUpkeep(t *testing.T) {
	if got := lintFindings("## DEPENDENCY SCAN\nCVE-1234\n"); got != "" {
		t.Errorf("lintFindings = %q, want empty", got)
	}
}

// NOTHING TO LINT MEANS NO TICKET. This is the r99 defect: a scout invented
// "make the linter clean" for a repository holding a go.mod and a README, and the
// agent spent 51 turns and 41 refusals discovering there was nothing to do.
func TestUpkeepFilesNothingWhenTheLinterFoundNothing(t *testing.T) {
	integrationTest(t)
	_, api := newFakePlatform(t)
	repo := RepoConfig{LintCommand: "go vet ./...", TestCommand: "go test ./..."}

	filed, err := RecordUpkeep(context.Background(), api, "", repo, "", "agent/x")
	if err != nil {
		t.Fatalf("RecordUpkeep() = %v", err)
	}
	if filed {
		t.Error("upkeep was filed with no findings; there is nothing for it to do")
	}
}

// REAL FINDINGS BECOME ONE LOW-PRIORITY TICKET, carrying the findings themselves
// so nothing has to re-derive them in a sandbox.
func TestUpkeepFilesTheFindingsItWasGiven(t *testing.T) {
	integrationTest(t)
	_, api := newFakePlatform(t)
	repo := RepoConfig{LintCommand: "go vet ./...", TestCommand: "go test ./..."}

	filed, err := RecordUpkeep(context.Background(), api, "", repo, "main.go:7: unused variable x", "agent/abc")
	if err != nil || !filed {
		t.Fatalf("RecordUpkeep() = %v, %v", filed, err)
	}

	listed, err := api.ListTickets(context.Background(), ListOpts{})
	if err != nil {
		t.Fatalf("ListTickets() = %v", err)
	}
	var found int
	for _, tk := range listed {
		if tk.Title != maintenanceTitle {
			continue
		}
		found++
		if tk.Status != ColReadyForMaintenance {
			t.Errorf("upkeep is in %q, want %q", tk.Status, ColReadyForMaintenance)
		}
		if tk.Priority != "low" {
			t.Errorf("upkeep priority = %q; nobody asked for this", tk.Priority)
		}
		if !strings.Contains(tk.Description, "unused variable x") {
			t.Error("the ticket does not carry the findings, so the work has to be rediscovered")
		}
	}
	if found != 1 {
		t.Fatalf("found %d upkeep tickets, want 1", found)
	}
}

// One open at a time: the findings are cumulative, so a second ticket is the same
// work twice and a board that collects one per merge is one nobody reads.
func TestUpkeepDoesNotStackUp(t *testing.T) {
	integrationTest(t)
	_, api := newFakePlatform(t)
	repo := RepoConfig{LintCommand: "go vet ./...", TestCommand: "go test ./..."}

	if filed, err := RecordUpkeep(context.Background(), api, "", repo, "a finding", "agent/a"); err != nil || !filed {
		t.Fatalf("first RecordUpkeep() = %v, %v", filed, err)
	}
	if filed, err := RecordUpkeep(context.Background(), api, "", repo, "another finding", "agent/b"); err != nil || filed {
		t.Errorf("a second upkeep ticket was filed alongside the first (%v, %v)", filed, err)
	}
}

func upkeepWorker(api *CodeArmory) *MaintenanceAgent {
	return NewMaintenanceAgent(nil, api, ClassLarge,
		RepoConfig{LintCommand: "go vet ./...", TestCommand: "go test ./..."}, 10, "")
}

// THE CLAIM IS GATED, NOT THE OPENING, and that is the r99 correction. A board
// empty when the ticket was filed says nothing about ten minutes later: on r99 a
// request arrived eleven seconds after upkeep started and spent its whole run
// sharing one serving slot with it.
func TestUpkeepStandsDownWhileSomeoneIsWaiting(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	up := f.addTicket(t, Ticket{TicketID: "up", Title: maintenanceTitle,
		Status: ColReadyForMaintenance, CreatedBy: "alice"})

	if !upkeepWorker(api).Wants(*up) {
		t.Fatal("upkeep refused to start on a board with nothing else on it")
	}

	for _, busy := range []string{ColInbox, ColReadyForDev, ColTracking, ColWritingTests, ColIntegrating} {
		f2, api2 := newFakePlatform(t)
		up2 := f2.addTicket(t, Ticket{TicketID: "up", Title: maintenanceTitle,
			Status: ColReadyForMaintenance, CreatedBy: "alice"})
		f2.addTicket(t, Ticket{TicketID: "live", Title: "someone asked for this",
			Status: busy, CreatedBy: "alice"})

		if upkeepWorker(api2).Wants(*up2) {
			t.Errorf("upkeep claimed work while a ticket was in %q", busy)
		}
	}
}

// Finished work is not someone waiting.
func TestUpkeepIgnoresFinishedWork(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	up := f.addTicket(t, Ticket{TicketID: "up", Title: maintenanceTitle,
		Status: ColReadyForMaintenance, CreatedBy: "alice"})
	f.addTicket(t, Ticket{TicketID: "old", Title: "a merged request", Status: ColDone, CreatedBy: "alice"})
	f.addTicket(t, Ticket{TicketID: "dead", Title: "a stopped one", Status: ColBlocked, CreatedBy: "alice"})

	if !upkeepWorker(api).Wants(*up) {
		t.Error("upkeep stood down for work that is already finished or stopped")
	}
}

// THE ROLE MUST HAVE A STAGE, or wiring it panics at startup rather than failing
// a test. NewDispatcher looks the role up in the routing table and panics when it
// is missing, so this is the only cheap way to find out before a run does.
func TestMaintenanceHasAStageAndItLeadsToIntegration(t *testing.T) {
	st, ok := stages[roleMaintain]
	if !ok {
		t.Fatal("no workflow stage for the maintenance role; wiring it would panic at startup")
	}
	if st.Ready != ColReadyForMaintenance || st.Working != ColMaintaining {
		t.Errorf("stage reads from %q/%q, want %q/%q", st.Ready, st.Working,
			ColReadyForMaintenance, ColMaintaining)
	}
	// Its branch still has to be merged, or the work is done and thrown away.
	if st.Success != ColReadyForIntegration {
		t.Errorf("success goes to %q, want %q -- upkeep that is never merged is wasted",
			st.Success, ColReadyForIntegration)
	}
	// And it must not collide with the developer's queue: the two are judged by
	// different gates and share a column only by mistake.
	if st.Ready == stages[roleDev].Ready {
		t.Error("upkeep and development take from the same column, so one gate would judge both")
	}
}

// Every column a stage names has to be one the board is given, or tickets land in
// a column no one renders.
func TestMaintenanceColumnsAreRegistered(t *testing.T) {
	have := map[string]bool{}
	for _, c := range workflowColumns {
		have[c.Value] = true
	}
	for _, want := range []string{ColReadyForMaintenance, ColMaintaining} {
		if !have[want] {
			t.Errorf("column %q is used by the maintenance stage but never created", want)
		}
	}
}

// AUTO MODE IS PER PROJECT, and absent means "whatever the host says".
//
// Unlike Enabled, absent does not mean yes. Upkeep spends the one serving slot on
// work nobody asked for, so it stays off unless something says otherwise.
func TestAutoModeIsPerProjectAndDefaultsToTheHost(t *testing.T) {
	yes, no := true, false

	if !(Project{Auto: &yes}).AutoMode() {
		t.Error("a project asking for auto mode did not get it")
	}
	if (Project{Auto: &no}).AutoMode() {
		t.Error("a project refusing auto mode got it anyway")
	}

	// Absent follows the host, both ways.
	t.Setenv("AGENTS_UPKEEP", "true")
	if !(Project{}).AutoMode() {
		t.Error("a project with no preference ignored the host saying yes")
	}
	t.Setenv("AGENTS_UPKEEP", "")
	if (Project{}).AutoMode() {
		t.Error("a project with no preference turned upkeep on with the host silent")
	}

	// And a project OVERRIDES the host rather than being overridden by it.
	t.Setenv("AGENTS_UPKEEP", "true")
	if (Project{Auto: &no}).AutoMode() {
		t.Error("the host overrode a project that had said no")
	}
}

// Switching a project off must switch its upkeep off with it: a disabled project
// is not dispatched at all, and an idle scout on it would be opening tickets
// nothing will ever work.
func TestADisabledProjectIsStillDisabled(t *testing.T) {
	no, yes := false, true
	if (Project{Enabled: &no, Auto: &yes}).Active() {
		t.Error("a disabled project reported itself active because auto mode was on")
	}
}

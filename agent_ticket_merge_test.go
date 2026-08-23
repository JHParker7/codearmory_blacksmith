package main

import (
	"context"
	"strings"
	"testing"
)

// THE TICKETS ARE CREATED FIRST AND MERGED SECOND.
//
// Collapsing the plan before anything was written was tried and produced a board
// carrying one generic ticket, which loses what the board is for: what was
// planned, what is done, and what each piece cost. Every unit of work the product
// manager finds is now a real ticket, and this stage folds them into one
// afterwards, leaving the originals marked with where their work went.
//
// It calls no model. Merging is concatenation, and a model asked to concatenate
// can paraphrase, reorder or drop — and a dropped requirement is the one failure
// nothing downstream can catch, measured on r80.

func mergeFixture(t *testing.T, f *fakePlatform) (string, []string) {
	t.Helper()
	root := "req00000"
	f.addTicket(t, Ticket{TicketID: root, Title: "the request", Status: ColTracking, CreatedBy: "alice"})
	ids := []string{"task0001", "task0002", "task0003"}
	for i, id := range ids {
		parent := root
		f.addTicket(t, Ticket{
			TicketID: id, ParentID: &parent, Status: ColReadyForTicketMerge, CreatedBy: "alice",
			Title:       []string{"Store", "API", "Board"}[i],
			Description: []string{"store criteria here", "api criteria here", "board criteria here"}[i],
		})
	}
	return root, ids
}

func TestMergingCarriesEveryTicketsText(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	_, ids := mergeFixture(t, f)

	_, _, err := NewTicketMergeAgent(api).Handle(context.Background(), f.get(ids[0]))
	if err != nil {
		t.Fatalf("Handle() = %v", err)
	}

	merged := f.get(ids[0]).Description
	// Every original's body has to survive whole. A summary is where a
	// requirement goes missing, and there is no reader downstream who could
	// notice it had.
	for _, want := range []string{"store criteria here", "api criteria here", "board criteria here"} {
		if !strings.Contains(merged, want) {
			t.Errorf("the merged brief lost %q", want)
		}
	}
	for _, want := range []string{"Store", "API", "Board"} {
		if !strings.Contains(merged, want) {
			t.Errorf("the merged brief does not name %q", want)
		}
	}
}

// THE ORIGINALS STAY ON THE BOARD, marked with where their work went. A ticket
// that simply vanished would read as work nobody did.
func TestMergedTicketsAreClosedAndSayWhereTheyWent(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	_, ids := mergeFixture(t, f)

	if _, _, err := NewTicketMergeAgent(api).Handle(context.Background(), f.get(ids[0])); err != nil {
		t.Fatalf("Handle() = %v", err)
	}

	for _, id := range ids[1:] {
		if got := f.get(id).Status; got != ColDone {
			t.Errorf("%s is %q after being merged, want %q", id, got, ColDone)
		}
		var explained bool
		for _, c := range f.get(id).Comments {
			if strings.Contains(c.Body, mergedIntoMarker) {
				explained = true
			}
		}
		if !explained {
			t.Errorf("%s was closed with nothing saying where its work went", id)
		}
	}
	// The absorbing ticket keeps going, and says what it now carries.
	if got := f.get(ids[0]).Status; got == ColDone {
		t.Error("the ticket that absorbed the others was closed too")
	}
}

// A request scoped as one unit of work has nothing to merge with, and must pass
// through rather than stall.
func TestALoneTaskPassesThrough(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	root := "req00000"
	f.addTicket(t, Ticket{TicketID: root, Status: ColTracking, CreatedBy: "alice"})
	parent := root
	f.addTicket(t, Ticket{TicketID: "task0001", ParentID: &parent, Status: ColReadyForTicketMerge, CreatedBy: "alice"})

	status, _, err := NewTicketMergeAgent(api).Handle(context.Background(), f.get("task0001"))
	if err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	if status != OutcomeSuccess {
		t.Errorf("status = %q; a lone task must pass through rather than stall the board", status)
	}
}

// Only siblings waiting to be merged are taken. One already past this stage is
// being worked, and absorbing it would delete work in flight.
func TestATicketPastThisStageIsNotAbsorbed(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	root, ids := mergeFixture(t, f)
	_ = root
	f.addTicket(t, Ticket{TicketID: "task0009", Status: ColInDev, CreatedBy: "alice"})

	if _, _, err := NewTicketMergeAgent(api).Handle(context.Background(), f.get(ids[0])); err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	if got := f.get("task0009").Status; got != ColInDev {
		t.Errorf("a ticket in development was absorbed (now %q)", got)
	}
}

// The brief must read in the order the work was planned, or the specification
// author meets the API before the type it returns.
func TestTheMergedBriefKeepsPlanningOrder(t *testing.T) {
	brief := mergedBrief([]Ticket{
		{Title: "Store", Description: "first"},
		{Title: "API", Description: "second"},
	})
	if strings.Index(brief, "first") > strings.Index(brief, "second") {
		t.Error("the merged brief is out of planning order")
	}
	if !strings.Contains(brief, "1 of 2") || !strings.Contains(brief, "2 of 2") {
		t.Error("the brief does not number the pieces, so nothing says how many there are")
	}
}

// THE ABSORBED TICKETS' SECTIONS GO WITH THEM. A section left queued against a
// ticket that no longer exists is written anyway, by an author briefed on a
// fraction of the work, onto a branch nobody develops.
//
// Measured on r88: four sections sat in ready_for_spec against absorbed tasks,
// and one of them had already started.
func TestAnAbsorbedTicketsSectionIsClosedToo(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	_, ids := mergeFixture(t, f)
	// A section under one of the tickets about to be absorbed.
	victim := ids[1]
	f.addTicket(t, Ticket{TicketID: "sec00001", ParentID: &victim, Status: ColReadyForSpec, CreatedBy: "alice"})

	if _, _, err := NewTicketMergeAgent(api).Handle(context.Background(), f.get(ids[0])); err != nil {
		t.Fatalf("Handle() = %v", err)
	}

	if got := f.get("sec00001").Status; got != ColDone {
		t.Errorf("the section of an absorbed ticket is %q; it will be written for a ticket that no longer exists", got)
	}
}

// ONE SECTION PER UNIT OF WORK, NOT ONE COVERING ALL OF THEM.
//
// An author is finished when its gate passes, and its gate passes as soon as its
// first file compiles and fails correctly. Give it five units and it stops after
// one — measured on r89, which delivered a fifth of the system and reported
// success. Letting it end its own stage instead was worse: on r90 it ran 49 turns
// and wrote the same file 41 times.
//
// So the brief is what gets sized. Several focused authors share this ticket's
// branch and produce one specification between them, and one developer implements
// it — whose own gate is honest, because green there means every test passes.
func TestEachUnitOfWorkGetsItsOwnSection(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	_, ids := mergeFixture(t, f)

	if _, _, err := NewTicketMergeAgent(api).Handle(context.Background(), f.get(ids[0])); err != nil {
		t.Fatalf("Handle() = %v", err)
	}

	listed, err := api.ListTickets(context.Background(), ListOpts{})
	if err != nil {
		t.Fatalf("ListTickets() = %v", err)
	}
	var sections []Ticket
	for _, tk := range listed {
		if tk.ParentID != nil && *tk.ParentID == ids[0] && tk.Status == ColReadyForSpec {
			sections = append(sections, f.get(tk.TicketID))
		}
	}
	if len(sections) != 3 {
		t.Fatalf("three units of work produced %d sections, want one each", len(sections))
	}

	// Each carries its OWN requirements and not the others', so its gate passes
	// exactly when its job is done.
	joined := ""
	for _, sec := range sections {
		joined += sec.Description
		hits := 0
		for _, criterion := range []string{"store criteria here", "api criteria here", "board criteria here"} {
			if strings.Contains(sec.Description, criterion) {
				hits++
			}
		}
		if hits != 1 {
			t.Errorf("a section carries %d of the three units' criteria, want exactly its own", hits)
		}
	}
	// And between them they still cover everything.
	for _, criterion := range []string{"store criteria here", "api criteria here", "board criteria here"} {
		if !strings.Contains(joined, criterion) {
			t.Errorf("no section covers %q; that requirement has no tests", criterion)
		}
	}
}

// THEY SHARE ONE BRANCH, so each author has to be told the others write to it
// too — the collision this warning prevents cost a whole task when two sections
// declared the same test name.
//
// It no longer says they write AT THE SAME TIME, because since the sections were
// chained they do not. A brief that describes a hazard the author cannot observe
// is a brief it learns to discount.
func TestEachSectionIsWarnedAboutItsSiblings(t *testing.T) {
	brief := unitSpecBrief(Ticket{Title: "Store", Description: "criteria"}, 2, 3)
	for _, want := range []string{"slice 2 of 3", "same branch", "touch no other test file"} {
		if !strings.Contains(brief, want) {
			t.Errorf("the section brief does not say %q", want)
		}
	}
	if strings.Contains(brief, "AT THE SAME TIME") {
		t.Error("the brief still warns about concurrent authors; the sections are chained and are not")
	}
}

// AND IT NAMES THE FILE, taken from the source file the plan assigned.
//
// Measured on r92: without this the store author appended its tests to the
// ticket author's file, spent three turns failing to quote a function back
// exactly, and the delivered repository had no store_test.go at all.
func TestTheSectionBriefNamesItsOwnTestFile(t *testing.T) {
	brief := unitSpecBrief(Ticket{
		Title:       "Implement the in-memory store",
		Description: "criteria here\n\n" + taskFileIntro + "store.go`, creating it if it does not exist.",
	}, 2, 3)
	if !strings.Contains(brief, "`store_test.go`") {
		t.Errorf("the brief does not name store_test.go, so the author picks a file:\n%s", brief)
	}
}

// A plan that named no file still has to produce a usable brief.
func TestTheSectionBriefFallsBackWhenNoFileWasAssigned(t *testing.T) {
	brief := unitSpecBrief(Ticket{Title: "Something", Description: "criteria only"}, 1, 1)
	if !strings.Contains(brief, "`spec_test.go`") {
		t.Errorf("the brief names no test file at all:\n%s", brief)
	}
}

// AND THEY ARE CHAINED, each waiting on the one before it.
//
// The sections share this ticket's branch, and a sandbox begins by resetting hard
// to that branch — so authors writing at once wipe each other's work and cannot
// tell that they have. Measured on r91, which is this stage without the chain:
// five sections claimed simultaneously, and four spent the run alternating
// "your edits changed nothing" against a gate reporting "no test files were
// written", because each one's file existed only in the tree that had just lost
// the race.
func TestSectionsWaitForTheOneBeforeThem(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	_, ids := mergeFixture(t, f)

	if _, _, err := NewTicketMergeAgent(api).Handle(context.Background(), f.get(ids[0])); err != nil {
		t.Fatalf("Handle() = %v", err)
	}

	listed, err := api.ListTickets(context.Background(), ListOpts{})
	if err != nil {
		t.Fatalf("ListTickets() = %v", err)
	}
	// In creation order, which is the order the units were merged in.
	var sections []Ticket
	for _, want := range []string{"Store", "API", "Board"} {
		for _, tk := range listed {
			if tk.ParentID != nil && *tk.ParentID == ids[0] && tk.Title == "Specification for "+want {
				full, err := api.GetTicket(context.Background(), tk.TicketID)
				if err != nil {
					t.Fatalf("GetTicket(%s) = %v", tk.TicketID, err)
				}
				sections = append(sections, full)
			}
		}
	}
	if len(sections) != 3 {
		t.Fatalf("found %d sections, want 3", len(sections))
	}

	for i, sec := range sections {
		var waits []string
		for _, d := range sec.DependsOn {
			waits = append(waits, d.TicketID)
		}
		if i == 0 {
			if len(waits) != 0 {
				t.Errorf("the first section waits for %v; nothing precedes it", waits)
			}
			continue
		}
		if !contains(waits, sections[i-1].TicketID) {
			t.Errorf("section %d waits for %v, not for section %d (%s); they will race on the branch and lose each other's tests",
				i+1, waits, i, sections[i-1].TicketID)
		}
	}
}

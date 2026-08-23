package main

import "testing"

// A DEVELOPER MUST LAND ON THE BRANCH ITS SPECIFICATION WAS WRITTEN TO, and for
// a task that is the task's own branch — the one its sections were told to
// assemble onto.
//
// The parent redirect used to apply to development as well as to specification.
// A task also has a parent (the request the product manager broke down), so
// every developer was sent to agent/<request>, where nothing had been written.
//
// Measured on r73: eight sections wrote their tests to four task branches, all
// four developers were pointed at agent/<request> and found an empty tree, the
// security stage reported "empty diff", and the integrator merged it. The board
// reported success in 10.3 minutes having delivered a single struct, with the
// whole specification stranded on four branches nobody merged.
func TestADeveloperWorksItsTaskBranchNotTheRequestBranch(t *testing.T) {
	repo := RepoConfig{BranchPrefix: "agent/"}
	request := "req00000"
	task := Ticket{TicketID: "task0000", ParentID: &request}

	dev := NewDevAgent(nil, nil, ClassLarge, repo, 10)
	got := dev.branchFor(task)

	if want := "agent/" + shortID(task.TicketID); got != want {
		t.Errorf("the developer works %q, want %q — the request's branch holds none of the task's tests", got, want)
	}
}

// THE TWO HALVES HAVE TO MEET. A section writes to its task's branch and the
// developer of that task reads it; if those two names differ, the specification
// is written and then silently discarded, which is exactly what r73 did.
func TestASectionWritesWhereItsTaskDeveloperReads(t *testing.T) {
	repo := RepoConfig{BranchPrefix: "agent/"}
	request := "req00000"
	task := Ticket{TicketID: "task0000", ParentID: &request}
	taskID := task.TicketID
	section := Ticket{TicketID: "sec00000", ParentID: &taskID}

	author := NewSpecAgent(nil, nil, ClassLarge, repo, 10)
	dev := NewDevAgent(nil, nil, ClassLarge, repo, 10)

	wrote := author.branchFor(section)
	reads := dev.branchFor(task)

	if wrote != reads {
		t.Errorf("the section wrote to %q and its developer reads %q; the specification never reaches the code", wrote, reads)
	}
}

// Two tasks of one request must not share a branch, or their developers
// overwrite each other. On r73 all four landed on agent/<request> together.
func TestTwoTasksOfOneRequestDoNotShareABranch(t *testing.T) {
	repo := RepoConfig{BranchPrefix: "agent/"}
	request := "req00000"
	first := Ticket{TicketID: "task0001", ParentID: &request}
	second := Ticket{TicketID: "task0002", ParentID: &request}

	dev := NewDevAgent(nil, nil, ClassLarge, repo, 10)
	if a, b := dev.branchFor(first), dev.branchFor(second); a == b {
		t.Errorf("both tasks develop on %q; their developers overwrite one another", a)
	}
}

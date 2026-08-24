package main

import (
	"strings"
	"testing"
)

// THE PROMPT IS ORDERED BY WHAT CHANGES, SO THE BACKEND CAN REUSE WHAT DOES NOT.
//
// 93% of every token this pipeline moves is prompt rather than answer — measured
// on r75, the developer read 387,419 tokens to write 24,557 — so a turn costs
// mostly the price of re-reading its own context.
//
// The serving backend caches a prompt PREFIX, and the effect is not marginal.
// Measured against this deployment at 25,791 prompt tokens: 30.1s cold, 0.5s for
// the identical prompt again, 1.3s for the same prefix with a different question
// appended — and 31.3s, no saving whatever, when a few hundred characters change
// in FRONT of the same block. That last case was the shape this prompt had.

func worldFixture() (Ticket, *devState) {
	t := Ticket{TicketID: "t1000000", Title: "Add a store", Priority: "high", Description: "Build a store."}
	s := &devState{
		iteration: 3,
		budget:    40,
		tree:      []string{"store.go", "store_test.go"},
		read:      map[string]string{"store.go": "package tracker\n"},
		trail:     []devStep{{action: "read_files", detail: "store.go"}},
	}
	return t, s
}

// THE PROPERTY THAT MATTERS: two turns that differ only in the agent's own
// progress must produce a byte-identical stable half. If they don't, the cache
// misses and the whole tree is re-processed.
func TestTheWorldHalfIsUnchangedByProgress(t *testing.T) {
	tk, s := worldFixture()
	first := renderDevWorld(tk, s)

	// A turn later: further along the budget, another action tried, a rejection
	// outstanding, a verification result in hand. The repository is untouched.
	s.iteration = 4
	s.trail = append(s.trail, devStep{action: "write_files", detail: "store.go"})
	s.notice = "Rejected: your edits changed nothing."
	s.lastTest = "--- FAIL: TestCreate"

	if second := renderDevWorld(tk, s); second != first {
		t.Error("the stable half changed although no file was read or written; the prefix cache will miss every turn")
	}
}

// Reading a file legitimately changes the world, and must.
func TestTheWorldHalfDoesChangeWhenTheRepositoryDoes(t *testing.T) {
	tk, s := worldFixture()
	first := renderDevWorld(tk, s)

	s.read["store_test.go"] = "package tracker\n"

	if renderDevWorld(tk, s) == first {
		t.Error("a newly read file did not reach the prompt")
	}
}

// The volatile content must be OUT of the stable half, or it is not stable.
func TestVolatileContentIsNotInTheWorldHalf(t *testing.T) {
	tk, s := worldFixture()
	s.notice = "Rejected: your edits changed nothing."
	world := renderDevWorld(tk, s)

	for _, leaked := range []string{"ITERATION", "ACTIONS YOU HAVE ALREADY TAKEN", "NOT ACCEPTED"} {
		if strings.Contains(world, leaked) {
			t.Errorf("%q is in the stable half; it changes every turn and defeats the cache", leaked)
		}
	}
	if !strings.Contains(world, "REPOSITORY FILES") {
		t.Error("the repository listing is not in the stable half, which is the block worth caching")
	}
}

// THE MODEL READS THE END MOST CLOSELY, so the rejection and the call to act must
// still be the last things on the page.
func TestTheProgressHalfEndsWithTheRejectionAndTheCue(t *testing.T) {
	_, s := worldFixture()
	s.notice = "Rejected: your edits changed nothing."
	progress := renderDevProgress(s)

	for _, want := range []string{"ITERATION", "ACTIONS YOU HAVE ALREADY TAKEN", "NOT ACCEPTED"} {
		if !strings.Contains(progress, want) {
			t.Errorf("%q is missing from the volatile half", want)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(progress), "Reply with one JSON action.") {
		t.Error("the call to act is no longer the last thing the model reads")
	}
}

// Nothing may be lost in the split: the two halves together are the whole state.
func TestTheTwoHalvesStillCarryEverything(t *testing.T) {
	tk, s := worldFixture()
	s.notice = "Rejected: your edits changed nothing."
	whole := renderDevState(tk, s)

	for _, want := range []string{"t1000000", "Add a store", "REPOSITORY FILES", "store.go",
		"ITERATION", "ACTIONS YOU HAVE ALREADY TAKEN", "NOT ACCEPTED", "Reply with one JSON action."} {
		if !strings.Contains(whole, want) {
			t.Errorf("the split lost %q from the rendered state", want)
		}
	}
}

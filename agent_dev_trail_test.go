package main

import (
	"strings"
	"testing"
)

// THE TRAIL MUST CARRY THE EVIDENCE, NOT THE LECTURE.
//
// A refusal is an explanation followed by the thing that went wrong. Clipping it
// to a fixed head kept exactly the wrong half: the no-op notice's preamble alone
// runs ~370 characters, so a 400-rune clip ended mid-sentence at "The failure
// was:" and dropped every line of the failure.
//
// Measured on r74: the developer on the API task was told seven times that its
// edit changed nothing and that the checks had failed, with the reason truncated
// to "--- t". Unable to see which test broke, it re-sent the same 240-byte edit
// every turn, and would have spent all 200 iterations doing it — the stuck
// ceiling does not fire while the tests are red.

// noticeLikeR74 is the shape the developer was actually handed.
func noticeLikeR74() string {
	// lastTest carries the gate's own preamble before the failure, which is what
	// pushed the useful lines past a 400-rune head clip on r74.
	return noopEditNotice(1, true, false,
		"The test gate failed on branch agent/29aff2a7 (exit 1). Fix this before anything else:\n"+
			"--- FAIL: TestListFiltersByStatus\n    handlers_read_test.go:41: got 500, want 400\n"+
			"--- FAIL: TestUnknownIDIs404\n    handlers_read_test.go:88: got 200, want 404\nFAIL")
}

func TestTheTrailKeepsTheFailureNotOnlyTheRefusal(t *testing.T) {
	var s devState
	s.remember("write_files: handlers.go lines 89-98", "RESULT: "+clipEnds(noticeLikeR74(), 200, 700))

	entry := s.history[0]
	for _, want := range []string{"TestListFiltersByStatus", "want 400"} {
		if !strings.Contains(entry, want) {
			t.Errorf("the trail dropped %q; the agent cannot see which test broke", want)
		}
	}

	// The budget that shipped, pinned so the regression cannot come back by
	// someone "simplifying" this to a plain clip: the preamble alone outruns it.
	if old := clip(noticeLikeR74(), 400); strings.Contains(old, "TestListFiltersByStatus") {
		t.Error("a 400-rune head clip now keeps the failure; this test no longer guards anything")
	}
}

// AN IDENTICAL STEP REPEATED IS ONE FACT, NOT SEVERAL. Writing it out several
// times spends the twenty-step window on one mistake — evicting the reads and
// edits that explain how the agent got there — and spends prompt on redundancy:
// r74's looping developer reached 12,565 prompt tokens to emit a 200-token
// repeat, most of it seven copies of one refusal.
func TestARepeatedStepCollapsesToOneEntry(t *testing.T) {
	var s devState
	for range 7 {
		s.remember("write_files: handlers.go lines 89-98", "RESULT: rejected, nothing changed")
	}

	if len(s.history) != 1 {
		t.Fatalf("seven identical steps produced %d trail entries, want 1", len(s.history))
	}
	if !strings.Contains(s.history[0], "7 times in a row") {
		t.Errorf("the collapsed entry does not say how many times: %q", s.history[0])
	}
}

// Collapsing must not swallow real progress: two different steps stay two.
func TestDifferentStepsAreNotCollapsed(t *testing.T) {
	var s devState
	s.remember("read_files: store.go", "RESULT: accepted.")
	s.remember("write_files: handlers.go", "RESULT: the change landed and the checks failed.")
	s.remember("read_files: store.go", "RESULT: accepted.")

	if len(s.history) != 3 {
		t.Fatalf("three distinct steps produced %d entries, want 3", len(s.history))
	}
}

// A step that repeats, then changes, then repeats again must count the second
// run from one — otherwise the tally describes a loop that is not happening.
func TestTheRepeatCountResetsAfterADifferentStep(t *testing.T) {
	var s devState
	s.remember("write_files: a.go", "RESULT: rejected")
	s.remember("write_files: a.go", "RESULT: rejected")
	s.remember("read_files: b.go", "RESULT: accepted.")
	s.remember("write_files: a.go", "RESULT: rejected")

	last := s.history[len(s.history)-1]
	if strings.Contains(last, "times in a row") {
		t.Errorf("a fresh step inherited an earlier repeat count: %q", last)
	}
}

package dev

import (
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/ticket"
)

func job() ticket.Ticket {
	return ticket.Ticket{
		ID: "t-1", Title: "Add filtering to the store", Priority: "high",
		Description: "List(Filter{Done:true}) must return only completed tasks.",
	}
}

func state() *State {
	return &State{
		Tree:      []string{"store.go", "store_test.go"},
		Read:      map[string]string{"store.go": "package main\n\nfunc List() {}\n"},
		Iteration: 3,
		Budget:    10,
	}
}

// THE SPLIT IS ABOUT PREFILL, NOT TIDINESS. 93% of every token this pipeline
// moves is prompt rather than answer — on r75 the developer read 387,419 tokens
// to write 24,557 — and a backend caches a prompt PREFIX. Measured at 25,791
// prompt tokens: 30.1s cold, 0.5s identical, 1.3s for the same prefix with a
// different question appended, and 31.3s — no saving whatever — when a few
// hundred characters change in FRONT of the same block.
//
// So nothing that changes per turn may appear before the repository contents.
func TestNothingVolatileSitsAboveTheCacheableBlock(t *testing.T) {
	s := state()
	s.Trail = []Step{{Action: ActionReadFiles, Detail: "store.go"}}
	s.Notice = "that edit matched nothing"
	s.LastTest = "FAIL store_test.go"

	world := RenderWorld(job(), Reasons{}, s)

	for _, volatile := range []string{"ITERATION", "ACTIONS YOU HAVE ALREADY TAKEN",
		"LAST VERIFICATION", "NOT ACCEPTED"} {
		if strings.Contains(world, volatile) {
			t.Errorf("%q appears in the cacheable half; the prefix changes every turn", volatile)
		}
	}
	// And the stable half must still carry the expensive part.
	if !strings.Contains(world, "REPOSITORY FILES") || !strings.Contains(world, "func List()") {
		t.Error("the cacheable half does not carry the repository")
	}

	// The whole turn is the two halves in that order, and nothing else.
	if got := Render(job(), Reasons{}, s); got != world+RenderProgress(s) {
		t.Error("the rendered turn is not its two halves in order")
	}
}

// SHOW THE BUDGET, NOT JUST THE COUNTER. Without a denominator the model has no
// way to know turns are scarce, and what that produced was repeatable: read one
// file, read another, until the attempt ended having never verified anything.
func TestTheAgentIsToldHowManyTurnsRemain(t *testing.T) {
	s := state()
	got := RenderProgress(s)
	if !strings.Contains(got, "ITERATION 3 OF 10") {
		t.Errorf("the budget is not shown:\n%s", got)
	}
	if !strings.Contains(got, "7 action(s) left") {
		t.Errorf("the remainder is not named:\n%s", got)
	}
	if !strings.Contains(got, "abandoned") {
		t.Error("the consequence of running out is not stated")
	}

	// AN OVERRUN READS AS ZERO, not as a negative number. A prompt saying
	// "-3 actions left" is a prompt the model has to interpret.
	s.Iteration = 14
	if !strings.Contains(RenderProgress(s), "0 action(s) left") {
		t.Errorf("an overrun budget renders wrongly:\n%s", RenderProgress(s))
	}

	// ZERO MEANS UNKNOWN, and the denominator is left off rather than printed as
	// "OF 0", which would tell the model it has no turns at all.
	s.Budget, s.Iteration = 0, 3
	got = RenderProgress(s)
	if !strings.Contains(got, "ITERATION 3\n") || strings.Contains(got, "OF 0") {
		t.Errorf("an unknown budget renders wrongly:\n%s", got)
	}
}

// THE VERIFICATION RESULT AND THE REJECTION ARE SEPARATE FIELDS. They shared one
// once, and every rejection overwrote the test result the model needed in order
// to justify finishing: the first refusal erased the evidence that the code
// passed.
func TestARejectionDoesNotEraseThePassingTestOutput(t *testing.T) {
	s := state()
	s.LastTest = "ok  store 0.2s"
	s.TestsPass = true
	s.Notice = "old_str matched 3 places; widen the quote"

	got := RenderProgress(s)
	if !strings.Contains(got, "ok  store 0.2s") {
		t.Error("the passing verification was lost behind the rejection")
	}
	if !strings.Contains(got, "PASSED") {
		t.Error("the verification is not labelled as having passed")
	}
	if !strings.Contains(got, "widen the quote") {
		t.Error("the rejection is not shown")
	}

	s.TestsPass = false
	if !strings.Contains(RenderProgress(s), "FAILED") {
		t.Error("a red verification is not labelled as failed")
	}
}

// THE RESTART BRIEF GOES LAST AND ON ITS OWN. It is the most important thing on
// the page for the agent reading it, and it is NOT a rejection — the agent being
// briefed did not take the action that came before.
func TestTheRestartBriefIsNotPresentedAsARejection(t *testing.T) {
	s := state()
	s.Restart = "YOUR MEMORY WAS CLEARED. The work on the branch is kept."
	s.Notice = "that edit matched nothing"

	got := RenderProgress(s)
	brief := strings.Index(got, "YOUR MEMORY WAS CLEARED")
	rejected := strings.Index(got, "NOT ACCEPTED")
	if brief == -1 {
		t.Fatal("the restart brief is missing")
	}
	if rejected != -1 && brief > rejected {
		t.Error("the restart brief is rendered under the rejection heading")
	}
}

// WHY IT CAME BACK IS THE WORK NOW, so it is framed as the task and sits above
// the repository rather than as context beneath it. Without it the send-back
// cannot converge: the prompt is rebuilt from the ticket every attempt, so a
// returned ticket arrives looking exactly like a fresh one.
func TestARoundTripCarriesItsReasonAndSaysNotToStartOver(t *testing.T) {
	t.Run("the reviewer sent it back", func(t *testing.T) {
		got := RenderWorld(job(), Reasons{Returned: "the filter is case-sensitive"}, state())
		if !strings.Contains(got, "the filter is case-sensitive") {
			t.Error("the finding is not carried")
		}
		if !strings.Contains(got, "do not start over") {
			t.Error("the agent is not told to correct rather than rewrite")
		}
		if strings.Index(got, "SENT BACK BY THE REVIEWER") > strings.Index(got, "REPOSITORY FILES") {
			t.Error("the reason is below the repository, where it reads as context")
		}
	})

	// r68: the author was handed back twice and both times opened with "I'll
	// write the unit tests for…" — it had no idea it was a repair, rewrote all
	// 215 lines from scratch and reproduced the identical bug.
	t.Run("the developer sent the specification back", func(t *testing.T) {
		got := RenderWorld(job(), Reasons{SpecRepair: "the fixture panics on an unescaped space"}, state())
		if !strings.Contains(got, "unescaped space") {
			t.Error("the fault is not carried")
		}
		for _, want := range []string{
			"do not rewrite the file\nfrom scratch",
			"do not weaken an assertion",
			"the developer may not edit",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("the author is not told %q", want)
			}
		}
	})

	// A fresh ticket carries neither, and says nothing about being sent back.
	t.Run("a fresh ticket", func(t *testing.T) {
		got := RenderWorld(job(), Reasons{}, state())
		if strings.Contains(got, "SENT BACK") || strings.Contains(got, "SENT THIS SPECIFICATION BACK") {
			t.Error("a fresh ticket was described as a repair")
		}
	})

	// Whitespace is not a reason. A comment containing only a newline would
	// otherwise print the heading with nothing under it.
	t.Run("a blank reason is no reason", func(t *testing.T) {
		got := RenderWorld(job(), Reasons{Returned: "  \n "}, state())
		if strings.Contains(got, "SENT BACK") {
			t.Error("a blank comment produced a send-back heading")
		}
	})
}

// A STAGED FILE READ BACK RETURNS THE AGENT'S OWN WRITES, and costs one of very
// few turns to learn nothing.
func TestTheAgentIsToldItsStagedFilesAreAlreadyShown(t *testing.T) {
	s := state()
	s.Staged = map[string]string{"store.go": "package main\n"}

	got := RenderWorld(job(), Reasons{}, s)
	if !strings.Contains(got, "reading them again returns your own writes") {
		t.Errorf("the agent is not told a re-read teaches it nothing:\n%s", got)
	}
	// Nothing about staging appears when nothing is staged.
	if strings.Contains(RenderWorld(job(), Reasons{}, state()), "ALREADY CHANGED") {
		t.Error("an unstaged attempt was told about staged files")
	}
}

// THE FILE CONTENTS ARE NUMBERED, which is what makes the line-addressed edit
// shape usable at all: the model quotes a number it can see rather than counting
// to one it cannot.
func TestReadFilesAreShownWithLineNumbers(t *testing.T) {
	got := NumberLines("package main\n\nfunc List() {}\n")
	want := "1\tpackage main\n2\t\n3\tfunc List() {}"
	if got != want {
		t.Errorf("NumberLines =\n%q\nwant\n%q", got, want)
	}
	// An empty file is one blank line, not a panic and not nothing.
	if got := NumberLines(""); got != "1\t" {
		t.Errorf("NumberLines(\"\") = %q", got)
	}
	// A file with no trailing newline keeps its last line.
	if got := NumberLines("a\nb"); got != "1\ta\n2\tb" {
		t.Errorf("NumberLines(\"a\\nb\") = %q", got)
	}
	if !strings.Contains(RenderWorld(job(), Reasons{}, state()), "1\tpackage main") {
		t.Error("read contents reach the prompt unnumbered")
	}
}

// FILES ARE RENDERED IN A STABLE ORDER. Map iteration order would change the
// prompt from turn to turn with no change in content — which, given the prefix
// cache above, costs the whole saving.
func TestTheReadFilesRenderInAStableOrder(t *testing.T) {
	s := state()
	s.Read = map[string]string{"z.go": "package z\n", "a.go": "package a\n", "m.go": "package m\n"}
	first := RenderWorld(job(), Reasons{}, s)
	for range 20 {
		if RenderWorld(job(), Reasons{}, s) != first {
			t.Fatal("the same state rendered two different prompts")
		}
	}
	if strings.Index(first, "--- a.go ---") > strings.Index(first, "--- z.go ---") {
		t.Error("the files are not in a sorted order")
	}
}

// A TICKET DESCRIPTION IS ATTACKER-INFLUENCED TEXT IN THE GENERAL CASE and
// context is scarce either way.
func TestTheTicketIsBoundedInThePrompt(t *testing.T) {
	long := job()
	long.Description = strings.Repeat("x", 20000)
	got := RenderWorld(long, Reasons{Returned: strings.Repeat("y", 20000)}, state())
	if len([]rune(got)) > MaxTicketRunes+MaxReasonRunes+2000 {
		t.Errorf("the prompt quotes %d runes of a 40,000-rune input", len([]rune(got)))
	}
}

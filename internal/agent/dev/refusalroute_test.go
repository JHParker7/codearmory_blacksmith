package dev

import (
	"strings"
	"testing"
)

// A DEVELOPER THAT NEVER LANDS A WRITE MUST STILL BE ABLE TO HAND BACK.
//
// The ordinary route needs a failing verification naming test files, and a
// refused edit produces none: it does not change the tree, and a verification
// only follows a change.
//
// Measured on run 81. The specification called t.Error with a format directive,
// which go vet rejects. The developer diagnosed it correctly and repeatedly —
// "fix vet error: remove unused %q directive" — and tried to edit store_test.go
// 21 times. Every attempt was refused, so no verification ever ran: 99 turns
// across three attempts, ZERO verifications, and each attempt died on the
// refusal ceiling reporting "made 20 actions in a row that changed nothing".
// The machinery to hand this back existed and was gated on evidence the
// developer could not generate.
func TestRefusalsAloneReachTheHandBackWhenNothingEverVerified(t *testing.T) {
	s := &State{} // no verification has run: LastTest is empty

	for range MaxTestEditRefusals {
		s.NoteTestEditRefusal(ModeDevelop, "store_test.go")
	}

	if s.SpecBroken == "" {
		t.Fatal("a developer refused its whole allowance on one test file, with no " +
			"verification to quote, still could not hand back")
	}
	if !strings.Contains(s.SpecBroken, "store_test.go") {
		t.Errorf("the hand-back does not name the file: %q", s.SpecBroken)
	}
	if !strings.Contains(s.FaultOrDefault(), "may not edit a test file") {
		t.Errorf("the fault does not say why the developer is stuck: %q", s.FaultOrDefault())
	}
}

// AND EVERY FILE IT WAS REFUSED ON IS NAMED, because the author has to know
// where to look and the developer touched more than one.
func TestEveryRefusedTestFileIsNamed(t *testing.T) {
	s := &State{}
	for range MaxTestEditRefusals {
		s.NoteTestEditRefusal(ModeDevelop, "store_test.go")
		s.NoteTestEditRefusal(ModeDevelop, "board_test.go")
	}

	for _, want := range []string{"store_test.go", "board_test.go"} {
		if !strings.Contains(s.SpecBroken, want) {
			t.Errorf("%q is missing from the hand-back: %q", want, s.SpecBroken)
		}
	}
	if strings.Count(s.SpecBroken, "store_test.go") != 1 {
		t.Errorf("a file refused many times is listed many times: %q", s.SpecBroken)
	}
}

// AND A FEW REFUSALS ARE NOT A VERDICT. Trying a test file once or twice is an
// agent finding the edge of what it may do, not evidence of a broken
// specification.
func TestAHandfulOfRefusalsIsNotEnough(t *testing.T) {
	s := &State{}
	for range MaxTestEditRefusals - 1 {
		s.NoteTestEditRefusal(ModeDevelop, "store_test.go")
	}

	if s.SpecBroken != "" {
		t.Errorf("the author was convicted after %d refusals, short of the %d ceiling: %q",
			MaxTestEditRefusals-1, MaxTestEditRefusals, s.SpecBroken)
	}
}

// AND ONLY THE DEVELOPER IS BOUND BY THIS. The stages that may write tests are
// not refused for doing so, and must never convict themselves.
func TestTheAuthorIsNotConvictedByItsOwnWrites(t *testing.T) {
	for _, mode := range []Mode{ModeTest, ModeSpecMerge} {
		s := &State{}
		for range MaxTestEditRefusals * 2 {
			s.NoteTestEditRefusal(mode, "store_test.go")
		}
		if s.SpecBroken != "" {
			t.Errorf("mode %d convicted itself: %q", mode, s.SpecBroken)
		}
	}
}

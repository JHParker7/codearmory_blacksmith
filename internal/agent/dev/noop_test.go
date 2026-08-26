package dev

import (
	"errors"
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/edit"
)

// AN EDIT THAT COULD NOT HAVE CHANGED ANYTHING IS NOT CHARGED FOR.
//
// The refusal has named this precisely for a long time and the model sends one
// anyway. Read off run 61: three of them in three minutes, labelled "no-op
// placeholder" and "placeholder to test", interleaved with three correct
// attempts at the very fix the referee had just advised. A message did not stop
// it, the shape cannot be made unrepresentable — old_str and replace are free
// strings and no schema can require them to differ — so what is left is the
// price.
//
// Each one also produced another identical failing verification, which is what
// drove the referee to buy the same routing verdict three times in that run.
func TestAnEditThatAsksForNoChangeCostsNoTurn(t *testing.T) {
	s := newStateWith("handlers.go", "package main\n\nfunc handle() {}\n")
	before := s.Refunded

	same := "func handle() {}"
	_, err := s.Advance(Action{
		Action: ActionWriteFile,
		Edits:  []edit.Edit{{Path: "handlers.go", OldStr: same, Replace: same}},
	}, ModeDevelop)

	if err == nil {
		t.Fatal("an edit asking for no change was accepted")
	}
	if !errors.Is(err, ErrNoChangeAsked) {
		t.Fatalf("the refusal is not identifiable as a no-change edit: %v", err)
	}
	if s.Refunded != before+1 {
		t.Errorf("the turn was charged for: Refunded went %d -> %d", before, s.Refunded)
	}
	// AND IT STILL SAYS WHY. Refunding must not quietly swallow the diagnosis.
	if !strings.Contains(s.Notice, "IDENTICAL") {
		t.Errorf("the refusal no longer names the cause: %q", s.Notice)
	}
}

// A REFUSAL THAT IS THE AGENT'S FAULT IS STILL CHARGED FOR. The refund is for
// edits that cost the repository nothing, not for every rejection — otherwise a
// developer could write badly for free and the budget would stop meaning
// anything.
func TestAnOrdinaryRefusalStillCostsATurn(t *testing.T) {
	s := newStateWith("handlers.go", "package main\n\nfunc handle() {}\n")
	before := s.Refunded

	_, err := s.Advance(Action{
		Action: ActionWriteFile,
		Edits: []edit.Edit{{
			Path: "handlers.go", OldStr: "text that is nowhere in the file",
			Replace: "something else",
		}},
	}, ModeDevelop)

	if err == nil {
		t.Fatal("an unmatched old_str was accepted")
	}
	if errors.Is(err, ErrNoChangeAsked) {
		t.Fatal("an unmatched quote was classified as a no-change edit")
	}
	if s.Refunded != before {
		t.Errorf("an ordinary refusal was forgiven: Refunded went %d -> %d",
			before, s.Refunded)
	}
}

// AND THE ATTEMPT IS STILL BOUNDED. The refund forgives the BUDGET; the refusal
// still counts toward MaxDeadRefusals, so a developer that only ever sends these
// still ends rather than looping for free.
func TestRepeatedNoChangeEditsStillReachTheRefusalCeiling(t *testing.T) {
	s := newStateWith("handlers.go", "package main\n\nfunc handle() {}\n")
	same := "func handle() {}"

	for range MaxDeadRefusals + 1 {
		s.Advance(Action{
			Action: ActionWriteFile,
			Edits:  []edit.Edit{{Path: "handlers.go", OldStr: same, Replace: same}},
		}, ModeDevelop)
	}

	if _, done := s.Exhausted(MaxDeadRefusals); !done {
		t.Error("a developer sending nothing but no-change edits never ran out")
	}
}

// newStateWith is a state that has already read one file.
func newStateWith(path, content string) *State {
	return &State{
		Tree:     []string{path},
		Read:     map[string]string{path: content},
		Staged:   map[string]string{},
		Missing:  map[string]bool{},
		Baseline: map[string]string{path: content},
	}
}

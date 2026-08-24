package dev

import (
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/edit"
)

// DERIVE THE DECLARATION FROM THE QUOTE, but only when the span is exactly one.
// A quote that no longer matches byte for byte — reindented, a comment changed —
// still names the thing it meant, and the declaration resolver can find it.
func TestAQuoteThatMissesButNamesOneDeclarationResolvesToIt(t *testing.T) {
	s := tree(map[string]string{"store.go": storeGo})
	// The quote names List but its body is not what the file holds, so no text
	// match is possible — whitespace normalisation cannot rescue it.
	mustApply(t, s, ModeDevelop, edit.Edit{
		Path:    "store.go",
		OldStr:  "func List() []Task {\n\treturn s.everything\n}",
		Replace: "func List() []Task {\n\treturn []Task{}\n}",
	})
	got := s.Staged["store.go"]
	if !strings.Contains(got, "return []Task{}") {
		t.Errorf("the edit did not land:\n%s", got)
	}
	// It REPLACED rather than appended: one List, not two.
	if strings.Count(got, "func List()") != 1 {
		t.Errorf("the declaration was duplicated:\n%s", got)
	}
}

// A SPAN COVERING SEVERAL DECLARATIONS IS A MULTI-DECLARATION EDIT, and naming
// it after its first line replaces one while inserting all of them — that put
// main, store and apiTasksHandler into main.go twice.
func TestAQuoteSpanningSeveralDeclarationsIsNotResolvedByName(t *testing.T) {
	s := tree(map[string]string{"store.go": storeGo})
	err := Apply(s, []edit.Edit{{
		Path: "store.go",
		// Two declarations, and a body neither of them has, so the text cannot
		// match and the span is not one declaration to name.
		OldStr: "func (s *Store) Add(t Task) {\n\ts.items = append(s.items, t)\n}\n\n" +
			"func List() []Task {\n\treturn s.everything\n}",
		Replace: "func Other() {}",
	}}, ModeDevelop)
	if err == nil {
		t.Fatalf("a multi-declaration quote was resolved by name:\n%s", s.Staged["store.go"])
	}
}

// A QUOTE THAT MISSED ALONGSIDE A DECLARATION NAME is a correct decl address
// with a redundant quote attached — and the schema requires every field, so the
// model cannot avoid sending both.
func TestAMissedQuoteFallsBackToTheDeclarationBesideIt(t *testing.T) {
	s := tree(map[string]string{"store.go": storeGo})
	mustApply(t, s, ModeDevelop, edit.Edit{
		Path:    "store.go",
		OldStr:  "this text is nowhere in the file",
		Decl:    "List",
		Replace: "func List() []Task {\n\treturn []Task{}\n}",
	})
	if !strings.Contains(s.Staged["store.go"], "return []Task{}") {
		t.Errorf("the decl fallback did not resolve:\n%s", s.Staged["store.go"])
	}
}

// A FILE THE UNDONE EDIT CREATED HAS NO PREVIOUS STATE AND MUST GO — from Read
// as well, so the agent reads it again rather than addressing text the tree no
// longer holds.
func TestUndoingACreateRemovesTheFileEntirely(t *testing.T) {
	s := tree(map[string]string{"store.go": storeGo})
	mustApply(t, s, ModeDevelop, edit.Edit{Path: "filter.go", Replace: "package main\n\nfunc Filter() {}\n"})
	if _, ok := s.Staged["filter.go"]; !ok {
		t.Fatal("the create did not stage")
	}

	what, ok := s.Undo()
	if !ok {
		t.Fatal("undo refused")
	}
	if _, still := s.Staged["filter.go"]; still {
		t.Error("the created file survived the undo")
	}
	if _, still := s.Read["filter.go"]; still {
		t.Error("the agent still sees a file that no longer exists")
	}
	if !strings.Contains(what, "the edit created it") {
		t.Errorf("undo does not say the file was removed rather than restored: %q", what)
	}
}

// AN UNDO THAT RESTORES NOTHING still succeeds and says so, rather than reading
// as a failure the agent has to diagnose.
func TestUndoingAWriteThatChangedNothingSaysSo(t *testing.T) {
	s := tree(map[string]string{"store.go": storeGo})
	// Stage a file, then push an identical snapshot so the previous state matches.
	mustApply(t, s, ModeDevelop, edit.Edit{
		Path: "store.go", OldStr: "return nil", Replace: "return []Task{}"})
	s.UndoStack = append(s.UndoStack, maps(s.Staged))

	what, ok := s.Undo()
	if !ok {
		t.Fatal("undo refused")
	}
	if what != "nothing differed" {
		t.Errorf("undo reported %q", what)
	}
}

// The clip helper is used on every history line, so a caller's arithmetic must
// not be able to panic it.
func TestClipIsSafeForAnyBound(t *testing.T) {
	if got := clip("hello", -3); got != "" {
		t.Errorf("clip(-3) = %q, want empty", got)
	}
	if got := clip("hello", 0); got != "" {
		t.Errorf("clip(0) = %q, want empty", got)
	}
	if got := clip("  hello  ", 99); got != "hello" {
		t.Errorf("clip(99) = %q", got)
	}
	// Rune-safe: clipping must not split a multi-byte character.
	if got := clip("héllo wörld", 4); got != "héll…" {
		t.Errorf("clip(4) = %q", got)
	}
}

// THE RANGE IS ONLY BLAMED WHEN IT DIFFERS FROM THE DECLARATION IT TOUCHES.
// When they are identical the agent already sent the whole thing, and telling it
// to "re-issue with start_line 11 and end_line 13" — which is exactly what it
// just sent — is a loop. Measured on the retest as "lines 230-241 cut across
// func main, which spans lines 230-241".
func TestAnAgentIsNeverToldToResendTheRangeItJustSent(t *testing.T) {
	s := tree(map[string]string{"store.go": storeGo})
	// Lines 11-13 are exactly func List. The replacement is valid as statements
	// and not as declarations, so it breaks the file without being malformed.
	got := refusal(t, s, ModeDevelop, edit.Edit{
		Path: "store.go", StartLine: 11, EndLine: 13, Replace: "\treturn nil",
	})

	if strings.Contains(got, "start_line 11 and end_line 13") {
		t.Errorf("the agent was told to re-send the range it just sent:\n%s", got)
	}
	if strings.Contains(got, "LINE RANGE is what broke it") {
		t.Errorf("a range covering the whole declaration was blamed anyway:\n%s", got)
	}
	// It still gets the damage, which is the thing it can act on.
	if !strings.Contains(got, "AS YOUR EDIT") {
		t.Errorf("the refusal does not show what the edit produced:\n%s", got)
	}
}

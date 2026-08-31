package tools

import (
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/edit"
)

func ws(t *testing.T, files map[string]string, g Guard) *Workspace {
	t.Helper()
	return NewWorkspace(files, g)
}

func TestAQuotedAnchorAddressesTheLinesItMatches(t *testing.T) {
	// The anchor matches whole LINES, not substrings — see edit.ResolveText.
	w := ws(t, map[string]string{"a.go": "package p\n\nfunc F() int {\n\treturn 1\n}\n"}, nil)

	if _, err := w.ApplyEdit(edit.Edit{
		Path: "a.go", OldStr: "\treturn 1", Replace: "\treturn 2",
	}); err != nil {
		t.Fatalf("edit: %v", err)
	}

	got, _ := w.Read("a.go")
	if !strings.Contains(got, "return 2") || strings.Contains(got, "return 1") {
		t.Fatalf("the anchored line was not replaced: %q", got)
	}
	if !strings.Contains(got, "package p") {
		t.Fatalf("lines outside the anchor were lost: %q", got)
	}
}

func TestANamedDeclarationIsReplacedWhole(t *testing.T) {
	w := ws(t, map[string]string{
		"a.go": "package p\n\nfunc F() int {\n\treturn 1\n}\n\nfunc G() {}\n",
	}, nil)

	if _, err := w.ApplyEdit(edit.Edit{
		Path: "a.go", Decl: "F", Replace: "func F() int {\n\treturn 42\n}",
	}); err != nil {
		t.Fatalf("edit: %v", err)
	}

	got, _ := w.Read("a.go")
	if !strings.Contains(got, "return 42") {
		t.Fatalf("the declaration was not replaced: %q", got)
	}
	if !strings.Contains(got, "func G() {}") {
		t.Fatalf("the neighbouring declaration was lost: %q", got)
	}
}

// The whole point of the addressing scheme: what you do not name, you do not
// change. A whole-file write over existing content is the one shape that
// silently violates it.
func TestAWholeFileWriteOverExistingContentIsRefused(t *testing.T) {
	w := ws(t, map[string]string{"a.go": "package p\n\nfunc F() {}\n"}, nil)

	_, err := w.ApplyEdit(edit.Edit{Path: "a.go", Replace: "package p\n"})
	if err == nil {
		t.Fatal("a whole-file write over existing content was allowed")
	}
	if !strings.Contains(err.Error(), "old_str") || !strings.Contains(err.Error(), "decl") {
		t.Fatalf("the refusal does not say what to do instead: %v", err)
	}
}

func TestAWholeFileWriteCreatesAFileThatDoesNotExist(t *testing.T) {
	w := ws(t, map[string]string{}, nil)

	line, err := w.ApplyEdit(edit.Edit{Path: "new.go", Replace: "package p\n"})
	if err != nil {
		t.Fatalf("creating a new file was refused: %v", err)
	}
	if !strings.Contains(line, "created") {
		t.Fatalf("a creation was not reported as one: %q", line)
	}
	if got, ok := w.Read("new.go"); !ok || !strings.Contains(got, "package p") {
		t.Fatalf("the file was not created: %q", got)
	}
}

// Carried forward from the previous developer loop, where an identical pair was
// 11 of 26 refusals in one measured window.
func TestAnIdenticalOldStrAndReplaceIsRefusedByName(t *testing.T) {
	w := ws(t, map[string]string{"a.go": "package p\n\nfunc F() {}\n"}, nil)

	_, err := w.ApplyEdit(edit.Edit{Path: "a.go", OldStr: "func F() {}", Replace: "func F() {}"})
	if err == nil {
		t.Fatal("an edit that changes nothing was applied")
	}
	if !strings.Contains(err.Error(), "IDENTICAL") {
		t.Fatalf("the refusal does not name the cause: %v", err)
	}
}

func TestAnEditThatChangesNothingIsRefused(t *testing.T) {
	w := ws(t, map[string]string{"a.md": "one\ntwo\nthree\n"}, nil)

	_, err := w.ApplyEdit(edit.Edit{Path: "a.md", StartLine: 2, EndLine: 2, Replace: "two"})
	if err == nil {
		t.Fatal("a no-op edit was applied")
	}
	if !strings.Contains(err.Error(), "exactly as it was") {
		t.Fatalf("the refusal does not name the cause: %v", err)
	}
}

// A duplicate declaration does not compile, and the compiler is a whole
// verification round away. Refusing at the write turns a lost round trip into a
// message naming the symbol.
func TestADuplicateDeclarationIsRefusedAtTheWrite(t *testing.T) {
	w := ws(t, map[string]string{"a.go": "package p\n\nfunc F() {}\n"}, nil)

	_, err := w.ApplyEdit(edit.Edit{
		Path: "a.go", OldStr: "func F() {}", Replace: "func F() {}\n\nfunc F() {}",
	})
	if err == nil {
		t.Fatal("an edit producing two declarations of F was applied")
	}
	if !strings.Contains(err.Error(), "F") {
		t.Fatalf("the refusal does not name the symbol: %v", err)
	}
}

func TestTheGuardRefusesBeforeAnythingIsApplied(t *testing.T) {
	w := ws(t, map[string]string{"a_test.go": "package p\n"}, NoTests)

	_, err := w.ApplyEdit(edit.Edit{Path: "a_test.go", Replace: "package q\n"})
	if err == nil {
		t.Fatal("a write to a test file was allowed")
	}
	if got, _ := w.Read("a_test.go"); got != "package p\n" {
		t.Fatalf("the refused edit still changed the file: %q", got)
	}
}

func TestUndoRestoresTheFileTheLastWriteChanged(t *testing.T) {
	before := "package p\n\nfunc F() int {\n\treturn 1\n}\n"
	w := ws(t, map[string]string{"a.go": before}, nil)

	if _, err := w.ApplyEdit(edit.Edit{Path: "a.go", OldStr: "\treturn 1", Replace: "\treturn 2"}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	path, ok := w.Undo()
	if !ok || path != "a.go" {
		t.Fatalf("undo reported %q, %v", path, ok)
	}
	if got, _ := w.Read("a.go"); got != before {
		t.Fatalf("undo did not restore the content: %q", got)
	}
}

func TestUndoRemovesAFileTheLastWriteCreated(t *testing.T) {
	w := ws(t, map[string]string{}, nil)

	if _, err := w.ApplyEdit(edit.Edit{Path: "new.go", Replace: "package p\n"}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if _, ok := w.Undo(); !ok {
		t.Fatal("undo reported nothing to undo")
	}
	if _, ok := w.Read("new.go"); ok {
		t.Fatal("undoing a creation left the file behind")
	}
}

// One level, on purpose. A second undo must report that there is nothing left
// rather than quietly unwinding the write before it.
func TestUndoIsOneLevelAndSaysSoOnTheSecondCall(t *testing.T) {
	w := ws(t, map[string]string{"a.md": "one\n"}, nil)

	if _, err := w.ApplyEdit(edit.Edit{Path: "a.md", OldStr: "one", Replace: "two"}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if _, ok := w.Undo(); !ok {
		t.Fatal("the first undo failed")
	}
	if _, ok := w.Undo(); ok {
		t.Fatal("a second undo unwound a write it should not have")
	}
}

// The caller has to be able to answer "what did this agent change", and it
// cannot if the map it passed in is the one being mutated.
func TestTheTreePassedInIsNotMutated(t *testing.T) {
	original := map[string]string{"a.md": "one\n"}
	w := ws(t, original, nil)

	if _, err := w.ApplyEdit(edit.Edit{Path: "a.md", OldStr: "one", Replace: "two"}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if original["a.md"] != "one\n" {
		t.Fatalf("the caller's map was mutated: %q", original["a.md"])
	}
}

func TestPathsAreSortedSoTwoIdenticalRunsBuildTheSamePrompt(t *testing.T) {
	w := ws(t, map[string]string{"c.go": "", "a.go": "", "b.go": ""}, nil)

	got := strings.Join(w.Paths(), ",")
	if got != "a.go,b.go,c.go" {
		t.Fatalf("paths are not sorted: %q", got)
	}
}

// SeedKnown puts the whole tree in the agent's memory up front, so a reviewer
// sees the code on turn one instead of reading files in to discover it.
func TestSeedKnownMakesTheWholeTreeKnownImmediately(t *testing.T) {
	w := ws(t, map[string]string{"b.go": "package b", "a.go": "package a"}, AllowAll)
	if len(w.Known()) != 0 {
		t.Fatalf("a fresh workspace knows nothing until read; got %v", w.Known())
	}
	w.SeedKnown()
	got := w.Known()
	if len(got) != 2 || got[0] != "a.go" || got[1] != "b.go" {
		t.Fatalf("SeedKnown should know every file, sorted; got %v", got)
	}
}

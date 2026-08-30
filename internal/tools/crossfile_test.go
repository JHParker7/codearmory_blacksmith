package tools

import (
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/edit"
)

// THE FAILURE THAT KILLED THE BUDGET-EXIT RUN. The developer declared four test
// functions in both store_test.go and handlers_test.go; the per-file duplicate
// gate cannot see across files, so the compiler reported it a whole check round
// later, and the repair edits then stalled the stage. The whole tree is in the
// workspace — a duplicate one file over is checkable at the write.
func TestADuplicateAcrossFilesIsRefusedAtTheWrite(t *testing.T) {
	w := NewWorkspace(map[string]string{
		"store_test.go": "package main\n\nfunc TestAddComment(t *testing.T) {}\n",
	}, AllowAll)

	_, err := w.ApplyEdit(edit.Edit{
		Path:    "handlers_test.go",
		Replace: "package main\n\nfunc TestAddComment(t *testing.T) {}\n",
	})
	if err == nil {
		t.Fatal("a cross-file duplicate was accepted")
	}
	if !strings.Contains(err.Error(), "TestAddComment") ||
		!strings.Contains(err.Error(), "store_test.go") {
		t.Fatalf("the refusal does not name the symbol and the other file: %v", err)
	}
}

// Different directories are different packages: the same name in another
// package is ordinary Go, not a collision.
func TestTheSameNameInAnotherDirectoryIsFine(t *testing.T) {
	w := NewWorkspace(map[string]string{
		"store/store.go": "package store\n\nfunc New() {}\n",
	}, AllowAll)

	if _, err := w.ApplyEdit(edit.Edit{
		Path:    "handler/handler.go",
		Replace: "package handler\n\nfunc New() {}\n",
	}); err != nil {
		t.Fatalf("a same-named symbol in another package was refused: %v", err)
	}
}

// package p and package p_test share a directory without sharing a namespace.
func TestAnExternalTestPackageDoesNotCollide(t *testing.T) {
	w := NewWorkspace(map[string]string{
		"api.go": "package api\n\nfunc Handle() {}\n",
	}, AllowAll)

	if _, err := w.ApplyEdit(edit.Edit{
		Path:    "api_test.go",
		Replace: "package api_test\n\nfunc Handle() {}\n",
	}); err != nil {
		t.Fatalf("an external test package was treated as a collision: %v", err)
	}
}

// Methods live under their receiver: two types with a String method are not a
// redeclaration, and refusing them would refuse half of ordinary Go.
func TestMethodsOnDifferentTypesDoNotCollide(t *testing.T) {
	w := NewWorkspace(map[string]string{
		"a.go": "package p\n\ntype A struct{}\n\nfunc (a A) String() string { return \"a\" }\n",
	}, AllowAll)

	if _, err := w.ApplyEdit(edit.Edit{
		Path:    "b.go",
		Replace: "package p\n\ntype B struct{}\n\nfunc (b B) String() string { return \"b\" }\n",
	}); err != nil {
		t.Fatalf("methods on different types were treated as duplicates: %v", err)
	}
}

// The syntax refusal now carries the parser's position, because "check the
// braces and quotes" survived eight consecutive refusals in one measured stage:
// the model rewrote the wrong part of the replacement every time.
func TestASyntaxRefusalNamesTheLine(t *testing.T) {
	w := NewWorkspace(map[string]string{
		"a.go": "package p\n\nfunc F() int {\n\treturn 1\n}\n",
	}, AllowAll)

	_, err := w.ApplyEdit(edit.Edit{
		Path: "a.go", Decl: "F",
		Replace: "func F() int {\n\treturn 1\n}\n}",
	})
	if err == nil {
		t.Fatal("broken Go was accepted")
	}
	if !strings.Contains(err.Error(), "line ") {
		t.Fatalf("the refusal names no position: %v", err)
	}
}

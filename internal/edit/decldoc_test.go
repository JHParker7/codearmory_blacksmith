package edit

import (
	"strings"
	"testing"
)

const withDoc = `package main

// handleBoard serves the HTML board at /, showing tickets grouped by status
// and a form to create a new ticket.
func handleBoard(w http.ResponseWriter, r *http.Request) {
	_ = w
}

func other() {}
`

// REPLACING A DECLARATION REPLACES ITS DOC COMMENT.
//
// Pos() on a FuncDecl is the "func" keyword, so a replacement carrying its own
// comment left the OLD one stranded above the new one and the file grew a
// duplicate every time. Observed live, and it does not merely look untidy — it
// does not converge: the developer saw the duplicate, spent a turn deleting it,
// rewrote the declaration, and duplicated it again. Nine turns on one function,
// alternating between two signatures it could no longer tell apart.
func TestReplacingADeclarationTakesItsDocComment(t *testing.T) {
	span, err := Resolve(withDoc, Edit{
		Path: "board.go", Decl: "handleBoard",
		Replace: "// handleBoard returns the handler for /.\nfunc handleBoard(s *Store) http.HandlerFunc {\n\treturn nil\n}",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	got, err := Apply(withDoc, span,
		"// handleBoard returns the handler for /.\nfunc handleBoard(s *Store) http.HandlerFunc {\n\treturn nil\n}")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if n := strings.Count(got, "// handleBoard"); n != 1 {
		t.Errorf("the file carries %d handleBoard doc comments, want 1:\n%s", n, got)
	}
	if strings.Contains(got, "showing tickets grouped by status") {
		t.Errorf("the replaced comment survived:\n%s", got)
	}
	// And nothing else moved.
	if !strings.Contains(got, "func other() {}") {
		t.Errorf("the neighbouring declaration was disturbed:\n%s", got)
	}
}

// A DECLARATION WITH NO COMMENT IS UNCHANGED BY THIS. The span starts at the
// keyword, as it always did.
func TestADeclarationWithoutADocCommentIsUnaffected(t *testing.T) {
	src := "package main\n\nfunc other() {}\n\nfunc third() {}\n"

	span, err := Resolve(src, Edit{Path: "a.go", Decl: "other", Replace: "func other() { println(1) }"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got, err := Apply(src, span, "func other() { println(1) }")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.Contains(got, "println(1)") || !strings.Contains(got, "func third() {}") {
		t.Errorf("the replacement went wrong:\n%s", got)
	}
}

// A COMMENT THAT IS NOT ATTACHED IS NOT A DOC COMMENT. A blank line between them
// detaches it in Go's own reading, so it belongs to nobody and must be left
// alone — taking it would delete a file-level note nobody asked to touch.
func TestADetachedCommentIsNotTakenWithTheDeclaration(t *testing.T) {
	src := "package main\n\n// a note about the file, not about anything below it\n\nfunc other() {}\n"

	span, err := Resolve(src, Edit{Path: "a.go", Decl: "other", Replace: "func other() { println(1) }"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got, err := Apply(src, span, "func other() { println(1) }")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.Contains(got, "a note about the file") {
		t.Errorf("a detached comment was swallowed by the declaration:\n%s", got)
	}
}

// TYPES AND VARS TOO. The same shape of loop is available on any declaration a
// doc comment can sit above.
func TestReplacingATypeTakesItsDocComment(t *testing.T) {
	src := "package main\n\n// Ticket is one row on the board.\ntype Ticket struct{ ID int }\n"

	span, err := Resolve(src, Edit{Path: "a.go", Decl: "Ticket",
		Replace: "// Ticket is one item.\ntype Ticket struct{ ID string }"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got, err := Apply(src, span, "// Ticket is one item.\ntype Ticket struct{ ID string }")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if strings.Contains(got, "one row on the board") {
		t.Errorf("the type's old doc comment survived:\n%s", got)
	}
}

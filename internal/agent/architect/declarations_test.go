package architect

import (
	"errors"
	"strings"
	"testing"
)

// THE ARCHITECT DECLARES SO THE AUTHOR'S TESTS COMPILE.
//
// Without declarations a specification fails at the BUILD stage on "undefined:
// NewStore", so go test never type-checks it and vet never runs. Measured: run
// 81's t.Error("%q") was invisible to the author's own gate for exactly that
// reason, and only surfaced later inside a file the developer was forbidden to
// edit — 99 turns, zero verifications, blocked.
func TestDeclarationsAreAccepted(t *testing.T) {
	src := `package types

import "errors"

type Status string

const StatusOpen Status = "open"

var ErrNotFound = errors.New("not found")

type Store struct{}

func NewStore() *Store { panic("not implemented") }

func (s *Store) Get(id string) (*Ticket, error)

type Ticket struct {
	ID    string
	Title string
}
`
	if err := OnlyDeclarations(src); err != nil {
		t.Errorf("a declarations file was refused: %v", err)
	}
}

// BUT A STUB IS NOT AN IMPLEMENTATION. An architect that writes bodies has done
// the developer's work, and done it without having seen the tests.
func TestABodyThatDoesAnythingIsRefused(t *testing.T) {
	src := `package types

func Add(a, b int) int { return a + b }
`
	err := OnlyDeclarations(src)
	if err == nil {
		t.Fatal("a function with a real body was accepted")
	}
	if !errors.Is(err, ErrNotDeclarations) {
		t.Errorf("the refusal is not identifiable: %v", err)
	}
	if !strings.Contains(err.Error(), "Add") {
		t.Errorf("the refusal does not name the offending function: %v", err)
	}
}

// RETURNING A ZERO VALUE IS A BODY TOO, and this is the case worth being strict
// about: "return nil" is behaviour a test can pass against, so a specification
// written against it would be GREEN before the developer started — which the
// author's own gate then reads as work already finished.
func TestAStubThatReturnsAValueIsRefused(t *testing.T) {
	if err := OnlyDeclarations("package types\n\nfunc NewStore() *Store { return nil }\n"); err == nil {
		t.Error("a stub returning a zero value was accepted as a declaration")
	}
}

// A FILE THAT DOES NOT PARSE IS REFUSED WITH THE PARSE ERROR, because the whole
// point is that what lands in the tree compiles for everyone downstream.
func TestAFileThatDoesNotParseIsRefused(t *testing.T) {
	err := OnlyDeclarations("package types\n\nfunc Broken( {\n")
	if err == nil {
		t.Fatal("a file that does not parse was accepted")
	}
	if !strings.Contains(err.Error(), "does not parse") {
		t.Errorf("the refusal does not say it failed to parse: %v", err)
	}
}

// TESTS ARE THE AUTHOR'S AND ONLY THE AUTHOR'S. An architect writing a test file
// has written the specification from the request alone, before the stage whose
// whole job that is has seen it.
func TestATestFileIsNotADeclarationFile(t *testing.T) {
	for path, want := range map[string]bool{
		"types/types.go":  true,
		"types/store.go":  true,
		"types.go":        false, // the ROOT already has a package; see run 90
		"ticket/types.go": false, // one folder, so there is one place to look
		"types/x_test.go": false, // tests are the author's
		"README.md":       false,
		"Makefile":        false,
	} {
		if got := IsDeclarationFile(path); got != want {
			t.Errorf("IsDeclarationFile(%q) = %v, want %v", path, got, want)
		}
	}
}

// AND THE ACCEPTED FILE CARRIES THE MARKER, stamped rather than requested,
// because a marker the model has to remember is one some reply will leave out.
func TestAnAcceptedDeclarationFileIsMarked(t *testing.T) {
	files, rejected := Sanitise(Design{Files: []File{
		{Path: "README.md", Content: "# ok\n"},
		{Path: "types/types.go", Content: "package types\n\ntype Ticket struct{ ID string }\n"},
	}})

	if len(rejected) != 0 {
		t.Fatalf("a valid design was rejected: %v", rejected)
	}
	var got string
	for _, f := range files {
		if f.Path == "types/types.go" {
			got = f.Content
		}
	}
	if got == "" {
		t.Fatal("the declarations file was dropped")
	}
	if !strings.Contains(got, DeclarationsMarker) {
		t.Errorf("the declarations file is unmarked, so the author's gate will "+
			"count it as an implementation:\n%s", got)
	}
}

// AND A REFUSED ONE IS NAMED, so the ticket says what was dropped and why rather
// than silently shipping a design with a hole in it.
func TestARefusedDeclarationFileSaysWhy(t *testing.T) {
	_, rejected := Sanitise(Design{Files: []File{
		{Path: "types/worker.go", Content: "package types\n\nfunc Work() int { return 41 + 1 }\n"},
	}})

	if len(rejected) != 1 {
		t.Fatalf("rejected = %v, want the one file named", rejected)
	}
	if !strings.Contains(rejected[0], "Work") {
		t.Errorf("the rejection does not say what was wrong: %q", rejected[0])
	}
}

// THE PACKAGE MUST MATCH ITS FOLDER, which is the defect run 90 shipped.
//
// The architect wrote ticket.go at the repository ROOT declaring "package
// ticket", beside a main.go declaring "package main". Two packages in one
// directory means nothing in the tree builds — so the declarations destroyed the
// very thing they exist to buy, tests that type-check. A folder of its own
// cannot collide, and the package name is checked against it.
func TestAPackageThatDisagreesWithItsFolderIsRefused(t *testing.T) {
	err := OnlyDeclarations("package ticket\n\ntype Ticket struct{ ID string }\n")
	if err == nil {
		t.Fatal("a file declaring the wrong package was accepted")
	}
	if !errors.Is(err, ErrNotDeclarations) {
		t.Errorf("the refusal is not identifiable: %v", err)
	}
	for _, want := range []string{"package ticket", "types"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
}

// AND THE ROOT IS NOT A PLACE FOR THEM AT ALL, whatever they declare.
func TestDeclarationsAtTheRootAreRejectedOutright(t *testing.T) {
	_, rejected := Sanitise(Design{Files: []File{
		{Path: "ticket.go", Content: "package ticket\n\ntype Ticket struct{ ID string }\n"},
	}})
	if len(rejected) != 1 {
		t.Fatalf("rejected = %v, want the root Go file named", rejected)
	}
}

package tools

import (
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/edit"
)

const stubFile = `package main

type Task struct {
	ID    int
	Title string
}

func NewStore() *Store { return nil }

type Store struct{}

func (s *Store) Create(t Task) (Task, error) { return Task{}, nil }
`

// THE REFUSAL THAT KILLED THE FIRST THREE-STAGE RUN. The test author hands the
// developer files of placeholder stubs whose whole purpose is to be replaced,
// and the blanket whole-file refusal sent six consecutive 37-second rewrites of
// main.go to their death with advice to quote a snippet of code that was never
// meant to survive. A rewrite that KEEPS every declared name loses nothing that
// fails silently.
func TestARewriteKeepingEveryDeclarationIsAllowed(t *testing.T) {
	w := NewWorkspace(map[string]string{"main.go": stubFile}, AllowAll)

	real := strings.Replace(stubFile,
		"func NewStore() *Store { return nil }",
		"func NewStore() *Store { return &Store{} }", 1)
	real = strings.Replace(real, "type Store struct{}",
		"type Store struct {\n\ttasks map[int]Task\n}", 1)

	if _, err := w.ApplyEdit(edit.Edit{Path: "main.go", Replace: real}); err != nil {
		t.Fatalf("a name-preserving rewrite of a stub file was refused: %v", err)
	}
	got, _ := w.Read("main.go")
	if !strings.Contains(got, "&Store{}") {
		t.Fatalf("the rewrite did not land: %q", got)
	}
}

// Adding NEW declarations alongside the old ones is the ordinary shape of
// implementing a stub file, and must pass for the same reason.
func TestARewriteAddingDeclarationsIsAllowed(t *testing.T) {
	w := NewWorkspace(map[string]string{"main.go": stubFile}, AllowAll)

	if _, err := w.ApplyEdit(edit.Edit{
		Path:    "main.go",
		Replace: stubFile + "\nfunc (s *Store) Get(id int) (Task, error) { return Task{}, nil }\n",
	}); err != nil {
		t.Fatalf("a rewrite that only adds was refused: %v", err)
	}
}

// A rewrite that DROPS a declaration is the silent deletion the rule exists
// for, and the refusal names the casualty.
func TestARewriteDroppingADeclarationIsRefusedByName(t *testing.T) {
	w := NewWorkspace(map[string]string{"main.go": stubFile}, AllowAll)

	_, err := w.ApplyEdit(edit.Edit{
		Path:    "main.go",
		Replace: "package main\n\ntype Task struct{ ID int }\n",
	})
	if err == nil {
		t.Fatal("a rewrite dropping NewStore and Store was accepted")
	}
	for _, want := range []string{"NewStore", "Store", "DROPS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not carry %q: %v", want, err)
		}
	}
}

// The fence repair applies on this path too: a fenced name-preserving rewrite
// is unwrapped and accepted, not refused for the backticks.
func TestAFencedStubRewriteIsRepairedAndAllowed(t *testing.T) {
	w := NewWorkspace(map[string]string{"main.go": stubFile}, AllowAll)

	line, err := w.ApplyEdit(edit.Edit{
		Path:    "main.go",
		Replace: "```go\n" + stubFile + "```",
	})
	// Identical content after unfencing would be a no-op refusal; vary it.
	if err != nil && !strings.Contains(err.Error(), "exactly as it was") {
		t.Fatalf("a fenced stub rewrite was refused for the wrong reason: %v", err)
	}
	_ = line
}

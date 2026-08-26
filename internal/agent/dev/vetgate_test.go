package dev

import (
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/edit"
)

// THE WRITE IS WHERE IT IS CAUGHT, and that is the whole point of the check.
//
// A fault the toolchain would reject on sight, written into a test file, is
// invisible to the author's own gate — with no implementation the package does
// not type-check, so vet never runs. By the time it surfaces it is inside a file
// the developer may not edit. Run 81: 99 turns across three attempts, not one
// verification, ticket blocked. Refusing the write is the last moment at which
// the agent that made the mistake still owns the file.
func TestAWriteTheToolchainWouldRejectIsRefused(t *testing.T) {
	s := newStateWith("store_test.go", "package main\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) {\n}\n")

	err := Apply(s, []edit.Edit{{
		Path: "store_test.go",
		Decl: "TestX",
		Replace: "func TestX(t *testing.T) {\n" +
			"\tt.Error(\"NewStore returned nil for %q\", \"x\")\n}",
	}}, ModeTest)

	if err == nil {
		t.Fatal("a format directive in a plain Error call was written")
	}
	if !strings.Contains(err.Error(), "Errorf") {
		t.Errorf("the refusal does not say what to use instead: %v", err)
	}
}

// AND A CORRECT WRITE IS STILL A WRITE. The check must not stand between the
// author and an ordinary edit.
func TestAnOrdinaryWriteIsUnaffectedByTheCheck(t *testing.T) {
	s := newStateWith("store_test.go", "package main\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) {\n}\n")

	err := Apply(s, []edit.Edit{{
		Path: "store_test.go",
		Decl: "TestX",
		Replace: "func TestX(t *testing.T) {\n" +
			"\tt.Errorf(\"got %q want %q\", \"a\", \"b\")\n}",
	}}, ModeTest)

	if err != nil {
		t.Fatalf("a correct formatting call was refused: %v", err)
	}
	if !strings.Contains(s.Staged["store_test.go"], "Errorf") {
		t.Error("the edit did not land")
	}
}

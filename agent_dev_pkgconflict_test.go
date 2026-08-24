package main

import (
	"strings"
	"testing"
)

// TWO PACKAGES IN ONE DIRECTORY IS THE SPECIFICATION'S FAULT AND NOBODY ELSE'S.
//
// Go allows one package name per directory. That name is set by the test file,
// which the developer may not edit, so every implementation file beside it must
// match a declaration the developer cannot change — there is no edit that
// compiles.
//
// Measured on r75: an author wrote "package api" into a directory whose other
// files said "package store". The tree stopped compiling, and because the
// toolchain reports this WITHOUT a file, line and column, goCompileError never
// matched it — so the hand-back that exists for exactly this case never fired and
// the developer looped on an impossible move until its budget ran out.

const r75Conflict = `found packages api (api.go) and store (store.go) in /workspace
FAIL	tracker [setup failed]
FAIL`

func TestAPackageConflictIsRecognised(t *testing.T) {
	got := packageConflictFiles(r75Conflict)

	if len(got) != 2 {
		t.Fatalf("packageConflictFiles returned %v, want the two conflicting files", got)
	}
	if got[0] != "api.go" || got[1] != "store.go" {
		t.Errorf("named %v, want [api.go store.go]", got)
	}
}

// The failure carries no position, which is exactly why it needed its own
// matcher — pinned so nobody folds it back into the position-based path.
func TestThePositionBasedMatcherCannotSeeAPackageConflict(t *testing.T) {
	if files, _ := compileErrorFiles(r75Conflict); len(files) > 0 {
		t.Errorf("compileErrorFiles now matches a package conflict (%v); this guard is obsolete", files)
	}
	if bad := nonUndefinedCompileErrors(r75Conflict); len(bad) > 0 {
		t.Errorf("nonUndefinedCompileErrors now matches a package conflict (%v)", bad)
	}
}

// Ordinary compile output must not be read as a package clash.
func TestExpectedRedIsNotAPackageConflict(t *testing.T) {
	out := `store_create_test.go:12:7: undefined: New
store_create_test.go:15:9: undefined: StatusOpen
FAIL	tracker [build failed]`

	if got := packageConflictFiles(out); len(got) > 0 {
		t.Errorf("undefined symbols read as a package conflict: %v", got)
	}
}

// The author is told the rule, because it is the only stage that can honour it.
func TestTheAuthorIsToldToMatchTheExistingPackage(t *testing.T) {
	prompt := testerSystemPrompt()

	for _, want := range []string{"ONE PACKAGE PER DIRECTORY", "copy its package line"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the test author is not told %q", want)
		}
	}
}

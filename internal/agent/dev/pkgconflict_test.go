package dev

import "testing"

// A TREE THAT DOES NOT BUILD IS NOT THE EXPECTED RED OF TEST-FIRST.
//
// Go reports a package clash without a position —
//
//	found packages main (board_test.go) and api (handlers_test.go) in /workspace
//
// — so NonUndefinedCompileErrors, which matches file:line:column, cannot see it.
//
// Measured on run 92, where it cost the ticket twice. Four spec agents wrote four
// different package clauses at the root because nothing said which package the
// code under test belongs to. The author's gate passed the result, so an
// unbuildable tree reached the developer; and because the gate said pass, the
// author stopped after correcting ONE file per hand-back instead of working until
// the package agreed everywhere. Five of six files were right and the sixth spent
// both repairs.
func TestAPackageConflictIsNotTheExpectedRed(t *testing.T) {
	const out = "found packages main (board_test.go) and api (handlers_test.go) in /workspace"

	if RedIsExpected(out) {
		t.Error("a directory holding two packages was treated as the red a " +
			"specification is supposed to produce; nothing in that tree compiles")
	}
}

// AND THE ACTUAL EXPECTED RED STILL IS ONE. Undefined symbols are what a
// specification written before its implementation MUST produce, and failing the
// author for them would mean no test-first ticket could ever leave the stage.
func TestUndefinedSymbolsAreStillTheExpectedRed(t *testing.T) {
	const out = "# demo [demo.test]\n" +
		"./store_test.go:12:7: undefined: NewStore\n" +
		"./store_test.go:13:9: undefined: StatusOpen"

	if !RedIsExpected(out) {
		t.Error("the expected red of test-first was rejected")
	}
}

// AND A CONFLICT MIXED WITH THE EXPECTED RED IS STILL A CONFLICT. The build
// failure comes first, so both appear together and the tree is still unbuildable.
func TestAConflictAlongsideUndefinedSymbolsStillFails(t *testing.T) {
	const out = "./store_test.go:12:7: undefined: NewStore\n" +
		"found packages main (board_test.go) and api (handlers_test.go) in /workspace"

	if RedIsExpected(out) {
		t.Error("a package conflict was excused because undefined symbols were " +
			"present alongside it")
	}
}

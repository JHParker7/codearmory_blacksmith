package main

import "testing"

// A MISSING IMPORT LOOKS EXACTLY LIKE A MISSING SYMBOL.
//
// Go reports both as "undefined: X", and for an unimported package X is the bare
// package name with no dot to give it away. The dotted case was already handled —
// "undefined: sync.atomic" cannot be defined by any code in this package — but
// the far commoner one, a file that uses url.QueryEscape and forgets to import
// net/url, was indistinguishable from the expected red of test-first.
//
// Measured on r82, and it cost the task its whole budget:
//
//	./handlers_read_test.go:181:66: undefined: url
//
// The filter read that as a symbol the implementation had yet to write, so the
// specification was never handed back. The developer — which may not edit tests —
// tried to satisfy it by declaring a QueryEscape helper, which cannot work
// because the compiler is looking for a package qualifier, then rewrote the same
// file until its turns ran out.
//
// Two model reviews had already passed the same file. The preflight said "the
// tests are consistent and implementable, all assertions are logically
// satisfiable" at high confidence, twice — it reads for logic, and a missing
// import is not a logical property. The compiler knew on the first run; the
// harness was discarding the answer.

func TestAMissingImportIsNotTheExpectedRed(t *testing.T) {
	out := `./handlers_read_test.go:181:66: undefined: url
./store_test.go:12:7: undefined: NewStore`

	bad := nonUndefinedCompileErrors(out)
	if len(bad) != 1 {
		t.Fatalf("got %d defects %v, want only the missing import", len(bad), bad)
	}
	if bad[0] != "handlers_read_test.go:181:66: undefined: url" {
		t.Errorf("kept %q, want the missing import", bad[0])
	}
}

// The expected red must still be exempt, or every test-first run hands itself
// back on the first compile.
func TestUndefinedImplementationSymbolsStayExempt(t *testing.T) {
	out := `./store_test.go:12:7: undefined: NewStore
./store_test.go:15:9: undefined: StatusOpen
./store_test.go:19:7: undefined: ErrNotFound`

	if bad := nonUndefinedCompileErrors(out); len(bad) > 0 {
		t.Errorf("symbols the developer owes were read as defects: %v", bad)
	}
}

func TestMissingImportRecognisesPackageNames(t *testing.T) {
	for _, line := range []string{
		"./a_test.go:1:1: undefined: url",
		"./a_test.go:1:1: undefined: httptest",
		"./a_test.go:1:1: undefined: json",
		"./a_test.go:1:1: undefined: strings",
	} {
		if !missingImport(line) {
			t.Errorf("%q was not recognised as a missing import", line)
		}
	}
	// Anything not in the closed set is an ordinary symbol. That is the safe
	// direction: it costs a turn, where the opposite costs the whole attempt.
	for _, line := range []string{
		"./a_test.go:1:1: undefined: NewStore",
		"./a_test.go:1:1: undefined: StatusOpen",
		"./a_test.go:1:1: undefined: Ticket",
	} {
		if missingImport(line) {
			t.Errorf("%q was mistaken for a package", line)
		}
	}
}

// The dotted case this was modelled on must keep working.
func TestADottedSelectorIsStillADefect(t *testing.T) {
	out := `./a_test.go:9:2: undefined: sync.atomic`

	if bad := nonUndefinedCompileErrors(out); len(bad) != 1 {
		t.Errorf("a package selector no longer reads as a defect: %v", bad)
	}
}

package main

import "testing"

// THE COMPILER'S CUTOFF MARKER MUST NOT READ AS A DEFECT.
//
// Go stops after ten errors and appends "too many errors" carrying the file and
// line of whichever error was tenth. A specification written before its
// implementation refers to undefined symbols; those are the expected red and are
// filtered. On a slice with more than ten, the cutoff is then the ONLY line left,
// and the author is told its tests fail to compile for some reason other than the
// missing implementation — the precise opposite of the truth.
//
// Measured on r72: one store slice was handed "store_create_test.go:50:7: too
// many errors", rewrote the same correct file 64 times in 20 minutes, and never
// finished.
func TestTheTenErrorCutoffIsNotASpecificationDefect(t *testing.T) {
	out := `store_create_test.go:12:7: undefined: New
store_create_test.go:15:9: undefined: StatusOpen
store_create_test.go:19:7: undefined: Create
store_create_test.go:50:7: too many errors`

	if bad := nonUndefinedCompileErrors(out); len(bad) > 0 {
		t.Errorf("undefined symbols plus the cutoff read as %d defects (%v); this is the expected red of test-first", len(bad), bad)
	}
}

// The cutoff must not become a blanket exemption either: a real defect reported
// alongside it is still a real defect, and this is the case the author CAN act
// on.
func TestARealDefectSurvivesTheCutoff(t *testing.T) {
	out := `store_create_test.go:12:7: undefined: New
store_create_test.go:31:2: no new variables on left side of :=
store_create_test.go:50:7: too many errors`

	bad := nonUndefinedCompileErrors(out)
	if len(bad) != 1 {
		t.Fatalf("got %d defects %v, want only the redeclaration", len(bad), bad)
	}
	if bad[0] != "store_create_test.go:31:2: no new variables on left side of :=" {
		t.Errorf("kept %q, want the real compile error", bad[0])
	}
}

// A specification with no implementation and FEWER than ten undefined symbols
// never triggered the cutoff, and must keep passing the gate exactly as before.
func TestUndefinedSymbolsAloneRemainExpectedRed(t *testing.T) {
	out := `store_create_test.go:12:7: undefined: New
store_create_test.go:15:9: undefined: StatusOpen`

	if bad := nonUndefinedCompileErrors(out); len(bad) > 0 {
		t.Errorf("undefined symbols read as defects: %v", bad)
	}
}

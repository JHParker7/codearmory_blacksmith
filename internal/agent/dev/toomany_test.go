package dev

import "testing"

// GO'S TRUNCATION MARKER IS NOT A DEFECT IN THE TESTS.
//
// The toolchain caps a package at ten errors and prints
// "store_test.go:54:7: too many errors" as the eleventh. It carries a file, a
// line and a column like any real diagnostic, so it reads as "a compile error
// that is not an undefined symbol" — the exact shape that means the
// specification is broken.
//
// Read off run 12: a store specification names eleven undefined symbols before
// its implementation exists, so Go truncated, and that one line made
// RedIsExpected false for a completely correct specification. The author was
// refused, rewrote the identical file, and repeated that for 65 turns.
func TestGoSayingItStoppedCountingIsNotASpecificationDefect(t *testing.T) {
	const truncated = "# demo [demo.test]\n" +
		"./store_test.go:12:7: undefined: NewStore\n" +
		"./store_test.go:13:33: undefined: StatusOpen\n" +
		"./store_test.go:20:7: undefined: NewStore\n" +
		"./store_test.go:54:7: too many errors\n" +
		"FAIL\tdemo [build failed]"

	if got := NonUndefinedCompileErrors(truncated); len(got) != 0 {
		t.Errorf("the truncation marker was counted as a defect: %v", got)
	}
	if !RedIsExpected(truncated) {
		t.Error("a correct test-first specification was judged broken because Go " +
			"stopped listing its undefined symbols")
	}
}

// AND THE STAGE THAT WROTE IT IS ALLOWED TO FINISH. This is the end the loop
// actually turns on; the unit above is only how it gets there.
func TestAnAuthorFinishesWhenGoTruncatedItsUndefinedSymbols(t *testing.T) {
	const truncated = "./store_test.go:12:7: undefined: NewStore\n" +
		"./store_test.go:54:7: too many errors\n" +
		"FAIL\tdemo [build failed]"

	for name, mode := range map[string]Mode{
		"the author writing the section": ModeTest,
		"the merge reconciling sections": ModeSpecMerge,
	} {
		t.Run(name, func(t *testing.T) {
			s := &State{Staged: map[string]string{"store_test.go": "package main\n"}}
			s.RecordVerification(truncated, true, mode)

			if !s.Finished() {
				t.Error("the author was held in the loop by Go's truncation marker; " +
					"on the live run it rewrote the same file 65 times")
			}
		})
	}
}

// TRUNCATION HIDES NOTHING THAT WAS ALREADY VISIBLE. A real defect listed above
// the marker must still be caught — ignoring the marker must not mean ignoring
// the errors it followed.
func TestARealDefectAboveTheTruncationMarkerIsStillCaught(t *testing.T) {
	const mixed = "./handlers_test.go:12:7: undefined: NewStore\n" +
		"./handlers_test.go:105:17: not enough arguments in call to handleList\n" +
		"./handlers_test.go:54:7: too many errors"

	got := NonUndefinedCompileErrors(mixed)
	if len(got) != 1 {
		t.Fatalf("expected exactly the arity error, got %v", got)
	}
	if RedIsExpected(mixed) {
		t.Error("a wrong argument count was excused along with the truncation marker")
	}
}

// THE MARKER IS MATCHED WHOLE, NOT BY THE WORD "errors".
//
// A LOOSE MATCH IS THE EASY VERSION OF THIS FIX AND IT IS WRONG: Go's own
// message for an unused import of the errors package is
// `"errors" imported and not used`, a genuine defect in a test file the author
// must fix, and any substring test for "errors" excuses it. It is a realistic
// one too — a specification that drops its last errors.Is assertion leaves the
// import behind.
func TestOnlyTheWholeTruncationMarkerIsExcused(t *testing.T) {
	const unusedImport = "./store_test.go:8:2: \"errors\" imported and not used"

	if got := NonUndefinedCompileErrors(unusedImport); len(got) != 1 {
		t.Errorf("an unused errors import was excused as a truncation marker: %v", got)
	}
	if RedIsExpected(unusedImport) {
		t.Error("a test file that does not compile was judged to be the expected red")
	}
}

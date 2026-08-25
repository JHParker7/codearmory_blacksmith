package dev

import (
	"strings"
	"testing"
)

// liveVetFailure is the verification output from the run that prompted all of
// this, verbatim. The developer spent 23 turns against it.
const liveVetFailure = `===ADVISORY-OPEN===
# demo
# [demo]
vet: ./handlers_test.go:17:2: undefined: t
===ADVISORY-CLOSE===
--- test-only imports in production code ---
no testing packages in production files
`

// THE TOOLCHAIN REPORTS THE SAME ERROR TWO WAYS and a matcher anchored at the
// line start sees only one. Every compile fault in the live run arrived
// vet-labelled, so the classification never ran and the developer was left to
// flail.
func TestAVetLabelledErrorIsStillACompileError(t *testing.T) {
	files, allTests := CompileErrorFiles(liveVetFailure)
	if len(files) != 1 || files[0] != "handlers_test.go" {
		t.Fatalf("CompileErrorFiles = %v, want the test file — a vet: prefix must "+
			"not hide the position behind it", files)
	}
	if !allTests {
		t.Fatal("the only file at fault is a test file")
	}
}

func TestTheSamePositionUnlabelledIsRecognisedToo(t *testing.T) {
	plain := "# demo [demo.test]\n./handlers_test.go:17:2: undefined: t\n"
	files, allTests := CompileErrorFiles(plain)
	if len(files) != 1 || !allTests {
		t.Fatalf("CompileErrorFiles = %v allTests=%v", files, allTests)
	}
}

// A HELPER THAT FORGOT ITS PARAMETER LOOKS EXACTLY LIKE A MISSING SYMBOL, and
// only a test file can bind a *testing.T. The one production route the model
// reaches for — declaring the symbol — needs the testing package, which the gate
// refuses in production code.
func TestAnUndefinedTestHandleIsNotTheExpectedRed(t *testing.T) {
	if RedIsExpected(liveVetFailure) {
		t.Fatal("`undefined: t` in a test file was read as the expected red of " +
			"test-first; no implementation can define a testing handle, so this " +
			"is the specification's fault and the developer will burn its budget")
	}
	got := NonUndefinedCompileErrors(liveVetFailure)
	if len(got) != 1 || !strings.Contains(got[0], "undefined: t") {
		t.Fatalf("NonUndefinedCompileErrors = %v", got)
	}
}

// AND THE EXPECTED RED IS STILL EXPECTED. This is the failure that costs
// healthy tickets when it is got wrong: every test-first specification produces
// it before its implementation exists.
func TestAnUndefinedImplementationSymbolIsTheExpectedRed(t *testing.T) {
	out := "./store_test.go:9:12: undefined: NewStore\n"
	if !RedIsExpected(out) {
		t.Fatal("`undefined: NewStore` is what a specification written before its " +
			"implementation MUST produce; blaming the author for it abandons " +
			"tickets that were nearly done")
	}
}

// A MISSING IMPORT LOOKS EXACTLY LIKE A MISSING SYMBOL. "undefined: url" is a
// test that forgot net/url, not a symbol the implementation owes it.
func TestAnUndefinedStdlibPackageIsNotTheExpectedRed(t *testing.T) {
	for _, sym := range []string{"url", "strconv", "httptest", "json"} {
		out := "./handlers_test.go:12:9: undefined: " + sym + "\n"
		if RedIsExpected(out) {
			t.Errorf("`undefined: %s` was read as expected red; no code in this "+
				"package can define a standard library package", sym)
		}
	}
}

// A DOTTED SYMBOL NAMES A PACKAGE MEMBER, which no code in this package defines.
func TestAnUndefinedPackageMemberIsNotTheExpectedRed(t *testing.T) {
	out := "./store_test.go:3:2: undefined: sync.atomic\n"
	if RedIsExpected(out) {
		t.Fatal("a dotted symbol cannot be defined by this package")
	}
}

// A TEST HANDLE IS ONLY UNSATISFIABLE IN A TEST FILE. In production, `t` is an
// ordinary identifier the implementation may well be expected to declare.
func TestATestHandleNameInProductionIsAnOrdinarySymbol(t *testing.T) {
	out := "./main.go:20:2: undefined: t\n"
	if !RedIsExpected(out) {
		t.Fatal("`undefined: t` in production code is a symbol like any other; " +
			"treating it as unsatisfiable would blame the author for the " +
			"developer's own missing declaration")
	}
}

// EVERYTHING THAT IS NOT AN UNDEFINED SYMBOL is a defect in the tests.
func TestOtherCompileErrorsAreNotExpectedRed(t *testing.T) {
	cases := []string{
		"./handlers_test.go:17:2: tasksHandler redeclared in this block",
		"./store_test.go:27:18: cannot use time.Now() (value of struct type time.Time) as *time.Time value",
		"./store_test.go:31:4: too many arguments in call to NewStore",
	}
	for _, out := range cases {
		if RedIsExpected(out + "\n") {
			t.Errorf("read as expected red: %s", out)
		}
	}
}

// ---- who owns the failure ----

// A COMPILE ERROR IN THE DEVELOPER'S OWN FILE IS THE DEVELOPER'S TO FIX, and
// while one stands the test files cannot be judged: Go reports a type error at
// the use site, which under test-first is a test file even when the declaration
// at fault is the implementation's.
func TestAnImplementationErrorMeansTheTestsAreNotYetToBlame(t *testing.T) {
	out := "vet: ./main.go:108:6: tasksHandler redeclared in this block\n" +
		"vet: ./handlers_test.go:17:2: undefined: t\n"
	files, allTests := CompileErrorFiles(out)
	if len(files) != 2 {
		t.Fatalf("CompileErrorFiles = %v, want both files", files)
	}
	if allTests {
		t.Fatal("main.go is not a test file; while it does not compile the " +
			"specification cannot be judged")
	}
}

func TestNoCompileErrorsAtAll(t *testing.T) {
	files, allTests := CompileErrorFiles("--- FAIL: TestThing\n    store_test.go:96: want 3, got 2\n")
	if files != nil || allTests {
		t.Fatalf("CompileErrorFiles = %v %v — a test FAILURE logs no column and is "+
			"ordinary red, not a broken specification", files, allTests)
	}
}

// ---- conclusive faults ----

func TestIntrinsicTestErrors(t *testing.T) {
	cases := []struct {
		name, out string
		want      bool
	}{
		{"declared and not used", "./store_test.go:12:2: declared and not used: tasks", true},
		{"no new variables", "./store_test.go:14:5: no new variables on left side of :=", true},
		{"imported and not used", `./store_test.go:4:2: "os" imported and not used`, true},
		{"syntax error", "./store_test.go:20:1: syntax error: unexpected }", true},
		{"vet-labelled", "vet: ./store_test.go:12:2: declared and not used: tasks", true},
		{"undefined is not intrinsic", "./store_test.go:9:12: undefined: NewStore", false},
		{"production file", "./main.go:12:2: declared and not used: tasks", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := len(IntrinsicTestErrors(c.out+"\n")) > 0
			if got != c.want {
				t.Fatalf("IntrinsicTestErrors found=%v, want %v", got, c.want)
			}
		})
	}
}

// A CONFLICT BETWEEN TWO TESTS IS THE AUTHOR'S: the developer may not edit
// either file, so no edit it can make compiles.
func TestPackageConflictFiles(t *testing.T) {
	out := "found packages demo (store_test.go) and tracker (board_test.go) in /tmp/work\n"
	got := PackageConflictFiles(out)
	if len(got) != 2 || got[0] != "store_test.go" || got[1] != "board_test.go" {
		t.Fatalf("PackageConflictFiles = %v", got)
	}
	if PackageConflictFiles("no conflict here") != nil {
		t.Fatal("a clean run reported a package conflict")
	}
}

// AND A CONFLICT INVOLVING THE DEVELOPER'S OWN FILE IS THE DEVELOPER'S.
//
// This blamed the author for exactly that. Read off a live hand-back:
//
//	found packages main (board_test.go) and store (store.go)
//
// The tests said "main" and the developer's own store.go said "store" — a file
// it had written one turn earlier and could have corrected in one edit. The
// specification was sound, and a repair attempt was spent telling its author to
// fix a file it does not own.
func TestAPackageConflictWithAnImplementationFileIsNotTheSpecificationsFault(t *testing.T) {
	out := "found packages main (board_test.go) and store (store.go) in /workspace\n"
	if got := PackageConflictFiles(out); got != nil {
		t.Errorf("the author was blamed for %v; store.go is the developer's own file "+
			"and one edit fixes it", got)
	}
}

// ---- panics ----

// THE STACK SETTLES WHOSE FAULT IT IS.
func TestAPanicRaisedOnlyInTestsIsTheSpecificationsFault(t *testing.T) {
	out := `panic: invalid NewRequest arguments; malformed HTTP version "2 HTTP/1.0"

goroutine 6 [running]:
net/http/httptest.NewRequest(...)
	/usr/local/go/src/net/http/httptest/httptest.go:88
demo.TestStatusFiltering.func3()
	handlers_status_test.go:189 +0x1a5
testing.tRunner()
	/usr/local/go/src/testing/testing.go:1690 +0xf4
`
	got := PanicOnlyInTests(out)
	if len(got) != 1 || got[0] != "handlers_status_test.go" {
		t.Fatalf("PanicOnlyInTests = %v, want the test file — every repo frame is "+
			"a test, so no implementation edit can reach it", got)
	}
}

// A PANIC THE IMPLEMENTATION COULD CAUSE puts an implementation frame in the
// trace, and that is the developer's job.
func TestAPanicThroughTheImplementationIsNotTheSpecificationsFault(t *testing.T) {
	out := `panic: assignment to entry in nil map

goroutine 6 [running]:
demo.(*Store).Create(...)
	store.go:41 +0x2c
demo.TestCreate()
	store_test.go:12 +0x88
testing.tRunner()
	/usr/local/go/src/testing/testing.go:1690 +0xf4
`
	if got := PanicOnlyInTests(out); got != nil {
		t.Fatalf("PanicOnlyInTests = %v, want none: store.go is in the trace, so "+
			"the developer can fix it", got)
	}
}

// AN ORDINARY FAILING TEST IS NOT A PANIC. Its log carries a test-file position
// like any stack frame, so without insisting on the panic itself every red test
// on the board would read as the specification's fault — which is the false
// positive that abandons healthy tickets.
func TestNoPanicAtAll(t *testing.T) {
	failing := "--- FAIL: TestCreateRejectsEmptyTitle (0.00s)\n" +
		"    store_test.go:96: Create(\"\") should return an error\n" +
		"--- FAIL: TestListReturnsAll (0.00s)\n" +
		"    store_test.go:112: want 3, got 2\nFAIL\n"

	if got := PanicOnlyInTests(failing); got != nil {
		t.Fatalf("PanicOnlyInTests = %v, want none: these are red tests, which is "+
			"the developer's job and not a defect in the specification", got)
	}
	if got := PanicOnlyInTests("--- FAIL: TestThing\n"); got != nil {
		t.Fatalf("PanicOnlyInTests = %v, want none", got)
	}
}

// The toolchain's own frames must not be mistaken for the implementation, or
// every panic looks like the developer's fault.
func TestToolchainFramesAreNotImplementationFrames(t *testing.T) {
	out := "panic: boom\n\t/usr/local/go/src/testing/testing.go:1690\n\tstore_test.go:12\n"
	if got := PanicOnlyInTests(out); len(got) != 1 {
		t.Fatalf("PanicOnlyInTests = %v, want the test file only", got)
	}
}

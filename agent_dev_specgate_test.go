package main

import (
	"strings"
	"testing"
)

// The assertion that cost r55 its run, copied from the branch rather than
// paraphrased: the message asks for an error and the condition fires when one
// arrives, so a correct implementation fails and only a wrong one passes.
const r55InvertedSpec = `package tracker

import "testing"

func TestStoreReturnsAppropriateErrorsForMissingTasks(t *testing.T) {
	store := NewStore()

	_, err := store.Get("non-existent")
	if err == nil {
		t.Error("Get should return error for non-existent task")
	}

	task := &Task{ID: "non-existent"}
	if err := store.Update("non-existent", task); err != nil {
		t.Errorf("Update should return error for non-existent task, got: %v", err)
	}

	if err := store.Delete("non-existent"); err != nil {
		t.Errorf("Delete should return error for non-existent task, got: %v", err)
	}
}
`

func TestInvertedErrorAssertions_CatchesTheR55Spec(t *testing.T) {
	got := invertedErrorAssertions(map[string]string{"store_operations_test.go": r55InvertedSpec})
	if len(got) != 2 {
		t.Fatalf("want the two inverted assertions (Update, Delete), got %d: %v", len(got), got)
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{"Update should return error", "Delete should return error"} {
		if !strings.Contains(joined, want) {
			t.Errorf("report does not name %q:\n%s", want, joined)
		}
	}
	// The Get check three lines above is written correctly and must survive, or
	// the gate would be teaching the author to break working assertions.
	if strings.Contains(joined, "Get should return error") {
		t.Errorf("flagged the CORRECT Get assertion:\n%s", joined)
	}
	if !strings.Contains(joined, "should be `err == nil`") {
		t.Errorf("report does not say what to change it to:\n%s", joined)
	}
}

func TestInvertedErrorAssertions_LeavesCorrectSpecsAlone(t *testing.T) {
	cases := map[string]string{
		"wants an error, fires when there is none": `package p
import "testing"
func TestA(t *testing.T) {
	if err := f(); err == nil {
		t.Error("should return error for bad input")
	}
}`,
		"wants success, fires when there is an error": `package p
import "testing"
func TestB(t *testing.T) {
	if err := f(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}`,
		// "should not return an error" contains "return an error"; classified by
		// the negative phrasing first, this is correct and must not be flagged.
		"negated wording with the matching condition": `package p
import "testing"
func TestC(t *testing.T) {
	if err := f(); err != nil {
		t.Errorf("Create should not return an error, got: %v", err)
	}
}`,
		"message says nothing about intent": `package p
import "testing"
func TestD(t *testing.T) {
	if err := f(); err != nil {
		t.Errorf("f: %v", err)
	}
}`,
		"not an error comparison at all": `package p
import "testing"
func TestE(t *testing.T) {
	if got := f(); got != 3 {
		t.Errorf("should fail differently: %v", got)
	}
}`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if got := invertedErrorAssertions(map[string]string{"x_test.go": src}); len(got) > 0 {
				t.Errorf("false positive: %v", got)
			}
		})
	}
}

func TestInvertedErrorAssertions_CatchesTheOtherDirection(t *testing.T) {
	// The mirror image: the message says an error is unwanted, but the check
	// only fires when there ISN'T one, so a failing call passes silently.
	src := `package p
import "testing"
func TestF(t *testing.T) {
	if err := f(); err == nil {
		t.Errorf("unexpected error from f: %v", err)
	}
}`
	got := invertedErrorAssertions(map[string]string{"y_test.go": src})
	if len(got) != 1 {
		t.Fatalf("want 1 finding, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "should be `err != nil`") {
		t.Errorf("wrong correction offered: %s", got[0])
	}
}

func TestInvertedErrorAssertions_IgnoresNonTestFiles(t *testing.T) {
	// Implementation files are the developer's to write and are not specifications;
	// this shape is ordinary error handling there, not a contradiction.
	src := `package p
func g() {
	if err := f(); err != nil {
		panic("should fail")
	}
}`
	if got := invertedErrorAssertions(map[string]string{"store.go": src}); len(got) > 0 {
		t.Errorf("gated a non-test file: %v", got)
	}
}

// The route that r57 proved was missing: the developer cannot advance the
// write-and-verify streak when the thing it wants to change is the test file, so
// wanting to change it — repeatedly, while that file is what fails to compile —
// has to be enough on its own.
func TestRepeatedTestFileRefusalsProveTheSpecBroken(t *testing.T) {
	s := &devState{lastCompileBroke: "store_operations_test.go"}
	notice := "Rejected: store_operations_test.go is a test file, and tests are written by a separate agent"

	for i := 1; i < maxTestEditRefusals; i++ {
		s.noProgress(notice)
		if s.specBroken != "" {
			t.Fatalf("concluded after %d refusals; maxTestEditRefusals is %d", i, maxTestEditRefusals)
		}
	}
	s.noProgress(notice)
	if s.specBroken != "store_operations_test.go" {
		t.Errorf("specBroken = %q after %d test-file refusals, want the failing file",
			s.specBroken, maxTestEditRefusals)
	}
}

// Without a verification saying the tests are what will not compile, refusals
// alone prove nothing — the developer may simply be misreading its brief.
func TestTestFileRefusalsAloneDoNotProveTheSpecBroken(t *testing.T) {
	s := &devState{} // no lastCompileBroke: nothing corroborates the refusals
	for i := 0; i < maxTestEditRefusals+3; i++ {
		s.noProgress("Rejected: x_test.go is a test file, and tests are written by a separate agent")
	}
	if s.specBroken != "" {
		t.Errorf("specBroken = %q with no failing compile to corroborate it", s.specBroken)
	}
}

// Refusals of other kinds must not accumulate toward this conclusion.
func TestOtherRefusalsDoNotProveTheSpecBroken(t *testing.T) {
	s := &devState{lastCompileBroke: "store_test.go"}
	for i := 0; i < maxTestEditRefusals+3; i++ {
		s.noProgress("Rejected: your edits produced no change — the file already contained that")
	}
	if s.specBroken != "" {
		t.Errorf("a no-op edit streak concluded the specification was broken: %q", s.specBroken)
	}
	if s.testEditRefusals != 0 {
		t.Errorf("testEditRefusals = %d, want 0", s.testEditRefusals)
	}
}

// A TEST THAT RUNS PROVES THE SPECIFICATION COMPILED. This used to assert the
// opposite — that failing tests plus repeated test-edit attempts proved the spec
// broken — and it fired on an ordinary implementation bug: a nil-map panic in the
// developer's own main.go was reported as "specification is unsatisfiable" and
// blocked the ticket for a person while the spec compiled cleanly.
//
// The case it was admitted for, a spec that compiles and still cannot be
// satisfied, is caught at authoring time by invertedErrorAssertions instead.
func TestAFailingTestRunIsNotEvidenceAgainstTheSpec(t *testing.T) {
	s := &devState{
		lastTest:           "panic: assignment to entry in nil map\n\tmain.go:46",
		testsPass:          false,
		lastTestEditTarget: "store_operations_test.go",
		// No lastCompileBroke: the tests compiled and RAN.
	}
	notice := "Rejected: store_operations_test.go is a test file, and tests are written by a separate agent"
	for range maxTestEditRefusals + 3 {
		s.noProgress(notice)
	}
	if s.specBroken != "" {
		t.Errorf("a runtime failure in the implementation blamed the spec: %q", s.specBroken)
	}
}

// Passing tests are not evidence of anything wrong: a developer poking at a test
// file while the suite is green is simply out of bounds.
func TestPassingTestsDoNotProveTheSpecBroken(t *testing.T) {
	s := &devState{lastTest: "ok tracker", testsPass: true}
	for i := 0; i < maxTestEditRefusals+3; i++ {
		s.noProgress("Rejected: x_test.go is a test file, and tests are written by a separate agent")
	}
	if s.specBroken != "" {
		t.Errorf("specBroken = %q while the tests were passing", s.specBroken)
	}
}

// Nor is a suite that has never run.
func TestUntestedTreeDoesNotProveTheSpecBroken(t *testing.T) {
	s := &devState{} // no lastTest at all
	for i := 0; i < maxTestEditRefusals+3; i++ {
		s.noProgress("Rejected: x_test.go is a test file, and tests are written by a separate agent")
	}
	if s.specBroken != "" {
		t.Errorf("specBroken = %q with no test result at all", s.specBroken)
	}
}

func TestTestFileInNotice(t *testing.T) {
	cases := map[string]string{
		"Rejected: store_operations_test.go is a test file, and tests are written by a separate agent": "store_operations_test.go",
		"Rejected: a/b/store_test.go is a test file, and tests are written":                            "a/b/store_test.go",
		"Rejected: main.go is a doc file":                                                              "",
		"Rejected: your edits produced no change":                                                      "",
	}
	for notice, want := range cases {
		if got := testFileInNotice(notice); got != want {
			t.Errorf("testFileInNotice(%.50q) = %q, want %q", notice, got, want)
		}
	}
}

func TestSpecFaultKindDistinguishesCompileFromFailure(t *testing.T) {
	if got := specFaultKind("x_test.go"); !strings.Contains(got, "COMPILE") {
		t.Errorf("compile fault described as %q", got)
	}
	if got := specFaultKind(""); strings.Contains(got, "COMPILE") {
		t.Errorf("a failing-but-compiling spec was described as a compile fault: %q", got)
	}
}

// r57 and r62 both spent every attempt on a verdict already visible in the first
// test run. A fault local to the test file needs no streak: no implementation
// change can affect it.
func TestIntrinsicTestErrors(t *testing.T) {
	conclusive := []string{
		"./store_operations_test.go:185:9: no new variables on left side of :=",
		"./store_operations_test.go:36:3: declared and not used: tasks",
		"store_test.go:4:2: \"os\" imported and not used",
		"./x_test.go:12:1: syntax error: unexpected }",
	}
	for _, out := range conclusive {
		if got := intrinsicTestErrors(out); len(got) != 1 {
			t.Errorf("not treated as conclusive: %q -> %v", out, got)
		}
	}

	// The implementation's job, and the expected red state of a test-first ticket.
	// Calling these a broken specification abandons tickets that are nearly done —
	// the measured *time.Time case.
	devsFault := []string{
		"./store_test.go:11:14: undefined: NewStore",
		"./store_test.go:27:18: cannot use time.Now().Add(time.Hour) (value of struct type time.Time) as *time.Time value in struct literal",
		"./store_test.go:9:2: not enough arguments in call to store.Add",
	}
	for _, out := range devsFault {
		if got := intrinsicTestErrors(out); len(got) != 0 {
			t.Errorf("blamed the specification for the implementation's fault: %q -> %v", out, got)
		}
	}

	// A fault in an implementation file is not the specification's problem at all.
	if got := intrinsicTestErrors("./store.go:8:2: declared and not used: x"); len(got) != 0 {
		t.Errorf("flagged a non-test file: %v", got)
	}
}

// The caller classifies a specification's failure by READING this script's
// output, so the output has to exist on the failing path. It used to be printed
// only when the tests passed, which left nonUndefinedCompileErrors with nothing to
// read and let a spec whose own tests would not compile pass as valid red.
func TestSpecScriptPrintsTheFailureOutput(t *testing.T) {
	a := &DevAgent{repo: RepoConfig{TestCommand: "go test ./..."}}
	script := a.testSpecScript(false)

	echoAt := strings.Index(script, `echo "$out"`)
	if echoAt < 0 {
		t.Fatal("the script never echoes the captured output; the caller cannot classify the failure")
	}
	guardAt := strings.Index(script, `if [ "$rc" -eq 0 ]`)
	if guardAt < 0 {
		t.Fatal("the vacuous-spec guard is gone")
	}
	if echoAt > guardAt {
		t.Error("the output is echoed INSIDE or after the pass-only branch, so a failing spec prints nothing")
	}
}

// With no test command there is nothing to run, and the parse check stands alone.
func TestSpecScriptWithoutATestCommandIsParseOnly(t *testing.T) {
	a := &DevAgent{}
	if got := a.testSpecScript(false); got != testParseScript {
		t.Errorf("expected the parse script alone, got %d extra bytes", len(got)-len(testParseScript))
	}
}

// A REPAIR MUST NOT BE HELD TO THE RED GATE. A specification handed back by the
// developer arrives on a branch where the implementation already exists, so the
// moment the author fixes what it was sent back for, its tests pass — and the
// gate then tells it that it has specified nothing. Measured on the dev fixture:
// the author corrected the compile error, was refused for the tests passing, and
// had no move that could satisfy both demands.
func TestARepairIsNotHeldToTheRedGate(t *testing.T) {
	a := &DevAgent{repo: RepoConfig{TestCommand: "go test ./..."}}

	fresh := a.testSpecScript(false)
	if !strings.Contains(fresh, "must fail against the current code") {
		t.Error("a fresh specification is no longer required to fail; it could merge unverified")
	}

	repair := a.testSpecScript(true)
	if strings.Contains(repair, "must fail against the current code") {
		t.Errorf("a repair is still required to fail against code that already exists:\n%s", repair)
	}
	// It must still COMPILE them. Dropping the test command along with the red
	// requirement was the first attempt at this, and it removed the only check that
	// catches the fault a specification is sent back for: gofmt finds syntax errors
	// and not type errors like "no new variables on left side of :=". The author
	// passed its gate, pushed, and the developer found the identical error.
	if !strings.Contains(repair, "gofmt") {
		t.Errorf("a repair skips the parse check:\n%s", repair)
	}
	if !strings.Contains(repair, a.repo.TestCommand) {
		t.Errorf("a repair never compiles the tests, so a type error would survive it:\n%s", repair)
	}
	if !strings.Contains(repair, `echo "$out"`) {
		t.Errorf("a repair discards the output, so nothing can classify the failure:\n%s", repair)
	}
}

// "undefined: NewStore" is what a specification written before its implementation
// MUST produce. A developer poking at the test file while that is the only failure
// is out of bounds, not onto something — and the widened evidence route fired on
// exactly that, handing back a perfectly healthy spec twice on the implementation
// fixture.
func TestTheExpectedRedStateIsNotEvidenceOfABrokenSpec(t *testing.T) {
	s := &devState{
		lastTest:      "./store_test.go:12:13: undefined: Store",
		testsPass:     false,
		redIsExpected: true, // set at verification: only "undefined:" errors
	}
	notice := "Rejected: store_test.go is a test file, and tests are written by a separate agent"
	for range maxTestEditRefusals + 3 {
		s.noProgress(notice)
	}
	if s.specBroken != "" {
		t.Errorf("a healthy spec was declared broken on the expected red state: %q", s.specBroken)
	}
}

// ...but a real fault still is, on the same number of attempts.
func TestARealFaultIsStillEvidenceWhenTheRedIsNotExpected(t *testing.T) {
	s := &devState{
		lastTest:  "./store_test.go:36:3: declared and not used: tasks",
		testsPass: false,
		// A real test-file compile fault, which is what the verification records.
		redIsExpected:      false,
		lastCompileBroke:   "store_test.go",
		lastTestEditTarget: "store_test.go",
	}
	notice := "Rejected: store_test.go is a test file, and tests are written by a separate agent"
	for range maxTestEditRefusals {
		s.noProgress(notice)
	}
	if s.specBroken != "store_test.go" {
		t.Errorf("specBroken = %q, want the suspect file", s.specBroken)
	}
}

// The STREAK route needs the same gate as the refusal route, and gating only one
// of them half-fixed the false positive: "undefined: NewStore" persists across
// every verification until the implementation is finished, so a developer three
// writes into a large ticket trips minSpecBrokenTries on a sound specification.
func TestTheStreakRouteAlsoIgnoresTheExpectedRedState(t *testing.T) {
	s := &devState{
		lastTest: "./store_test.go:12:13: undefined: Store", testsPass: false,
		redIsExpected:    true,
		specBrokenTries:  minSpecBrokenTries + 2, // well past the streak threshold
		lastCompileBroke: "store_test.go",
	}
	// Whatever the streak says, expected red is not evidence.
	if s.testEditRefusals >= maxTestEditRefusals {
		t.Fatal("fixture assumes no test-edit refusals")
	}
	notice := "Rejected: store_test.go is a test file, and tests are written by a separate agent"
	for range maxTestEditRefusals + 2 {
		s.noProgress(notice)
	}
	if s.specBroken != "" {
		t.Errorf("a sound spec was declared broken on expected red with a long streak: %q", s.specBroken)
	}
}

// "undefined: X" is the expected red state ONLY for an unqualified X. A dotted
// name is a package selector that no code in this package can define — the test
// meant to import sync/atomic and wrote sync.atomic instead.
//
// Measured on the implementation fixture: the developer wrote a correct 138-line
// implementation, the sole remaining error was `undefined: sync.atomic`, and the
// hand-back was suppressed purely because the line began with "undefined:".
func TestADottedUndefinedNameIsASpecFaultNotExpectedRed(t *testing.T) {
	expectedRed := []string{
		"./store_test.go:12:13: undefined: Store",
		"./store_test.go:24:14: undefined: Task",
		"./store_test.go:43:29: undefined: NewStore",
	}
	for _, line := range expectedRed {
		if got := nonUndefinedCompileErrors(line); len(got) != 0 {
			t.Errorf("expected red treated as a fault: %q -> %v", line, got)
		}
	}

	specFaults := []string{
		"./store_test.go:169:10: undefined: sync.atomic",
		"./store_test.go:12:2: undefined: rand.Shuffle",
	}
	for _, line := range specFaults {
		if got := nonUndefinedCompileErrors(line); len(got) != 1 {
			t.Errorf("a package selector no implementation can define was excused: %q -> %v", line, got)
		}
	}
}

func TestUndefinedSymbolExtraction(t *testing.T) {
	cases := map[string]string{
		"./a_test.go:1:2: undefined: Store":            "Store",
		"./a_test.go:1:2: undefined: sync.atomic":      "sync.atomic",
		"./a_test.go:1:2: undefined: NewStore (typo?)": "NewStore",
		"./a_test.go:1:2: declared and not used: x":    "",
	}
	for line, want := range cases {
		if got := undefinedSymbol(line); got != want {
			t.Errorf("undefinedSymbol(%q) = %q, want %q", line, got, want)
		}
	}
}

// A SECTION WHOSE SUBJECT A SIBLING ALREADY BUILT MUST NOT BE HELD TO THE RED
// GATE EITHER. The sections of one task share a branch, so whichever lands first
// leaves its implementation there for the rest — and the next author to write a
// specification of that same code finds its tests green through no fault of its
// own. Measured on r67: "Implement basic task operations" was speccing a store
// the sibling "concurrent access control" section had already built and merged.
// The author diagnosed it correctly every turn and had no legal move, because
// the only route to red was a test it knew to be wrong. It rewrote the correct
// file instead — byte-identical, so a no-op refusal — 18 times.
func TestASpecOfAlreadyImplementedCodeIsNotHeldToTheRedGate(t *testing.T) {
	a := &DevAgent{repo: RepoConfig{TestCommand: "go test ./...", Branch: "dev"}}
	script := a.testSpecScript(false)

	// The green path must ASK THE TREE which kind of green this is, rather than
	// assuming the implementation cannot exist yet.
	//
	// It must ask whether the code EXISTS, not whether this branch added it. The
	// first version diffed the branch against its base, which misses the case where
	// a sibling section already MERGED — measured on r68, where main.go held the
	// whole Store and the branch diff showed one test file.
	if !strings.Contains(script, "exported declaration") {
		t.Errorf("the script never asks whether the subject is already built:\n%s", script)
	}
	if strings.Contains(script, "git diff --name-only origin/") {
		t.Error("the script asks whether THIS BRANCH added an implementation, which misses a merged sibling")
	}
	if !strings.Contains(script, alreadyImplementedMarker) {
		t.Error("the script cannot report an already-implemented subject, so green is always called vacuous")
	}
	// And it must exclude the tests themselves from that question: an author's own
	// test files always declare exported Test functions, so counting them would
	// exempt every specification ever written.
	if !strings.Contains(script, "grep -v '_test[.]go$'") {
		t.Errorf("test files are not excluded, so every specification looks implemented:\n%s", script)
	}
	// The vacuous case must SURVIVE — this is a narrowing, not a removal.
	if !strings.Contains(script, vacuousSpecMarker) {
		t.Error("the vacuous-spec guard is gone; a specification asserting nothing could now merge")
	}
}

// The two markers must stay distinguishable, since they lead to opposite verdicts:
// one ends the stage as finished work, the other sends the author back.
func TestSpecMarkersAreDistinct(t *testing.T) {
	if alreadyImplementedMarker == vacuousSpecMarker {
		t.Fatal("the markers are identical; green cannot be classified")
	}
	if strings.Contains(alreadyImplementedMarker, vacuousSpecMarker) ||
		strings.Contains(vacuousSpecMarker, alreadyImplementedMarker) {
		t.Error("one marker contains the other, so a substring match classifies green wrongly")
	}
}

// A PANIC RAISED ENTIRELY INSIDE THE TESTS IS THE SPECIFICATION'S FAULT, and the
// developer must be able to hand it back. Runtime failures were kept out of the
// evidence deliberately — a panic is normally the implementation misbehaving —
// but r68 stalled a section on a test that panicked in its own fixture setup:
// httptest.NewRequest with an unescaped space in the query string, so "2 HTTP/1.0"
// parsed as the HTTP version. The tests compiled, so the compile route was blind
// to it, and the developer broke main.go repeatedly trying to compensate.
func TestAPanicInsideTheTestsIsTheSpecsFault(t *testing.T) {
	// The real trace, trimmed to the frames that matter.
	out := `--- FAIL: TestStatusParameterFiltering (0.00s)
panic: invalid NewRequest arguments; malformed HTTP version "2 HTTP/1.0" [recovered, repanicked]

goroutine 15 [running]:
testing.tRunner.func1.2({0x636500, 0xc000030730})
	/home/u/.local/share/mise/installs/go/1.25.6/src/testing/testing.go:1872 +0x237
net/http/httptest.NewRequestWithContext({0x6ecc90, 0x8d5aa0})
	/home/u/.local/share/mise/installs/go/1.25.6/src/net/http/httptest/httptest.go:52 +0x714
tracker.TestStatusParameterFiltering.func3(0xc0000e1500)
	/work/repo/handlers_status_test.go:189 +0x34e`

	got := panicOnlyInTests(out)
	if len(got) != 1 || got[0] != "handlers_status_test.go" {
		t.Fatalf("the test's own panic was not recognised: %v", got)
	}
	if k := specFaultKindFor("", true); !strings.Contains(k, "PANIC") {
		t.Errorf("the hand-back describes it as an assertion failure: %q", k)
	}
}

// AND A PANIC THE IMPLEMENTATION CAUSED MUST NOT BE. This is the case the
// exclusion existed for: a nil map, a bad index, anything in the code under test.
// The trace names an implementation file, and that is what tells them apart.
func TestAPanicFromTheImplementationIsNotHandedBack(t *testing.T) {
	out := `panic: assignment to entry in nil map

goroutine 6 [running]:
tracker.(*Store).Add(0xc000010030)
	/work/repo/main.go:42 +0x55
tracker.TestStoreAdd(0xc0000e1500)
	/work/repo/store_operations_test.go:17 +0x88`

	if got := panicOnlyInTests(out); got != nil {
		t.Errorf("a panic inside main.go was blamed on the specification: %v", got)
	}
}

// No panic at all is not evidence of anything.
func TestPlainFailuresAreNotPanics(t *testing.T) {
	out := "--- FAIL: TestThing (0.00s)\n    thing_test.go:12: got 3, want 4\nFAIL"
	if got := panicOnlyInTests(out); got != nil {
		t.Errorf("an ordinary failing assertion was read as a panic: %v", got)
	}
}

// A MESSAGE THAT POINTS AWAY FROM ITS CAUSE COSTS MORE THAN ITS FREQUENCY. The
// agent's reasoning is sound, it acts on what the error says, and no number of
// retries converges because every attempt aims at the wrong argument.
//
// Measured on r68 in two different sections: a test built
// httptest.NewRequest("GET", "/api/tasks?q=Task 1", nil), Go reported
// `malformed HTTP version "1 HTTP/1.0"`, and the author concluded both times that
// "the method parameter is being" passed wrongly. The method was fine.
func TestMisleadingFailuresAreTranslated(t *testing.T) {
	out := `panic: invalid NewRequest arguments; malformed HTTP version "1 HTTP/1.0"`
	e := explainConfusingFailure(out)
	if e == "" {
		t.Fatal("the misleading httptest error is not translated")
	}
	if !strings.Contains(e, "raw space") {
		t.Errorf("the explanation does not name the real cause: %q", e)
	}
	// It must actively steer AWAY from the wrong fix the agent kept attempting.
	if !strings.Contains(e, "leave the method alone") {
		t.Errorf("nothing rules out the argument the agent kept blaming: %q", e)
	}
	if !strings.Contains(explainSuffix(out), "raw space") {
		t.Error("the explanation is not appended to the hand-back")
	}
}

// An ordinary failure explains itself and must not be embellished.
func TestPlainFailuresAreNotTranslated(t *testing.T) {
	out := "--- FAIL: TestThing\n    thing_test.go:12: got 3, want 4"
	if e := explainConfusingFailure(out); e != "" {
		t.Errorf("an ordinary failure was given a spurious explanation: %q", e)
	}
	if s := explainSuffix(out); s != "" {
		t.Errorf("suffix added to a clear failure: %q", s)
	}
}

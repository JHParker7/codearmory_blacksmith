package gate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runSpec executes the author's gate against a real tree, under `set -e` as the
// sandbox preamble sets it. A SCRIPT THAT IS ONLY ASSERTED ON AS A STRING is a
// script nobody ran: three separate faults here were shell behaviour, not text.
func runSpec(t *testing.T, files map[string]string, testCommand string, repairing bool) (string, bool) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not available")
	}
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", "-c", "set -e\n"+SpecScript(testCommand, repairing))
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

// A stub `go` that reports whatever the fixture wants, so these tests do not
// depend on a toolchain or on a real suite.
func fakeTest(t *testing.T, exitCode int, output string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "faketest")
	body := "#!/bin/sh\ncat <<'EOF'\n" + output + "\nEOF\nexit " + itoa(exitCode) + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

const goodSpec = `package main

import "testing"

func TestListReturnsOnlyDone(t *testing.T) {
	if got := List(Filter{Done: true}); len(got) != 1 {
		t.Fatalf("got %d", len(got))
	}
}
`

// RED IS THE POINT OF A TEST-FIRST STAGE, and it is the ONLY outcome that
// passes this gate.
func TestASpecificationThatFailsIsTheOneThatPasses(t *testing.T) {
	cmd := fakeTest(t, 1, "# command-line-arguments\n./store_test.go:6:12: undefined: List")
	out, ok := runSpec(t, map[string]string{"store_test.go": goodSpec}, cmd, false)

	if !ok {
		t.Fatalf("a correctly red specification was refused:\n%s", out)
	}
	if got := ReadSpec(out, ok); got != SpecRed {
		t.Errorf("verdict = %v, want SpecRed\n%s", got, out)
	}
	if !strings.Contains(out, "fails as it should") {
		t.Errorf("the gate does not say the red state is wanted:\n%s", out)
	}
	// THE FAILURE TEXT IS ALWAYS PRINTED, on the red path too: the caller reads it
	// to separate "undefined: List" — expected — from a fault in the tests
	// themselves. Discarding it on this path left that check with nothing to read.
	if !strings.Contains(out, "undefined: List") {
		t.Errorf("the failing output was discarded on the red path:\n%s", out)
	}
}

// A SPECIFICATION THAT PASSES BEFORE THE IMPLEMENTATION EXISTS IS NOT A
// SPECIFICATION. The cost lands two stages later: the developer's gate passes on
// the first commit because there is nothing to satisfy, the reviewer sees green,
// and unverified code merges looking exactly like verified code.
func TestASuiteThatAssertsNothingIsRefused(t *testing.T) {
	vacuous := "package main\n\nimport \"testing\"\n\nfunc TestNothing(t *testing.T) {}\n"
	cmd := fakeTest(t, 0, "ok  \texample\t0.002s")

	// A PRISTINE BASELINE: package main with a single func main and nothing
	// exported.
	out, ok := runSpec(t, map[string]string{
		"main_test.go": vacuous,
		"main.go":      "package main\n\nfunc main() {}\n",
	}, cmd, false)

	if ok {
		t.Fatalf("a vacuous specification passed its gate:\n%s", out)
	}
	if got := ReadSpec(out, ok); got != SpecVacuous {
		t.Errorf("verdict = %v, want SpecVacuous\n%s", got, out)
	}
}

// GREEN HAS TWO CAUSES AND THEY NEED OPPOSITE ANSWERS.
//
// r67: "Implement basic task operations" was speccing a store its sibling
// section had already built. The author diagnosed it correctly every turn and
// had no move — the only way to go red was to write a test it knew to be wrong —
// so it rewrote the correct file 18 times, which is a no-op refusal each time.
func TestASuiteThatPassesBecauseTheCodeExistsIsFinishedWork(t *testing.T) {
	cmd := fakeTest(t, 0, "ok  \texample\t0.002s")
	impl := `package main

type Task struct{ ID string }

type Store struct{ tasks []Task }

func NewStore() *Store { return &Store{} }

func (s *Store) Add(t Task) {}

func (s *Store) List() []Task { return s.tasks }
`
	out, ok := runSpec(t, map[string]string{"store_test.go": goodSpec, "store.go": impl}, cmd, false)

	if !ok {
		t.Fatalf("finished work was refused:\n%s", out)
	}
	if got := ReadSpec(out, ok); got != SpecAlreadyBuilt {
		t.Errorf("verdict = %v, want SpecAlreadyBuilt\n%s", got, out)
	}
	if !strings.Contains(out, "already built") {
		t.Errorf("the gate does not say why the green is acceptable:\n%s", out)
	}
}

// THE QUESTION IS WHETHER THE CODE EXISTS, NOT WHERE IT CAME FROM.
//
// The first version diffed the branch against its base, which answers a
// different question and is wrong by exactly one route: a sibling's work reaches
// an author either still on the shared branch, where a diff sees it, or already
// MERGED to the base, where a diff cannot. r68 hit the second — main.go held
// eight exported declarations, the branch diff showed one test file, the
// exemption missed, and the author looped 60 times.
func TestCodeAlreadyMergedToTheBaseStillCountsAsBuilt(t *testing.T) {
	cmd := fakeTest(t, 0, "ok")
	// Nothing here is new on a branch: it is simply what the tree holds.
	merged := "package main\n\ntype Task struct{}\n\nfunc NewStore() {}\n\nfunc (s *store) Add() {}\n"

	out, ok := runSpec(t, map[string]string{"main_test.go": goodSpec, "main.go": merged}, cmd, false)
	if got := ReadSpec(out, ok); got != SpecAlreadyBuilt {
		t.Errorf("verdict = %v; merged code was not recognised as built\n%s", got, out)
	}
}

// A STUB IS NOT AN IMPLEMENTATION. Only EXPORTED declarations count, because the
// pristine baseline is package main with a single unexported func main — so a
// stub reads as zero and any real implementation reads as more.
func TestUnexportedScaffoldingDoesNotCountAsAnImplementation(t *testing.T) {
	cmd := fakeTest(t, 0, "ok")
	stub := "package main\n\nfunc main() {}\n\nfunc helper() {}\n\ntype config struct{}\n"

	out, ok := runSpec(t, map[string]string{"main_test.go": goodSpec, "main.go": stub}, cmd, false)
	if got := ReadSpec(out, ok); got != SpecVacuous {
		t.Errorf("verdict = %v; a stub was accepted as an implementation\n%s", got, out)
	}
}

// REPAIRING RELAXES THE RED REQUIREMENT, NOT THE COMPILE CHECK.
//
// Conflating them was the first version and it was worse than the problem:
// skipping the test command meant nothing compiled the tests, and gofmt catches
// syntax errors but NOT "no new variables on left side of :=" — exactly the
// fault the developer sends a specification back for. Measured immediately: the
// author passed its gate, pushed, and the developer handed it straight back.
func TestARepairMustStillCompileEvenThoughItNeedNotFail(t *testing.T) {
	t.Run("a green repair is accepted", func(t *testing.T) {
		cmd := fakeTest(t, 0, "ok  \texample\t0.01s")
		out, ok := runSpec(t, map[string]string{"store_test.go": goodSpec}, cmd, true)
		if !ok {
			t.Fatalf("a repair was refused for passing:\n%s", out)
		}
		if !strings.Contains(out, "only compilation is required") {
			t.Errorf("the repair path does not say what it demands:\n%s", out)
		}
	})

	t.Run("the test command still runs and its output is kept", func(t *testing.T) {
		cmd := fakeTest(t, 2, "./store_test.go:9:6: no new variables on left side of :=")
		out, _ := runSpec(t, map[string]string{"store_test.go": goodSpec}, cmd, true)
		if !strings.Contains(out, "no new variables on left side of :=") {
			t.Errorf("a repair skipped the compile check:\n%s", out)
		}
	})
}

// A SUITE THAT DOES NOT PARSE IS THE AUTHOR'S OWN MISTAKE.
func TestTestsThatDoNotParseAreRefusedWithTheReason(t *testing.T) {
	broken := "package main\n\nfunc TestBroken(t *testing.T) {\n"
	out, ok := runSpec(t, map[string]string{"store_test.go": broken}, "", false)

	if ok {
		t.Fatalf("a file that does not parse passed:\n%s", out)
	}
	if got := ReadSpec(out, ok); got != SpecBroken {
		t.Errorf("verdict = %v, want SpecBroken", got)
	}
	// THE ERROR TEXT MUST SURVIVE. Without the "|| true" the script dies on the
	// assignment under set -e, the text is never echoed, and the stage reports a
	// failure with an empty body — observed exactly that way: an author told "the
	// tests do not parse" with nothing after it rewrote the file blindly until
	// its budget ran out.
	if !strings.Contains(out, "store_test.go") {
		t.Errorf("the parse error was swallowed; the author has nothing to act on:\n%s", out)
	}
}

// FORMATTING ALONE MUST NOT FAIL THE STAGE — the commit hook already runs the
// formatter, and gofmt reports an unformatted file the same way it reports one
// it could not read.
func TestBadlyFormattedButValidTestsAreNotRefused(t *testing.T) {
	ugly := "package main\nimport \"testing\"\nfunc TestX(t *testing.T){\nif  1==2  {\nt.Fatal(\"no\")\n}\n}\n"
	out, ok := runSpec(t, map[string]string{"store_test.go": ugly}, "", false)
	if !ok {
		t.Errorf("valid but unformatted tests were refused:\n%s", out)
	}
}

func TestAnAuthorThatWroteNothingIsToldSo(t *testing.T) {
	out, ok := runSpec(t, map[string]string{"main.go": "package main\n\nfunc main() {}\n"}, "", false)
	if ok {
		t.Fatal("an author that wrote no tests passed its gate")
	}
	if !strings.Contains(out, "no test files were written") {
		t.Errorf("the refusal does not name the cause:\n%s", out)
	}
}

// GOFMT IS GIVEN THE TEST FILES, NEVER ".". Measured on r92: 8.5 seconds per
// section against 512ms for the developer's gate, which runs the same preamble
// AND the whole suite. The difference was "." descending the tree.
//
// ASSERTED BY BEHAVIOUR RATHER THAN BY THE LITERAL GLOB. This used to require
// the exact string "gofmt -e -l *_test.go", which pinned one spelling of the fix
// rather than the property — and that spelling was wrong: it globs the ROOT, so
// a specification written into a package directory was invisible to it.
func TestTheParseCheckDoesNotWalkTheWholeTree(t *testing.T) {
	if strings.Contains(SpecParseScript, "gofmt -e -l .") {
		t.Error("the parse check walks the tree; it took 8.5s per section that way")
	}

	// A badly formatted NON-test file must not reach gofmt, or the author is
	// failed for a file it may not edit.
	dir := t.TempDir()
	writeFile(t, dir+"/store.go", "package main\nfunc  X( ) {\n}\n")
	writeFile(t, dir+"/store_test.go", "package main\n\nfunc TestX() {}\n")

	out, ok := runIn(t, dir, SpecParseScript)
	if !ok {
		t.Fatalf("a well-formed specification failed its parse check:\n%s", out)
	}
}

// AND IT FINDS TESTS WHEREVER THE ARCHITECT PUT THEM.
//
// Read off run 71. The architect designed packages rather than one flat main,
// the author wrote ticket/types_test.go, and the root glob found nothing — so
// the gate answered "no test files were written" in the same prompt that listed
// ticket/types_test.go as already on the branch. The author rewrote the
// identical 931-token file 101 times against a check it could never pass.
func TestTheParseCheckFindsTestsInSubdirectories(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir+"/ticket", 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir+"/ticket/types_test.go", "package ticket\n\nfunc TestX() {}\n")

	out, ok := runIn(t, dir, SpecParseScript)
	if !ok {
		t.Fatalf("a specification in a package directory failed its own gate:\n%s", out)
	}
	if strings.Contains(out, "no test files were written") {
		t.Errorf("tests in a subdirectory were reported as absent:\n%s", out)
	}
	if !strings.Contains(out, "types_test.go") {
		t.Errorf("the gate did not name the test it found:\n%s", out)
	}
}

// AND A PARSE ERROR IN A SUBDIRECTORY IS STILL CAUGHT. Widening the search must
// not widen it past the point of checking what it finds.
func TestAParseErrorInASubdirectoryIsStillCaught(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir+"/ticket", 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir+"/ticket/types_test.go", "package ticket\n\nfunc TestX() {\n")

	out, ok := runIn(t, dir, SpecParseScript)
	if ok {
		t.Errorf("a test file that does not parse passed the gate:\n%s", out)
	}
}

// writeFile puts one file on disk for a gate script to look at.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// runIn executes a gate script in dir, because the thing under test is shell.
func runIn(t *testing.T, dir, script string) (string, bool) {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

// EVERY SPLICED COMMAND MUST BE SAFE UNDER `set -e`, which the sandbox preamble
// sets. A test command that exits non-zero is the NORMAL case here, not an
// exceptional one.
func TestTheScriptSurvivesATestCommandThatExitsNonZero(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not available")
	}
	for _, repairing := range []bool{false, true} {
		for _, cmd := range []string{"false", "(exit 3)", "echo failing; false"} {
			script := SpecScript(cmd, repairing)
			if o, err := exec.Command("sh", "-n", "-c", script).CombinedOutput(); err != nil {
				t.Errorf("repairing=%v %q: not valid shell: %v\n%s", repairing, cmd, err, o)
			}
		}
	}
}

// AN EMPTY TEST COMMAND MEANS THERE IS NOTHING TO RUN, so the gate checks only
// that the tests parse rather than demanding a red it cannot observe.
func TestWithNoTestCommandOnlyTheParseCheckRuns(t *testing.T) {
	got := SpecScript("", false)
	if got != SpecParseScript {
		t.Error("a repository with no test command was given a red requirement it cannot meet")
	}
}

func TestTheVerdictPrefersTheMoreSpecificMarker(t *testing.T) {
	// A suite whose own output happens to contain the vacuous marker must not
	// override the harness's own later verdict that the code is already built.
	both := "some test printed " + SpecVacuousMarker + "\n" + SpecAlreadyBuiltMarker
	if got := ReadSpec(both, true); got != SpecAlreadyBuilt {
		t.Errorf("verdict = %v, want SpecAlreadyBuilt", got)
	}
	if got := ReadSpec("plain failure", false); got != SpecBroken {
		t.Errorf("verdict = %v, want SpecBroken", got)
	}
	if got := ReadSpec("all good", true); got != SpecRed {
		t.Errorf("verdict = %v, want SpecRed", got)
	}
}

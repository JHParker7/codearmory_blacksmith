package gate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// run executes a generated script in a real shell, in a directory the test set
// up.
//
// THE SCRIPT IS THE PRODUCT of this package, and the bugs it has had were all in
// the shell rather than in the Go around it: a redirect binding to the wrong half
// of a chain, a quote closed by an apostrophe. Asserting on the string would have
// caught none of them.
func run(t *testing.T, dir, script string) (out string, ok bool) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not available")
	}

	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = dir
	b, err := cmd.CombinedOutput()
	return string(b), err == nil
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestEveryGeneratedScriptIsValidShell(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not available")
	}
	scripts := map[string]string{
		"test":        TestScript("go test ./..."),
		"test import": TestImportScript(),
		"full": Script(Options{
			LintCommand:     "go vet ./...",
			TestCommand:     "go test ./...",
			CriticalCommand: "gosec ./...",
		}),
		// A command carrying an apostrophe: prose in a repository's own scripts is
		// not unusual, and an unbalanced quote makes the shell exit on a syntax
		// error with nothing run and nothing to explain it.
		"apostrophe": TestScript("echo \"this project's tests\" && go test ./..."),
	}
	for name, s := range scripts {
		if out, err := exec.Command("sh", "-n", "-c", s).CombinedOutput(); err != nil {
			t.Errorf("%s script is not valid shell: %v\n%s", name, err, out)
		}
	}
}

func TestAPassingTestCommandLeavesNoMarker(t *testing.T) {
	dir := t.TempDir()
	out, ok := run(t, dir, TestScript("echo 'ok  	store	0.1s'; true"))
	if !ok {
		t.Fatalf("a passing command failed the gate:\n%s", out)
	}
	if !strings.Contains(out, "ok  	store") {
		t.Errorf("the command's own output was lost:\n%s", out)
	}
	if got := Failed(out); got != ReasonUnknown {
		t.Errorf("a passing run reported gate %q", got)
	}
}

// THE FAILING ASSERTION'S SOURCE IS QUOTED, so the developer does not spend a
// whole model round trip fetching five lines the sandbox is standing next to.
func TestAFailingTestQuotesTheSourceItPointsAt(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "store_test.go", strings.Join([]string{
		"package store",                // 1
		"",                             // 2
		"import \"testing\"",           // 3
		"",                             // 4
		"func TestAdd(t *testing.T) {", // 5
		"\tif got := 0; got != 2 {",    // 6
		"\t\tt.Errorf(\"want 2\")",     // 7
		"\t}",                          // 8
		"}",                            // 9
	}, "\n"))

	script := TestScript("echo 'store_test.go:7: want 2, got 0'; false")
	out, ok := run(t, dir, script)
	if ok {
		t.Fatalf("a failing command passed the gate:\n%s", out)
	}
	if !strings.Contains(out, "=== store_test.go around line 7 ===") {
		t.Fatalf("the failing file was not quoted:\n%s", out)
	}
	// The asserting line itself, with its number, is the whole point.
	if !strings.Contains(out, "t.Errorf(\"want 2\")") {
		t.Errorf("the assertion was not shown:\n%s", out)
	}
	// And it must say the developer may not edit it.
	if !strings.Contains(out, "you may not edit") {
		t.Errorf("the quoted source does not say it is off limits:\n%s", out)
	}
	if got := Failed(out); got != ReasonTest {
		t.Errorf("Failed() = %q, want %q", got, ReasonTest)
	}
}

// THE REDIRECT BINDS TO THE LAST ELEMENT OF AN && CHAIN, not to the whole of it.
// Unbraced, a build failure escaped to the console, the test never ran, the
// output file was never created, and the agent was handed "No such file or
// directory" above its real error — a refusal pointing at nothing.
func TestABuildFailureInAChainIsCapturedNotLost(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "main.go", "package main\n\nfunc main() {\n\tundefined()\n}\n")

	// The first half fails, so the second never runs. Both halves' output must
	// still be captured.
	script := TestScript("echo 'main.go:4: undefined: undefined' >&2; false && go test ./...")
	out, ok := run(t, dir, script)
	if ok {
		t.Fatalf("a failing chain passed the gate:\n%s", out)
	}
	if strings.Contains(out, "No such file") {
		t.Fatalf("the output file was never written; the redirect bound to the wrong half:\n%s", out)
	}
	if !strings.Contains(out, "undefined: undefined") {
		t.Errorf("the build error was lost:\n%s", out)
	}
	// And because it was captured, the source quoting can find the file it names.
	if !strings.Contains(out, "=== main.go around line 4 ===") {
		t.Errorf("the build error's file was not quoted:\n%s", out)
	}
}

// A GATE THAT NAMES A FILE THAT DOES NOT EXIST must skip it rather than fail
// noisily: the failure output can name a path from a dependency's source.
func TestAQuotedPathThatIsNotInTheRepositoryIsSkipped(t *testing.T) {
	dir := t.TempDir()
	out, ok := run(t, dir, TestScript("echo '/go/pkg/mod/example.com/dep@v1/thing.go:22: boom'; false"))
	if ok {
		t.Fatal("a failing command passed the gate")
	}
	if strings.Contains(out, "No such file") || strings.Contains(out, "cannot open") {
		t.Errorf("a path outside the repository produced an error:\n%s", out)
	}
	if got := Failed(out); got != ReasonTest {
		t.Errorf("Failed() = %q", got)
	}
}

// THE RULE IS ABSOLUTE IN GO, which is what makes it safe as a gate: the testing
// packages have no legitimate place in a non-test file.
func TestAProductionFileImportingATestingPackageFailsTheBranch(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.com/x\n\ngo 1.25\n")
	write(t, dir, "main.go", "package main\n\nimport \"net/http/httptest\"\n\n"+
		"func createTask(w *httptest.ResponseRecorder) {}\n")

	out, ok := run(t, dir, TestImportScript())
	if ok {
		t.Fatalf("a production file importing httptest passed:\n%s", out)
	}
	if !strings.Contains(out, "main.go") {
		t.Errorf("the offending file was not named:\n%s", out)
	}
	// IT NAMES THE CAUSE, not the symptom: the agent could not find this fault by
	// reading assertion output, because every failing assertion pointed at the
	// test and the test was right.
	if !strings.Contains(out, "written to satisfy the") {
		t.Errorf("the advice does not explain what went wrong:\n%s", out)
	}
	if got := Failed(out); got != ReasonTestImport {
		t.Errorf("Failed() = %q, want %q", got, ReasonTestImport)
	}
}

func TestATestFileImportingTestingIsFine(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.com/x\n\ngo 1.25\n")
	write(t, dir, "main.go", "package main\n\nfunc main() {}\n")
	write(t, dir, "main_test.go", "package main\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) {}\n")

	out, ok := run(t, dir, TestImportScript())
	if !ok {
		t.Errorf("a test file importing testing failed the gate:\n%s", out)
	}
	if !strings.Contains(out, "no testing packages in production files") {
		t.Errorf("the gate did not report a clean result:\n%s", out)
	}
}

// GUARDED ON THE MODULE FILE, so a repository in another language simply skips
// it rather than being judged by a Go rule.
func TestARepositoryThatIsNotGoSkipsTheImportGate(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "main.go", "package main\n\nimport \"testing\"\n") // no go.mod

	out, ok := run(t, dir, TestImportScript())
	if !ok {
		t.Errorf("a repository with no module file was failed by a Go rule:\n%s", out)
	}
}

// THE LINTER CANNOT FAIL THE RUN. A finding it cannot resolve would otherwise
// trap the agent: fixed budget, spent trying, correct change thrown away over a
// lint rule.
func TestTheLinterIsAdvisoryAndFenced(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.com/x\n\ngo 1.25\n")
	write(t, dir, "main.go", "package main\n\nfunc main() {}\n")

	script := Script(Options{
		LintCommand: "echo 'main.go:3:1: exported function needs a comment'; false",
		TestCommand: "true",
	})
	out, ok := run(t, dir, script)
	if !ok {
		t.Fatalf("a failing linter failed the branch:\n%s", out)
	}
	if got := Advice(out); !strings.Contains(got, "needs a comment") {
		t.Errorf("Advice() = %q, want the linter's finding", got)
	}
	if got := Failed(out); got != ReasonUnknown {
		t.Errorf("Failed() = %q after an advisory finding", got)
	}
}

// THE CRITICAL ANALYSIS RUNS AFTER THE TESTS, so a change that does not build
// hears about being broken first rather than being told about a scanner it
// could not have satisfied.
func TestTheCriticalAnalysisGatesButOnlyAfterTheTests(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.com/x\n\ngo 1.25\n")
	write(t, dir, "main.go", "package main\n\nfunc main() {}\n")

	t.Run("it gates", func(t *testing.T) {
		out, ok := run(t, dir, Script(Options{
			TestCommand:     "true",
			CriticalCommand: "echo 'hardcoded credential'; false",
		}))
		if ok {
			t.Fatalf("a critical finding passed:\n%s", out)
		}
		if got := Failed(out); got != ReasonCritical {
			t.Errorf("Failed() = %q, want %q", got, ReasonCritical)
		}
	})

	t.Run("the tests come first", func(t *testing.T) {
		out, _ := run(t, dir, Script(Options{
			TestCommand:     "echo 'FAIL: the suite is red'; false",
			CriticalCommand: "echo 'RAN THE SCANNER'; false",
		}))
		if strings.Contains(out, "RAN THE SCANNER") {
			t.Errorf("the scanner ran on a branch whose tests were already red:\n%s", out)
		}
		if got := Failed(out); got != ReasonTest {
			t.Errorf("Failed() = %q, want the tests to be blamed", got)
		}
	})
}

// THE IMPORT GATE RUNS BEFORE THE TESTS, because a production file importing a
// testing package makes every later result meaningless.
func TestTheImportGateRunsBeforeTheTests(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.com/x\n\ngo 1.25\n")
	write(t, dir, "main.go", "package main\n\nimport \"testing\"\n\nvar _ = testing.Short\n")

	out, _ := run(t, dir, Script(Options{TestCommand: "echo 'RAN THE TESTS'; true"}))
	if strings.Contains(out, "RAN THE TESTS") {
		t.Errorf("the tests ran against a tree whose production code imports testing:\n%s", out)
	}
	if got := Failed(out); got != ReasonTestImport {
		t.Errorf("Failed() = %q, want %q", got, ReasonTestImport)
	}
}

// "CHECKS" IS THE HONEST ANSWER when nothing was marked: a sandbox that died
// before any gate ran did not fail a gate, and naming one sends the reader to a
// command that never executed.
func TestFailedReportsWhichGateOrAdmitsItDoesNotKnow(t *testing.T) {
	cases := map[string]string{
		"":                                   ReasonUnknown,
		"the sandbox was killed":             ReasonUnknown,
		Marker:                               ReasonUnknown,
		Marker + "test":                      ReasonTest,
		Marker + "test\nmore output":         ReasonTest,
		Marker + "critical-security ":        ReasonCritical,
		"before\n" + Marker + "build\nafter": ReasonBuild,
	}
	for out, want := range cases {
		if got := Failed(out); got != want {
			t.Errorf("Failed(%q) = %q, want %q", out, got, want)
		}
	}
}

// THE LAST MARKER WINS. A test's own output can contain the marker text, and
// what the harness wrote is always last.
func TestTheHarnessesOwnMarkerWinsOverOneInTheOutput(t *testing.T) {
	out := "FAIL: a test printed " + Marker + "test-imports-in-production\n" + Marker + ReasonTest
	if got := Failed(out); got != ReasonTest {
		t.Errorf("Failed() = %q, want the marker the harness wrote", got)
	}
}

// A PANIC BURIES ITS OWN CAUSE: the message, then twenty frames of runtime and
// testing internals that are the same on every failure there has ever been.
func TestStackNoiseIsStrippedAndTheRepositorysOwnFramesKept(t *testing.T) {
	out := strings.Join([]string{
		"go: downloading example.com/dep v1.2.3",
		"--- FAIL: TestAdd",
		"    store_test.go:155: should have 2 item(s), but has 0",
		"panic: runtime error: index out of range [0] with length 0",
		"",
		"goroutine 7 [running]:",
		"testing.tRunner.func1.2({0x7b7c60, 0xa86cc0})",
		"\t/usr/local/go/src/testing/testing.go:1974 +0x419",
		"runtime.gopanic({0x7b7c60?, 0xa86cc0?})",
		"\t/usr/local/go/src/runtime/panic.go:860 +0x13a",
		"example.com/x.TestAdd(0xc00020ab48)",
		"\t/tmp/work/store_test.go:157 +0x1a9",
	}, "\n")

	got := StripToolChatter(out)

	// The diagnosis survives.
	if !strings.Contains(got, "should have 2 item(s), but has 0") {
		t.Error("the assertion was stripped; that one line is the whole diagnosis")
	}
	// The repository's own frame survives, because it names the line to look at.
	if !strings.Contains(got, "store_test.go:157") {
		t.Error("the repository's own stack frame was stripped")
	}
	// The noise does not.
	for _, noise := range []string{
		"go: downloading", "testing.tRunner", "runtime.gopanic",
		"/usr/local/go/src/testing/testing.go", "/usr/local/go/src/runtime/panic.go",
	} {
		if strings.Contains(got, noise) {
			t.Errorf("%q survived the strip", noise)
		}
	}
}

// A frame is two lines. Dropping the function without its indented file:line
// would leave an orphaned path that reads as a repository file.
func TestAStrippedFrameTakesItsFileLineWithIt(t *testing.T) {
	out := "testing.tRunner(0xc000)\n\t/usr/local/go/src/testing/testing.go:1974\nkeep me\n"
	got := StripToolChatter(out)
	if strings.Contains(got, "testing.go:1974") {
		t.Errorf("an orphaned file:line survived: %q", got)
	}
	if !strings.Contains(got, "keep me") {
		t.Errorf("ordinary output was stripped: %q", got)
	}
}

func TestAdviceIsEmptyWhenNothingWasFenced(t *testing.T) {
	if got := Advice("just some output"); got != "" {
		t.Errorf("Advice() = %q", got)
	}
	// An unterminated fence still yields what it opened, rather than nothing: a
	// linter killed mid-output has still said something worth passing on.
	got := Advice(AdviceOpen + "\nvet: something\n")
	if !strings.Contains(got, "vet: something") {
		t.Errorf("Advice() = %q, want the finding despite the missing close", got)
	}
}

func TestAnEmptyOptionsProducesAScriptThatStillChecksImports(t *testing.T) {
	s := Script(Options{})
	if !strings.Contains(s, "testing packages in production files") {
		t.Error("the import gate is skipped when no commands are configured")
	}
	if strings.Contains(s, AdviceOpen) {
		t.Error("an advisory fence was emitted with no linter configured")
	}
}

// THE SANDBOX RUNS EVERY SCRIPT UNDER `set -e`, and that is what makes the
// bracing load-bearing rather than tidy.
//
// A configured command may be a chain. Unbraced, `2>&1` and `|| true` bind to
// its LAST element only, so an earlier element that fails is unguarded — and
// under `set -e` an unguarded failure aborts the whole verification before the
// tests are ever reached. An ADVISORY check must not be able to do that.
func TestAnAdvisoryChainCannotAbortTheVerification(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.com/x\n\ngo 1.25\n")
	write(t, dir, "main.go", "package main\n\nfunc main() {}\n")

	script := "set -e\n" + Script(Options{
		// The first half fails, the second still has something to say.
		LintCommand: "false; echo 'main.go:3:1: a finding'",
		TestCommand: "echo 'REACHED THE TESTS'; true",
	})

	out, ok := run(t, dir, script)
	if !ok {
		t.Fatalf("an advisory chain aborted the verification:\n%s", out)
	}
	if !strings.Contains(out, "REACHED THE TESTS") {
		t.Errorf("the tests never ran; a lint chain took the whole run with it:\n%s", out)
	}
	if got := Advice(out); !strings.Contains(got, "a finding") {
		t.Errorf("Advice() = %q, want what the chain still managed to say", got)
	}
}

// The same for the gate that DOES block: unbraced, a chain whose last element
// succeeds reports success however the earlier ones went.
func TestACriticalChainIsJudgedAsAWhole(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.com/x\n\ngo 1.25\n")
	write(t, dir, "main.go", "package main\n\nfunc main() {}\n")

	out, ok := run(t, dir, "set -e\n"+Script(Options{
		TestCommand:     "true",
		CriticalCommand: "false && echo 'unreachable'",
	}))
	if ok {
		t.Fatalf("a failing critical chain passed:\n%s", out)
	}
	if got := Failed(out); got != ReasonCritical {
		t.Errorf("Failed() = %q, want %q — the blocking gate must say which check blocked", got, ReasonCritical)
	}
}

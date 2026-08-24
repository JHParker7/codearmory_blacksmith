package delegate

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/code-armory-app/blacksmith/internal/ticket"
)

func job() ticket.Ticket {
	return ticket.Ticket{
		ID: "t-1", Title: "Add filtering to the task store",
		// WRITTEN FOR THE SPECIFICATION AUTHOR, which is exactly why it must not
		// reach the developer.
		Description: "Write a test for each of these and STOP:\n  - List(Filter{Done:true}) returns only completed tasks",
	}
}

// THE TICKET'S DESCRIPTION IS DELIBERATELY EXCLUDED. It tells the specification
// author to write tests and stop, so passing it to a developer contradicts the
// rules in the same prompt — measured: an agent given both wrote tests and a
// 243-line index.html nobody asked for.
func TestTheDeveloperIsNeverGivenTheAuthorsInstructions(t *testing.T) {
	brief := Brief(job(), "go test ./...", false)

	if !strings.Contains(brief, "Add filtering to the task store") {
		t.Error("the brief does not carry the title, which is the developer's context")
	}
	for _, leaked := range []string{"Write a test for each", "and STOP"} {
		if strings.Contains(brief, leaked) {
			t.Errorf("the author's instruction reached the developer: %q", leaked)
		}
	}
}

// THE TESTS ARE THE SPECIFICATION, and the rule is backed by the filesystem
// rather than only stated — a rule the agent may decline to follow is not a
// rule.
func TestTheBriefStatesTheRulesTheHarnessOtherwiseEnforces(t *testing.T) {
	brief := Brief(job(), "go test ./...", false)

	for _, want := range []string{
		"ARE the specification",
		"READ-ONLY",
		"READ the failing test files before writing anything",
		"Where it disagrees with a test, the test wins",
		"go test ./...",
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("the brief does not say %q:\n%s", want, brief)
		}
	}
	// AND THE FILESYSTEM BACKS IT.
	if !strings.Contains(LockTestsScript, "chmod a-w *_test.go") {
		t.Error("the specification is not made read-only")
	}
	// A repository with no test files must not fail the run over the chmod.
	if !strings.Contains(LockTestsScript, "|| true") {
		t.Error("a repository with no test files would fail on the chmod")
	}
}

// THE RETRY WORDING IS DIFFERENT, AND ONLY ON A RETRY. On a first attempt there
// is no previous theory to rule out, and telling a fresh agent that "your
// previous attempt" failed invents a history it then reasons from.
func TestOnlyARetryIsToldItHasAlreadyTriedSomething(t *testing.T) {
	first := Brief(job(), "go test ./...", false)
	retry := Brief(job(), "go test ./...", true)

	if strings.Contains(first, "PREVIOUS ATTEMPT") {
		t.Errorf("a first attempt was told about a history it does not have:\n%s", first)
	}
	if !strings.Contains(retry, "PREVIOUS ATTEMPT DID NOT WORK") {
		t.Errorf("a retry was not told its last theory was wrong:\n%s", retry)
	}
	if !strings.Contains(retry, "Do not repeat the same change") {
		t.Errorf("a retry was not warned off repeating itself:\n%s", retry)
	}
}

// "DO NOT REPEAT THE SAME CHANGE" IS A SENTENCE A FRESH PROCESS CANNOT OBEY.
// Each attempt starts with no memory, so it re-derives the same wrong theory and
// writes the same code again — r70: four attempts, each blaming "routing
// issues", while the defect was six lines away in Add(). Its own diff is the
// only thing that makes "you already tried that" checkable.
func TestARetryIsShownItsOwnPreviousDiff(t *testing.T) {
	if !strings.Contains(PreviousDiffScript, "git diff HEAD~1") {
		t.Error("a retry is not shown what it already changed")
	}
	// NOT THE TESTS: those are the specification and did not change.
	if !strings.Contains(PreviousDiffScript, "':!*_test.go'") {
		t.Error("the diff includes the specification, which the agent did not write")
	}
	// A FIRST COMMIT HAS NO PARENT, and asking for one must not fail the script.
	if !strings.Contains(PreviousDiffScript, "git rev-parse HEAD~1 >/dev/null 2>&1") {
		t.Error("a branch with one commit would fail on the diff")
	}
	// BOUNDED: a large previous attempt must not fill the window it is being
	// shown in.
	if !strings.Contains(PreviousDiffScript, "head -120") {
		t.Error("the previous diff is unbounded")
	}
}

// AN EXECUTION HAS A DEADLINE AND THE WORK MUST FIT INSIDE IT. An attempt that
// overruns is still holding the lease when verification tries to submit, which
// reports as "already claimed" from a stage that had actually done its work.
func TestAnAttemptLeavesRoomForTheCommitAndThePush(t *testing.T) {
	for _, budget := range []int64{900, 600, 300} {
		got := AttemptSeconds(budget)
		if got >= budget {
			t.Errorf("budget %d gave the agent %d, leaving nothing for the push", budget, got)
		}
		if budget-got != Reserve {
			t.Errorf("budget %d reserved %d, want %d", budget, budget-got, Reserve)
		}
	}

	// AN UNSET BUDGET GETS THE DEFAULT rather than zero, which would give the
	// agent no time at all.
	if got := AttemptSeconds(0); got != DefaultBudget-Reserve {
		t.Errorf("an unset budget gave %d", got)
	}

	// AND A TINY BUDGET STILL PRODUCES A USABLE ATTEMPT rather than a negative
	// one: a floor is more honest than an agent killed on arrival.
	for _, tiny := range []int64{1, 100, 150, 160} {
		if got := AttemptSeconds(tiny); got < MinAttempt {
			t.Errorf("budget %d gave the agent %ds", tiny, got)
		}
	}
}

// A SUITE THAT ALREADY PASSED IS NOT NOTHING. The agent correctly did no work,
// and treating that as a failed attempt would retry a ticket that is finished.
func TestAnAlreadyPassingSuiteIsNotAFailedAttempt(t *testing.T) {
	if ProducedNothing("the tests already pass; " + ChangedNothingMarker) {
		t.Error("a ticket that was already finished was treated as a failed attempt")
	}
	if !ProducedNothing("...\n" + ChangedNothingMarker + "\n") {
		t.Error("an attempt that changed nothing was not recognised")
	}
	if ProducedNothing("wrote store.go and pushed") {
		t.Error("a real attempt was reported as having produced nothing")
	}
}

// A KILLED ATTEMPT MUST STILL SAY SOMETHING. r70: one attempt ended with exit 15
// after 163 seconds, stdout and stderr both empty, no reap in forge's log and
// the timeout nowhere near expiry. Nothing could be attributed, because the
// signal killed the shell before the line that flushes the agent's own log.
func TestAKilledAttemptIsRecognisedAndSaysHowLongItRan(t *testing.T) {
	if !WasKilled(TerminatedMarker + " SIGTERM after 163s ---") {
		t.Error("a terminated attempt was not recognised")
	}
	if WasKilled("wrote store.go and pushed") {
		t.Error("an ordinary attempt was reported as killed")
	}

	for _, sig := range []string{"TERM", "INT"} {
		if !strings.Contains(TrapScript, "' "+sig+"\n") {
			t.Errorf("nothing is trapped for %s", sig)
		}
	}
}

// AND THE TRAP IS RUN, not read. What matters is that a killed attempt actually
// prints how long it had been going — r70's silent SIGTERM is the failure, and a
// substring check would pass against a trap that reports zero.
func TestTheTrapReportsHowLongTheAttemptHadRun(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not available")
	}

	// A shell that installs the trap and then waits, as the real one does: a
	// POSIX shell does not run a trap while a FOREGROUND command is running, so
	// the sleep has to be backgrounded and waited on — which is exactly the shape
	// the delegate script uses.
	cmd := exec.Command("sh", "-c", TrapScript+"sleep 20 &\nwait\n")
	// Its own process group, so the backgrounded sleep can be taken down with it.
	// Left alone it inherits stdout and holds it open for its full duration, and
	// the test then waits on a sleep nobody is interested in.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var buf bytes.Buffer
	cmd.Stdout = &buf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	// Let the clock move before the signal, so a working trap reports at least a
	// second and a broken one reports zero.
	time.Sleep(1200 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	// Give the trap a moment to print, then take down what it left behind.
	time.Sleep(300 * time.Millisecond)
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Wait()

	got := buf.String()

	if !WasKilled(got) {
		t.Fatalf("the trap did not report a termination:\n%s", got)
	}
	// IT REPORTS THE ATTEMPT'S OWN ELAPSED TIME, which means the number has to be
	// plausible — not zero, and not the epoch. A trap that starts its clock at 0
	// reports a billion seconds and still reads as "after Ns ---", which is why
	// this parses the figure rather than matching the shape.
	_, rest, found := strings.Cut(got, "SIGTERM after ")
	if !found {
		t.Fatalf("the trap does not say how long it ran:\n%s", got)
	}
	digits, _, found := strings.Cut(rest, "s ---")
	if !found {
		t.Fatalf("the trap's elapsed time is malformed:\n%s", got)
	}
	secs, err := strconv.Atoi(strings.TrimSpace(digits))
	if err != nil {
		t.Fatalf("the trap reported %q, which is not a number of seconds", digits)
	}
	if secs < 1 {
		t.Errorf("the trap reported %ds; a silent kill is what r70 could not attribute", secs)
	}
	if secs > 60 {
		t.Errorf("the trap reported %ds, which is not this attempt's elapsed time — "+
			"its clock never started", secs)
	}
}

// EACH ATTEMPT IS ITS OWN EXECUTION AGAINST A RESET WORKING TREE, so the red
// state has to travel OUT on stdout and back IN as an argument. Nothing survives
// in the sandbox.
func TestTheRedStateTravelsBetweenAttempts(t *testing.T) {
	// DELIBERATELY OUT OF ORDER: the shell's `sort -u` and this reader must agree,
	// or the same red state compares unequal between two attempts and every test
	// reads as newly broken.
	out := "some output\n" + FailingMarker + "TestZeta TestAlpha TestMiddle \nmore output\n"

	got := FailingAtStart(out)
	want := []string{"TestAlpha", "TestMiddle", "TestZeta"}
	if len(got) != len(want) {
		t.Fatalf("FailingAtStart = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the baseline is not in a stable order: %v, want %v", got, want)
		}
	}

	// An attempt that published nothing yields nothing rather than a phantom.
	if got := FailingAtStart("no marker here"); got != nil {
		t.Errorf("FailingAtStart = %v", got)
	}
	if got := FailingAtStart(FailingMarker + "   "); len(got) != 0 {
		t.Errorf("an empty list read as %v", got)
	}
}

// THE DISTINCTION DECIDES WHO IS AT FAULT. A suite still red because the
// ticket's own tests are unsatisfied is an unfinished job; one red because the
// agent broke something that used to pass is a REGRESSION, and reporting the
// second as the first sends the next attempt to fix the wrong thing.
func TestATestTheAgentBrokeIsToldApartFromOneItWasGivenToFix(t *testing.T) {
	baseline := []string{"TestList", "TestFilter"}

	// Still failing what it was given: nothing new.
	if got := NewlyFailing(baseline, []string{"TestList", "TestFilter"}); len(got) != 0 {
		t.Errorf("the ticket's own red tests were reported as regressions: %v", got)
	}
	// It fixed one and broke another.
	got := NewlyFailing(baseline, []string{"TestList", "TestAdd"})
	if len(got) != 1 || got[0] != "TestAdd" {
		t.Errorf("NewlyFailing = %v, want the one it broke", got)
	}
	// A green suite has no regressions.
	if got := NewlyFailing(baseline, nil); len(got) != 0 {
		t.Errorf("a passing suite reported %v", got)
	}
	// AND NO BASELINE MEANS EVERYTHING IS NEW, which is the honest reading when
	// the first attempt published nothing: better to over-report a regression
	// than to hide one.
	if got := NewlyFailing(nil, []string{"TestA", "TestB"}); len(got) != 2 {
		t.Errorf("with no baseline, NewlyFailing = %v", got)
	}
}

// ONE KEY, NOT TWO. Writing a second provider form "to be safe" required an "id"
// field, so the model registry failed validation and the agent refused to start
// at all — reported as "produced no change", twice, because a crashed agent
// leaves no diff.
func TestTheAgentSettingsAreValidJSONAndSayOneThing(t *testing.T) {
	var got map[string]any
	if err := json.Unmarshal([]byte(Settings()), &got); err != nil {
		t.Fatalf("the settings are not valid JSON: %v\n%s", err, Settings())
	}
	if _, has := got["modelProviders"]; has {
		t.Error("the settings carry a second provider form, which fails validation")
	}

	m, ok := got["model"].(map[string]any)
	if !ok {
		t.Fatalf("the settings have no model block: %v", got)
	}
	gen, _ := m["generationConfig"].(map[string]any)
	if gen["contextWindowSize"] != float64(ContextTokens) {
		t.Errorf("the window is %v, want %d", gen["contextWindowSize"], ContextTokens)
	}
	comp, _ := m["chatCompression"].(map[string]any)
	if comp["contextPercentageThreshold"] != CompressionThreshold {
		t.Errorf("compaction starts at %v, want %v",
			comp["contextPercentageThreshold"], CompressionThreshold)
	}
}

// THE WINDOW IS UNDER THE SLOT, NOT EQUAL TO IT: the server counts the whole
// request including the completion, so an agent filling it exactly is refused
// with a 400 — an attempt lost to arithmetic rather than to the work.
func TestTheContextWindowLeavesRoomForTheReply(t *testing.T) {
	const slot = 65536
	if ContextTokens >= slot {
		t.Errorf("the agent is told it has %d against a %d slot", ContextTokens, slot)
	}
	// AND COMPACTION STARTS WELL BEFORE THE CEILING, because later compactions
	// reclaim almost nothing — one run went 81,651 to 81,273 tokens.
	if CompressionThreshold >= 0.85 {
		t.Errorf("compaction starts at %v, which is the documented default that did not work",
			CompressionThreshold)
	}
}

// EVERY SPLICED COMMAND MUST BE VALID SHELL under the preamble's `set -e`.
func TestTheScriptsAreValidShell(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not available")
	}
	scripts := map[string]string{
		"the trap":            TrapScript,
		"locking the tests":   LockTestsScript,
		"the previous diff":   PreviousDiffScript,
		"the settings":        SettingsScript(),
		"publishing failures": PublishFailingScript("go test ./...", []string{"TestA", "TestB"}),
		// A test command that fails is the NORMAL case here.
		"publishing a red suite": PublishFailingScript("false", nil),
		"an awkward baseline":    PublishFailingScript("go test ./...", []string{"It's", "Odd"}),
	}
	for name, s := range scripts {
		if out, err := exec.Command("sh", "-n", "-c", "set -e\n"+s).CombinedOutput(); err != nil {
			t.Errorf("%s is not valid shell: %v\n%s", name, err, out)
		}
	}
}

// THE PUBLISHED LIST IS WHAT THE NEXT ATTEMPT READS, so the round trip has to
// actually work — not merely look right.
func TestThePublishedFailingListRoundTrips(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not available")
	}
	dir := t.TempDir()

	// A stub test command that reports two failures the way `go test` does.
	stub := filepath.Join(dir, "faketest")
	body := "#!/bin/sh\ncat <<'EOF'\n--- FAIL: TestList (0.00s)\n    store_test.go:9: no\n" +
		"--- FAIL: TestFilter (0.00s)\nFAIL\nEOF\nexit 1\n"
	if err := os.WriteFile(stub, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sh", "-c", "set -e\n"+PublishFailingScript(stub, nil))
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the script failed: %v\n%s", err, out)
	}

	got := FailingAtStart(string(out))
	if len(got) != 2 || got[0] != "TestFilter" || got[1] != "TestList" {
		t.Errorf("the round trip produced %v, want the two failing tests", got)
	}
}

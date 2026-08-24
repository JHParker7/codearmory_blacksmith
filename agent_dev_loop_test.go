package main

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// HANDLE MUST STOP WHEN THE MODEL WILL NOT.
//
// Every loop guard in this file's neighbours is tested by calling stuck()
// directly with its counters already set. That proves stuck() decides correctly
// and proves nothing about Handle reaching it with those counters climbing —
// which is the only property that matters, and the one three live runs kept
// disproving after the unit tests had passed.
//
// Measured on r97c: 83 consecutive identical refusals with two freshly written
// ceilings compiled in, neither of which fired, no stop comment on the ticket,
// and the attempt still going when it was killed. Nothing in the suite noticed,
// because nothing in the suite drove Handle with a model that repeats itself.
//
// So this is the shape of test that has to exist before another ceiling is
// added: a stub model that answers the same way for ever, and an assertion that
// the loop ends anyway — deterministic, no GPU, milliseconds.

// repeatingModel answers with the same reply to every request, for ever, and
// counts how many times it was asked.
func repeatingModel(t *testing.T, reply string) (*Gateway, func() int) {
	t.Helper()
	var n int
	gw, _ := testGateway(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		completionHandler(reply)(w, r)
	}, 4)
	return gw, func() int { return n }
}

// A developer that keeps asking for the same impossible thing earns the identical
// refusal every time. It must not be allowed to spend its whole budget doing so.
func TestHandleStopsWhenTheModelRepeatsARefusedAction(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "Add a greeting", CreatedBy: "alice"})
	f.execStdout = "main.go\nmain_test.go\ngo.mod\n"

	const budget = 60
	// undo_edit with nothing written this attempt is refused with the identical
	// text every time, through noProgress, and needs no file state to set up.
	gw, calls := repeatingModel(t, `{"action":"undo_edit"}`)
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr-loop", "t1", "dev-agent")

	agent := NewDevAgent(gw, api, ClassLarge, RepoConfig{
		URL: "https://example.invalid/org/repo", Branch: "main",
		BranchPrefix: "agent/", Image: "golang:1.25", PipelineID: "pipe-1",
	}, budget)

	done := make(chan struct{})
	var status, detail string
	var err error
	go func() {
		status, detail, err = agent.Handle(ctx, f.get("t1"))
		close(done)
	}()
	<-done

	if err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	if status == OutcomeSuccess {
		t.Fatalf("status = success (%s); the model never made a legal change", detail)
	}
	// The point of the test. Spending the whole budget on one refusal repeated is
	// the failure; stopping early is the behaviour.
	if got := calls(); got >= budget {
		t.Errorf("the model was asked %d times against a budget of %d — Handle never broke the loop (%s)",
			got, budget, detail)
	} else {
		t.Logf("stopped after %d of %d turns: %s", got, budget, detail)
	}
}

// AND THE CASE THAT ACTUALLY GOT AWAY: every write byte-identical to the last.
//
// This is r97c's stimulus exactly. The first write lands; every one after it
// leaves the tree unchanged, so no check can run and the verdict never moves.
// Live, that produced 83 consecutive refusals and no ending. If Handle stops
// here, the live cause is elsewhere; if it does not, this is the reproduction
// three runs and two wrong fixes failed to pin down.
func TestHandleStopsWhenEveryWriteChangesNothing(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "Add a greeting", CreatedBy: "alice"})
	f.execStdout = "main.go\ngo.mod\n"
	// The CHECKS must fail without the survey failing with them, or the stage
	// either finishes on the first write or dies before it starts.
	f.execFailIfScriptContains = "go test"

	const budget = 60
	gw, calls := repeatingModel(t,
		`{"action":"write_files","summary":"add main","type":"feat",`+
			`"edits":[{"path":"main.go","start_line":0,"end_line":0,`+
			`"replace":"package main\n\nfunc main() {}\n"}]}`)
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr-noop", "t1", "dev-agent")

	agent := NewDevAgent(gw, api, ClassLarge, RepoConfig{
		URL: "https://example.invalid/org/repo", Branch: "main",
		BranchPrefix: "agent/", Image: "golang:1.25", PipelineID: "pipe-1",
		TestCommand: "go test ./...",
	}, budget)

	status, detail, err := agent.Handle(ctx, f.get("t1"))
	if err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	if got := calls(); got >= budget {
		t.Errorf("the model was asked %d times against a budget of %d -- every write after the first "+
			"changed nothing and Handle never broke the loop (status %s, %s)", got, budget, status, detail)
	} else {
		t.Logf("stopped after %d of %d turns: %s (%s)", got, budget, detail, status)
	}

}

// WHAT THIS FILE DOES NOT YET COVER, and why it is written down rather than
// half-tested.
//
// Both cases above accumulate refusals cleanly because one reply repeated
// verbatim lands once and is a no-op after that, so nothing verifies in between.
// The live failures do not look like that. After any verification that RETURNS A
// VERDICT -- pass or fail -- Handle clears notice, refusals, staleReads and
// noopEdits together. Refusals therefore only accumulate BETWEEN verifications,
// and an agent that lands a change every turn can never reach the twenty-turn
// ceiling however long it goes in circles. Only the turn budget bounds it.
//
// That is what r97c was doing. Its gate ran every turn and answered exit 128,
// "fatal: couldn't find remote ref" -- a verdict, not an error -- so the counters
// reset every turn and 83 refusals never became 20. The broken gate was an
// artifact of that reproduction, which started a developer stage with no branch
// ever pushed; the resetting is not.
//
// A cycling model was written to cover it and deleted again: it finished
// successfully in two turns because the injected gate failure did not reach the
// verify script, so it asserted nothing. The thing worth testing is oscillation
// -- r95's branch reads "add a /nonexistent route", "remove the /nonexistent
// route handler", "add a /nonexistent route" -- and the detector for it is a tree
// hash seen twice, which treeHash() already computes for verifiedTree. That needs
// a fake whose gate reliably fails, which this one does not yet give.

// WHAT THIS FILE CANNOT COVER, and why.
//
// Both cases above accumulate refusals cleanly because one reply repeated
// verbatim lands once and is a no-op after that, so nothing verifies in between.
// The live failures are the other shape: an agent whose writes DO land, whose
// tree therefore moves every turn, and whose gate answers identically anyway.
//
// That cannot be driven through this fake. It models one canned stdout for every
// command and has no file contents, so an agent cannot be made to land
// alternating edits: a whole-file create is refused the second time round, and an
// anchored edit is refused because the file "has not been read". Three attempts
// at it produced three tests that passed or failed for reasons unrelated to the
// thing under test.
//
// The decision that shape depends on is tested directly instead — see
// TestAVerdictThatHasNotMovedKeepsTheRefusalCount, which drives devState.
// recordVerdict. Closing the gap properly means giving the fake a filesystem,
// which is its own piece of work.

// A VERDICT THAT HAS NOT MOVED KEEPS THE COUNT.
//
// refusals feeds two mechanisms: the loop breaker's ceiling and devTemperature's
// ramp. Clearing it on an unchanged verdict disables both, which is why an agent
// alternating between two states ran to its whole budget on r102 while the sampler
// stayed deterministic at temperature 0.
func TestAVerdictThatHasNotMovedKeepsTheRefusalCount(t *testing.T) {
	s := &devState{refusals: 7, staleReads: 3, noopEdits: 2, notice: "old", lastTest: "FAIL: TestFoo"}

	s.recordVerdict("FAIL: TestFoo", false)

	if s.refusals != 7 {
		t.Errorf("refusals = %d, want 7 kept — the gate said exactly what it said last time", s.refusals)
	}
	// The tree DID move, so these two are genuinely over.
	if s.staleReads != 0 || s.noopEdits != 0 {
		t.Errorf("staleReads = %d, noopEdits = %d, want both cleared", s.staleReads, s.noopEdits)
	}
	if s.notice != "" {
		t.Errorf("notice = %q, want cleared", s.notice)
	}
}

// A verdict that HAS moved clears it: the agent learned something, and the count
// describes a state it has left. Cutting an agent off for repeating itself while
// recovering is what the generous ceiling exists to prevent.
func TestAVerdictThatMovedClearsTheRefusalCount(t *testing.T) {
	s := &devState{refusals: 7, lastTest: "FAIL: TestFoo"}

	s.recordVerdict("FAIL: TestBar", false)

	if s.refusals != 0 {
		t.Errorf("refusals = %d, want 0 — the failure changed, so the run of them is over", s.refusals)
	}
	if s.lastTest != "FAIL: TestBar" {
		t.Errorf("lastTest = %q, want the new verdict", s.lastTest)
	}
}

// The first verdict of an attempt has nothing to compare against and must not be
// read as a repeat.
func TestTheFirstVerdictIsNotARepeat(t *testing.T) {
	s := &devState{refusals: 4}

	s.recordVerdict("FAIL: TestFoo", false)

	if s.refusals != 0 {
		t.Errorf("refusals = %d, want 0 on the first verdict of an attempt", s.refusals)
	}
}

// Passing is recorded either way, so the stage can end on it.
func TestAPassingVerdictIsRecorded(t *testing.T) {
	s := &devState{lastTest: "FAIL: TestFoo"}

	s.recordVerdict("ok", true)

	if !s.testsPass || s.lastTest != "ok" {
		t.Errorf("testsPass = %v, lastTest = %q; a pass must be recorded", s.testsPass, s.lastTest)
	}
}

// A WRITE THAT COUNTS AS REAL AND THEN REFUSES MUST STILL ACCUMULATE.
//
// This is the loop that survived three fixes. changedFromBaseline compares the
// agent's own staged map and says the write landed, so the turn cleared the
// counters; git then found nothing to COMMIT, raised errNothingToCommit, and
// refused the same turn. Cleared then incremented, every turn, so neither the
// loop breaker nor devTemperature's ramp ever saw more than one.
//
// Measured on r105 with the counters in the transcript: six consecutive refusals
// reading "refusals=1 noop=1" while writes climbed 4, 5, 6, 7, 8, 9.
//
// The state is driven directly because the fake cannot produce this pairing: it
// has no file contents, so it cannot make a staged write that git considers
// already committed.
func TestAnOptimisticWriteDoesNotClearTheCount(t *testing.T) {
	s := &devState{refusals: 6, noopEdits: 3, staleReads: 2, notice: "old", lastTest: "FAIL: TestFoo"}

	// What the write branch now does: the notice is stale, the counts are not
	// yet known to be.
	s.notice = ""

	if s.refusals != 6 {
		t.Errorf("refusals = %d; a write cleared the count before anything checked it", s.refusals)
	}

	// The verification then says nothing changed, and that is a refusal.
	s.noProgress("Rejected: your edits changed nothing")
	if s.refusals != 7 {
		t.Errorf("refusals = %d, want 7 — the run continues rather than restarting", s.refusals)
	}

	// Only a verdict that moved clears them, which is where the answer is known.
	s.recordVerdict("FAIL: TestBar", false)
	if s.refusals != 0 {
		t.Errorf("refusals = %d; a changed verdict is real progress and should clear", s.refusals)
	}
}

// A COMPOSED GATE MUST HAVE ALL OF ITS OUTPUT CAPTURED.
//
// The gate is configurable and is now a chain: "go build ./... && go test ./...".
// A redirect binds to the LAST element of an && chain, so the build's output
// escaped the capture, and when the build failed the test never ran and never
// created the file the script then cats.
//
// Measured on r107: the developer was handed "cat: /tmp/.gate-out: No such file
// or directory" above its real error, on every failing turn. A refusal pointing
// at nothing is the most expensive kind this repository has.
func TestAComposedGateCapturesEveryPartsOutput(t *testing.T) {
	integrationTest(t)
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no shell")
	}

	// The first half speaks and fails, so the second never runs — exactly the
	// shape of a build failing before the tests.
	script := testGateScript("{ echo BUILD_SAID_THIS >&2; false; } && echo TESTS_RAN")
	out, err := exec.Command("sh", "-c", script).CombinedOutput()
	if err == nil {
		t.Fatal("the gate reported success for a command that failed")
	}
	got := string(out)

	if strings.Contains(got, "No such file") {
		t.Errorf("the agent is told a file is missing before it is told what broke:\n%s", got)
	}
	if !strings.Contains(got, "BUILD_SAID_THIS") {
		t.Errorf("the first half of the chain was not captured:\n%s", got)
	}
	if strings.Contains(got, "TESTS_RAN") {
		t.Errorf("the second half ran despite the first failing:\n%s", got)
	}
	if !strings.Contains(got, gateMarker) {
		t.Errorf("the failure marker is missing, so nothing downstream knows the gate failed:\n%s", got)
	}
}

// And a passing chain still reports what both halves said.
func TestAPassingComposedGateKeepsItsOutput(t *testing.T) {
	integrationTest(t)
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no shell")
	}
	script := testGateScript("echo BUILD_OK && echo TESTS_OK")
	out, err := exec.Command("sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("the gate failed on a passing chain: %v\n%s", err, out)
	}
	for _, want := range []string{"BUILD_OK", "TESTS_OK"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the gate lost %q from a passing chain:\n%s", want, out)
		}
	}
}

// THE CEILING IS A KNOB, and zero turns it off.
//
// Whether ending an attempt at twenty refusals helps is an open question: it
// bounds the waste and throws away the context that attempt had built. Making it
// settable is what turns that into a measurement.
func TestTheDeadRefusalCeilingIsSettable(t *testing.T) {
	t.Setenv("AGENTS_DEV_MAX_DEAD_REFUSALS", "")
	if got := deadRefusalCeiling(); got != maxDeadRefusals {
		t.Errorf("unset ceiling = %d, want the default %d", got, maxDeadRefusals)
	}
	t.Setenv("AGENTS_DEV_MAX_DEAD_REFUSALS", "5")
	if got := deadRefusalCeiling(); got != 5 {
		t.Errorf("ceiling = %d, want 5", got)
	}
	// Nonsense falls back rather than disabling the bound by accident.
	t.Setenv("AGENTS_DEV_MAX_DEAD_REFUSALS", "banana")
	if got := deadRefusalCeiling(); got != maxDeadRefusals {
		t.Errorf("a bad value gave %d; it must fall back to %d", got, maxDeadRefusals)
	}
}

// Off means the attempt runs to its turn budget, as it did before the counters
// were fixed and the ceiling began firing.
func TestZeroDisablesTheCeiling(t *testing.T) {
	t.Setenv("AGENTS_DEV_MAX_DEAD_REFUSALS", "0")
	if got := deadRefusalCeiling(); got != 0 {
		t.Fatalf("ceiling = %d, want 0", got)
	}
	s := &devState{refusals: 500}
	if _, _, done := (&DevAgent{}).stuck(context.Background(), nil, Ticket{}, s); done {
		t.Error("the attempt was ended with the ceiling switched off")
	}
}

// A VERIFICATION MOVES THE TREE, SO WHAT THE AGENT ONLY READ MAY HAVE MOVED WITH
// IT.
//
// doRead skips any path already cached, deliberately: re-reading is meant to be
// free. Free is right, permanent is not. A verification resets the working tree
// to the branch, so a file the agent merely read can have been repaired by
// another stage since.
//
// Measured on r111. The reconciler repaired a shadowed *testing.T in
// ticket_test.go and pushed at 15:15:48; the developer read the broken version
// once and kept it, wrote the same fix the reconciler had already made, was
// refused for editing a test file, re-read the cached copy, saw the broken
// version again, and repeated until its ceiling.
func TestAVerificationForgetsReadsOfFilesTheAgentDidNotWrite(t *testing.T) {
	s := &devState{
		read: map[string]string{
			"ticket_test.go": "func TestX(t *testing.T) { t := Ticket{} }", // repaired elsewhere
			"store.go":       "package main // its own work",
		},
		missing: map[string]bool{"board.go": true},
		staged:  map[string]string{"store.go": "package main // its own work"},
	}

	s.recordVerdict("FAIL: something", false)

	if _, still := s.read["ticket_test.go"]; still {
		t.Error("a file the agent only read survived a verification; the branch may have moved under it")
	}
	// Its own writes are re-applied on top, so they still match what it holds.
	if _, kept := s.read["store.go"]; !kept {
		t.Error("the agent's own staged file was dropped; re-reading it costs a sandbox to be told what it wrote")
	}
	if s.missing["board.go"] {
		t.Error("a file recorded as missing stayed missing; another stage may have added it")
	}
}

// The survey at the start of an attempt runs ON THE BRANCH, because the sandbox
// is held per ticket and outlives the attempt that cloned it.
func TestTheSurveyRunsOnTheBranch(t *testing.T) {
	src, err := os.ReadFile("agent_dev.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	i := strings.Index(string(src), "func (a *DevAgent) listRepo(")
	if i < 0 {
		t.Fatal("listRepo is gone")
	}
	body := string(src)[i : i+1800]
	if !strings.Contains(body, "git fetch -q origin") {
		t.Error("the survey no longer syncs the tree to the branch; a second attempt would read the first attempt's clone")
	}
	// AND TOLERANTLY. On a first attempt the branch does not exist yet, and a
	// strict fetch aborts the survey under the prelude's set -e: r112 blocked all
	// five of its sections on "couldn't find remote ref".
	if !strings.Contains(body, "|| true") {
		t.Error("the survey's fetch is strict; a first attempt has no branch to fetch and would abort")
	}
}

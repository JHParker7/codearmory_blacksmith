package dev

import (
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/edit"
	"github.com/code-armory-app/blacksmith/internal/model"
)

func loopState() *State {
	return &State{
		Tree:     []string{"store.go"},
		Read:     map[string]string{"store.go": storeGo},
		Staged:   map[string]string{},
		Baseline: map[string]string{"store.go": storeGo},
		Missing:  map[string]bool{},
	}
}

// THE MESSAGES ARE ORDERED BY VOLATILITY, not by topic. A few hundred changed
// characters in FRONT of the repository listing cost the whole prefix cache:
// 31.3s against 1.3s, measured.
func TestTheStableHalfIsItsOwnMessageAndComesFirst(t *testing.T) {
	s := loopState()
	s.Iteration, s.Budget = 3, 10
	s.Remember("read_files(store.go)", "read 1 file")

	req := BuildRequest(job(), Reasons{}, s, ModeDevelop, "SYSTEM", true)

	if len(req.Messages) != 4 {
		t.Fatalf("%d messages, want system, world, history, progress", len(req.Messages))
	}
	if req.Messages[0].Role != "system" {
		t.Errorf("the first message is %q", req.Messages[0].Role)
	}
	if !strings.Contains(req.Messages[1].Content, "REPOSITORY FILES") {
		t.Error("the second message is not the cacheable half")
	}
	if strings.Contains(req.Messages[1].Content, "ITERATION") {
		t.Error("the cacheable half carries the turn counter")
	}
	// THE HISTORY IS A USER TURN. Anything in the assistant role is a
	// demonstration of what to produce.
	if req.Messages[2].Role != "user" || !strings.Contains(req.Messages[2].Content, "ALREADY TRIED") {
		t.Errorf("the history is not a user turn: %+v", req.Messages[2])
	}
	if !strings.Contains(req.Messages[3].Content, "ITERATION 3 OF 10") {
		t.Error("the volatile half is not last")
	}
	for _, m := range req.Messages {
		if m.Role == "assistant" {
			t.Error("a message was placed in the assistant role")
		}
	}

	// An agent with no history sends three messages, not an empty one.
	if n := len(BuildRequest(job(), Reasons{}, loopState(), ModeDevelop, "S", true).Messages); n != 3 {
		t.Errorf("%d messages with no history, want 3", n)
	}
}

// TOOLS WHERE THE BACKEND HAS THEM, the grammar where it does not — and never
// both, which would let the two shapes disagree.
func TestTheReplyIsConstrainedOneWayOrTheOther(t *testing.T) {
	withTools := BuildRequest(job(), Reasons{}, loopState(), ModeDevelop, "S", true)
	if len(withTools.Tools) == 0 {
		t.Error("a tool-capable backend was offered no tools")
	}
	if withTools.Schema != nil {
		t.Error("tools and a grammar were sent together")
	}

	without := BuildRequest(job(), Reasons{}, loopState(), ModeDevelop, "S", false)
	if without.Schema == nil {
		t.Error("a backend without tools was left unconstrained")
	}
	if len(without.Tools) != 0 {
		t.Error("tools were sent to a backend that has none")
	}
}

// THE SAMPLER IS DECODED GREEDILY UNTIL THE AGENT IS DEMONSTRABLY STUCK, and
// every counter that says so has to reach the request.
func TestTheRequestCarriesTheSamplerRamp(t *testing.T) {
	if got := BuildRequest(job(), Reasons{}, loopState(), ModeDevelop, "S", true); got.Temperature != 0 {
		t.Errorf("a healthy first turn was decoded at %v", got.Temperature)
	}
	s := loopState()
	s.StaleReads = 4
	if got := BuildRequest(job(), Reasons{}, s, ModeDevelop, "S", true); got.Temperature == 0 {
		t.Error("a stuck agent was still decoded deterministically")
	}
}

// A READ COSTS NO TURN, and the refund has to be applied on the one path every
// read takes.
func TestAReadRefundsItsTurnAndCountsTowardTheReadRun(t *testing.T) {
	s := loopState()
	verify, err := s.Advance(Action{Action: ActionReadFiles, Paths: []string{"store.go"}}, ModeDevelop)
	if err != nil || verify {
		t.Fatalf("a read asked for a verification: verify=%v err=%v", verify, err)
	}
	if s.Refunded != 1 {
		t.Errorf("Refunded = %d, want the turn given back", s.Refunded)
	}
	if s.ConsecutiveReads != 1 {
		t.Errorf("ConsecutiveReads = %d", s.ConsecutiveReads)
	}
	if len(s.Trail) != 1 {
		t.Error("the read is not in the trail the agent is shown")
	}

	// THE BUDGET THE PROMPT SHOWS IS THE BUDGET THE LOOP ENFORCES. Otherwise the
	// agent counts down to zero and keeps going.
	if got := Budget(50, s.Refunded); got != 51 {
		t.Errorf("Budget = %d, want the refund included", got)
	}
}

// ANY ACTION THAT IS NOT A READ ENDS THE READ RUN, because the run is what names
// the failure — an agent that reads, writes, then reads is not the agent that
// read 87 times.
func TestAWriteEndsTheReadRun(t *testing.T) {
	s := loopState()
	s.ConsecutiveReads = 40

	verify, err := s.Advance(Action{
		Action: ActionWriteFiles, Summary: "add a filter", Type: "feat",
		Edits: []edit.Edit{{Path: "store.go", OldStr: "return nil", Replace: "return []Task{}"}},
	}, ModeDevelop)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if !verify {
		t.Error("a write that changed the tree did not trigger the checks")
	}
	if s.ConsecutiveReads != 0 {
		t.Errorf("ConsecutiveReads = %d after a write", s.ConsecutiveReads)
	}
	// The description is carried forward from the action that supplied it: the
	// commit is written during verification, BEFORE any finish.
	if s.Summary != "add a filter" || s.CommitType != "feat" {
		t.Errorf("the commit description was not carried: %q %q", s.Summary, s.CommitType)
	}
}

// A WRITE THAT CHANGED NOTHING IS NOT A WRITE, and the tree is compared rather
// than a counter incremented — nine separate code paths lost track of a counter
// in one day of runs.
func TestAWriteThatChangesNothingIsRefusedAndNotVerified(t *testing.T) {
	// A genuine no-op: rewrite the file with its own content.
	s2 := loopState()
	s2.Staged["store.go"] = storeGo
	before := TreeHash(s2.Staged)
	verify, err := s2.Advance(Action{
		Action: ActionWriteFiles, Summary: "x", Type: "feat",
		Edits: []edit.Edit{{Path: "store.go", StartLine: 1, EndLine: lineCount(storeGo), Replace: storeGo}},
	}, ModeDevelop)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if verify {
		t.Error("a no-op write triggered the checks")
	}
	if TreeHash(s2.Staged) != before {
		t.Error("a no-op write changed the tree")
	}
	if s2.NoopEdits != 1 || s2.Refusals != 1 {
		t.Errorf("the no-op was not counted: noops=%d refusals=%d", s2.NoopEdits, s2.Refusals)
	}
	if !strings.Contains(s2.Notice, "changed nothing") && !strings.Contains(s2.Notice, "no change") {
		t.Errorf("the notice does not name the cause: %q", s2.Notice)
	}
}

// A REFUSED WRITE IS A REFUSAL, and the message the agent gets is the one Apply
// produced — not a restatement of it.
func TestARefusedWriteCarriesApplysOwnReason(t *testing.T) {
	s := loopState()
	verify, err := s.Advance(Action{
		Action: ActionWriteFiles, Summary: "x", Type: "feat",
		Edits: []edit.Edit{{Path: "store_test.go", Replace: "package main\n"}},
	}, ModeDevelop)
	if err == nil {
		t.Fatal("a developer was allowed to write a test file")
	}
	if verify {
		t.Error("a refused write triggered the checks")
	}
	if !strings.Contains(s.Notice, "tests are written by a separate agent") {
		t.Errorf("the notice does not carry the reason: %q", s.Notice)
	}
	if s.Refusals != 1 {
		t.Errorf("Refusals = %d", s.Refusals)
	}
}

// A REWIND IS PROGRESS, NOT A REFUSAL: it costs a turn and buys a file the agent
// can address again, which is strictly better than editing blind against a file
// it has broken.
func TestAnUndoClearsTheRefusalRunRatherThanAddingToIt(t *testing.T) {
	s := loopState()
	if _, err := s.Advance(Action{
		Action: ActionWriteFiles, Summary: "x", Type: "feat",
		Edits: []edit.Edit{{Path: "store.go", OldStr: "return nil", Replace: "return []Task{}"}},
	}, ModeDevelop); err != nil {
		t.Fatal(err)
	}
	s.Refusals = 5

	if _, err := s.Advance(Action{Action: ActionUndoEdit}, ModeDevelop); err != nil {
		t.Fatal(err)
	}
	if s.Refusals != 0 {
		t.Errorf("an undo counted as a refusal: %d", s.Refusals)
	}
	if !strings.Contains(s.Notice, "Undone") || !strings.Contains(s.Notice, "store.go") {
		t.Errorf("the undo does not say what it restored: %q", s.Notice)
	}

	// With nothing to undo it IS a refusal, because nothing happened.
	if _, err := s.Advance(Action{Action: ActionUndoEdit}, ModeDevelop); err != nil {
		t.Fatal(err)
	}
	if s.Refusals != 1 {
		t.Errorf("an undo with nothing behind it did not count: %d", s.Refusals)
	}
}

// THE STAGE ENDS BY ITSELF. Across 3,498 recorded turns the agents called finish
// once, and one ran to iteration 439 still reading — so waiting to be told is
// waiting for something that does not come.
func TestTheStageFinishesOnItsOwnSuccessCondition(t *testing.T) {
	s := loopState()
	if s.Finished() {
		t.Error("an attempt that has done nothing reported as finished")
	}

	s.Staged["store.go"] = storeGo + "\nfunc List2() {}\n"
	s.RecordVerification("ok  store 0.1s", true, ModeDevelop)
	if !s.Finished() {
		t.Error("a verified, passing, non-empty tree did not finish")
	}

	// A PASS AGAINST A TREE THAT HAS SINCE CHANGED IS NOT A PASS.
	s.Staged["store.go"] += "\nfunc List3() {}\n"
	if s.Finished() {
		t.Error("a stale pass finished the stage")
	}

	// Nor is a pass with nothing written: there is nothing to hand on.
	empty := loopState()
	empty.RecordVerification("ok", true, ModeDevelop)
	if empty.Finished() {
		t.Error("an empty tree finished the stage")
	}
}

// A RED VERIFICATION COUNTS, AND A GREEN ONE CLEARS THE RUN — the referee needs
// the count, and a semantic failure must be able to reach it.
func TestVerificationsAreCountedAndTheTreeIsFingerprinted(t *testing.T) {
	s := loopState()
	s.Staged["store.go"] = "package main\n"

	s.RecordVerification("FAIL", false, ModeDevelop)
	s.RecordVerification("FAIL", false, ModeDevelop)
	if s.FailedVerifications != 2 {
		t.Errorf("FailedVerifications = %d", s.FailedVerifications)
	}
	if !s.TreeVerified() {
		t.Error("the tree that was just verified reports as unverified")
	}

	s.RecordVerification("ok", true, ModeDevelop)
	if s.FailedVerifications != 0 {
		t.Errorf("a green verification did not clear the run: %d", s.FailedVerifications)
	}
}

// TRUNCATION AND MALFORMEDNESS READ THE SAME AND NEED OPPOSITE FIXES.
func TestAnUnusableReplyIsToldWhichKindItWas(t *testing.T) {
	err := errTest("no JSON object in the reply")

	cut := ParseFailureNotice(model.ChatResult{
		FinishReason: model.FinishLength, CompletionTokens: 40}, err)
	if !strings.Contains(cut, "CUT OFF") || !strings.Contains(cut, "SMALLER edit") {
		t.Errorf("a truncated reply was not told to write less:\n%s", cut)
	}

	bad := ParseFailureNotice(model.ChatResult{CompletionTokens: 12}, err)
	if strings.Contains(bad, "CUT OFF") {
		t.Errorf("a short refusal was blamed on the ceiling:\n%s", bad)
	}
	if !strings.Contains(bad, "no JSON object") {
		t.Errorf("the reason was not carried:\n%s", bad)
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }

// LOOSER THAN THE REFUSAL CEILING, because a model handed the parse error
// usually produces a clean action next turn — treating two bad replies as
// terminal would throw away a task over a stray code fence.
func TestARunOfUnparseableRepliesEndsTheAttemptEventually(t *testing.T) {
	s := loopState()
	for range MaxParseFailures - 1 {
		s.ParseFails++
		if _, done := s.StopParsing(); done {
			t.Fatalf("the attempt ended after %d bad replies", s.ParseFails)
		}
	}
	s.ParseFails++
	why, done := s.StopParsing()
	if !done {
		t.Fatal("a run of unparseable replies never ended the attempt")
	}
	if !strings.Contains(why, "could not be parsed") {
		t.Errorf("the reason does not name the cause: %q", why)
	}
	if MaxParseFailures >= MaxDeadRefusals {
		t.Error("the parse bound is no looser than the refusal ceiling")
	}
}

// THE PROSE IS KEPT AND THE SYNTAX IS DROPPED. "I wrote main.go lines 31-41"
// does not tell the next turn that it had already concluded there were duplicate
// NewStore declarations — and that conclusion is what it kept re-deriving, four
// commits running.
func TestTheTurnRecordKeepsTheReasoningWithoutTheSyntax(t *testing.T) {
	act := Action{Action: ActionWriteFiles, Edits: []edit.Edit{
		{Path: "store.go", StartLine: 31, EndLine: 41, Replace: "func List() {}"},
	}}
	reply := `There are two NewStore declarations; removing the second. ` +
		`{"action":"write_files","edits":[{"path":"store.go"}]}`

	got := TurnRecord(act, reply)
	if !strings.Contains(got, "two NewStore declarations") {
		t.Errorf("the reasoning was lost:\n%s", got)
	}
	if strings.Contains(got, `"action"`) {
		t.Errorf("the syntax survived, and the model imitates what it is shown:\n%s", got)
	}
	if !strings.Contains(got, "store.go lines 31-41") {
		t.Errorf("the record does not say what was done:\n%s", got)
	}
}

// AN EXHAUSTED ATTEMPT OTHERWISE REPORTS ONLY ITS LAST TEST OUTPUT, and one that
// ended on a write has none at all — so the ticket says "stopped after 8
// iterations" and nothing else, which is unusable.
func TestAFailureSaysWhatTheAttemptActuallyDid(t *testing.T) {
	got := TrailSummary([]Step{
		{Action: ActionReadFiles, Detail: "store.go"},
		{Action: ActionReadFiles, Detail: "server.go"},
		{Action: ActionWriteFiles, Detail: "store.go lines 1-3, 40B [ab12cd]"},
	})
	for _, want := range []string{"3 actions", "2× read_files", "1× write_files", "the last was"} {
		if !strings.Contains(got, want) {
			t.Errorf("the summary does not say %q:\n%s", want, got)
		}
	}
	if got := TrailSummary(nil); !strings.Contains(got, "no actions") {
		t.Errorf("an attempt that did nothing reported %q", got)
	}
}

// A REAL WRITE CLEARS THE NO-OP RUN, or the counter keeps climbing across
// unrelated mistakes and the agent is told it has made its "5th edit in a row
// that changed nothing" on its first.
func TestARealWriteResetsTheNoopRun(t *testing.T) {
	s := loopState()
	s.Staged["store.go"] = storeGo
	whole := edit.Edit{Path: "store.go", StartLine: 1, EndLine: lineCount(storeGo), Replace: storeGo}

	// Two no-ops in a row.
	for range 2 {
		if _, err := s.Advance(Action{
			Action: ActionWriteFiles, Summary: "x", Type: "feat",
			Edits: []edit.Edit{whole},
		}, ModeDevelop); err != nil {
			t.Fatal(err)
		}
	}
	if s.NoopEdits != 2 {
		t.Fatalf("NoopEdits = %d, want 2", s.NoopEdits)
	}

	// A real one.
	if _, err := s.Advance(Action{
		Action: ActionWriteFiles, Summary: "x", Type: "feat",
		Edits: []edit.Edit{{Path: "store.go", OldStr: "return nil", Replace: "return []Task{}"}},
	}, ModeDevelop); err != nil {
		t.Fatal(err)
	}
	if s.NoopEdits != 0 {
		t.Errorf("NoopEdits = %d after a real write; the run never resets", s.NoopEdits)
	}

	// And the next no-op is the FIRST again, which is the message that says an
	// earlier attempt may already have written the file.
	if _, err := s.Advance(Action{
		Action: ActionWriteFiles, Summary: "x", Type: "feat",
		Edits: []edit.Edit{{Path: "store.go", StartLine: 1, EndLine: lineCount(s.Staged["store.go"]),
			Replace: s.Staged["store.go"]}},
	}, ModeDevelop); err != nil {
		t.Fatal(err)
	}
	if s.NoopEdits != 1 {
		t.Errorf("NoopEdits = %d, want the run to have restarted", s.NoopEdits)
	}
	if strings.Contains(s.Notice, "in a row") {
		t.Errorf("a first no-op was reported as a run:\n%s", s.Notice)
	}
}

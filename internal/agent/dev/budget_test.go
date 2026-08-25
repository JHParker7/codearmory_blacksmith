package dev

import (
	"strings"
	"testing"
)

// EVERY WAY AN ATTEMPT CAN END, and each says which bound it hit — a failure
// reported as "stopped after 8 iterations" and nothing else is unusable.
func TestEveryWayAnAttemptEndsNamesTheBoundItHit(t *testing.T) {
	cases := []struct {
		name  string
		state State
		want  string
	}{
		{"the turn budget", State{Iteration: 51, Budget: 50}, "budget of 50"},
		{"the diagnostic ceiling", State{Iteration: MaxTotalIterations + 1, Budget: 0},
			"diagnostic ceiling"},
		{"reading and never writing", State{ConsecutiveReads: MaxConsecutiveReads},
			"read 100 times in a row"},
		{"refusing with nothing to show", State{Refusals: MaxDeadRefusals},
			"20 actions in a row that changed nothing"},
	}
	for _, c := range cases {
		why, done := c.state.Exhausted(MaxDeadRefusals)
		if !done {
			t.Errorf("%s: the attempt was allowed to continue", c.name)
			continue
		}
		if !strings.Contains(why, c.want) {
			t.Errorf("%s: reason = %q, want it to name %q", c.name, why, c.want)
		}
	}
}

// A HEALTHY ATTEMPT IS NEVER STOPPED. Every bound above has, at some point in
// this repository's history, killed work it should not have.
func TestAnAttemptWithRoomLeftIsNotStopped(t *testing.T) {
	s := State{
		Iteration: 6, Budget: 50,
		Refusals: MaxRepeatRefusals, ConsecutiveReads: 12,
	}
	if why, done := s.Exhausted(MaxDeadRefusals); done {
		t.Errorf("a working attempt was stopped: %s", why)
	}
}

// AN UNKNOWN BUDGET IS NOT A BUDGET OF ZERO. Zero means "nothing configured",
// and treating it as a limit would end every attempt on its first turn.
func TestAnUnsetBudgetDoesNotEndTheAttempt(t *testing.T) {
	if why, done := (&State{Iteration: 1, Budget: 0}).Exhausted(MaxDeadRefusals); done {
		t.Errorf("an unset budget ended the attempt: %s", why)
	}
	// The diagnostic ceiling still applies, which is what makes it safe.
	if _, done := (&State{Iteration: MaxTotalIterations + 1, Budget: 0}).Exhausted(MaxDeadRefusals); !done {
		t.Error("an unset budget let a runaway run forever")
	}
}

// A BUDGET OF N GIVES N TURNS, NOT N-1. The check runs at the TOP of a turn, so
// at the start of turn N the agent has taken N-1 actions — and a budget of one
// with a ">=" test gives the agent no turn at all.
func TestABudgetOfNGivesExactlyNTurns(t *testing.T) {
	for _, budget := range []int{1, 2, 8, 50} {
		// The last turn it is entitled to.
		last := State{Iteration: budget, Budget: budget}
		if why, done := last.Exhausted(MaxDeadRefusals); done {
			t.Errorf("budget %d: turn %d was refused (%s)", budget, budget, why)
		}
		// And the one after it is not.
		past := State{Iteration: budget + 1, Budget: budget}
		if _, done := past.Exhausted(MaxDeadRefusals); !done {
			t.Errorf("budget %d: turn %d was allowed", budget, budget+1)
		}
	}
}

// ZERO TURNS THE REFUSAL CEILING OFF, which is what it was before it existed.
//
// It is a knob because whether ending an attempt here HELPS is an open question:
// every run since it started firing has ended its first developer attempt on it,
// which bounds the waste but throws away the context that attempt had built.
// r96 did the same work in six turns without reaching it.
func TestTheRefusalCeilingCanBeTurnedOff(t *testing.T) {
	s := State{Refusals: 500, Iteration: 5, Budget: 50}
	if why, done := s.Exhausted(0); done {
		t.Errorf("a disabled ceiling still fired: %s", why)
	}
	// And the budget is then the only bound, which is what it was before.
	s.Iteration = 51
	if _, done := s.Exhausted(0); !done {
		t.Error("with the ceiling off, the budget did not end the attempt")
	}
}

// REFUSALS GO UP IN EXACTLY ONE PLACE, which is what keeps the loop breaker
// honest: a rejection added later counts toward termination automatically. It
// was invisible once already — r56 failed four developer attempts at twenty
// refusals each and reported a refusal count of ZERO.
func TestARefusalIsCountedAndAProgressClearsTheRun(t *testing.T) {
	var s State
	s.NoProgress("Rejected: old_str matched nothing")
	s.NoProgress("Rejected: old_str matched nothing")
	if s.Refusals != 2 {
		t.Errorf("Refusals = %d, want 2", s.Refusals)
	}
	if !strings.Contains(s.Notice, "matched nothing") {
		t.Errorf("the notice does not carry the refusal: %q", s.Notice)
	}

	s.Progress()
	if s.Refusals != 0 || s.Notice != "" {
		t.Errorf("an action that changed something did not end the run: %d %q", s.Refusals, s.Notice)
	}
}

// THERE IS ONLY ONE ACTION THAT CAN MAKE PROGRESS NOW. Naming a verification —
// which the agent can no longer choose — cost 28 turns on the dev fixture, and
// two guards giving opposite orders abandoned an attempt four refusals later.
func TestTheDirectiveNamesTheOneActionThatCanHelp(t *testing.T) {
	var s State

	// It stays quiet at first: one refusal is a mistake, not a pattern.
	s.NoProgress("Rejected: something")
	if strings.Contains(s.Notice, "in a row") {
		t.Errorf("a single refusal produced a lecture: %q", s.Notice)
	}

	s.NoProgress("Rejected: something")
	if !strings.Contains(s.Notice, "actually different") {
		t.Errorf("the directive does not say what would help: %q", s.Notice)
	}
	// IT NAMES NO TOOL AT ALL. It used to name write_files, and went on naming it
	// after the write tool was flattened to write_file — pointing an agent that
	// was already making no progress at a tool it could not call. The model can
	// see which tools it has; naming one was never the useful part and was the
	// part that went stale. The test that should have caught the drift asserted
	// the same literal, so it drifted too.
	for _, tool := range Actions {
		if strings.Contains(s.Notice, tool) {
			t.Errorf("the directive names the tool %q; it will be wrong the next "+
				"time a tool is renamed: %q", tool, s.Notice)
		}
	}
	if strings.Contains(s.Notice, "run_tests") {
		t.Errorf("the directive names an action the agent cannot choose: %q", s.Notice)
	}
	// NO THREAT OF ABANDONMENT: repetition no longer abandons anything on its
	// own, and a prompt that threatens a consequence it cannot deliver teaches the
	// model to discount the next one.
	for _, lie := range []string{"abandon", "will be stopped", "last chance"} {
		if strings.Contains(strings.ToLower(s.Notice), lie) {
			t.Errorf("the directive threatens %q, which it cannot deliver: %q", lie, s.Notice)
		}
	}
	// What IS true is that the turns are being spent.
	if !strings.Contains(s.Notice, "cost a turn") {
		t.Errorf("the directive does not say what repetition actually costs: %q", s.Notice)
	}
}

// THE WORK IS NOT THE PROBLEM; THE MEMORY IS. An attempt that has run forty
// turns has usually written real code and then lost the thread — 77 turns of
// which 46 were repeat verifications, the trail showing 46 identical entries
// under a heading telling it not to repeat itself.
func TestAResetKeepsTheWorkAndDiscardsTheConfusion(t *testing.T) {
	s := &State{
		Iteration: 40,
		Staged:    map[string]string{"store.go": "package main\n"},
		Read:      map[string]string{"store.go": "package main\n"},
		Trail:     []Step{{Action: ActionReadFiles}, {Action: ActionWriteFiles}},
		Notice:    "Rejected: nothing changed",
		Refusals:  9, NoopEdits: 4, ConsecutiveReads: 30,
		LastTest: "FAIL store_test.go:12", TestsPass: false,
		UndoStack: []map[string]string{{}},
	}
	s.Remember("write_files(store.go)", "staged")

	s.ResetAgent("YOU ARE PICKING UP THIS TICKET PART-WAY THROUGH")

	// WHAT SURVIVES IS THE WORLD.
	if len(s.Staged) != 1 {
		t.Error("the reset discarded the work; the push would lose it outright")
	}
	// The agent's own writes become ground truth: to the agent that continues,
	// they are simply the code that was already there.
	if s.Baseline["store.go"] != "package main\n" {
		t.Errorf("the previous turns' work is not part of the baseline: %v", s.Baseline)
	}
	// THE LAST VERIFICATION SURVIVES: "fix the remaining failures" needs the
	// failures, or the fresh agent spends its first turn rediscovering them.
	if s.LastTest == "" {
		t.Error("the reset discarded what is still failing")
	}

	// EVERYTHING ELSE IS MEMORY OF HOW IT GOT HERE.
	if s.Trail != nil || s.History != nil {
		t.Error("the reset kept the trail the agent was imitating")
	}
	if s.Refusals != 0 || s.NoopEdits != 0 || s.Notice != "" {
		t.Errorf("the reset kept a counter: %+v", s)
	}
	// THE READ RUN DELIBERATELY SURVIVES. It is not memory of how the agent got
	// here, it is evidence about the agent itself — and clearing it every forty
	// turns means an agent that only ever reads can never reach the hundred-read
	// ceiling, so the ticket reports "ran to 200 turns" instead of naming the
	// actual failure.
	if s.ConsecutiveReads != 30 {
		t.Errorf("ConsecutiveReads = %d; the read ceiling can now never fire", s.ConsecutiveReads)
	}
	if s.UndoStack != nil {
		t.Error("the reset kept an undo stack pointing at a tree the agent no longer knows")
	}
	if !strings.Contains(s.Restart, "PART-WAY THROUGH") {
		t.Errorf("the fresh agent was not briefed: %q", s.Restart)
	}
	if s.Resets != 1 || s.LastResetAt != 40 {
		t.Errorf("the reset was not recorded: resets=%d at=%d", s.Resets, s.LastResetAt)
	}
}

// A REPEAT COUNTER THAT SURVIVED A RESET would collapse the first entry of the
// fresh agent's history into the old one's, which is the confusion being thrown
// away.
func TestAResetClearsTheRepeatCollapseToo(t *testing.T) {
	s := &State{Staged: map[string]string{}}
	s.Remember("write_files(a.go)", "staged")
	s.Remember("write_files(a.go)", "staged")

	s.ResetAgent("fresh")
	s.Remember("write_files(a.go)", "staged")

	if len(s.History) != 1 {
		t.Fatalf("the fresh agent inherited %d entries", len(s.History))
	}
	if strings.Contains(s.History[0], "times in a row") {
		t.Errorf("the fresh agent's first action was counted as a repeat: %q", s.History[0])
	}
}

// A RESET THAT HAS NOT HELPED FOUR TIMES WILL NOT HELP A FIFTH.
func TestResetsAreBounded(t *testing.T) {
	s := &State{Iteration: AgentResetTurns}
	if !s.DueForReset() {
		t.Fatal("an attempt at the reset interval was not due")
	}
	for range MaxAgentResets {
		s.ResetAgent("x")
		s.Iteration += AgentResetTurns
	}
	if s.DueForReset() {
		t.Errorf("a %dth reset was allowed", s.Resets+1)
	}
}

// THE INTERVAL IS SINCE THE LAST RESET, not since the start: otherwise the
// second reset fires on the turn after the first.
func TestAResetIsNotDueImmediatelyAfterOne(t *testing.T) {
	s := &State{Iteration: AgentResetTurns}
	s.ResetAgent("x")
	s.Iteration++
	if s.DueForReset() {
		t.Error("a reset fired one turn after the last one")
	}
	s.Iteration = AgentResetTurns * 2
	if !s.DueForReset() {
		t.Error("the second reset never became due")
	}
}

// AN AGENT TOLD ONLY THAT ITS MEMORY WAS CLEARED has every reason to start over,
// which is the one outcome the reset exists to prevent.
func TestTheRestartBriefSaysTheWorkIsKept(t *testing.T) {
	got := RestartBrief(ModeDevelop, []string{"store.go", "filter.go"}, 2)
	for _, want := range []string{
		"ALREADY WRITTEN", "store.go", "filter.go",
		"rather than starting over", "restart 2",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the brief does not say %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "not a draft to redo") != true {
		t.Errorf("the brief does not head off a rewrite:\n%s", got)
	}

	// AN AGENT WITH NOTHING WRITTEN MUST NOT BE TOLD ITS WORK IS ON THE BRANCH.
	// It would then look for files that are not there.
	fresh := RestartBrief(ModeDevelop, nil, 1)
	if strings.Contains(fresh, "ALREADY WRITTEN") {
		t.Errorf("an agent with no work was told it had some:\n%s", fresh)
	}
	if !strings.Contains(fresh, "Nothing has been written") {
		t.Errorf("the fresh brief does not say the tree is untouched:\n%s", fresh)
	}
}

// THE TWO ROUTES TO "THIS SPECIFICATION CANNOT BE SATISFIED" ARE SEPARATE
// BOUNDS, and they were in direct conflict once: proving it through repeated
// verification required work the refusal ceiling forbade. r57 hit 63 test-file
// refusals against a spec whose only fault was `declared and not used: tasks`,
// the attempt was killed at 20 refusals four times over, and the specification
// was never once judged broken.
func TestTheTwoRoutesToABrokenSpecificationDoNotBlockEachOther(t *testing.T) {
	if MaxTestEditRefusals >= MaxDeadRefusals {
		t.Errorf("the test-edit route (%d) needs more refusals than the ceiling allows (%d)",
			MaxTestEditRefusals, MaxDeadRefusals)
	}
	if MinSpecBrokenTries >= MaxDeadRefusals {
		t.Errorf("the verification route (%d tries) cannot complete before the ceiling (%d)",
			MinSpecBrokenTries, MaxDeadRefusals)
	}
	// And the repeat ceiling must not be the thing that ends an attempt before
	// either route is reachable.
	if MaxRepeatRefusals >= MaxTestEditRefusals+MaxDeadRefusals {
		t.Error("the repeat ceiling arrives before a broken specification can be proved")
	}
}

// THE BOUNDS MUST ORDER SENSIBLY, or a stricter one silently makes a looser one
// unreachable — which is how a guard meant to stop waste became the reason two
// tickets reached nobody.
func TestTheBoundsDoNotMakeEachOtherUnreachable(t *testing.T) {
	if MaxDeadRefusals <= MaxRepeatRefusals {
		t.Error("the dead-refusal ceiling arrives no later than the repeat ceiling")
	}
	if MaxTotalIterations <= DefaultMaxIterations {
		t.Error("the diagnostic ceiling is inside the working budget")
	}
	if AgentResetTurns >= MaxTotalIterations {
		t.Error("no reset can ever fire before the diagnostic ceiling")
	}
	// A reset must be able to happen more than once inside the ceiling, or
	// MaxAgentResets is decoration.
	if AgentResetTurns*2 > MaxTotalIterations {
		t.Error("only one reset fits inside the diagnostic ceiling")
	}
}

// THE BRIEF MUST MATCH THE JOB. One loop serves three stages, and the first
// version told all of them to "make the remaining test failures pass" — which is
// the developer's job and the EXACT INVERSE of the specification author's, whose
// tests are supposed to fail. Observed on a live ticket: an author was restarted
// twice and instructed, in its own prompt, to do the one thing its gate refuses.
func TestARestartedStageIsToldItsOwnJob(t *testing.T) {
	author := RestartBrief(ModeTest, []string{"store_test.go"}, 1)
	if strings.Contains(author, "make the remaining test failures pass") {
		t.Errorf("the author was told to do the inverse of its job:\n%s", author)
	}
	if !strings.Contains(author, "must FAIL against the current code") {
		t.Errorf("the author was not told what makes its tests a specification:\n%s", author)
	}

	dev := RestartBrief(ModeDevelop, []string{"store.go"}, 1)
	if !strings.Contains(dev, "make the remaining test failures pass") {
		t.Errorf("the developer was not told its job:\n%s", dev)
	}

	cover := RestartBrief(ModeCoverage, []string{"coverage_test.go"}, 1)
	if !strings.Contains(cover, "must not be edited") {
		t.Errorf("the coverage stage was not told the specification is not its to change:\n%s", cover)
	}

	// EVERY MODE SAYS SOMETHING, or a restarted agent is briefed on nothing.
	for _, mode := range []Mode{ModeDevelop, ModeTest, ModeCoverage} {
		if strings.TrimSpace(mode.RestartJob()) == "" {
			t.Errorf("mode %d has no restart brief", mode)
		}
	}
}

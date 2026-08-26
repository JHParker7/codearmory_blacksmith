package dev

import (
	"strings"
	"testing"
)

// A RESET ARRIVES WHILE THE LOOP IS RUNNING, NOT AFTER IT.
//
// Forty turns was set before there was a day of runs to read against it, and
// measured on those it almost never fired: run 95's developer circled main() for
// 18 turns, run 94 oscillated on board.go for 36, run 93 managed 43. Loops form
// at ten to twenty turns.
func TestTheMemoryIsClearedWhileTheLoopIsStillRunning(t *testing.T) {
	s := &State{Iteration: AgentResetTurns}
	if !s.DueForReset() {
		t.Errorf("no reset after %d turns; the loops this exists for are shorter "+
			"than that", AgentResetTurns)
	}

	// Run 94's developer oscillated for 36 turns. It should be reset twice.
	s2 := &State{}
	resets := 0
	for s2.Iteration = 1; s2.Iteration <= 36; s2.Iteration++ {
		if s2.DueForReset() {
			s2.ResetAgent("brief")
			resets++
		}
	}
	if resets < 2 {
		t.Errorf("a 36-turn attempt was reset %d time(s); run 94 ran that long "+
			"without one", resets)
	}
}

// THE LAST VERIFICATION SURVIVES THE RESET, which is the whole point of clearing
// memory rather than restarting the attempt: "fix the remaining failures" needs
// the failures. A fresh agent without them spends its first turn rediscovering
// what the previous one already knew.
func TestTheFailingOutputSurvivesAReset(t *testing.T) {
	const failure = "--- FAIL: TestMainListensOnPortFromEnv (5.02s)\n" +
		"    main_test.go:58: server did not start listening on port 36893"

	s := &State{
		Iteration: AgentResetTurns,
		LastTest:  failure,
		Staged:    map[string]string{"main.go": "package main\n\nfunc main() {}\n"},
		Trail:     []Step{{Action: "write_file", Detail: "main.go"}},
	}
	s.ResetAgent("you are picking this up part-way through")

	if s.LastTest != failure {
		t.Errorf("the failing output was discarded by the reset; the fresh agent has "+
			"nothing to work from:\n%q", s.LastTest)
	}
	// AND THE WORK SURVIVES AS GROUND TRUTH. Staged has to live: the push
	// re-applies it, so clearing it would lose unverified writes outright.
	if s.Staged["main.go"] == "" {
		t.Error("the code the agent had written was discarded")
	}
	if s.Baseline["main.go"] == "" {
		t.Error("the agent's own writes were not folded into the baseline, so its " +
			"next edit reads as discarding code that was already there")
	}
	// AND THE CONFUSION DOES NOT.
	if len(s.Trail) != 0 {
		t.Error("the action trail survived; the repetition it records is what the " +
			"reset exists to break")
	}
}

// Bounding is covered by TestResetsAreBounded in budget_test.go.

// THE FRESH AGENT IS TOLD IT IS PICKING UP WORK, not starting it. Without that
// it reads the staged files as someone else's and rewrites them from scratch.
func TestTheResetAgentIsBriefed(t *testing.T) {
	s := &State{Iteration: AgentResetTurns, Staged: map[string]string{"main.go": "x"}}
	s.ResetAgent(RestartBrief(ModeDevelop, []string{"main.go"}, 1))

	if !strings.Contains(s.Restart, "main.go") {
		t.Errorf("the brief does not name the files already written: %q", s.Restart)
	}
}

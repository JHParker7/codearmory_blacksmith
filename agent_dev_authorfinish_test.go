package main

import (
	"strings"
	"testing"
)

// EVERY STAGE IS ENDED BY ITS GATE, INCLUDING THE SPECIFICATION AUTHOR.
//
// Both alternatives were tried on real runs and both were worse.
//
// The gate ending the author truncates it when its brief holds several units of
// work: its gate passes as soon as its first file compiles and fails correctly,
// which is true after one slice as much as after five. Measured on r89 — an
// author briefed on five units wrote one test file in three turns, and the
// pipeline reported success on a repository that did not compile.
//
// Letting the author end its own stage was measured worse. On r90, with finish
// restored, the same author ran 49 turns and wrote the same file 41 times without
// ever finishing — which is what the tool was removed for originally: across
// 3,498 recorded turns the agents called it once, and one ran to iteration 439
// still reading. That behaviour is not confined to the model this repository
// replaced.
//
// So the BRIEF is what gets sized, not the stopping rule. One unit of work per
// author means the gate and the job end at the same moment.

func TestNoStageEndsItsOwnSuccessfulRun(t *testing.T) {
	for _, mode := range []agentMode{modeDevelop, modeTest, modeCoverage, modeSpecMerge} {
		if contains(actionsFor(mode), actionFinish) {
			t.Errorf("mode %v can end its own stage; measured on r90, an author given that ran 49 turns rewriting one file", mode)
		}
	}
}

// The offered tools and the accepted ones must not drift apart — a tool offered
// and then refused is the trap the filter exists to prevent.
func TestOfferedToolsMatchWhatIsAccepted(t *testing.T) {
	for _, mode := range []agentMode{modeDevelop, modeTest} {
		for _, tool := range devTools(mode) {
			if !contains(actionsFor(mode), tool.Name) {
				t.Errorf("mode %v is offered %q but would refuse it", mode, tool.Name)
			}
		}
	}
}

// The author must not be told to call a tool it does not have.
func TestTheAuthorIsNotToldToFinish(t *testing.T) {
	if strings.Contains(testerSystemPrompt(), "CALL finish") {
		t.Error("the author is told to call finish, which is not in its vocabulary; every such turn is refused")
	}
}

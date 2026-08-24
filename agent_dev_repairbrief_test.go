package main

import (
	"strings"
	"testing"
)

// THE RECONCILER HAS TWO JOBS AND THE BRIEF HAS TO SAY WHICH.
//
// Reconciling collisions is the first. The second arrived when the developer's
// hand-back was routed here — this is the only stage allowed to edit a task's
// tests, so an unsatisfiable specification is its to repair — and for a while the
// brief still described only the first, in terms that actively denied the second:
// "YOUR ONLY JOB is to make these files compile TOGETHER. Nothing else."
//
// Measured on r83, and it cost seventeen minutes. The files compiled perfectly,
// so by that brief there was nothing to do. The agent went looking anyway: three
// attempts to edit api.go (refused — it may not touch implementation) and sixteen
// no-op edits, over seventy-six turns.
//
// A stage given a new responsibility and not told about it does not conclude that
// it has nothing to do. It improvises.

func TestTheRepairBriefDescribesTheRepair(t *testing.T) {
	repair := specMergeSystemPrompt(true)

	if strings.Contains(repair, "YOUR ONLY JOB is to make these files compile TOGETHER") {
		t.Error("a hand-back is briefed as a collision merge; the files already compile and there is nothing for it to do")
	}
	for _, want := range []string{
		"could not satisfy", // what happened
		"panics inside",     // r81's shape
		"import block",      // r82's shape
		"contradict each other",
	} {
		if !strings.Contains(repair, want) {
			t.Errorf("the repair brief does not mention %q", want)
		}
	}
}

// The collision brief must survive unchanged: it is the common case and the one
// with the expensive temptation.
func TestTheCollisionBriefIsUnchanged(t *testing.T) {
	merge := specMergeSystemPrompt(false)

	for _, want := range []string{"compile TOGETHER", "DO NOT DELETE A TEST"} {
		if !strings.Contains(merge, want) {
			t.Errorf("the reconciler brief lost %q", want)
		}
	}
	if merge == specMergeSystemPrompt(true) {
		t.Error("both jobs get the same brief, which is the bug this splits")
	}
}

// BOTH BRIEFS MUST REFUSE TO WEAKEN A TEST. It is the one temptation that makes
// the failure go away, compiles, passes the gate, and silently removes a
// requirement nothing downstream can tell is missing.
func TestNeitherBriefPermitsWeakeningATest(t *testing.T) {
	for name, prompt := range map[string]string{
		"collision": specMergeSystemPrompt(false),
		"repair":    specMergeSystemPrompt(true),
	} {
		if !strings.Contains(strings.ToUpper(prompt), "NOT") {
			t.Errorf("the %s brief has no prohibition at all", name)
		}
		if !strings.Contains(strings.ToLower(prompt), "weaken") {
			t.Errorf("the %s brief does not forbid weakening an assertion", name)
		}
	}
}

// The repair brief must send a genuine implementation fault back rather than
// invite the stage to fix it — editing non-test files is refused, and an agent
// that keeps trying spends the ticket's budget discovering that.
func TestTheRepairBriefSaysWhatToDoWithACodeFault(t *testing.T) {
	repair := specMergeSystemPrompt(true)

	if !strings.Contains(repair, "DO NOT EDIT THE IMPLEMENTATION") {
		t.Error("the repair brief does not say the implementation is off limits")
	}
	if !strings.Contains(repair, "finish") {
		t.Error("the repair brief does not tell it how to hand a code fault back")
	}
}

// The mode alone cannot decide: one agent serves many tickets, and which job this
// is belongs to the ticket.
func TestTheJobComesFromTheTicketNotTheAgent(t *testing.T) {
	a := NewSpecMergeAgent(nil, nil, ClassLarge, RepoConfig{}, 10)

	fresh := a.systemPrompt(Ticket{TicketID: "t1000000"})
	handed := a.systemPrompt(Ticket{TicketID: "t1000000", Comments: []Comment{
		{Body: specRepairMarker + " the tests cannot pass"},
	}})

	if fresh == handed {
		t.Error("a handed-back ticket gets the same brief as a fresh one; the ticket is what distinguishes them")
	}
}

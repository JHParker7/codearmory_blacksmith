package dev

import (
	"context"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/agent/referee"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/transcript"
)

// THE VERDICT MUST NOT OUTLIVE THE EVIDENCE FOR IT.
//
// Read off run 42, which blocked a ticket over this. Attempt two opened with
// compile errors in three test files — genuinely the author's, nothing the
// developer could fix — and the verdict was set. Four minutes later the only
// errors left were three on one line of handlers.go, the developer's own file
// and a single edit to fix. endOnBrokenSpec reads the FIELD, not the output, so
// the stale verdict sent it back to the author, spent the last repair and
// blocked the ticket.
func TestAVerdictIsClearedWhenTheFailureLeavesTheTestFiles(t *testing.T) {
	s := &State{Staged: map[string]string{"handlers.go": "package main\n"}}

	// The evidence that justifies a verdict: every implicated file is a test.
	s.SpecBrokenTries = MinSpecBrokenTries
	s.Writes = 9
	s.JudgeSpec("./handlers_test.go:12:9: declared and not used: got\n"+
		"./main_test.go:20:2: missing return", ModeDevelop)
	if s.SpecBroken == "" {
		t.Fatal("no verdict was reached on test-only compile errors, so there is " +
			"nothing to go stale")
	}

	// The failure the developer is actually facing now, in its own file.
	s.JudgeSpec("./handlers.go:94:27: cannot use req.Title (variable of type *string) "+
		"as string value in argument to s.Update", ModeDevelop)

	if s.SpecBroken != "" {
		t.Errorf("the verdict survived into a failure in the developer's own file: %q — "+
			"the author would be sent work it cannot do", s.SpecBroken)
	}
	if s.SpecFault != "" {
		t.Errorf("the stale cause survived: %q", s.SpecFault)
	}
}

// AND A VERDICT ON EVIDENCE THAT STILL STANDS IS KEPT. Clearing on every
// verification would make the hand-back unreachable.
func TestAVerdictSurvivesWhileItsEvidenceDoes(t *testing.T) {
	s := &State{Staged: map[string]string{"handlers.go": "package main\n"}}
	s.SpecBrokenTries = MinSpecBrokenTries
	s.Writes = 9

	out := "./handlers_test.go:12:9: declared and not used: got"
	s.JudgeSpec(out, ModeDevelop)
	if s.SpecBroken == "" {
		t.Fatal("no verdict to keep")
	}
	s.JudgeSpec(out, ModeDevelop)
	if s.SpecBroken == "" {
		t.Error("a verdict was dropped while the fault that justified it was unchanged")
	}
}

// THE SECOND OPINION IS ASKED BY THE EVIDENCE, NOT BY A SCHEDULE.
//
// A verdict ruled on one failure says nothing about the next, so a failure that
// has CHANGED is a question that has not been asked.
func TestAChangedFailureAsksTheRefereeAgain(t *testing.T) {
	j := &sequenceJudge{}
	a := &Agent{ref: j, mode: ModeDevelop}
	s := &State{FailedVerifications: RefereeAfterFailures}

	s.LastTest = "--- FAIL: TestA\n    a_test.go:10: got 1 want 2"
	a.consultReferee(context.Background(), ticket.Ticket{}, s)
	if j.judged != 1 {
		t.Fatalf("the first look did not happen: judged=%d", j.judged)
	}

	// Same failure again: nothing new to rule on.
	a.consultReferee(context.Background(), ticket.Ticket{}, s)
	if j.judged != 1 {
		t.Errorf("an unchanged failure bought a second verdict: judged=%d", j.judged)
	}

	// A different failure entirely — the question has moved.
	s.LastTest = "./handlers.go:94:27: cannot use req.Title as string"
	a.consultReferee(context.Background(), ticket.Ticket{}, s)
	if j.judged != 2 {
		t.Errorf("a changed failure did not reach the referee: judged=%d", j.judged)
	}
}

// IT CONVICTS BUT DOES NOT ACQUIT, and the deference is deliberate.
//
// A compile fault local to a test file is conclusive on sight, so while such a
// verdict stands there is nothing to arbitrate and the referee is not asked at
// all. An acquittal here would put a sampler's bad day between a broken
// specification and the only agent that can fix it. What made a standing verdict
// dangerous was that it outlived its evidence, and that is fixed in JudgeSpec,
// where it was caused — see TestAVerdictIsClearedWhenTheFailureLeavesTheTestFiles.
func TestTheRefereeIsNotAskedWhileAVerdictStands(t *testing.T) {
	j := &sequenceJudge{answers: []*referee.Verdict{
		{Owner: referee.OwnerDev, Confidence: "high", Reason: "the implementation is wrong"},
	}}
	a := &Agent{ref: j, mode: ModeDevelop}
	s := &State{
		FailedVerifications: RefereeAfterFailures,
		SpecBroken:          "handlers_test.go",
		SpecFault:           "they do not compile",
		LastTest:            "./handlers_test.go:12:9: declared and not used",
	}

	a.consultReferee(context.Background(), ticket.Ticket{}, s)

	if j.judged != 0 {
		t.Errorf("the referee was asked to arbitrate a fault that is conclusive "+
			"on sight: judged=%d", j.judged)
	}
	if s.SpecBroken == "" {
		t.Error("the standing verdict was dropped by a consult that should not have run")
	}
}

// compile-time proof the fixture still satisfies the interface the agent wants.
var _ Referee = (*sequenceJudge)(nil)

var _ = transcript.Recorder{}

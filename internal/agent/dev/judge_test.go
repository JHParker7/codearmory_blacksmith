package dev

import (
	"context"
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/agent/referee"
	"github.com/code-armory-app/blacksmith/internal/edit"
	"github.com/code-armory-app/blacksmith/internal/forge"
	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

// redRun drives the developer through n failing verifications with a referee
// attached, and reports what the loop did.
//
// The verdict is set on Judge rather than Preflight, because the two halves are
// asked at different times and the point of these tests is the later one.
func redRun(t *testing.T, j *judge, n int) (workflow.Outcome, string) {
	t.Helper()

	// THE EDITS ALTERNATE so that every one of them actually applies. old_str must
	// match exactly once, so repeating the same edit makes the second fail to
	// apply — and an edit that does not apply never reaches a verification, which
	// is the thing being counted here.
	// THE READ COMES FIRST because an edit may only address a file the agent has
	// read. Without it every write is refused, no write reaches a verification,
	// and the count this drives never moves — which looks exactly like a referee
	// that is never asked.
	replies := []model.ChatResult{readCall("main.go")}
	for i := range n {
		from, to := "return nil", "return []Task{}"
		if i%2 == 1 {
			from, to = to, from
		}
		replies = append(replies, writeCall(edit.Edit{
			Path: "main.go", OldStr: from, Replace: to,
		}))
	}
	verdicts := make([]forge.Result, 0, n)
	for range n {
		verdicts = append(verdicts, red())
	}

	b := &box{
		tree:     []string{"main.go", "store_test.go"},
		files:    map[string]string{"main.go": storeGo, "store_test.go": "package main\n"},
		verdicts: verdicts,
	}
	out, detail, err := devWithJudge(&gw{replies: replies}, b, &board{}, j).
		Handle(context.Background(), devTicket())
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	return out, detail
}

// THE HALF THAT WAS NEVER ASKED FOR.
//
// Preflight reads the tests alone, so it cannot see a test that builds its own
// local value and calls code reading a package-level one: that compiles, nothing
// panics, and the assertions fail against state the code cannot reach. There is
// no pattern to match — only a judgement about two files read together against a
// real failure. Measured on a live board: 25 verification runs on exactly that.
func TestTheRefereeIsAskedOnceThereIsEnoughToJudge(t *testing.T) {
	j := &judge{judgement: &referee.Verdict{
		Owner:      referee.OwnerSpec,
		Reason:     "the test writes to a local store the handler cannot read",
		Confidence: "high",
	}}

	out, detail := redRun(t, j, RefereeAfterFailures+2)

	if j.judged == 0 {
		t.Fatalf("the referee was never asked, so the attempt runs to its ceiling on "+
			"a specification nothing could satisfy (outcome %q: %s)", out, detail)
	}
	if out != workflow.OutcomeReturned {
		t.Errorf("outcome = %q (%s), want the ticket handed back to its author", out, detail)
	}
}

// NOT BEFORE THERE IS EVIDENCE. Test-first work is RED BY DESIGN at the start,
// so asking on the first failure would put the referee on every ticket at its
// most misleading moment — and a wrong "spec" spends one of only two repairs.
func TestTheRefereeIsNotAskedBeforeThereIsEvidence(t *testing.T) {
	j := &judge{judgement: &referee.Verdict{
		Owner: referee.OwnerSpec, Reason: "no", Confidence: "high",
	}}

	redRun(t, j, RefereeAfterFailures-1)

	if j.judged != 0 {
		t.Errorf("the referee was asked after fewer than %d failed verifications; the "+
			"first red is the expected state of test-first work, not evidence of a fault",
			RefereeAfterFailures)
	}
}

// BOUNDED PER ATTEMPT, AND IT USED TO BE ONCE.
//
// The old rule was defended on the grounds that the specification does not
// change while the developer works. True, and not the point: WHICH PART of it
// the developer is stuck against changes completely, and a verdict taken at the
// earliest legal moment was then frozen over everything that followed. Run 21
// spent seventy turns oscillating against an assertion no implementation could
// satisfy, holding a verdict reached ninety seconds in.
//
// What keeps the cost down now is not asking once but asking rarely: a compile
// error never reaches the referee at all, and an assertion failure reaches it
// only after coming back RefereeOnRecurrence times. The ceiling is the backstop,
// not the mechanism.
func TestTheRefereeIsAskedABoundedNumberOfTimesPerAttempt(t *testing.T) {
	j := &judge{judgement: &referee.Verdict{
		Owner: referee.OwnerDev, Reason: "the handler is missing", Confidence: "high",
	}}

	redRun(t, j, RefereeAfterFailures+5)

	if j.judged == 0 {
		t.Error("the referee was never asked at all")
	}
	if j.judged > MaxRefereeAsks {
		t.Errorf("the referee was asked %d times in one attempt, past the %d ceiling",
			j.judged, MaxRefereeAsks)
	}
}

// A MAYBE IS WORTH LESS THAN THE DEVELOPER'S NEXT ATTEMPT, because the action it
// triggers is bounded: two repairs and then a person.
func TestALowConfidenceJudgementDoesNotHandTheTicketBack(t *testing.T) {
	j := &judge{judgement: &referee.Verdict{
		Owner: referee.OwnerSpec, Reason: "possibly", Confidence: "low",
	}}

	out, _ := redRun(t, j, RefereeAfterFailures+2)

	if out == workflow.OutcomeReturned {
		t.Error("a low-confidence verdict handed the specification back; that spends " +
			"one of two repairs on a guess")
	}
}

// BLAMING THE DEVELOPER IS NOT A HAND-BACK. It is the ordinary case, and the
// developer's own retry path is the answer to it — sending it to the author
// gives the one agent that cannot fix it something to do.
func TestBlamingTheDeveloperDoesNotReturnTheTicket(t *testing.T) {
	j := &judge{judgement: &referee.Verdict{
		Owner: referee.OwnerDev, Reason: "the handler is not wired", Confidence: "high",
	}}

	out, _ := redRun(t, j, RefereeAfterFailures+2)

	if out == workflow.OutcomeReturned {
		t.Error("a verdict against the developer sent the specification back to its " +
			"author, who has nothing to fix")
	}
}

// NO EVIDENCE, NO VERDICT WORTH HAVING. A referee shown no failing output is
// guessing, and its guess arrives wearing a confidence field the caller treats
// as authority.
func TestTheRefereeIsShownTheFailureItIsRulingOn(t *testing.T) {
	j := &judge{judgement: &referee.Verdict{
		Owner: referee.OwnerDev, Reason: "x", Confidence: "high",
	}}

	redRun(t, j, RefereeAfterFailures+2)

	if j.judged == 0 {
		t.Fatal("the referee was never asked")
	}
	if strings.TrimSpace(j.out) == "" {
		t.Error("the referee was asked to rule with no failing output in front of it")
	}
	if len(j.read) == 0 {
		t.Error("the referee was asked to rule with none of the files it is judging")
	}
}

// A HOST WITH NO REFEREE STILL DEVELOPS. Nil is a valid configuration, and the
// deterministic routes are exactly what they were before this existed.
func TestWithoutARefereeTheLoopIsUnchanged(t *testing.T) {
	b := &box{
		tree:     []string{"main.go", "store_test.go"},
		files:    map[string]string{"main.go": storeGo, "store_test.go": "package main\n"},
		verdicts: []forge.Result{red(), red(), red(), red()},
	}
	g := &gw{replies: []model.ChatResult{
		writeCall(edit.Edit{Path: "main.go", OldStr: "return nil", Replace: "return []Task{}"}),
		writeCall(edit.Edit{Path: "main.go", OldStr: "return []Task{}", Replace: "return nil"}),
		writeCall(edit.Edit{Path: "main.go", OldStr: "return nil", Replace: "return []Task{}"}),
		writeCall(edit.Edit{Path: "main.go", OldStr: "return []Task{}", Replace: "return nil"}),
	}}

	if _, _, err := devAgent(g, b, &board{}, Options{}).
		Handle(context.Background(), devTicket()); err != nil {
		t.Fatalf("a developer with no referee could not run at all: %v", err)
	}
}

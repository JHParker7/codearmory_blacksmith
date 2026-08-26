package dev

import (
	"context"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/agent/referee"
	"github.com/code-armory-app/blacksmith/internal/edit"
	"github.com/code-armory-app/blacksmith/internal/forge"
	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/transcript"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

// sequenceJudge answers differently each time it is asked, which is the whole
// point: the first verdict and a later one are reached on different evidence.
type sequenceJudge struct {
	judge
	answers []*referee.Verdict
}

func (s *sequenceJudge) Judge(_ context.Context, _ *transcript.Recorder, _ ticket.Ticket,
	read map[string]string, out string) *referee.Verdict {
	s.judged++
	s.read, s.out = read, out
	if s.judged-1 < len(s.answers) {
		return s.answers[s.judged-1]
	}
	return nil
}

// runStalled drives the developer through n failing verifications whose output
// is IDENTICAL every time, which is the shape a stalled attempt makes, and
// returns what the loop decided.
//
// It is redRun's structure with a Referee interface rather than the concrete
// fixture, so a judge that answers differently on the second ask can be used.
// The edits alternate so each one applies: old_str must match exactly once, and
// an edit that does not apply never reaches a verification — which would look
// exactly like a referee that was never asked.
func runStalled(t *testing.T, j Referee, n int) (workflow.Outcome, string) {
	t.Helper()

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
	// THE BUDGET HAS TO OUTLAST THE STALL. devAgent defaults to 8 turns, and the
	// second consult cannot arrive before RefereeAfterFailures + RefereeOnStall
	// verifications — so at the default the loop ended first and the test read as
	// "the referee was never asked again" when it had never been given the chance.
	a := devAgent(&gw{replies: replies}, b, &board{}, Options{MaxTurns: n + 4})
	a.ref = j

	out, detail, err := a.Handle(context.Background(), devTicket())
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	return out, detail
}

// SameFailure IS THE STALL SIGNAL, so pin exactly what moves it.
//
// A developer that is converging produces changing output. One that has stopped
// produces the identical block over and over — on run 21 the same four lines
// repeated for dozens of turns.
func TestIdenticalRedVerificationsAreCounted(t *testing.T) {
	const fail = "--- FAIL: TestRoutes\n    main_test.go:46: got 404"
	s := &State{Staged: map[string]string{"main.go": "package main\n"}}

	s.RecordVerification(fail, false, ModeDevelop)
	if s.SameFailure != 0 {
		t.Fatalf("the first red counted as a repeat: %d", s.SameFailure)
	}
	for i := 2; i <= 4; i++ {
		s.RecordVerification(fail, false, ModeDevelop)
	}
	if s.SameFailure != 3 {
		t.Errorf("three repeats counted as %d", s.SameFailure)
	}

	// A DIFFERENT FAILURE IS PROGRESS. The developer moved the work somewhere
	// new, which is exactly what the counter must not treat as being stuck.
	s.RecordVerification(fail+"\n    main_test.go:52: and another", false, ModeDevelop)
	if s.SameFailure != 0 {
		t.Errorf("a changed failure left the stall counter at %d", s.SameFailure)
	}

	s.RecordVerification(fail, false, ModeDevelop)
	s.RecordVerification("ok\n", true, ModeDevelop)
	if s.SameFailure != 0 {
		t.Errorf("a passing verification left the stall counter at %d", s.SameFailure)
	}
}

// THE VERDICT MUST NOT BE FROZEN AT THE EARLIEST MOMENT IT COULD BE TAKEN.
//
// Read off run 21: the referee was asked 90 seconds in and correctly answered
// "dev" — there was a compile error in board.go. RefereeAsked was then set and
// the developer spent 70 more turns oscillating against an assertion no
// implementation could satisfy, with the failure completely different from the
// one that had been judged. Nothing asked again; the attempt died on the clock.
func TestAStalledAttemptAsksTheRefereeAgain(t *testing.T) {
	j := &sequenceJudge{}
	runStalled(t, j, 18)
	if j.judged < 2 {
		t.Errorf("the referee was asked %d time(s) across a stalled attempt; the "+
			"first verdict was frozen while the failure changed underneath it", j.judged)
	}
}

// THE QUESTIONS ARE SPACED, not merely capped.
//
// Resetting SameFailure when the referee is asked is what spaces them. Without
// the reset the stall condition stays true and the next turn asks again on the
// same evidence, so the whole allowance is spent within two turns of the first
// stall and nothing is left for a later, different failure.
//
// The ceiling alone does not catch that: three asks bunched together and three
// asks spread across the attempt both total three. The arithmetic, with
// RefereeAfterFailures 3 and RefereeOnStall 8: the first ask lands on iteration
// 5, the second on 13, the third on 21. Eighteen turns therefore fit exactly two
// — unless the counter never restarts, when the third arrives on iteration 12.
func TestTheStallCounterRestartsWithEachQuestion(t *testing.T) {
	j := &sequenceJudge{}
	runStalled(t, j, 14) // MaxTurns becomes 18

	if j.judged != 2 {
		t.Errorf("the referee was asked %d times in 18 turns, want exactly 2; the "+
			"allowance is being spent on consecutive turns instead of on separate "+
			"stalls", j.judged)
	}
}

// AND IT IS BOUNDED. The referee is a large-model call; a stalled developer
// would otherwise buy one every RefereeOnStall turns until the ceiling.
func TestAStalledAttemptDoesNotBuyAVerdictEveryTurn(t *testing.T) {
	j := &sequenceJudge{}
	runStalled(t, j, 60)
	if j.judged > MaxRefereeAsks {
		t.Errorf("a stalled attempt bought %d verdicts, more than the %d ceiling",
			j.judged, MaxRefereeAsks)
	}
}

// AND A LATE VERDICT STILL ENDS THE ATTEMPT. Re-asking is worth nothing if the
// answer cannot act — the second opinion has to reach the same hand-back the
// mechanical route uses.
func TestASpecVerdictReachedLateStillHandsTheTicketBack(t *testing.T) {
	j := &sequenceJudge{answers: []*referee.Verdict{
		nil, // the early look: not enough to go on
		{Owner: referee.OwnerSpec, Confidence: "high",
			Reason: "the test treats 404 as proof a route is unregistered, " +
				"which an empty store returns for a valid route"},
	}}
	out, detail := runStalled(t, j, 18)

	if j.judged < 2 {
		t.Fatalf("the referee was only asked %d time(s); the late verdict never happened",
			j.judged)
	}
	if out != workflow.OutcomeReturned {
		t.Errorf("outcome = %q (%s), want the ticket returned to its author",
			out, detail)
	}
}

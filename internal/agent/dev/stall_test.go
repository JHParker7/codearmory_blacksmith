package dev

import (
	"context"
	"strings"
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
// is identical every time — a suite that BUILDS and fails the same assertion —
// and returns what the loop decided.
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
	a := devAgent(&gw{replies: replies}, b, &board{}, Options{MaxTurns: n + 4})
	a.ref = j

	out, detail, err := a.Handle(context.Background(), devTicket())
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	return out, detail
}

// ONE FAULT THAT MOVED DOWN A FILE IS ONE FAULT.
//
// Read off run 50: the referee was asked about "undefined bytesReader" at
// handlers.go:67, the developer edited above it, and the identical fault at
// handlers.go:68 was counted as a new question and bought a second verdict 62
// seconds later. Two of three consults went to one typo.
func TestAFaultThatMovedIsTheSameFault(t *testing.T) {
	a := "# demo\n./handlers.go:67:14: undefined: bytesReader"
	b := "# demo\n./handlers.go:68:14: undefined: bytesReader"
	if FailureFingerprint(a) != FailureFingerprint(b) {
		t.Errorf("a line-number drift made one fault look like two:\n%q\n%q",
			FailureFingerprint(a), FailureFingerprint(b))
	}

	// The same message in a DIFFERENT file is a different fault.
	c := "# demo\n./board.go:67:14: undefined: bytesReader"
	if FailureFingerprint(a) == FailureFingerprint(c) {
		t.Error("two files were collapsed into one fault")
	}
}

// A CYCLE IS WHAT BEING STUCK ACTUALLY LOOKS LIKE, and a consecutive counter
// cannot see one.
//
// Run 21's developer alternated between Go 1.22 pattern routing and a /tickets/
// prefix every thirty seconds. Run 50's alternated between a heading-order
// failure and a missing-form failure, each fix re-breaking the other. In neither
// did a failure ever repeat twice in a row.
func TestAnAlternatingFailureIsCountedAsRecurrence(t *testing.T) {
	s := &State{Staged: map[string]string{"board.go": "package main\n"}}
	A := "--- FAIL: TestHeadingOrder\n    board_test.go:31: open before closed"
	B := "--- FAIL: TestCreateForm\n    board_test.go:88: missing description field"

	for _, out := range []string{A, B, A, B, A} {
		s.RecordVerification(out, false, ModeDevelop)
	}

	if got := s.FailureSightings[FailureFingerprint(A)]; got != 3 {
		t.Errorf("the recurring failure was counted %d times, want 3 — an "+
			"alternating pair reads as progress to a consecutive counter", got)
	}
}

// A COMPILE ERROR IS NOT AN OPEN QUESTION, so it never reaches the referee.
//
// The matcher attributes it without an opinion. On run 50 eight of ten consults
// were spent on compile errors and every one came back "dev", which the matcher
// already knew.
func TestCompileErrorsNeverReachTheReferee(t *testing.T) {
	j := &sequenceJudge{}
	a := &Agent{ref: j, mode: ModeDevelop}
	s := &State{
		FailedVerifications: RefereeAfterFailures,
		LastTest:            "# demo\n./handlers.go:139:22: cannot use &NotFound{} as error",
		FailureSightings:    map[string]int{},
	}
	s.FailureSightings[FailureFingerprint(s.LastTest)] = 99 // recurring, and still not asked

	a.consultReferee(context.Background(), ticket.Ticket{}, s)

	if j.judged != 0 {
		t.Errorf("a compile error bought a verdict the matcher had already reached: "+
			"judged=%d", j.judged)
	}
}

// AND A FAILURE THAT KEEPS CHANGING IS PROGRESS, not a question.
func TestAChangingFailureDoesNotReachTheReferee(t *testing.T) {
	j := &sequenceJudge{}
	a := &Agent{ref: j, mode: ModeDevelop}
	s := &State{FailedVerifications: RefereeAfterFailures, FailureSightings: map[string]int{}}

	for _, out := range []string{
		"--- FAIL: TestOne\n    a_test.go:10: got 1 want 2",
		"--- FAIL: TestTwo\n    a_test.go:20: got 3 want 4",
		"--- FAIL: TestThree\n    a_test.go:30: got 5 want 6",
	} {
		s.RecordVerification(out, false, ModeDevelop)
		a.consultReferee(context.Background(), ticket.Ticket{}, s)
	}

	if j.judged != 0 {
		t.Errorf("a developer working through three different failures was "+
			"second-guessed %d time(s)", j.judged)
	}
}

// WHAT DOES REACH IT is a suite that builds and fails the same assertion again
// and again — the one case no mechanical route can attribute.
func TestARecurringAssertionReachesTheReferee(t *testing.T) {
	j := &sequenceJudge{}
	a := &Agent{ref: j, mode: ModeDevelop}
	s := &State{FailedVerifications: RefereeAfterFailures, FailureSightings: map[string]int{}}

	out := "--- FAIL: TestRoutesAreRegistered\n    main_test.go:46: GET /tickets/1 got 404"
	for range RefereeOnRecurrence {
		s.RecordVerification(out, false, ModeDevelop)
		a.consultReferee(context.Background(), ticket.Ticket{}, s)
	}

	if j.judged != 1 {
		t.Errorf("a failure seen %d times was judged %d time(s), want once",
			RefereeOnRecurrence, j.judged)
	}
}

// ASKING RESETS THE COUNT, so the allowance is SPACED rather than spent at once.
//
// Without the reset the condition stays true and the next turn buys another
// verdict on the same evidence, so the whole allowance goes in three consecutive
// turns and nothing is left for a later, different failure. The ceiling alone
// cannot catch that: three asks bunched and three asks spread both total three.
func TestAskingResetsTheSightingsForThatFailure(t *testing.T) {
	j := &sequenceJudge{}
	a := &Agent{ref: j, mode: ModeDevelop}
	s := &State{FailedVerifications: RefereeAfterFailures, FailureSightings: map[string]int{}}

	out := "--- FAIL: TestRoutes\n    main_test.go:46: GET /tickets/1 got 404"
	ask := func() {
		s.RecordVerification(out, false, ModeDevelop)
		a.consultReferee(context.Background(), ticket.Ticket{}, s)
	}

	for range RefereeOnRecurrence {
		ask()
	}
	if j.judged != 1 {
		t.Fatalf("the first consult did not happen as expected: judged=%d", j.judged)
	}

	// One short of the threshold again: the question is not yet re-opened.
	for range RefereeOnRecurrence - 1 {
		ask()
	}
	if j.judged != 1 {
		t.Errorf("the allowance was spent on consecutive turns: judged=%d after only "+
			"%d further sightings", j.judged, RefereeOnRecurrence-1)
	}

	// The failure comes back once more, reaching the threshold a second time.
	ask()
	if j.judged != 2 {
		t.Errorf("a failure that recurred a second full time did not re-open the "+
			"question: judged=%d", j.judged)
	}
}

// THE VERDICT REACHES THE AGENT THAT HAS TO ACT ON IT.
//
// It used to be discarded unless it blamed the specification. Run 50 bought ten
// diagnoses of the form "handlers.go:139 and 143 pass non-pointer values to
// errors.As, which requires a pointer to a type that implements error" and threw
// every one away, while the developer went on failing the same way.
func TestAVerdictAgainstTheDeveloperBecomesAdviceToIt(t *testing.T) {
	const reason = "handlers.go passes non-pointer values to errors.As, which " +
		"requires a pointer to a type implementing error"
	j := &sequenceJudge{answers: []*referee.Verdict{
		{Owner: referee.OwnerDev, Confidence: "high", Reason: reason},
	}}
	a := &Agent{ref: j, mode: ModeDevelop}
	s := &State{FailedVerifications: RefereeAfterFailures, FailureSightings: map[string]int{}}

	out := "--- FAIL: TestUpdate\n    handlers_test.go:12: got 500 want 404"
	for range RefereeOnRecurrence {
		s.RecordVerification(out, false, ModeDevelop)
		a.consultReferee(context.Background(), ticket.Ticket{}, s)
	}

	if s.Hint != reason {
		t.Fatalf("the diagnosis was discarded; Hint = %q", s.Hint)
	}
	if s.SpecBroken != "" {
		t.Errorf("advice to the developer was treated as a hand-back: %q", s.SpecBroken)
	}

	// And the agent actually sees it, framed as advice rather than a refusal.
	p := RenderProgress(s)
	if !strings.Contains(p, reason) {
		t.Error("the second opinion never reached the prompt")
	}
	if !strings.Contains(p, "advice, not a refusal") {
		t.Error("the second opinion is not distinguished from a rejected action")
	}
}

// AND IT IS BOUNDED. The referee is a large-model call; a developer stuck on one
// assertion would otherwise buy one every RefereeOnRecurrence sightings forever.
func TestAStalledAttemptDoesNotBuyAVerdictEveryTurn(t *testing.T) {
	j := &sequenceJudge{}
	runStalled(t, j, 60)
	if j.judged > MaxRefereeAsks {
		t.Errorf("a stalled attempt bought %d verdicts, more than the %d ceiling",
			j.judged, MaxRefereeAsks)
	}
}

// AND A SPEC VERDICT REACHED LATE STILL ENDS THE ATTEMPT. Asking again is worth
// nothing if the answer cannot act.
func TestASpecVerdictReachedLateStillHandsTheTicketBack(t *testing.T) {
	j := &sequenceJudge{answers: []*referee.Verdict{
		{Owner: referee.OwnerSpec, Confidence: "high",
			Reason: "the test treats 404 as proof a route is unregistered, which an " +
				"empty store returns for a valid route"},
	}}
	out, detail := runStalled(t, j, 18)

	if j.judged == 0 {
		t.Fatal("the referee was never asked")
	}
	if out != workflow.OutcomeReturned {
		t.Errorf("outcome = %q (%s), want the ticket returned to its author", out, detail)
	}
}

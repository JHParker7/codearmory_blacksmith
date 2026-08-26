package dev

import (
	"context"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/ticket"
)

// TIME IS NOT PART OF A FAULT.
//
// go test ends every run with a line like "FAIL	demo	0.519s", and that number
// changes every time. Leaving it in the fingerprint meant a failure that had not
// moved still hashed differently on each verification.
//
// Measured on run 94: the same two failing tests came back 19 times while the
// developer oscillated on board.go, and the sightings counter never reached
// RefereeOnRecurrence because no two of the nineteen looked alike. The referee
// was asked once, at the start, and never again.
func TestTheSameFailureAtADifferentSpeedIsTheSameFailure(t *testing.T) {
	a := "--- FAIL: TestBoardLists (0.00s)\n" +
		"    board_test.go:78: POST /tickets: status = 404, want 201\n" +
		"FAIL\tdemo\t0.519s\nok  \tdemo/types\t(cached)"
	b := "--- FAIL: TestBoardLists (0.00s)\n" +
		"    board_test.go:78: POST /tickets: status = 404, want 201\n" +
		"FAIL\tdemo\t0.402s\nok  \tdemo/types\t0.013s"

	if FailureFingerprint(a) != FailureFingerprint(b) {
		t.Errorf("a run that took longer read as a different fault:\n%q\n%q",
			FailureFingerprint(a), FailureFingerprint(b))
	}
}

// AND A DIFFERENT FAULT IS STILL A DIFFERENT FAULT. Stripping the timings must
// not blur two failures into one.
func TestStrippingTimesDoesNotCollapseDistinctFailures(t *testing.T) {
	a := "--- FAIL: TestBoardLists (0.00s)\n    board_test.go:78: got 404, want 201\nFAIL\tdemo\t0.5s"
	b := "--- FAIL: TestBoardEscapes (0.00s)\n    board_test.go:122: got 500, want 200\nFAIL\tdemo\t0.5s"

	if FailureFingerprint(a) == FailureFingerprint(b) {
		t.Error("two different failures were collapsed into one fingerprint")
	}
}

// AND THE REFEREE IS REACHED because of it. The unit above is only how it gets
// there; this is the behaviour run 94 needed and did not have.
func TestARepeatedFailureAtVaryingSpeedsReachesTheReferee(t *testing.T) {
	j := &sequenceJudge{}
	a := &Agent{ref: j, mode: ModeDevelop}
	s := &State{FailedVerifications: RefereeAfterFailures, FailureSightings: map[string]int{}}

	for i, secs := range []string{"0.519s", "0.402s", "0.611s"} {
		out := "--- FAIL: TestBoardLists (0.00s)\n" +
			"    board_test.go:78: POST /tickets: status = 404, want 201\n" +
			"FAIL\tdemo\t" + secs
		s.RecordVerification(out, false, ModeDevelop)
		a.consultReferee(context.Background(), ticket.Ticket{}, s)
		_ = i
	}

	if j.judged != 1 {
		t.Errorf("one failure seen three times at three speeds was judged %d time(s), "+
			"want once — run 94 saw it nineteen times and asked nobody", j.judged)
	}
}

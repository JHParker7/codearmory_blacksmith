package dev

import "testing"

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

// AND THE SIGHTINGS ACCUMULATE ACROSS THEM, which is the property run 94 needed
// and did not have.
//
// Asserted without consulting, because a consult deliberately resets the count
// for that fingerprint — so counting after one proves nothing. What matters is
// that nineteen occurrences of one failure at nineteen different speeds are
// nineteen sightings of the SAME fault rather than nineteen different ones.
func TestSightingsAccumulateAcrossVaryingSpeeds(t *testing.T) {
	s := &State{Staged: map[string]string{"board.go": "package main\n"}}

	for _, secs := range []string{"0.519s", "0.402s", "0.611s", "0.480s"} {
		s.RecordVerification("--- FAIL: TestBoardLists (0.00s)\n"+
			"    board_test.go:78: POST /tickets: status = 404, want 201\n"+
			"FAIL\tdemo\t"+secs, false, ModeDevelop)
	}

	if len(s.FailureSightings) != 1 {
		t.Fatalf("one failure at four speeds produced %d distinct faults; run 94 saw "+
			"nineteen and asked nobody", len(s.FailureSightings))
	}
	for _, n := range s.FailureSightings {
		if n != 4 {
			t.Errorf("the failure was counted %d times, want 4", n)
		}
	}
}

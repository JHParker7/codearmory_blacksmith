package dev

import (
	"strings"
	"testing"
)

// A REPEAT IS NOT REFUSED. An edit names the EXACT text it replaces, so an agent
// re-checking what it is about to match is doing what the edit format requires,
// not looping.
func TestARepeatedReadIsServedRatherThanRefused(t *testing.T) {
	s := &State{
		Read:    map[string]string{"store.go": "package main\n"},
		Missing: map[string]bool{"gone.go": true},
	}

	got := s.PlanRead([]string{"store.go"})
	if !got.Stale || len(got.Fresh) != 0 {
		t.Errorf("a known file was fetched again: %+v", got)
	}

	// A path the tree listed but a read could not find is also "already answered":
	// asking again reaches a sandbox to learn the same nothing.
	if got := s.PlanRead([]string{"gone.go"}); !got.Stale {
		t.Errorf("a known-missing file was fetched again: %+v", got)
	}

	// A MIXED READ FETCHES ONLY WHAT IS NEW, so the agent is not charged a sandbox
	// for the part it already had.
	got = s.PlanRead([]string{"store.go", "filter.go", "gone.go"})
	if got.Stale {
		t.Error("a read with something new in it was called stale")
	}
	if len(got.Fresh) != 1 || got.Fresh[0] != "filter.go" {
		t.Errorf("Fresh = %v, want only the unread file", got.Fresh)
	}
}

// THE AGENT CANNOT TELL A STALE READ FROM A FRESH ONE — the contents look the
// same either way — so the notice is the only thing that says the turn told it
// nothing.
func TestAStaleReadSaysItWasFreeAndTaughtNothing(t *testing.T) {
	s := &State{StaleReads: 1}
	got := s.StaleReadNotice([]string{"store.go", "filter.go"})

	for _, want := range []string{"store.go", "filter.go", "refunded", "told you nothing"} {
		if !strings.Contains(got, want) {
			t.Errorf("the notice does not say %q:\n%s", want, got)
		}
	}
	// It stays quiet about looping until there is a loop.
	if strings.Contains(got, "in a row") {
		t.Errorf("a single re-read was described as a pattern:\n%s", got)
	}
}

// THE ADVICE BECOMES BLUNT, and it still does not end the attempt: only the
// budget does that, and a model re-reading is not a model doing something wrong.
func TestARunOfStaleReadsIsToldPlainlyToWrite(t *testing.T) {
	s := &State{StaleReads: MaxStaleReads}
	got := s.StaleReadNotice([]string{"store.go"})

	if !strings.Contains(got, "3 times in a row") {
		t.Errorf("the run is not counted:\n%s", got)
	}
	if !strings.Contains(got, "write_files") {
		t.Errorf("the notice does not name the action that helps:\n%s", got)
	}
	if !strings.Contains(got, "run by themselves") {
		t.Errorf("the notice does not say the checks are not the agent's to call:\n%s", got)
	}
}

// FOUR SITUATIONS PRODUCE THE SAME SYMPTOM AND NEED DIFFERENT ANSWERS. "Your
// edits changed nothing" is true in all four and actionable in none.
func TestANoopEditIsToldWhichOfFourSituationsItIsIn(t *testing.T) {
	cases := []struct {
		name          string
		noops         int
		verified      bool
		pass          bool
		lastTest      string
		want, notWant string
	}{
		{
			name: "finished and proved", verified: true, pass: true,
			want: "this ticket is finished", notWant: "Make a real change",
		},
		{
			name: "already tested and failing", verified: true, lastTest: "FAIL store_test.go:12",
			want: "FAIL store_test.go:12", notWant: "PASSED",
		},
		{
			name: "repeating itself", noops: 3,
			want: "3rd edit in a row", notWant: "earlier attempt",
		},
		{
			// Often not the agent's fault: a retry inherits a branch an earlier
			// attempt already wrote to.
			name: "the first one", noops: 1,
			want:    "An earlier attempt at this ticket may already have written it",
			notWant: "in a row",
		},
	}
	for _, c := range cases {
		got := NoopEditNotice(c.noops, c.verified, c.pass, c.lastTest)
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: the notice does not say %q:\n%s", c.name, c.want, got)
		}
		if c.notWant != "" && strings.Contains(got, c.notWant) {
			t.Errorf("%s: the notice wrongly says %q:\n%s", c.name, c.notWant, got)
		}
	}
}

// A FAILURE THE AGENT CANNOT SEE IS A FAILURE IT CANNOT FIX: the whole point of
// the tested-and-failing branch is putting the output back in front of it.
func TestATestedAndFailingNoopCarriesTheFailure(t *testing.T) {
	long := strings.Repeat("FAIL: something\n", 400)
	got := NoopEditNotice(1, true, false, long)

	if !strings.Contains(got, "FAIL: something") {
		t.Error("the failure was not carried")
	}
	// Bounded, because it goes into a prompt that already holds the repository.
	if len([]rune(got)) > 1700 {
		t.Errorf("the notice is %d runes; it would crowd out the work", len([]rune(got)))
	}
}

func TestTheOrdinalReadsAsACount(t *testing.T) {
	for n, want := range map[int]string{
		1: "1st", 2: "2nd", 3: "3rd", 4: "4th",
		11: "11th", 12: "12th", 13: "13th",
		21: "21st", 22: "22nd", 23: "23rd", 111: "111th",
	} {
		if got := ordinal(n); got != want {
			t.Errorf("ordinal(%d) = %q, want %q", n, got, want)
		}
	}
}

// GREEDY DECODING IS RIGHT UNTIL IT IS NOT. Temperature 0 is correct for a task
// with one right answer, and it is exactly what makes a loop inescapable: the
// same prompt yields the same token, so a refused agent re-derives the identical
// action.
func TestTheSamplerWarmsOnlyOnceTheAgentIsDemonstrablyStuck(t *testing.T) {
	if Temperature(0) != 0 {
		t.Error("a first attempt is not decoded greedily")
	}
	if Temperature(1) <= 0 {
		t.Error("a stuck agent is still decoded deterministically")
	}
	if Temperature(2) <= Temperature(1) {
		t.Error("the ramp does not rise with the evidence")
	}
}

// CAPPED LOW, AND 0.8 WAS MEASURED DOING REAL DAMAGE. The reply carries code AND
// PRECISE INTEGERS — the line range — and the integers cannot tolerate heat the
// prose can. Nineteen attempts before the ramp: 0-6 syntax breaks each, inverted
// ranges almost unknown. Three attempts at 0.8: 17-27 syntax breaks and up to 15
// ranges whose end line preceded their start.
func TestTheSamplerIsCappedWellBelowWhatBrokeTheIntegers(t *testing.T) {
	for _, stuck := range []int{4, 10, 100, 10000} {
		if got := Temperature(stuck); got > MaxTemperature {
			t.Errorf("Temperature(%d) = %v, above the cap of %v", stuck, got, MaxTemperature)
		}
	}
	if MaxTemperature >= 0.8 {
		t.Errorf("the cap is %v; 0.8 produced 17-27 syntax breaks per attempt", MaxTemperature)
	}
}

// IT TAKES EVERY COUNTER THAT SAYS "NOT PROGRESSING", or the fixpoint moves to
// whichever one was left out. Keying it on refusals alone left exactly that
// hole: a stale re-read is refunded rather than refused, so it never touched the
// refusal count, temperature stayed at 0, and 66 of 68 turns were identical
// reads of main.go with refusals sitting at 1.
func TestEveryCounterThatSaysStuckFeedsTheRamp(t *testing.T) {
	cases := map[string]State{
		"refusals":       {Refusals: 4},
		"stale re-reads": {StaleReads: 4},
		"noop edits":     {NoopEdits: 4},
	}
	for name, s := range cases {
		if s.Stuck() == 0 {
			t.Errorf("%s does not count as being stuck", name)
		}
		if Temperature(s.Stuck()) == 0 {
			t.Errorf("%s left the sampler deterministic", name)
		}
	}
	// A healthy agent is not warmed by any of them.
	if (&State{}).Stuck() != 0 {
		t.Error("a fresh attempt reads as stuck")
	}
}

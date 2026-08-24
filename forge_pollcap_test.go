package main

import (
	"testing"
	"time"
)

// THE POLL CAP IS WHAT THE PIPELINE WAITS ON.
//
// The interval doubles from pollMin, so the only instants anyone can learn an
// execution finished are the cumulative sums. At a 5s cap those were 0.5, 1.5,
// 3.5, 7.5, 12.5, 17.5 — and every one of r77's 69 recorded sandbox durations was
// one of exactly those six numbers, with nothing in between. They were not
// measurements of work; they were the moments someone looked.

// pollLandings returns the instants at which a waiter can observe completion.
func pollLandings(min, max time.Duration, n int) []time.Duration {
	var out []time.Duration
	var at time.Duration
	for i := range n {
		at += execPollInterval(i, min, max)
		out = append(out, at)
	}
	return out
}

// The observable instants must be dense enough that waiting is not the cost.
func TestTheWaiterLearnsWithinASecondOfCompletion(t *testing.T) {
	landings := pollLandings(pollMin, pollMax, 30)

	worst := time.Duration(0)
	for i := 1; i < len(landings); i++ {
		if gap := landings[i] - landings[i-1]; gap > worst {
			worst = gap
		}
	}
	if worst > time.Second {
		t.Errorf("an execution can finish %s before anyone looks; on r77 that gap was 5s and cost ~90s of a 12-minute run", worst)
	}
}

// THE OLD CAP, PINNED. Everything above is only worth having because the
// previous bounds produced exactly the six values r77 recorded.
func TestTheOldCapProducedTheSixValuesR77Recorded(t *testing.T) {
	got := pollLandings(500*time.Millisecond, 5*time.Second, 6)
	want := []time.Duration{
		500 * time.Millisecond,
		1500 * time.Millisecond,
		3500 * time.Millisecond,
		7500 * time.Millisecond,
		12500 * time.Millisecond,
		17500 * time.Millisecond,
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("landing %d = %s, want %s — the histogram this change was built on no longer reproduces", i, got[i], want[i])
		}
	}
}

// The backoff must still back off: a tight poll for the whole of a long build is
// load on the control plane for no information, which is why pollMin is small and
// the cap exists at all.
func TestTheIntervalStillBacksOff(t *testing.T) {
	if first, second := execPollInterval(0, pollMin, pollMax), execPollInterval(1, pollMin, pollMax); second <= first {
		t.Errorf("interval did not grow: %s then %s", first, second)
	}
	if execPollInterval(20, pollMin, pollMax) != pollMax {
		t.Error("the interval does not settle at the cap; a long build would poll forever at a shrinking gap")
	}
}

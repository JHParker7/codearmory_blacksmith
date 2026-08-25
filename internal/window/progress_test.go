package window

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestProgressBarFill(t *testing.T) {
	cases := []struct{ done, total, filled int }{
		{0, 10, 0},
		{5, 10, 5},
		{10, 10, 10},
		{1, 3, 3}, // 1*10/3
	}
	for _, c := range cases {
		got := ProgressBar(c.done, c.total, 0, 10)
		bar := strings.Fields(got)[0]
		if n := strings.Count(bar, "█"); n != c.filled {
			t.Fatalf("ProgressBar(%d,%d) filled %d cells, want %d (%q)",
				c.done, c.total, n, c.filled, got)
		}
		if w := utf8.RuneCountInString(bar); w != 10 {
			t.Fatalf("bar is %d cells wide, want 10 — a bar that changes width "+
				"shifts every column beside it on each refresh", w)
		}
	}
}

func TestProgressBarNamesTheCounts(t *testing.T) {
	// The bar alone cannot be read off. "7/13" is what someone actually quotes.
	if got := ProgressBar(7, 13, 0, 10); !strings.Contains(got, "7/13") {
		t.Fatalf("ProgressBar = %q, want the counts written out", got)
	}
}

func TestProgressBarWithoutABreakdown(t *testing.T) {
	if got := ProgressBar(0, 0, time.Minute, 10); got != "" {
		t.Fatalf("ProgressBar = %q, want empty — a ticket with no children has no "+
			"progress to report and an empty bar reads as no progress", got)
	}
	if got := ProgressBar(3, -1, time.Minute, 10); got != "" {
		t.Fatalf("ProgressBar = %q, want empty for a negative total", got)
	}
}

// AN EXTRAPOLATION FROM ONE SAMPLE IS A GUESS WEARING A NUMBER'S CLOTHES. This
// board's sections have ranged from ninety seconds to four failed attempts.
func TestProgressBarWithholdsTheETAUntilItMeansSomething(t *testing.T) {
	if got := ProgressBar(1, 10, time.Minute, 10); strings.Contains(got, "left") {
		t.Fatalf("ProgressBar = %q, want no estimate from a single sample", got)
	}
	if got := ProgressBar(MinSamplesForETA, 10, time.Minute, 10); !strings.Contains(got, "left") {
		t.Fatalf("ProgressBar = %q, want an estimate once %d have finished",
			got, MinSamplesForETA)
	}
}

func TestProgressBarETAIsThisRunsOwnPace(t *testing.T) {
	// Four done in twenty minutes is five minutes each; six remain, so thirty.
	got := ProgressBar(4, 10, 20*time.Minute, 10)
	if !strings.Contains(got, "30m00s left") {
		t.Fatalf("ProgressBar = %q, want ~30m00s left — elapsed/done × remaining", got)
	}
}

func TestProgressBarNoETAWhenFinished(t *testing.T) {
	if got := ProgressBar(10, 10, time.Hour, 10); strings.Contains(got, "left") {
		t.Fatalf("ProgressBar = %q, want no estimate on a finished request", got)
	}
}

func TestProgressBarNoETAWithoutAClock(t *testing.T) {
	// Elapsed is zero when the request has no start time to measure from; dividing
	// by it would print "~0s left" on work that has not begun.
	if got := ProgressBar(5, 10, 0, 10); strings.Contains(got, "left") {
		t.Fatalf("ProgressBar = %q, want no estimate without an elapsed time", got)
	}
}

// A BOARD CAN REPORT MORE FINISHED CHILDREN THAN IT HAS. Unclamped, the width
// arithmetic goes negative and strings.Repeat panics — a crash in a read-only
// window, from a board that is merely inconsistent.
func TestProgressBarSurvivesAnOvercount(t *testing.T) {
	got := ProgressBar(14, 10, time.Minute, 10)
	if n := strings.Count(got, "█"); n != 10 {
		t.Fatalf("ProgressBar = %q, want the bar clamped full", got)
	}
	if got := ProgressBar(-3, 10, time.Minute, 10); strings.Count(got, "█") != 0 {
		t.Fatalf("ProgressBar = %q, want an empty bar for a negative count", got)
	}
}

func TestProgressBarMinimumWidth(t *testing.T) {
	// A width below eight cannot show tenths, and a caller on a very narrow
	// terminal should get a small bar rather than an empty or negative one.
	for _, w := range []int{-5, 0, 3, 8} {
		got := ProgressBar(1, 2, 0, w)
		bar := strings.Fields(got)[0]
		if n := utf8.RuneCountInString(bar); n != 8 {
			t.Fatalf("width %d gave a %d-cell bar, want the floor of 8", w, n)
		}
	}
}

func TestProgressBarWidthLeavesRoomForTheTitle(t *testing.T) {
	if ProgressBarWidth > 16 {
		t.Fatalf("ProgressBarWidth = %d — the row also carries a title, a stage, an "+
			"age and a reason, and the bar is glanced at rather than measured",
			ProgressBarWidth)
	}
}

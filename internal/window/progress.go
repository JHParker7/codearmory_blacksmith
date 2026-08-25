package window

import (
	"fmt"
	"strings"
	"time"
)

// MinSamplesForETA is how many finished children an estimate needs.
//
// Two, because one tells you nothing about variance and this board's sections
// have ranged from ninety seconds to four failed attempts. A number that swings
// by an order of magnitude between refreshes is worse than no number.
const MinSamplesForETA = 2

// ProgressBarWidth keeps the bar narrow enough to leave the title readable.
//
// Ten cells on a row that already carries a title, a stage, an age and a reason.
// The bar is there to be glanced at, not measured off.
const ProgressBarWidth = 10

// ProgressBar renders how far a request has got, and how long the rest is likely
// to take.
//
// THE ESTIMATE IS FROM THIS RUN, not from a constant. Sections on one board vary
// enormously — some merge in ninety seconds, some fail four times first — so the
// only honest predictor is the pace this particular request has managed so far:
// elapsed divided by completed, times what is left. It is shown only once enough
// has finished to mean anything, because an extrapolation from one sample is a
// guess wearing a number's clothes.
func ProgressBar(done, total int, elapsed time.Duration, width int) string {
	if total <= 0 {
		return "" // nothing was broken down, so there is no progress to show
	}
	if width < 8 {
		width = 8
	}
	// CLAMPED, because a board can report more finished children than it has: a
	// merged section that is also counted done by its own column double-counts,
	// and an unclamped bar then writes a negative repeat count and panics.
	if done < 0 {
		done = 0
	}
	if done > total {
		done = total
	}

	filled := done * width / total
	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
	out := fmt.Sprintf("%s %d/%d", bar, done, total)

	if done >= MinSamplesForETA && done < total && elapsed > 0 {
		per := elapsed / time.Duration(done)
		out += " · ~" + RuntimeText(per*time.Duration(total-done)) + " left"
	}
	return out
}

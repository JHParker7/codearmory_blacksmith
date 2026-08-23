package main

import "testing"

// clip is called with a width computed by subtraction at every call site, and any
// of those subtractions can go negative. It crashed the window with
// "slice bounds out of range [:-1]" when a third project put a 16-character
// "[ab-qwen38-r71] " tag on a row that could not hold it.
func TestClipSurvivesAWidthThatWentNegative(t *testing.T) {
	for _, max := range []int{0, -1, -100} {
		got := clip("a title that will not fit", max)
		if got != "" {
			t.Errorf("clip(_, %d) = %q, want empty", max, got)
		}
	}
}

func TestClipLeavesShortTextAlone(t *testing.T) {
	if got := clip("short", 40); got != "short" {
		t.Errorf("clip = %q, want it untouched", got)
	}
}

func TestClipMarksWhatItCut(t *testing.T) {
	// The ellipsis is the difference between a shortened title and a wrong one.
	got := clip("a considerably longer title", 10)
	if got != "a consider…" {
		t.Errorf("clip = %q", got)
	}
}

func TestClipCountsRunesNotBytes(t *testing.T) {
	// Cutting a multi-byte rune in half produces a replacement character on the
	// row, which looks like corrupted data rather than a truncation.
	got := clip("ときどきときどき", 4)
	if got != "ときどき…" {
		t.Errorf("clip = %q, want four runes and an ellipsis", got)
	}
}

func TestClipTrimsBeforeMeasuring(t *testing.T) {
	if got := clip("   padded   ", 6); got != "padded" {
		t.Errorf("clip = %q", got)
	}
}

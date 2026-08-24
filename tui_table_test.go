package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// A COLUMN IS MEASURED IN WHAT THE TERMINAL SHOWS, NOT IN BYTES.
//
// fmt's %-*s pads to a count of bytes, and every cell on this board may carry
// colour: a styled cell is its text wrapped in ANSI escapes that occupy bytes and
// no columns. So %-*s padded a coloured cell by however many bytes the escapes
// took, and the column ended short.
//
// It showed as a table that lined up on some rows and not others, because only
// SOME cells are styled. A row whose title carried a project tag was padded about
// twenty columns short while an untagged row beside it was correct, and every
// column after the title shifted with it.

func TestPaddingCountsColumnsNotBytes(t *testing.T) {
	plain := "ticket title"
	styled := cDim.Render("[proj] ") + "ticket title"

	// The two differ in bytes by the length of the escapes...
	if len(styled) == len(plain)+7 {
		t.Skip("styling is disabled in this environment; the bug cannot reproduce")
	}
	// ...and padTo must still bring both to the same visible width.
	if got := lipgloss.Width(padTo(plain, 40)); got != 40 {
		t.Errorf("padded a plain cell to %d columns, want 40", got)
	}
	if got := lipgloss.Width(padTo(styled, 40)); got != 40 {
		t.Errorf("padded a styled cell to %d columns, want 40 — this is the misalignment", got)
	}
}

// The bug itself, pinned: fmt cannot do this, so nobody should put %-*s back.
func TestFmtPaddingIsTheThingThatBroke(t *testing.T) {
	styled := cDim.Render("[proj] ") + "title"
	if lipgloss.Width(styled) >= len(styled) {
		t.Skip("styling is disabled in this environment")
	}

	byFmt := padWithFmt(styled, 40)
	if lipgloss.Width(byFmt) == 40 {
		t.Error("byte padding now matches display width; this guard is obsolete")
	}
}

// padWithFmt is the old behaviour, kept only so the test above can show it is
// wrong. Nothing else may call it.
func padWithFmt(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}

func TestPadLeftRightAlignsInColumns(t *testing.T) {
	if got := lipgloss.Width(padLeft("4m54s", runtimeWidth)); got != runtimeWidth {
		t.Errorf("runtime cell is %d columns, want %d", got, runtimeWidth)
	}
	if !strings.HasSuffix(padLeft("33s", 7), "33s") {
		t.Error("padLeft did not right-align; runtimes will not line up on their last digit")
	}
}

// A cell already wider than its column is left alone rather than truncated here:
// clipping is the caller's decision and it has already been made by the time a
// value reaches the padder.
func TestAnOversizedCellIsNotTruncatedByThePadder(t *testing.T) {
	long := strings.Repeat("x", 20)
	if padTo(long, 5) != long {
		t.Error("padTo truncated a cell; clipping belongs to the caller")
	}
	if padLeft(long, 5) != long {
		t.Error("padLeft truncated a cell")
	}
}

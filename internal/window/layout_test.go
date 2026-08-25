package window

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// EVERY ROW MUST FIT ITS TERMINAL. Bubbletea cuts a line that does not, without
// an ellipsis and wherever it happens to run out — which is how the state cell,
// the only one that says whether a person is needed, came to be the first thing
// dropped and the only one dropped silently.
func TestARowSpendsExactlyTheWidthItHas(t *testing.T) {
	for _, term := range []int{20, 30, 40, 50, 60, 72, 80, 100, 120, 140, 200} {
		for _, want := range []int{0, 7, 20, 45, 80, 300} {
			c := LayOutRow(Rule(term), want)

			total := rowFixed + c.Title + 2 + c.State
			if c.Priority > 0 {
				total += c.Priority + 1
			}
			if c.Runtime > 0 {
				total += c.Runtime + 1
			}
			if total != Rule(term) {
				t.Errorf("term=%d want=%d: the row spends %d of %d columns (%+v)",
					term, want, total, Rule(term), c)
			}
			if c.Title < 0 || c.State < 0 || c.Priority < 0 || c.Runtime < 0 {
				t.Fatalf("term=%d want=%d: a negative width reaches strings.Repeat "+
					"and panics: %+v", term, want, c)
			}
		}
	}
}

// THE STATE IS THE LAST CELL TO BE GIVEN UP, not the first. The runtime and the
// priority are both recoverable from the detail view; "this one is blocked" is
// what makes a person open it at all.
func TestANarrowRowGivesUpTheRuntimeBeforeTheState(t *testing.T) {
	wide := LayOutRow(Rule(100), 30)
	if wide.Runtime == 0 || wide.Priority == 0 {
		t.Fatalf("a hundred columns is enough for every cell: %+v", wide)
	}

	narrow := LayOutRow(Rule(62), 30)
	if narrow.Runtime != 0 {
		t.Errorf("at 62 columns the runtime is still being paid for: %+v", narrow)
	}
	if narrow.State < MinState {
		t.Errorf("the state was squeezed below its floor while another column was "+
			"still being paid for: %+v", narrow)
	}
}

// The title column is sized to what is on the board. A fixed cap spent the width
// whether or not anything needed it: short titles on a wide terminal padded sixty
// columns of nothing and then cut the state.
func TestTheTitleColumnIsSizedToItsContent(t *testing.T) {
	short := LayOutRow(Rule(140), 7)
	long := LayOutRow(Rule(140), 80)

	if short.Title >= long.Title {
		t.Errorf("a board of short titles claims as much as one of long titles: "+
			"%d vs %d", short.Title, long.Title)
	}
	if short.State <= long.State {
		t.Errorf("the width the titles did not need went nowhere: %d vs %d",
			short.State, long.State)
	}
	if long.Title > MaxTitle {
		t.Errorf("a very long title was allowed past its cap: %d", long.Title)
	}
}

// --- what the cut looks like -------------------------------------------------

func TestFittingMarksWhereItCut(t *testing.T) {
	cells := []Cell{{"BLOCKED — needs you", ToneAlert}, {" · 3m", ToneQuiet}}

	got := FitCells(cells, 12)
	if len(got) != 1 {
		t.Fatalf("a budget of 12 kept %d cells", len(got))
	}
	if !strings.HasSuffix(got[0].Text, "…") {
		t.Errorf("the cut is not marked, so %q reads as the whole label", got[0].Text)
	}
	if n := len([]rune(got[0].Text)); n != 12 {
		t.Errorf("the fitted cell is %d columns, not the 12 it was given", n)
	}
	if got[0].Tone != ToneAlert {
		t.Error("the cut cell lost its tone, so an alert renders as ordinary text")
	}
}

func TestFittingKeepsWholeCellsWhileTheyFit(t *testing.T) {
	cells := []Cell{{"waiting", ToneWaiting}, {" · 3m", ToneQuiet}}

	got := FitCells(cells, 100)
	if len(got) != 2 || got[0].Text != "waiting" || got[1].Text != " · 3m" {
		t.Errorf("a budget with room to spare still altered the cells: %+v", got)
	}
}

// A CUT MUST FALL INSIDE A CELL, never across the rendered string. The state is
// several differently styled runs, and truncating the rendered text would cut
// through an escape sequence and leave the rest of the line the wrong colour.
func TestFittingNeverSplitsAnEarlierCell(t *testing.T) {
	cells := []Cell{{"abcde", ToneAlert}, {"fghij", ToneQuiet}}

	got := FitCells(cells, 7)
	if len(got) != 2 {
		t.Fatalf("want both cells, got %+v", got)
	}
	if got[0].Text != "abcde" {
		t.Errorf("the first cell was altered though it fitted: %q", got[0].Text)
	}
	if got[1].Text != "f…" {
		t.Errorf("the second cell = %q, want the cut to fall inside it", got[1].Text)
	}
}

func TestFittingNothingIntoNothing(t *testing.T) {
	if got := FitCells([]Cell{{"anything", ToneQuiet}}, 0); got != nil {
		t.Errorf("a budget of zero produced %+v", got)
	}
	if got := FitCells(nil, 20); got != nil {
		t.Errorf("no cells produced %+v", got)
	}
}

// --- the hint line ------------------------------------------------------------

// WHOLE HINTS ONLY. The line was joined at full length and left to the terminal
// to cut, so a sixty-column window ended on "r " with its description gone,
// trailed by the empty styling of every hint cut away entirely. It read as a
// broken render rather than as a line that did not fit.
func TestTheHintLineDropsWholeHints(t *testing.T) {
	all := []string{"↑↓ move", "enter detail", "/ ask", "n new ticket", "r refresh"}

	wide := Model{width: 200}.keys(all...)
	for _, want := range []string{"move", "detail", "ask", "new ticket", "refresh"} {
		if !strings.Contains(wide, want) {
			t.Errorf("a 200-column line dropped %q: %q", want, wide)
		}
	}

	narrow := Model{width: 40}.keys(all...)
	if w := lipgloss.Width(narrow); w > Rule(40) {
		t.Errorf("the hint line is %d columns on a 40-column terminal: %q", w, narrow)
	}
	if !strings.Contains(narrow, "move") {
		t.Errorf("the first hint did not survive: %q", narrow)
	}
	// Whatever was dropped must be gone ENTIRELY — no orphaned key with no
	// description, and no separator with nothing after it.
	if strings.Contains(narrow, "refresh") {
		t.Errorf("a hint that could not fit was rendered anyway: %q", narrow)
	}
}

// A terminal with room for one hint shows one WHOLE hint, not two broken ones.
//
// Twenty-two rather than something smaller: at twenty and below the width is not
// narrow, it is UNKNOWN — no WindowSizeMsg has arrived yet — and Rule answers
// with the eighty-column assumption instead.
func TestANarrowHintLineShowsWholeHintsOrNone(t *testing.T) {
	got := Model{width: 22}.keys("enter detail", "r refresh")

	if w := lipgloss.Width(got); w > Rule(22) {
		t.Errorf("the hint line is %d columns on a 22-column terminal: %q", w, got)
	}
	if strings.Contains(got, "refresh") {
		t.Errorf("a hint that could not fit was rendered anyway: %q", got)
	}
	if !strings.Contains(got, "detail") {
		t.Errorf("the hint that did fit was dropped: %q", got)
	}
}

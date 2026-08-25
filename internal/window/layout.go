package window

import "strings"

// The row's cells, and what each is worth when the terminal is narrow.
const (
	// MinState is the least the state cell may show. "BLOCKED — needs you" is
	// nineteen columns, so eighteen plus an ellipsis still reads as the alert it
	// is rather than as a word that happens to start with "BLOCK".
	MinState = 18

	// MaxTitle caps the title so a long one cannot crowd out everything to its
	// right on a wide terminal.
	MaxTitle = 60

	// rowFixed is what a row spends before any sized cell: the two-column live
	// marker, the short id, and the gap after it.
	rowFixed = 2 + ShortIDLen + 2
)

// RowWidths is how one row spends the terminal's width. A zero width means the
// column is dropped entirely, separator and all.
type RowWidths struct {
	Title    int
	Priority int
	Runtime  int
	State    int
}

// LayOutRow decides what a row can afford, in order of what a person actually
// needs from it.
//
// THE STATE IS NOT THE EXPENDABLE CELL, AND IT USED TO BE. The row was laid out
// by sizing the title from the terminal width and appending the state to
// whatever was left — which could be nothing. Bubbletea truncates each line to
// the terminal, so the overflow was cut silently and without an ellipsis:
//
//	at 100 columns  BLOCKED — needs you
//	at  80 columns  BLOCKED — needs you
//	at  60 columns  BLOCKED␣            <- reads as a complete label
//	at  40 columns  (nothing at all)
//
// Sixty columns is a split pane, and there "waiting for review" and "waiting to
// be designed" both render as "waiting " — indistinguishable, and with no
// ellipsis to say so. The one cell that says whether a ticket needs a person was
// the first thing sacrificed and the only one cut without a mark.
//
// So the concessions run the other way now: the runtime goes first, then the
// priority, then the title down to MinTitle. Those three are all recoverable by
// opening the detail view; "this one is blocked" is what makes a person open it
// at all.
//
// wantTitle is how wide the widest title on the board would like to be. SIZED TO
// CONTENT, because a fixed cap spends the width whether or not anything needs it:
// a board of short titles on a wide terminal padded sixty columns of nothing and
// then cut the state, so "waits on store" was lost to blank space.
func LayOutRow(rule, wantTitle int) RowWidths {
	w := RowWidths{Priority: PriorityWidth, Runtime: RuntimeWidth}

	// spare is what is left for the title and the state once the columns still
	// standing have been paid for. A dropped column takes its separator with it.
	spare := func() int {
		n := rule - rowFixed - 2 // the gap after the title
		if w.Priority > 0 {
			n -= w.Priority + 1
		}
		if w.Runtime > 0 {
			n -= w.Runtime + 1
		}
		return n
	}

	if spare() < MinTitle+MinState {
		w.Runtime = 0
	}
	if spare() < MinTitle+MinState {
		w.Priority = 0
	}

	rest := spare()

	// Below the two floors together, there is nothing left to concede: split what
	// there is and let both cells clip. Guarded rather than left to arithmetic
	// because a negative width reaches strings.Repeat, and that panics.
	if rest < MinTitle+MinState {
		if rest < 0 {
			rest = 0
		}
		w.State = rest / 2
		w.Title = rest - w.State
		return w
	}

	// The title asks for what its content needs, within its floor and its cap.
	w.Title = wantTitle
	if w.Title > MaxTitle {
		w.Title = MaxTitle
	}
	if w.Title < MinTitle {
		w.Title = MinTitle
	}

	// Then the state is made whole out of whatever is left — and if that is not
	// enough for it, the title gives back down to its own floor.
	if rest-w.Title < MinState {
		w.Title = rest - MinState
		if w.Title < MinTitle {
			w.Title = MinTitle
		}
	}
	w.State = rest - w.Title
	return w
}

// FitCells trims a row's state cells to a budget, marking the cut.
//
// CELL BY CELL, because the state is several differently styled runs — the
// label, the age, the reason it is waiting — and a cut has to fall inside one of
// them while leaving the earlier ones intact. Truncating the rendered string
// instead would cut through an escape sequence and leave the rest of the line
// wearing the wrong colour.
//
// The ellipsis is the point: without it a clipped label reads as a complete one,
// and "waiting " is a sentence about a ticket that is not the sentence the board
// meant to write.
func FitCells(cells []Cell, budget int) []Cell {
	if budget <= 0 {
		return nil
	}

	var out []Cell
	left := budget
	for _, c := range cells {
		runes := []rune(c.Text)
		if len(runes) <= left {
			out = append(out, c)
			left -= len(runes)
			continue
		}
		// This cell is where the budget runs out. One column goes to the ellipsis,
		// so the text keeps left-1 — and at a budget of one the cell is the
		// ellipsis alone, which still says "there was more".
		if left > 0 {
			out = append(out, Cell{string(runes[:left-1]) + "…", c.Tone})
		}
		return out
	}
	return out
}

// PadRunes is PadTo for text that carries no styling, counted in runes.
func PadRunes(s string, n int) string {
	if r := len([]rune(s)); r < n {
		return s + strings.Repeat(" ", n-r)
	}
	return s
}

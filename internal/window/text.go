package window

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/code-armory-app/blacksmith/internal/ticket"
)

// ShortIDLen is how much of a ticket id the window shows.
//
// EIGHT, and the same eight the claim records, the branch names and the
// dependency lines use. The rows refer to each other — "waits on cb583cbd" is
// only useful if cb583cbd can be found without opening every ticket — so the
// form has to match everywhere it appears or the cross-reference fails.
const ShortIDLen = 8

// ShortID is the id as it appears on a row.
func ShortID(id string) string {
	if len(id) <= ShortIDLen {
		return id
	}
	return id[:ShortIDLen]
}

// ShortDuration is the COARSE clock, for ages rather than runtimes.
//
// Deliberately not RuntimeText: the question "how long has this been sitting
// here" is answered by "42m", and the seconds are noise that changes on every
// refresh. RuntimeText keeps seconds because the question there is which of two
// finished tickets was slower — see its own comment.
func ShortDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// StageSince is when the ticket entered the state it is in now.
//
// READ OFF THE LAST ACTIVITY rather than a column-change timestamp, because the
// store keeps no history of transitions — but every stage in this pipeline both
// moves the ticket and writes a comment when it claims it and again when it
// reports, so the newest of those is when the current state began. UpdatedAt is
// taken too, in case something moved the ticket without commenting.
//
// It is a lower bound, and honest as one: a ticket that has sat untouched shows
// the age of the last thing that happened to it, which is exactly the number a
// person is looking for when they ask why nothing is happening.
func StageSince(t ticket.Ticket) time.Time {
	at := t.UpdatedAt
	for _, c := range t.Comments {
		if c.CreatedAt.After(at) {
			at = c.CreatedAt
		}
	}
	if at.IsZero() {
		at = t.CreatedAt
	}
	return at
}

// WrapTo breaks text to a width, so a paragraph of reasoning does not run off
// the side of the pane.
//
// WORD BOUNDARIES, not a hard cut: the reasoning panel exists to be read, and a
// diagnosis split mid-identifier is exactly the part that has to survive. A word
// longer than the width is emitted whole and allowed to overrun rather than
// being broken, because a path or a symbol is worth more intact than fitted.
func WrapTo(s string, width int) []string {
	if width < 20 {
		width = 20
	}
	var out []string
	for _, para := range strings.Split(s, "\n") {
		words := strings.Fields(para)
		if len(words) == 0 {
			out = append(out, "")
			continue
		}
		line := words[0]
		for _, w := range words[1:] {
			if len([]rune(line))+1+len([]rune(w)) > width {
				out = append(out, line)
				line = w
				continue
			}
			line += " " + w
		}
		out = append(out, line)
	}
	return out
}

// PadTo and PadLeft size a cell to a column.
//
// MEASURED WITH lipgloss.Width, NOT len. Every cell here may carry colour, and
// an escape sequence is bytes that occupy no columns — padding by length adds
// spaces for them and every column after the first styled cell drifts. The
// headings share these widths precisely so that drift is visible the moment it
// starts.
func PadTo(s string, n int) string {
	if w := lipgloss.Width(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

func PadLeft(s string, n int) string {
	if w := lipgloss.Width(s); w < n {
		return strings.Repeat(" ", n-w) + s
	}
	return s
}

const (
	// PriorityWidth is the priority column, wide enough for "critical".
	PriorityWidth = 9
	// RuntimeWidth is the runtime column, wide enough for "12m34s" and the "+" a
	// running ticket carries.
	RuntimeWidth = 7
)

// Rule is the width of the horizontal rules and the budget every row is laid out
// against.
func Rule(termWidth int) int {
	if termWidth > 20 {
		return termWidth - 1
	}
	// Before the first WindowSizeMsg the terminal's width is unknown. Eighty is
	// the safe assumption; guessing wide would wrap every row of the first frame.
	return 78
}

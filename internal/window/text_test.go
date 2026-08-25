package window

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/code-armory-app/blacksmith/internal/ticket"
)

func TestShortID(t *testing.T) {
	if got := ShortID("cb583cbd-1111-2222-3333-444444444444"); got != "cb583cbd" {
		t.Fatalf("ShortID = %q, want the first 8", got)
	}
	if got := ShortID("abc"); got != "abc" {
		t.Fatalf("ShortID = %q, want a short id returned whole", got)
	}
	if got := ShortID(""); got != "" {
		t.Fatalf("ShortID = %q, want empty", got)
	}
}

// THE FORM MUST MATCH EVERYWHERE. A dependency line reading "waits on cb583cbd"
// is only useful if the row for that ticket shows the same eight characters.
func TestShortIDMatchesTheDependencyForm(t *testing.T) {
	id := "cb583cbd-1111-2222-3333-444444444444"
	if ShortID(id) != id[:ShortIDLen] {
		t.Fatal("ShortID must be exactly the first ShortIDLen characters")
	}
}

func TestShortDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{45 * time.Second, "45s"},
		{90 * time.Second, "1m"},
		{42 * time.Minute, "42m"},
		{3 * time.Hour, "3h"},
		{47 * time.Hour, "47h"},
		{72 * time.Hour, "3d"},
		{-time.Minute, "0s"},
	}
	for _, c := range cases {
		if got := ShortDuration(c.d); got != c.want {
			t.Fatalf("ShortDuration(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}

// THE COARSE CLOCK IS FOR AGES, THE FINE ONE FOR RUNTIMES. An age that changes
// every second is noise on a two-second refresh; a runtime without seconds
// cannot be compared with another runtime, which is the only reason to show one.
func TestShortDurationIsCoarserThanRuntimeText(t *testing.T) {
	d := 5*time.Minute + 44*time.Second
	if ShortDuration(d) == RuntimeText(d) {
		t.Fatal("the two clocks must differ: one answers 'how stale', the other 'which was slower'")
	}
	if strings.Contains(ShortDuration(d), "44") {
		t.Fatalf("ShortDuration = %q, want the seconds dropped", ShortDuration(d))
	}
	if !strings.Contains(RuntimeText(d), "44") {
		t.Fatalf("RuntimeText = %q, want the seconds kept", RuntimeText(d))
	}
}

// ---- StageSince ----

func TestStageSinceTakesTheNewestComment(t *testing.T) {
	base := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	newest := base.Add(30 * time.Minute)
	tk := ticket.Ticket{
		CreatedAt: base,
		UpdatedAt: base.Add(5 * time.Minute),
		Comments: []ticket.Comment{
			{CreatedAt: base.Add(10 * time.Minute)},
			{CreatedAt: newest},
			{CreatedAt: base.Add(20 * time.Minute)},
		},
	}
	if got := StageSince(tk); !got.Equal(newest) {
		t.Fatalf("StageSince = %v, want the newest comment %v — every stage comments "+
			"when it claims and again when it reports", got, newest)
	}
}

func TestStageSinceFallsBackThroughUpdatedToCreated(t *testing.T) {
	base := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)

	// A ticket moved without a comment still has an UpdatedAt.
	upd := ticket.Ticket{CreatedAt: base, UpdatedAt: base.Add(time.Hour)}
	if got := StageSince(upd); !got.Equal(base.Add(time.Hour)) {
		t.Fatalf("StageSince = %v, want UpdatedAt when no comment is newer", got)
	}

	// A brand new ticket has neither.
	fresh := ticket.Ticket{CreatedAt: base}
	if got := StageSince(fresh); !got.Equal(base) {
		t.Fatalf("StageSince = %v, want CreatedAt %v", got, base)
	}
}

func TestStageSinceIgnoresOlderComments(t *testing.T) {
	base := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	tk := ticket.Ticket{
		CreatedAt: base,
		UpdatedAt: base.Add(time.Hour),
		Comments:  []ticket.Comment{{CreatedAt: base.Add(time.Minute)}},
	}
	if got := StageSince(tk); !got.Equal(base.Add(time.Hour)) {
		t.Fatalf("StageSince = %v, want the LATER of the two", got)
	}
}

// ---- WrapTo ----

func TestWrapToBreaksOnWords(t *testing.T) {
	got := WrapTo("the routing uses HasPrefix on a path missing its leading slash", 24)
	for _, line := range got {
		if len([]rune(line)) > 24 {
			t.Fatalf("line %q is %d wide, want <= 24", line, len([]rune(line)))
		}
	}
	if strings.Join(got, " ") != "the routing uses HasPrefix on a path missing its leading slash" {
		t.Fatalf("wrapping lost or reordered words: %q", got)
	}
}

// A PATH OR A SYMBOL IS WORTH MORE INTACT THAN FITTED. It is the part of a
// diagnosis you copy.
func TestWrapToKeepsALongWordWhole(t *testing.T) {
	long := "internal/agent/dev/agent_dev_prompt.go:1284"
	got := WrapTo("see "+long+" now", 20)
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, long) {
		t.Fatalf("WrapTo broke a long token; got %q", got)
	}
}

func TestWrapToKeepsParagraphs(t *testing.T) {
	got := WrapTo("first\n\nsecond", 40)
	if len(got) != 3 || got[0] != "first" || got[1] != "" || got[2] != "second" {
		t.Fatalf("WrapTo = %q, want the blank line kept — it separates two thoughts", got)
	}
}

func TestWrapToMinimumWidth(t *testing.T) {
	// A width below twenty produces one word per line, which is unreadable.
	got := WrapTo("some reasonably long piece of reasoning here", 2)
	for _, line := range got {
		if len([]rune(line)) > 20 && !strings.Contains(line, " ") {
			continue
		}
		if len([]rune(line)) > 20 {
			t.Fatalf("line %q exceeds the floor width", line)
		}
	}
	if len(got) > 4 {
		t.Fatalf("got %d lines for a short sentence: the floor width was not applied", len(got))
	}
}

func TestWrapToEmpty(t *testing.T) {
	if got := WrapTo("", 40); len(got) != 1 || got[0] != "" {
		t.Fatalf("WrapTo(\"\") = %q, want one empty line", got)
	}
}

// ---- padding ----

// AN ESCAPE SEQUENCE IS BYTES THAT OCCUPY NO COLUMNS. Padding by len adds spaces
// for them and every column after the first styled cell drifts.
func TestPadToMeasuresDisplayWidthNotBytes(t *testing.T) {
	styled := lipgloss.NewStyle().Bold(true).Render("abc")
	if len(styled) == 3 {
		t.Skip("lipgloss emitted no escapes in this environment")
	}
	got := PadTo(styled, 10)
	if w := lipgloss.Width(got); w != 10 {
		t.Fatalf("PadTo produced %d display columns, want 10 — padding by byte "+
			"length drifts every column after a styled cell", w)
	}
}

func TestPadTo(t *testing.T) {
	if got := PadTo("ab", 5); got != "ab   " {
		t.Fatalf("PadTo = %q", got)
	}
	if got := PadTo("abcdef", 3); got != "abcdef" {
		t.Fatalf("PadTo = %q, want an over-wide cell left alone rather than cut: "+
			"truncating here would silently drop content the caller already sized", got)
	}
}

func TestPadLeft(t *testing.T) {
	if got := PadLeft("5m", 6); got != "    5m" {
		t.Fatalf("PadLeft = %q, want right-aligned — a column of durations only "+
			"compares if the units line up", got)
	}
	if got := PadLeft("abcdef", 3); got != "abcdef" {
		t.Fatalf("PadLeft = %q, want it left alone", got)
	}
}

// Multi-byte characters occupy their display width, not their byte count.
func TestPadToWithMultiByteText(t *testing.T) {
	if w := lipgloss.Width(PadTo("héllo", 10)); w != 10 {
		t.Fatalf("PadTo gave %d columns for multi-byte text, want 10", w)
	}
}

// ---- widths ----

func TestRuleBeforeTheTerminalIsMeasured(t *testing.T) {
	// bubbletea sends no WindowSizeMsg until after the first frame.
	for _, w := range []int{0, 1, 20} {
		if got := Rule(w); got != 78 {
			t.Fatalf("Rule(%d) = %d, want the 78-column assumption: guessing wide "+
				"wraps every row of the opening frame", w, got)
		}
	}
	if got := Rule(120); got != 119 {
		t.Fatalf("Rule(120) = %d, want 119", got)
	}
}

func TestTitleWidthIsBounded(t *testing.T) {
	if got := TitleWidth(40); got != 20 {
		t.Fatalf("TitleWidth(40) = %d, want the floor of 20 — a narrow terminal "+
			"still needs a readable title", got)
	}
	if got := TitleWidth(400); got != 60 {
		t.Fatalf("TitleWidth(400) = %d, want the ceiling of 60 — on a very wide "+
			"terminal the state column should not be pushed off the eye's path", got)
	}
}

// THE FIXED COLUMNS KEEP THEIR WIDTH AND THE TITLE GIVES WAY. An id or a runtime
// that wrapped would be useless; a clipped title is still readable.
func TestTitleWidthLeavesRoomForTheFixedColumns(t *testing.T) {
	const term = 120
	fixed := ShortIDLen + 2 + PriorityWidth + RuntimeWidth + 2
	if TitleWidth(term)+fixed >= Rule(term) {
		t.Fatalf("TitleWidth(%d) = %d plus %d of fixed columns does not fit in %d",
			term, TitleWidth(term), fixed, Rule(term))
	}
}

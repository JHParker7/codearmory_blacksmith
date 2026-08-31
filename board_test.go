package main

import (
	"strings"
	"testing"
)

// The board lists tickets the operator navigates: a coloured status, a severity
// for findings, and the label. This checks a mixed board renders with the
// selection cursor on the right row.
func TestBoardRenderListsTicketsWithStatus(t *testing.T) {
	v := boardView{
		reachable:    true,
		openSecurity: 1,
		openQuality:  1,
		resolved:     3,
		tickets: []boardTicket{
			{id: "1", label: "build a task API", status: "in_progress", request: true},
			{id: "2", label: "nil MaxBytesReader panics", status: "open", severity: "critical", kind: "security"},
			{id: "3", label: "PUT cannot clear Description", status: "resolved", severity: "medium", kind: "quality"},
		},
	}
	out := v.render(1) // cursor on the finding

	if !strings.Contains(out, "1 security · 1 quality open") || !strings.Contains(out, "3 fixed") {
		t.Errorf("summary line wrong:\n%s", out)
	}
	if !strings.Contains(out, "in progress") {
		t.Errorf("in_progress status should render as 'in progress':\n%s", out)
	}
	if !strings.Contains(out, "build a task API") || !strings.Contains(out, "request") {
		t.Errorf("request row missing:\n%s", out)
	}
	if !strings.Contains(out, "nil MaxBytesReader panics") || !strings.Contains(out, "resolved") {
		t.Errorf("findings/status not listed:\n%s", out)
	}
	if !strings.Contains(out, "▸") {
		t.Errorf("selection cursor not drawn:\n%s", out)
	}
}

// A long board windows around the selection so the rest of the screen survives,
// and says how many rows are hidden each way.
func TestBoardRenderWindowsAroundSelection(t *testing.T) {
	v := boardView{reachable: true}
	for i := 0; i < 30; i++ {
		v.tickets = append(v.tickets, boardTicket{id: "x", label: "finding", status: "open", severity: "low", kind: "quality"})
	}
	out := v.render(20)
	if !strings.Contains(out, "above") || !strings.Contains(out, "below") {
		t.Errorf("a windowed board should mark hidden rows above and below:\n%s", out)
	}
	// The window is bounded, not the whole 30 rows.
	if n := strings.Count(out, "finding"); n > 12 {
		t.Errorf("expected a bounded window, got %d rows:\n%s", n, out)
	}
}

func TestBoardRenderEmptyIsHonest(t *testing.T) {
	out := boardView{reachable: true}.render(0)
	if !strings.Contains(out, "the board is empty") {
		t.Errorf("an empty board should say so:\n%s", out)
	}
}

// wrapLines keeps the author's own breaks and never exceeds the width.
func TestWrapLinesRespectsWidthAndBreaks(t *testing.T) {
	got := wrapLines("one two three\nfour", 8)
	for _, l := range got {
		if len(l) > 8 {
			t.Errorf("line %q exceeds width 8", l)
		}
	}
	if got[len(got)-1] != "four" {
		t.Errorf("the explicit line break was not kept: %v", got)
	}
}

package main

// The board as the TUI reads it: a live view of what the whole workshop is
// doing, not just this screen's own runs. A request built by a batch process,
// a finding filed by a reviewer, a fix auto-mode is working — all of it is on
// the board, and this is how the TUI surfaces it so a person watching one
// screen sees every process's work, navigates it, and opens any ticket to read
// why it stands where it does.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/code-armory-app/blacksmith/internal/ticket"
)

// boardTicket is one row in the navigable board: enough to draw it and to fetch
// its detail. kind is empty for a request.
type boardTicket struct {
	id       string
	label    string // title with the kind/request prefix stripped
	status   string // open | in_progress | resolved | closed
	severity string
	kind     string // "security" | "quality" | "" for a request
	request  bool
}

// boardView is a snapshot of the board for the screen: the tickets in the order
// the operator reads them, plus the counts for the summary line.
type boardView struct {
	tickets      []boardTicket
	openSecurity int
	openQuality  int
	resolved     int // findings already fixed — progress the refiner has made
	reachable    bool
}

// snapshotBoard reads the board once. Best-effort: an unreachable board yields
// an empty, not-reachable view rather than an error, because the TUI must keep
// drawing.
//
// The rows are ordered the way the work flows: requests building now, then the
// open findings in the exact order -auto works them (security first, then by
// severity — the refiner's queue), then a tail of what has been resolved.
func snapshotBoard() boardView {
	if tickets == nil {
		return boardView{}
	}
	ctx := context.Background()
	inprog, err1 := tickets.List(ctx, ticket.ListOpts{Status: ticket.StatusInProgress})
	open, err2 := tickets.List(ctx, ticket.ListOpts{Status: ticket.StatusOpen})
	if err1 != nil || err2 != nil {
		return boardView{}
	}
	resolved, _ := tickets.List(ctx, ticket.ListOpts{Status: ticket.StatusResolved})

	v := boardView{reachable: true}

	for _, t := range inprog {
		if label, ok := strings.CutPrefix(t.Title, "request: "); ok {
			v.tickets = append(v.tickets, boardTicket{
				id: t.ID, label: label, status: t.Status, request: true,
			})
		}
	}

	var findings []boardTicket
	for _, t := range open {
		if bt, ok := findingRow(t); ok {
			findings = append(findings, bt)
			if bt.kind == "security" {
				v.openSecurity++
			} else {
				v.openQuality++
			}
		}
	}
	sort.SliceStable(findings, func(i, j int) bool {
		if (findings[i].kind == "security") != (findings[j].kind == "security") {
			return findings[i].kind == "security"
		}
		return rankOf(findings[i].severity) < rankOf(findings[j].severity)
	})
	v.tickets = append(v.tickets, findings...)

	// A bounded tail of resolved findings, most recent first, so the fixes are
	// visible without the whole history crowding out the live queue.
	const resolvedTail = 8
	shown := 0
	for i := len(resolved) - 1; i >= 0 && shown < resolvedTail; i-- {
		if bt, ok := findingRow(resolved[i]); ok {
			v.tickets = append(v.tickets, bt)
			shown++
		}
	}
	for _, t := range resolved {
		if _, ok := findingRow(t); ok {
			v.resolved++
		}
	}
	return v
}

// findingRow turns a kinded finding ticket into a board row, or reports that
// the ticket is not a finding (a request, anything unkinded).
func findingRow(t ticket.Ticket) (boardTicket, bool) {
	var kind string
	switch {
	case strings.HasPrefix(t.Title, "security: "):
		kind = "security"
	case strings.HasPrefix(t.Title, "quality: "):
		kind = "quality"
	default:
		return boardTicket{}, false
	}
	return boardTicket{
		id:       t.ID,
		label:    strings.TrimPrefix(strings.TrimPrefix(t.Title, "security: "), "quality: "),
		status:   t.Status,
		severity: t.Priority,
		kind:     kind,
	}, true
}

// render draws the board section for the TUI: a summary line and the ticket
// list, with the selected row marked. Windowed around the selection so a long
// board never pushes the rest of the screen away, and the selected row stays in
// view as the cursor moves.
func (v boardView) render(sel int) string {
	var b strings.Builder
	summary := fmt.Sprintf("   %d security · %d quality open", v.openSecurity, v.openQuality)
	if v.resolved > 0 {
		summary += fmt.Sprintf(" · %d fixed", v.resolved)
	}
	b.WriteString(titleStyle.Render("board") + dimStyle.Render(summary) + "\n")

	if len(v.tickets) == 0 {
		b.WriteString(dimStyle.Render("   the board is empty") + "\n")
		return b.String()
	}

	const window = 10
	start := 0
	if sel >= window {
		start = sel - window + 1
	}
	end := start + window
	if end > len(v.tickets) {
		end = len(v.tickets)
	}
	if start > 0 {
		b.WriteString(dimStyle.Render(fmt.Sprintf("   ↑ %d above", start)) + "\n")
	}
	for i := start; i < end; i++ {
		b.WriteString(v.tickets[i].row(i == sel) + "\n")
	}
	if end < len(v.tickets) {
		b.WriteString(dimStyle.Render(fmt.Sprintf("   ↓ %d below", len(v.tickets)-end)) + "\n")
	}
	return b.String()
}

// row draws one ticket line: a cursor when selected, a coloured status badge, a
// severity for a finding, and the label.
func (t boardTicket) row(selected bool) string {
	cursor := "  "
	if selected {
		cursor = promptStyle.Render("▸ ")
	}
	badge := boardStatusStyle(t.status).Render(fmt.Sprintf("%-11s", boardStatusLabel(t.status)))
	if t.request {
		return cursor + badge + " " + titleStyle.Render("request") + " " + truncate(t.label, 50)
	}
	sev := boardSevStyle(t.severity).Render(fmt.Sprintf("%-8s", t.severity))
	return cursor + badge + " " + sev + " " + dimStyle.Render(t.kind[:3]) + " " + truncate(t.label, 46)
}

func boardStatusLabel(s string) string {
	if s == ticket.StatusInProgress {
		return "in progress"
	}
	return s
}

package main

// The board as the TUI reads it: a live view of what the whole workshop is
// doing, not just this screen's own runs. A request built by a batch process,
// a finding filed by a reviewer, a fix auto-mode is working — all of it is on
// the board, and this is how the TUI surfaces it so a person watching one
// screen sees every process's work.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/code-armory-app/blacksmith/internal/ticket"
)

// render draws the board section for the TUI: a one-line summary, the requests
// building now, and the open findings in the order -auto works them. The board
// IS the refiner's queue, so the screen shows it as one. Bounded to a handful
// of rows so a long backlog does not push the input line off-screen.
func (v boardView) render() string {
	var b strings.Builder
	summary := fmt.Sprintf("   %d security · %d quality open", v.openSecurity, v.openQuality)
	if v.resolved > 0 {
		summary += fmt.Sprintf(" · %d fixed", v.resolved)
	}
	b.WriteString("\n" + titleStyle.Render("board") + dimStyle.Render(summary) + "\n")

	for _, r := range v.activeRequests {
		b.WriteString("   " + runStyle.Render("▷ ") + truncate(r, 64) + dimStyle.Render("  building") + "\n")
	}
	if len(v.findings) == 0 {
		b.WriteString(dimStyle.Render("   no open findings") + "\n")
		return b.String()
	}
	const maxRows = 8
	for i, f := range v.findings {
		if i >= maxRows {
			b.WriteString(dimStyle.Render(fmt.Sprintf("   …and %d more", len(v.findings)-maxRows)) + "\n")
			break
		}
		sev := boardSevStyle(f.severity).Render(fmt.Sprintf("%-8s", f.severity))
		tag := dimStyle.Render(f.kind[:3])
		b.WriteString("   " + sev + " " + tag + " " + truncate(f.title, 58) + "\n")
	}
	return b.String()
}

// boardFinding is one finding as the screen lists it: its kind and severity for
// ordering and colour, and its title with the kind prefix already stripped.
type boardFinding struct {
	kind     string // "security" | "quality"
	severity string
	title    string
}

// boardView is a snapshot of the board for the screen: the requests in flight,
// the findings still open (the refiner's queue), and how many have been fixed.
type boardView struct {
	activeRequests []string       // titles of in_progress requests
	findings       []boardFinding // open findings, security first then by severity
	openSecurity   int
	openQuality    int
	resolved       int // findings already fixed — progress the refiner has made
	reachable      bool
}

// snapshotBoard reads the board once. Best-effort: an unreachable board yields
// an empty, not-reachable view rather than an error, because the TUI must keep
// drawing.
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
	// Resolved is best-effort on top: a store that lists open but not resolved
	// still draws, just without the fixed count.
	resolved, _ := tickets.List(ctx, ticket.ListOpts{Status: ticket.StatusResolved})

	v := boardView{reachable: true}
	for _, t := range inprog {
		if title, ok := strings.CutPrefix(t.Title, "request: "); ok {
			v.activeRequests = append(v.activeRequests, title)
		}
	}
	sort.Strings(v.activeRequests)

	for _, t := range open {
		var kind string
		switch {
		case strings.HasPrefix(t.Title, "security: "):
			kind = "security"
			v.openSecurity++
		case strings.HasPrefix(t.Title, "quality: "):
			kind = "quality"
			v.openQuality++
		default:
			continue
		}
		v.findings = append(v.findings, boardFinding{
			kind:     kind,
			severity: t.Priority,
			title:    strings.TrimPrefix(strings.TrimPrefix(t.Title, "security: "), "quality: "),
		})
	}
	// SAME ORDER THE REFINER WORKS THEM: all security before all quality, each
	// block by severity. What the screen shows top-to-bottom is what -auto takes
	// next-to-last, so the board reads as the queue it is.
	sort.SliceStable(v.findings, func(i, j int) bool {
		if (v.findings[i].kind == "security") != (v.findings[j].kind == "security") {
			return v.findings[i].kind == "security"
		}
		return rankOf(v.findings[i].severity) < rankOf(v.findings[j].severity)
	})

	for _, t := range resolved {
		if strings.HasPrefix(t.Title, "security: ") || strings.HasPrefix(t.Title, "quality: ") {
			v.resolved++
		}
	}
	return v
}

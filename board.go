package main

// The board as the TUI reads it: a live view of what the whole workshop is
// doing, not just this screen's own runs. A request built by a batch process,
// a finding filed by a reviewer, a fix auto-mode is working — all of it is on
// the board, and this is how the TUI surfaces it so a person watching one
// screen sees every process's work.

import (
	"context"
	"sort"
	"strings"

	"github.com/code-armory-app/blacksmith/internal/ticket"
)

// boardView is a snapshot of the board for the screen: the requests in flight
// and the findings waiting.
type boardView struct {
	activeRequests []string // titles of in_progress requests
	openSecurity   int
	openQuality    int
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
	v := boardView{reachable: true}
	for _, t := range inprog {
		if title, ok := strings.CutPrefix(t.Title, "request: "); ok {
			v.activeRequests = append(v.activeRequests, title)
		}
	}
	sort.Strings(v.activeRequests)
	for _, t := range open {
		switch {
		case strings.HasPrefix(t.Title, "security: "):
			v.openSecurity++
		case strings.HasPrefix(t.Title, "quality: "):
			v.openQuality++
		}
	}
	return v
}

package main

// Request tracking on the plane's ticket board.
//
// Every run is a ticket: opened in_progress at submission, commented and
// resolved (or left open) at the verdict — so the board answers "what has the
// workshop been asked, and how did it go" without reading a single log. The
// security reviewer's findings are CHILD tickets of the request that produced
// them, which keeps a finding attached to its context and off the repository
// branch, where it would read as a curated vulnerability list to anyone with
// clone access.
//
// ALL OF IT BEST-EFFORT, like the git journal and for the same reason: the
// board is a record of the work, not part of it. A run whose only defect is
// an unreachable ticket store has still built the thing.

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/code-armory-app/blacksmith/internal/platform"
	"github.com/code-armory-app/blacksmith/internal/ticket"
)

// tickets is the plane's store, wired at startup when one is configured, and
// curRequest is the run now holding the GPU — package variables by the same
// one-run-at-a-time argument as curGit and announce.
var (
	tickets    *platform.Store
	curRequest string
)

// openRequestTicket puts the run on the board before any stage moves.
func openRequestTicket(task, dir string) {
	curRequest = ""
	if tickets == nil {
		return
	}
	project, run := projectAndRun(dir)
	t, err := tickets.Create(context.Background(), ticket.Ticket{
		Title:       "request: " + truncate(task, 90),
		Description: task + "\n\nproject: " + project + "\nrun: " + run,
		Status:      ticket.StatusInProgress,
		Priority:    "medium",
	})
	if err != nil {
		slog.Warn("request tracking: create failed — the run itself is unaffected", "error", err)
		return
	}
	curRequest = t.ID
	slog.Info("request tracked", "ticket", t.ID)
}

// closeRequestTicket records the verdict. A pass resolves; a failure stays
// OPEN with the reason, because an unresolved request is work someone still
// owes an answer.
func closeRequestTicket(passed bool, note string) {
	if tickets == nil || curRequest == "" {
		return
	}
	ctx := context.Background()
	if _, err := tickets.AddComment(ctx, curRequest, note); err != nil {
		slog.Warn("request tracking: comment failed", "error", err)
	}
	status := ticket.StatusOpen
	if passed {
		status = ticket.StatusResolved
	}
	if _, err := tickets.Update(ctx, curRequest, ticket.Update{Status: status}); err != nil {
		slog.Warn("request tracking: update failed", "error", err)
	}
	curRequest = ""
}

// fileFinding is what the security reviewer's file_ticket tool lands on: one
// finding, one child ticket of the request that produced it.
func fileFinding(title, body, severity string) (string, error) {
	if tickets == nil {
		return "", fmt.Errorf("no ticket store configured")
	}
	t := ticket.Ticket{
		Title:       "security: " + title,
		Description: body,
		Status:      ticket.StatusOpen,
		Priority:    severity,
	}
	if curRequest != "" {
		id := curRequest
		t.ParentID = &id
	}
	created, err := tickets.Create(context.Background(), t)
	if err != nil {
		return "", err
	}
	return created.ID, nil
}

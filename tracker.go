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
	"strings"

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

// fileFinding is what a reviewer's file_ticket tool lands on: one finding, one
// child ticket of the request that produced it, KIND-labelled so auto-mode can
// work security before quality. Both reviewers file through here; the kind is
// bound by the stage, not chosen by the model.
//
// DEDUP IS SERVER-SIDE, because the model cannot be trusted to check first —
// the first scanner-fed run filed the same "no authentication on any endpoint"
// three times, once as a ticket ABOUT the duplication. An open finding whose
// title already exists on the board is not filed again; its id is returned as
// if it were, so the reviewer sees success and moves on.
func fileFinding(kind, title, body, severity string) (string, error) {
	if tickets == nil {
		return "", fmt.Errorf("no ticket store configured")
	}
	full := kind + ": " + title
	if id := existingFindingID(full); id != "" {
		slog.Info("finding already on the board; not refiled", "ticket", id, "title", full)
		return id, nil
	}
	t := ticket.Ticket{
		Title:       full,
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

// existingFindingID returns the id of an OPEN ticket with this exact title, or
// empty. Exact-title only on purpose: fuzzy matching would silently swallow a
// genuinely new finding whose wording merely resembled an old one, and a
// missed finding is worse than a duplicate a person closes in a click.
func existingFindingID(title string) string {
	if tickets == nil {
		return ""
	}
	open, err := tickets.List(context.Background(), ticket.ListOpts{Status: ticket.StatusOpen})
	if err != nil {
		slog.Warn("dedup: could not list open tickets; filing anyway", "error", err)
		return ""
	}
	for _, t := range open {
		if t.Title == title {
			return t.ID
		}
	}
	return ""
}

// countOpenFindings is how the post-merge audit tells whether a fresh full run
// turned anything up: the number of open kinded findings on the board, before
// and after. A rise means the audit filed something the next cycle must work.
func countOpenFindings() int {
	if tickets == nil {
		return 0
	}
	open, err := tickets.List(context.Background(), ticket.ListOpts{Status: ticket.StatusOpen})
	if err != nil {
		return 0
	}
	n := 0
	for _, t := range open {
		if strings.HasPrefix(t.Title, "security: ") || strings.HasPrefix(t.Title, "quality: ") {
			n++
		}
	}
	return n
}

// openFindings renders the board's current open findings for a reviewer's
// context, so it knows what is already filed before it starts and does not
// spend turns re-deriving them. Empty when the board is unreachable or clean.
func openFindings() string {
	if tickets == nil {
		return ""
	}
	open, err := tickets.List(context.Background(), ticket.ListOpts{Status: ticket.StatusOpen})
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, t := range open {
		if strings.HasPrefix(t.Title, "security: ") || strings.HasPrefix(t.Title, "quality: ") {
			fmt.Fprintf(&b, "- [%s] %s\n", t.Priority, t.Title)
		}
	}
	return b.String()
}

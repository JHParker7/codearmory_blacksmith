// Package upkeep is the work the department gives itself when nobody is waiting.
//
// UPKEEP IS THE LINTER'S LEFTOVERS, HARVESTED FROM WORK THAT ALREADY HAPPENED.
// The review stage runs the linter over every branch it reads and its own prompt
// calls that output advisory — nothing has ever acted on one. So those findings
// become a low-priority ticket, worked whenever nobody is waiting on anything
// else.
//
// THE FIRST VERSION SCOUTED FOR WORK INSTEAD, and one run measured what that
// costs. A scout opened "make the linter clean" on an idle board eleven seconds
// after startup — correct by its own rule, since the board was empty — and then a
// real request arrived. Ten minutes later the run had spent 94 model turns, 56 of
// them on upkeep, 41 of those refusals, against a repository holding a module
// file and a README with nothing to lint at all.
//
// Two things were wrong and this shape fixes both. Findings now come from a REAL
// RUN, so they exist only when they are real: nothing to lint means no ticket
// rather than an agent inventing work. And the CLAIM is gated rather than the
// opening, so a request arriving mid-idle is not queued behind upkeep that has
// already started.
package upkeep

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/code-armory-app/blacksmith/internal/config"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

// Title is matched to find an open upkeep ticket, so it is fixed text rather
// than anything derived.
const Title = "Upkeep: clear the linter's findings"

// LintSectionLabel is the heading the review stage writes above the linter's
// output.
//
// SHARED RATHER THAN RETYPED: two stages agreeing on prose by accident is a bug
// waiting for someone to reword one of them.
const LintSectionLabel = "LINT (style and correctness, advisory)"

// Store is the board operations this needs.
type Store interface {
	List(ctx context.Context, opts ticket.ListOpts) ([]ticket.Ticket, error)
	Create(ctx context.Context, t ticket.Ticket) (ticket.Ticket, error)
}

// WithMaintenance gives a developer the upkeep gate: the linter AND the tests,
// together, in that order.
//
// ORDER MATTERS. Lint first means a run only going to fail on style stops before
// paying for the test suite, and it puts the linter's complaint at the TOP of the
// output the agent reads rather than under a screen of test results.
//
// Composed into the test command rather than added as a new field, because the
// gate is whatever that command says it is — every stage that verifies anything
// already goes through it, so nothing downstream needs to know this ticket is
// different.
func WithMaintenance(repo config.Repo) config.Repo {
	if repo.LintCommand == "" {
		return repo
	}
	if repo.TestCommand == "" {
		repo.TestCommand = repo.LintCommand
		return repo
	}
	repo.TestCommand = repo.LintCommand + " && " + repo.TestCommand
	return repo
}

// LintFindings pulls the linter's section out of the analysis a review
// collected.
//
// ONLY THE LINTER'S. The same text carries a dependency scan, whose findings are
// in third-party code and are not this repository's to fix, and a static
// analysis pass whose output is a lead rather than a defect. Upkeep is for the
// one of the three that names something MECHANICAL in code we own.
func LintFindings(analysis string) string {
	_, rest, ok := strings.Cut(analysis, "## "+LintSectionLabel)
	if !ok {
		return ""
	}
	// To the next section heading, or the end.
	if next := strings.Index(rest, "\n## "); next >= 0 {
		rest = rest[:next]
	}
	return strings.TrimSpace(rest)
}

// Brief carries the findings THEMSELVES rather than only naming the command that
// produced them: these came from a real run over real code, and re-deriving them
// would cost a sandbox to learn what is already known.
func Brief(repo config.Repo, findings, branch string) string {
	var b strings.Builder
	b.WriteString("Nobody is waiting on this. It is upkeep, and it is the lowest priority on the board.\n\n")
	fmt.Fprintf(&b, "The linter reported these while reviewing `%s`:\n\n```\n%s\n```\n\n",
		branch, clip(findings, 4000))
	fmt.Fprintf(&b, "Clear what it names, then make `%s` pass.\n\n", WithMaintenance(repo).TestCommand)

	// THE GATE IS THE AUTHORITY, NOT THE LIST. The findings are from an earlier
	// branch, so some may already be gone and some may not apply — and an agent
	// told otherwise will edit until the list is satisfied rather than until the
	// gate is.
	b.WriteString("The findings above are from an earlier branch, so some may already be gone and some " +
		"may not apply — the gate is the authority, not this list. Change as little as possible: every " +
		"edit here is one nobody asked for.\n\n")

	// The tests are the specification and they run in the same gate.
	b.WriteString("Do not change behaviour to satisfy the linter. The tests in this repository are the " +
		"specification and they run in the same gate; a lint fix that breaks one is not a fix.")
	return b.String()
}

// Record files the linter's findings as low-priority work, and reports whether
// it filed anything.
//
// ONE OPEN TICKET AT A TIME. The findings are CUMULATIVE — the next review
// reports whatever is still there — so a second ticket would be the same work
// twice, and a board that collects one per merge is a board nobody reads.
func Record(ctx context.Context, store Store, boardID string, repo config.Repo, findings, branch string) (bool, error) {
	findings = strings.TrimSpace(findings)
	if findings == "" || repo.LintCommand == "" {
		return false, nil
	}

	all, err := store.List(ctx, ticket.ListOpts{BoardID: boardID})
	if err != nil {
		return false, fmt.Errorf("upkeep: list: %w", err)
	}
	for _, t := range all {
		if t.Title == Title && !workflow.Terminal(t.Status) {
			return false, nil
		}
	}

	opened, err := store.Create(ctx, ticket.Ticket{
		Title:       Title,
		Description: Brief(repo, findings, branch),
		Status:      workflow.ColReadyForMaintenance,
		// LOWEST THERE IS. Nothing asked for this, and the claim gate reads it back
		// to decide whether anything outranks it.
		Priority: "low",
		BoardID:  ticket.Ptr(boardID),
	})
	if err != nil {
		return false, fmt.Errorf("upkeep: open a ticket: %w", err)
	}

	slog.InfoContext(ctx, "filed the linter's findings as upkeep",
		"ticket_id", opened.ID, "board_id", boardID)
	return true, nil
}

// ClaimTimeout bounds the board read the gate makes.
//
// It runs on EVERY POLL of an otherwise idle dispatcher, so it must not be able
// to hang one.
const ClaimTimeout = 10 * time.Second

// Gate decides whether upkeep may start.
type Gate struct {
	store   Store
	boardID string

	// Timeout bounds the board read. Zero means ClaimTimeout.
	Timeout time.Duration
}

// NewGate builds the claim gate for a board.
func NewGate(store Store, boardID string) *Gate {
	return &Gate{store: store, boardID: boardID}
}

// Wants takes upkeep only when nothing anyone asked for is live.
//
// GATED AT THE CLAIM, NOT AT THE OPENING, and that is the whole correction. The
// first version decided when to OPEN the ticket, which answers the question at
// the wrong moment: a board empty at that instant says nothing about the next ten
// minutes, and a request arriving eleven seconds later spent its run sharing one
// serving slot with upkeep that had already started.
//
// Asked here, the answer is CURRENT. A request that arrives while upkeep is
// queued goes first; one that arrives while upkeep is mid-stage still waits for
// it — which is a stage rather than a run, and is the cost of not abandoning work
// halfway.
//
// A READ FAILURE MEANS NO. The board is the only thing that knows whether anyone
// is waiting, and upkeep proceeding on a guess is the failure this exists to
// prevent.
func (g *Gate) Wants(t ticket.Ticket) bool {
	timeout := g.Timeout
	if timeout <= 0 {
		timeout = ClaimTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	all, err := g.store.List(ctx, ticket.ListOpts{BoardID: g.boardID})
	if err != nil {
		slog.WarnContext(ctx, "upkeep could not read the board, so it is standing down", "error", err)
		return false
	}

	for _, other := range all {
		if other.ID == t.ID || workflow.Terminal(other.Status) {
			continue
		}
		// Another upkeep ticket is not someone waiting.
		if other.Title == Title {
			continue
		}
		return false
	}
	return true
}

// clip bounds the findings quoted into a brief, cutting on a rune boundary.
func clip(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

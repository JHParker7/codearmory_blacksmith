package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

// UPKEEP IS THE LINTER'S LEFTOVERS, HARVESTED FROM WORK THAT ALREADY HAPPENED.
//
// The security stage runs the linter over every branch it reviews and its own
// prompt calls the output advisory — nothing has ever acted on one. So those
// findings become a low-priority ticket, worked whenever nobody is waiting on
// anything else.
//
// THE FIRST VERSION SCOUTED FOR WORK INSTEAD, and r99 measured what that costs. A
// scout opened "make the linter clean" on an idle board eleven seconds after
// startup — correct by its own rule, since the board was empty — and then a real
// request arrived. Ten minutes later the run had spent 94 model turns, 56 of them
// (59%) on upkeep, 41 of those refusals, on a repository holding a go.mod and a
// README with nothing to lint at all.
//
// Two things were wrong and this shape fixes both. Findings now come from a real
// run, so they exist only when they are real: nothing to lint means no ticket,
// rather than an agent inventing work. And the claim is gated (see Wants) rather
// than the opening, so a request that arrives mid-idle is not queued behind
// upkeep that has already started.

// withMaintenance gives a developer the upkeep gate: the linter AND the tests,
// together, in that order.
//
// ORDER MATTERS. Lint first means a run that is only going to fail on style stops
// before paying for the test suite, and it puts the linter's complaint at the top
// of the output the agent reads rather than under a screen of test results.
//
// Composed into TestCommand rather than added as a new field because the gate is
// whatever that command says it is — every stage that verifies anything already
// goes through it, so nothing downstream needs to know this ticket is different.
func withMaintenance(repo RepoConfig) RepoConfig {
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

// maintenanceTitle is matched to find an open upkeep ticket, so it is fixed text
// rather than anything derived.
const maintenanceTitle = "Upkeep: clear the linter's findings"

// lintSectionLabel is the heading the security stage writes above the linter's
// output. Shared rather than retyped: two stages agreeing on prose by accident is
// a bug waiting for someone to reword one of them.
const lintSectionLabel = "LINT (style and correctness, advisory)"

// lintFindings pulls the linter's section out of the analysis blob the security
// stage collected.
//
// ONLY THE LINTER'S. The same blob carries a dependency scan, whose findings are
// in third-party code and are not this repository's to fix, and a static analysis
// pass whose output is a lead rather than a defect. Upkeep is for the one of the
// three that names something mechanical in code we own.
func lintFindings(analysis string) string {
	_, rest, ok := strings.Cut(analysis, "## "+lintSectionLabel)
	if !ok {
		return ""
	}
	// To the next section heading, or the end.
	if next := strings.Index(rest, "\n## "); next >= 0 {
		rest = rest[:next]
	}
	return strings.TrimSpace(rest)
}

// upkeepBrief carries the findings themselves, unlike the version that only
// named the command: these came from a real run over real code, and re-deriving
// them would cost a sandbox to learn what is already known.
func upkeepBrief(repo RepoConfig, findings, branch string) string {
	var b strings.Builder
	b.WriteString("Nobody is waiting on this. It is upkeep, and it is the lowest priority on the board.\n\n")
	fmt.Fprintf(&b, "The linter reported these while reviewing `%s`:\n\n```\n%s\n```\n\n",
		branch, clip(findings, 4000))
	fmt.Fprintf(&b, "Clear what it names, then make `%s` pass.\n\n", withMaintenance(repo).TestCommand)
	b.WriteString("The findings above are from an earlier branch, so some may already be gone and some " +
		"may not apply — the gate is the authority, not this list. Change as little as possible: every " +
		"edit here is one nobody asked for.\n\n")
	b.WriteString("Do not change behaviour to satisfy the linter. The tests in this repository are the " +
		"specification and they run in the same gate; a lint fix that breaks one is not a fix.")
	return b.String()
}

// RecordUpkeep files the linter's findings as low-priority work, and reports
// whether it filed anything.
//
// ONE OPEN TICKET AT A TIME. The findings are cumulative — the next review will
// report whatever is still there — so a second ticket would be the same work
// twice, and a board that collects one per merge is a board nobody reads.
func RecordUpkeep(ctx context.Context, api *CodeArmory, boardID string, repo RepoConfig, findings, branch string) (bool, error) {
	findings = strings.TrimSpace(findings)
	if findings == "" || repo.LintCommand == "" {
		return false, nil
	}
	all, err := api.ListTickets(ctx, ListOpts{BoardID: boardID})
	if err != nil {
		return false, fmt.Errorf("upkeep: list: %w", err)
	}
	for _, t := range all {
		if t.Title == maintenanceTitle && t.Status != ColDone && t.Status != ColBlocked {
			return false, nil
		}
	}
	opened, err := api.CreateTicket(ctx, Ticket{
		Title:       maintenanceTitle,
		Description: upkeepBrief(repo, findings, branch),
		Status:      ColReadyForMaintenance,
		// LOWEST THERE IS. Nothing asked for this, and Wants reads it back to
		// decide whether anything outranks it.
		Priority: "low",
		BoardID:  boardPtr(boardID),
	})
	if err != nil {
		return false, fmt.Errorf("upkeep: open a ticket: %w", err)
	}
	slog.InfoContext(ctx, "filed the linter's findings as upkeep",
		"ticket_id", opened.TicketID, "board_id", boardID)
	return true, nil
}

// upkeepEnabled is the host-wide default for auto mode. A project overrides it;
// see Project.AutoMode.
//
// OFF BY DEFAULT. It spends the serving slot on work nobody asked for, and on a
// host where a request can arrive at any moment that is a choice to make rather
// than a default to inherit.
func upkeepEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("AGENTS_UPKEEP")), "true")
}

// MaintenanceAgent is the developer loop with a claim gate.
//
// The loop is the same one deliberately: everything that makes the developer
// safe — it may not touch *_test.go, its writes are gated, its turns are
// bounded — applies unchanged, and a second loop would be a second set of those
// guards to keep in step. What differs is the gate it is judged by, the column it
// is fed from, and when it is allowed to start.
type MaintenanceAgent struct {
	*DevAgent
	api     *CodeArmory
	boardID string
}

// NewMaintenanceAgent builds the upkeep worker.
func NewMaintenanceAgent(gw *Gateway, api *CodeArmory, class Class, repo RepoConfig, maxIterations int, boardID string) *MaintenanceAgent {
	a := NewDevAgent(gw, api, class, withMaintenance(repo), maxIterations)
	a.role = roleMaintain
	return &MaintenanceAgent{DevAgent: a, api: api, boardID: boardID}
}

// upkeepClaimTimeout bounds the board read Wants makes. It runs on every poll of
// an otherwise idle dispatcher, so it must not be able to hang one.
const upkeepClaimTimeout = 10 * time.Second

// Wants takes upkeep only when nothing anyone asked for is live.
//
// GATED AT THE CLAIM, NOT AT THE OPENING, and that is the whole correction. The
// first version decided when to OPEN the ticket, which answers the question at
// the wrong moment: a board empty at that instant says nothing about the next ten
// minutes, and on r99 a request arrived eleven seconds later and spent its run
// sharing one serving slot with upkeep that had already started.
//
// Asked here, the answer is current. A request that arrives while upkeep is
// queued goes first; one that arrives while upkeep is mid-stage still waits for
// it, which is a stage rather than a run and is the cost of not abandoning work
// halfway.
//
// A READ FAILURE MEANS NO. The board is the only thing that knows whether anyone
// is waiting, and upkeep proceeding on a guess is the failure this exists to
// prevent.
func (a *MaintenanceAgent) Wants(t Ticket) bool {
	ctx, cancel := context.WithTimeout(context.Background(), upkeepClaimTimeout)
	defer cancel()
	all, err := a.api.ListTickets(ctx, ListOpts{BoardID: a.boardID})
	if err != nil {
		slog.WarnContext(ctx, "upkeep could not read the board, so it is standing down", "error", err)
		return false
	}
	for _, other := range all {
		if other.TicketID == t.TicketID {
			continue
		}
		if other.Status == ColDone || other.Status == ColBlocked {
			continue
		}
		// Another upkeep ticket is not someone waiting.
		if other.Title == maintenanceTitle {
			continue
		}
		return false
	}
	return true
}

package main

// Auto mode: with no request to work on, blacksmith works the BOARD. It pulls
// the highest-priority open finding — security before quality, then by
// severity — clones the project the finding is about, fixes it, and resolves
// the ticket. This is what the reviewers' tickets were for: a queue the
// workshop drains when idle.
//
// SECURITY FIRST IS STRUCTURAL, not a heuristic the model can miss: findings
// are kinded at filing time, and the selector orders every security finding
// ahead of every quality one regardless of severity, because a low security
// hole outranks a high quality nit.
//
// Best-effort and BOUNDED like everything that touches the board: a fix that
// does not converge in its stage budget is left open with a comment for a
// person, and auto-mode moves to the next finding rather than grinding on one.

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/code-armory-app/blacksmith/internal/agents"
	"github.com/code-armory-app/blacksmith/internal/ticket"
)

// severityRank orders a finding within its kind. Unknown severities sort
// last, so a mis-severitied ticket is worked after the ones that named a real
// level rather than jumping the queue.
var severityRank = map[string]int{"critical": 0, "high": 1, "medium": 2, "low": 3}

func rankOf(sev string) int {
	if r, ok := severityRank[strings.ToLower(sev)]; ok {
		return r
	}
	return len(severityRank)
}

// finding is one actionable board item: an open ticket whose title a reviewer
// kinded.
type finding struct {
	id       string
	kind     string // "security" | "quality"
	severity string
	title    string
	body     string
	parent   string
}

// pickFindings returns the open findings in the order auto-mode should work
// them: all security before all quality, each block by severity. Tickets that
// are not reviewer findings — requests, anything unkinded — are skipped.
func pickFindings(open []ticket.Ticket, skip map[string]bool) []finding {
	var out []finding
	for _, t := range open {
		if skip[t.ID] {
			continue // already attempted this session; left open for a person
		}
		kind := ""
		switch {
		case strings.HasPrefix(t.Title, "security: "):
			kind = "security"
		case strings.HasPrefix(t.Title, "quality: "):
			kind = "quality"
		default:
			continue
		}
		parent := ""
		if t.ParentID != nil {
			parent = *t.ParentID
		}
		out = append(out, finding{
			id: t.ID, kind: kind, severity: t.Priority,
			title: t.Title, body: t.Description, parent: parent,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].kind == "security") != (out[j].kind == "security") {
			return out[i].kind == "security" // security first, always
		}
		return rankOf(out[i].severity) < rankOf(out[j].severity)
	})
	return out
}

// findingRepo is where a finding's code lives: the project's repository and
// the run branch the finding was filed against, read from the parent request
// ticket's description ("project: X\nrun: Y").
type findingRepo struct{ project, run string }

func repoOfFinding(f finding) (findingRepo, bool) {
	if tickets == nil || f.parent == "" {
		return findingRepo{}, false
	}
	req, err := tickets.Get(context.Background(), f.parent)
	if err != nil {
		return findingRepo{}, false
	}
	var r findingRepo
	for _, line := range strings.Split(req.Description, "\n") {
		if v, ok := strings.CutPrefix(line, "project: "); ok {
			r.project = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(line, "run: "); ok {
			r.run = strings.TrimSpace(v)
		}
	}
	return r, r.project != "" && r.run != ""
}

// resolveFinding closes a finding ticket with a comment on what happened.
func resolveFinding(id string, fixed bool, note string) {
	if tickets == nil || id == "" {
		return
	}
	ctx := context.Background()
	if _, err := tickets.AddComment(ctx, id, note); err != nil {
		slog.Warn("auto: finding comment failed", "error", err)
	}
	if !fixed {
		return // left OPEN for a person; the comment says why
	}
	if _, err := tickets.Update(ctx, id, ticket.Update{Status: ticket.StatusResolved}); err != nil {
		slog.Warn("auto: finding resolve failed", "error", err)
	}
}

// autoNext works ONE finding: pick, clone, fix, push, resolve. It returns
// whether it found anything to do, so the caller's loop can stop when the
// board is drained rather than spin.
func autoNext(ctx context.Context, maker agents.Creator, base string, attempted map[string]bool) (worked bool, err error) {
	if tickets == nil {
		return false, fmt.Errorf("auto mode needs a ticket board; none is configured")
	}
	open, err := tickets.List(ctx, ticket.ListOpts{Status: ticket.StatusOpen})
	if err != nil {
		return false, fmt.Errorf("reading the board: %w", err)
	}
	todo := pickFindings(open, attempted)
	if len(todo) == 0 {
		return false, nil
	}
	f := todo[0]
	// ATTEMPTED ONCE PER SESSION, recorded BEFORE the work so that a finding
	// left open for a person is not re-picked next cycle. A truly unfixable
	// finding — auth the tests forbid, a design change no minimal edit makes —
	// stays open, but auto-mode moves past it instead of grinding forever.
	// Recorded before, not after, so a mid-fix crash cannot resurrect the loop.
	attempted[f.id] = true
	slog.Info("auto: working a finding", "kind", f.kind, "severity", f.severity, "title", f.title)

	repo, ok := repoOfFinding(f)
	if !ok {
		resolveFinding(f.id, false,
			"auto mode could not tell which project or branch this finding is about — its parent "+
				"request names no project/run. A person needs to point it at the code.")
		return true, nil
	}

	dir, cloneErr := cloneRunBranch(ctx, base, repo)
	if cloneErr != nil {
		resolveFinding(f.id, false, "auto mode could not clone the code: "+cloneErr.Error())
		return true, nil
	}

	files, readErr := readTree(dir)
	if readErr != nil {
		resolveFinding(f.id, false, "auto mode could not read the cloned tree: "+readErr.Error())
		return true, nil
	}

	task := "Fix this reported finding:\n\n" + f.title + "\n\n" + f.body
	agent := maker.FixFinding(files)
	out, tree, runErr := runStage(ctx, func(map[string]string) (*agents.Agent, error) { return agent, nil }, files, task)
	if runErr != nil {
		resolveFinding(f.id, false, "auto mode's fix attempt errored: "+firstLineOf(runErr.Error()))
		return true, nil
	}
	if !out.Passed {
		resolveFinding(f.id, false,
			"auto mode could not fix this within its budget; a person should take it. Last check:\n"+
				truncate(out.LastCheck, 500))
		return true, nil
	}

	if pushErr := pushFixBranch(ctx, tree, dir, repo, f); pushErr != nil {
		resolveFinding(f.id, false, "auto mode fixed this locally but could not push the fix branch: "+pushErr.Error())
		return true, nil
	}

	// THE REVIEW GATE. The fix passed its own tests; now a reviewer reads the
	// diff and decides whether it merges to dev. Nothing reaches dev without
	// passing here — an auto-fix is a proposal, and dev is what the next
	// request and every human build on.
	diff, diffErr := diffOfHead(ctx, dir)
	if diffErr != nil {
		resolveFinding(f.id, false, "the fix is on its branch, but its diff could not be read for review: "+diffErr.Error())
		return true, nil
	}

	merged := ""
	m := maker
	m.MergeFix = func(reason string) (string, error) {
		res, err := mergeFixToDev(ctx, base, repo, f)
		if err != nil {
			return "", err
		}
		merged = reason
		return res, nil
	}
	reviewTask := "Review this proposed fix.\n\nThe finding:\n" + f.title + "\n\n" + f.body +
		"\n\nThe diff of the fix:\n" + truncate(diff, 8000)
	reviewAgent := m.MergeReviewer(files)
	rout, _, rerr := runStage(ctx, func(map[string]string) (*agents.Agent, error) { return reviewAgent, nil }, files, reviewTask)
	if rerr != nil {
		resolveFinding(f.id, false, "the fix is on its branch; the review stage errored: "+firstLineOf(rerr.Error()))
		return true, nil
	}

	if merged != "" {
		resolveFinding(f.id, true, "auto mode fixed this, review approved it, and it merged to dev: "+merged)
	} else {
		// The reviewer did not merge — its verdict is why, and the fix waits
		// on its branch for a person. Left OPEN, and in the session skip-set.
		resolveFinding(f.id, false,
			"auto mode fixed this and pushed a fix branch, but review did NOT approve the merge:\n"+
				truncate(rout.Answer, 600)+"\n\nThe fix waits on its branch for a person.")
	}
	return true, nil
}

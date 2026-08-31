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

// autoProject works a WHOLE project's findings as one unit, because dev must
// not receive a project's fixes until every finding ABOVE LOW SEVERITY is
// resolved. Merging a fix for a low nit while a critical hole is still open
// would put a knowingly-vulnerable tree on the branch a person integrates from;
// the merge waits for the serious ones.
//
// It picks the highest-priority open finding, works every open finding that
// belongs to the SAME project on one accumulating clone — fix, review, and on
// approval commit — and only when no above-low finding is left unfixed does it
// merge the batch to dev. If even one above-low finding cannot be fixed, the
// whole merge is HELD: the approved fixes wait for a person rather than land a
// half-secured tree on dev.
//
// Returns whether it found a project to work, so the caller's loop stops when
// the board is drained rather than spins.
func autoProject(ctx context.Context, maker agents.Creator, base string, attempted map[string]bool) (worked bool, err error) {
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

	// The project to work is the one the highest-priority finding is about.
	// ATTEMPTED IS RECORDED BEFORE THE WORK, so a finding left open for a person
	// is not re-picked next cycle — a truly unfixable finding stays open but the
	// loop moves past it instead of grinding forever.
	lead := todo[0]
	attempted[lead.id] = true
	repo, ok := repoOfFinding(lead)
	if !ok {
		resolveFinding(lead.id, false,
			"auto mode could not tell which project or branch this finding is about — its parent "+
				"request names no project/run. A person needs to point it at the code.")
		return true, nil
	}

	// Gather every open finding for this project, in the same priority order,
	// and mark them all attempted: the project gets one pass per session.
	var group []finding
	for _, f := range todo {
		if r, ok := repoOfFinding(f); ok && r == repo {
			group = append(group, f)
			attempted[f.id] = true
		}
	}
	slog.Info("auto: working a project", "project", repo.project, "run", repo.run, "findings", len(group))

	dir, cloneErr := cloneRunBranch(ctx, base, repo)
	if cloneErr != nil {
		for _, f := range group {
			resolveFinding(f.id, false, "auto mode could not clone the code: "+cloneErr.Error())
		}
		return true, nil
	}

	// Work each finding on the shared clone; approved fixes accumulate as
	// commits, rejected work is discarded before the next finding.
	var approved []finding
	blockedAboveLow := false
	for _, f := range group {
		switch outcome, note := fixReviewOne(ctx, maker, dir, repo, f); outcome {
		case fixApproved:
			approved = append(approved, f)
		case fixNoop:
			// The tree already satisfies this finding — a sibling fix covered it,
			// or it targets what the developer cannot edit (a test file), and the
			// source scanner does not flag it. Not a fix, not an open hole: left
			// for a person, but it does NOT hold the merge.
			resolveFinding(f.id, false, note)
			restoreToHead(ctx, dir)
		default: // fixFailed
			resolveFinding(f.id, false, note)
			if isAboveLow(f.severity) {
				blockedAboveLow = true // a serious finding is genuinely unfixed: the merge waits
			}
			restoreToHead(ctx, dir) // drop the rejected work before the next finding
		}
	}

	if len(approved) == 0 {
		return true, nil // nothing landed; every finding is already resolved-open
	}

	// THE MERGE GATE. dev takes the project ONLY when every above-low finding is
	// fixed. If one is not, the approved fixes are real and committed on the
	// clone, but they do not merge — a person settles the blocker first.
	if blockedAboveLow {
		held := "auto mode fixed and approved this, but the merge to dev is HELD: the project still has " +
			"an above-low finding it could not fix, and dev must not take the project until every finding " +
			"above low severity is resolved. The approved fix is ready; a person should settle the blocker."
		for _, f := range approved {
			resolveFinding(f.id, false, held)
		}
		slog.Info("auto: merge held; an above-low finding is unfixed",
			"project", repo.project, "approved", len(approved))
		return true, nil
	}

	// Every above-low finding is resolved: push the accumulated batch and merge
	// it to dev in one step.
	batch := "fix/batch-" + repo.run
	if pErr := pushBranch(ctx, dir, batch); pErr != nil {
		for _, f := range approved {
			resolveFinding(f.id, false, "auto mode fixed and approved this but could not push the batch branch: "+pErr.Error())
		}
		return true, nil
	}
	msg := fmt.Sprintf("merge: %d approved fix(es) for run/%s", len(approved), repo.run)
	res, mErr := mergeBranchToDev(ctx, base, repo, batch, msg)
	if mErr != nil {
		for _, f := range approved {
			resolveFinding(f.id, false, "auto mode fixed and approved this; the batch merge to dev failed: "+firstLineOf(mErr.Error()))
		}
		return true, nil
	}
	for _, f := range approved {
		resolveFinding(f.id, true, fmt.Sprintf(
			"auto mode fixed this and it merged to dev with the project's other approved fixes: %s", res))
	}
	slog.Info("auto: project merged to dev",
		"project", repo.project, "approved", len(approved), "result", res)

	// ANOTHER FULL RUN. The fixes are in; now run the whole detector suite and
	// both reviewers over the merged tree, so anything left behind or newly
	// exposed becomes a fresh finding the next cycle works. A project is done
	// only when a full run turns up nothing.
	auditAfterMerge(ctx, maker, dir, repo, lead.parent)
	return true, nil
}

// auditAfterMerge is the "another full run" a fixed project gets: the scanner
// suite and both reviewers re-examine the merged tree and file any finding that
// survived the fixes or that the fixes newly exposed. Those land on the board,
// the outer loop picks them up, and the project converges — done only when an
// audit turns up nothing. Best-effort: a failed audit never un-merges the fixes
// that already landed.
func auditAfterMerge(ctx context.Context, maker agents.Creator, dir string, repo findingRepo, requestID string) {
	files, err := readTree(dir)
	if err != nil {
		slog.Warn("auto: post-merge audit could not read the tree", "error", err)
		return
	}
	// New findings belong to the same request as the ones just fixed, so they
	// stay attached to the project's context on the board.
	prev := curRequest
	curRequest = requestID
	defer func() { curRequest = prev }()

	before := countOpenFindings()
	stages := []struct {
		name  string
		build func(map[string]string) *agents.Agent
	}{
		{agents.StagePlanSec, maker.PlanSec},
		{agents.StagePlanReview, maker.PlanReview},
	}
	for _, st := range stages {
		tree := runScanners(ctx, maker.Sandbox, copyTree(files), st.name)
		task := "This project was just fixed and merged to dev. Re-examine it as a fresh full review " +
			"and file any finding that still stands or that the fixes newly exposed."
		if existing := openFindings(); existing != "" {
			task += "\n\n--- findings already on the board (do NOT refile) ---\n" + existing
		}
		build := st.build
		if _, _, rerr := runStage(ctx, func(t map[string]string) (*agents.Agent, error) { return build(t), nil }, tree, task); rerr != nil {
			slog.Warn("auto: post-merge audit stage errored", "stage", st.name, "error", rerr)
		}
	}
	if after := countOpenFindings(); after > before {
		slog.Info("auto: post-merge audit filed new findings; the loop will work them",
			"project", repo.project, "new", after-before)
	} else {
		slog.Info("auto: post-merge audit found nothing new", "project", repo.project)
	}
}

// fixReviewOne runs the fix↔review negotiation for ONE finding on the shared
// accumulating clone. The fix agent proposes, the reviewer judges the delta the
// fix makes ON TOP of the fixes already committed, and a rejection is not the
// end: the reviewer's reasons go BACK to the fix agent as its next task, so it
// revises rather than abandons. On approval it commits the fix to the clone —
// advancing what the next finding builds on — and returns true. Bounded, and
// the fix carries forward between rounds so each revision builds on the last.
func fixReviewOne(ctx context.Context, maker agents.Creator, dir string, repo findingRepo, f finding) (fixOutcome, string) {
	cur, err := readTree(dir)
	if err != nil {
		return fixFailed, "auto mode could not read the working tree: " + err.Error()
	}
	// THE SCANNER BASELINE: what the source detector says about the tree BEFORE
	// this fix. The fix is confirmed only if a re-run comes back with this issue
	// gone and nothing new — measured against here.
	baseline := scanSignatures(runKindScanners(ctx, maker.Sandbox, cur, f.kind))
	feedback := ""
	for cycle := 1; cycle <= maxFixReviewCycles; cycle++ {
		task := "Fix this reported finding:\n\n" + f.title + "\n\n" + f.body
		if feedback != "" {
			task += "\n\nA PREVIOUS FIX WAS REJECTED IN REVIEW. Address exactly what the " +
				"reviewer said and revise your fix:\n" + feedback
		}

		agent := maker.FixFinding(cur)
		out, tree, runErr := runStage(ctx, func(map[string]string) (*agents.Agent, error) { return agent, nil }, cur, task)
		if runErr != nil {
			return fixFailed, "auto mode's fix attempt errored: " + firstLineOf(runErr.Error())
		}
		if !out.Passed {
			return fixFailed, "auto mode could not fix this within its budget; a person should take it. Last check:\n" +
				truncate(out.LastCheck, 500)
		}
		cur = tree // carry the attempt forward: the next revision builds on it

		// Lay the fix on the shared clone and stage its delta, so the reviewer
		// sees only THIS finding's change on top of the fixes already committed.
		if err := replaceTree(dir, cur); err != nil {
			return fixFailed, "auto mode could not lay the fix down for review: " + err.Error()
		}
		diff, diffErr := stagedDiff(ctx, dir)
		if diffErr != nil {
			return fixFailed, "the fix could not be diffed for review: " + diffErr.Error()
		}
		if strings.TrimSpace(diff) == "" {
			// NO CHANGE. The accumulated tree already satisfies this finding — a
			// sibling fix covered it (the build files near-duplicate findings, and
			// exact-title dedup lets them through), or it targets what the developer
			// cannot edit (a test file). Ask the SOURCE SCANNER: if it no longer
			// flags the issue this is nothing to do, NOT an open hole, and must not
			// hold the merge. Only a scanner that STILL flags it is a real unfixed
			// finding. Measured live: two duplicate criticals and a dup high all
			// "changed no files" and wrongly held a merge whose real fixes were in.
			after := scanSignatures(runKindScanners(ctx, maker.Sandbox, cur, f.kind))
			if scanVerify(baseline, after).stuck {
				return fixFailed, "auto mode changed nothing and the " + f.kind +
					" scanner still reports the issue; a person should take it."
			}
			return fixNoop, "auto mode made no change here — the tree already satisfies this finding " +
				"(a sibling fix, or a target the developer cannot edit) and the scanner does not flag it. " +
				"Left open for a person to confirm or close; it does not hold the merge."
		}

		// THE REVIEW GATE. approve just RECORDS approval; the merge to dev is a
		// batch that runs once the project's above-low findings are all fixed.
		approved := false
		m := maker
		m.MergeFix = func(string) (string, error) {
			approved = true
			return "Approved. It will merge to dev once the project's above-low findings are all fixed.", nil
		}
		reviewTask := "Review this proposed fix.\n\nThe finding:\n" + f.title + "\n\n" + f.body +
			"\n\nThe diff of the fix:\n" + truncate(diff, 8000)
		reviewAgent := m.MergeReviewer(cur)
		rout, _, rerr := runStage(ctx, func(map[string]string) (*agents.Agent, error) { return reviewAgent, nil }, cur, reviewTask)
		if rerr != nil {
			return fixFailed, "the review stage errored: " + firstLineOf(rerr.Error())
		}

		if approved {
			// THE SCANNER GATE. The reviewer said yes; now the tool that raised
			// the finding runs again on the fixed tree and must not raise it
			// again. A fix that the detector still flags — or that lights up the
			// detector somewhere new — is not done, whatever the reviewer thought.
			report := runKindScanners(ctx, maker.Sandbox, cur, f.kind)
			switch v := scanVerify(baseline, scanSignatures(report)); {
			case len(v.introduced) > 0:
				feedback = "The reviewer approved, but re-running the " + f.kind +
					" scanner shows your fix INTRODUCED new findings at: " +
					strings.Join(v.introduced, ", ") + ". Remove them; do not trade one issue for another."
				slog.Info("auto: scanner regressed on the fix; revising",
					"finding", f.title, "introduced", len(v.introduced))
			case v.stuck:
				feedback = "The reviewer approved, but re-running the " + f.kind +
					" scanner STILL reports the issue — the fix must make the detector stop flagging it. " +
					"Latest scanner output:\n" + truncate(report, 2000)
				slog.Info("auto: scanner still flags the issue; revising", "finding", f.title)
			default:
				// Confirmed by both reviewer and scanner. Commit so the next
				// finding builds on it and the batch branch carries it; the delta
				// is already staged.
				msg := "fix: " + strings.TrimPrefix(strings.TrimPrefix(f.title, "security: "), "quality: ")
				if out, err := gitCmd(ctx, dir, "commit", "-q", "-m", msg); err != nil {
					return fixFailed, "auto mode approved the fix but could not commit it: " + firstLineOf(out)
				}
				return fixApproved, ""
			}
			slog.Info("auto: scanner held back an approved fix; revising",
				"cycle", cycle, "of", maxFixReviewCycles, "finding", f.title)
			continue
		}

		// Rejected: feed the reason back and revise.
		feedback = strings.TrimSpace(rout.Answer)
		slog.Info("auto: review rejected the fix; revising",
			"cycle", cycle, "of", maxFixReviewCycles, "finding", f.title)
	}

	return fixFailed, fmt.Sprintf(
		"auto mode revised the fix through %d cycles without both a review approval and a clean re-scan; "+
			"a person should settle it. The last objection:\n%s", maxFixReviewCycles, truncate(feedback, 600))
}

// fixOutcome is how one finding's fix↔review negotiation ended.
type fixOutcome int

const (
	fixFailed   fixOutcome = iota // could not fix-and-verify; GATES the merge if the finding is above low
	fixApproved                   // fixed, reviewer-approved, scanner-verified, committed to the batch
	fixNoop                       // the tree already satisfies it and the scanner is quiet — not a fix, not a hole; never gates
)

// isAboveLow reports whether a finding's severity outranks "low" — critical,
// high, or medium. These are the findings that GATE the merge to dev: a
// project's fixes wait until every one of them is resolved. A low finding, or
// an unrecognized severity, does not hold the merge.
func isAboveLow(sev string) bool {
	return rankOf(sev) < rankOf("low")
}

// maxFixReviewCycles bounds the fix↔review negotiation for one finding. Three,
// because a fix and a reviewer that cannot converge in three rounds are
// disagreeing about something a person should decide, not looping toward it.
const maxFixReviewCycles = 3

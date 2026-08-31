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

	// THE BATCH FIX. All of the project's findings are fixed in ONE developer
	// run and verified once — the scanners must go quiet and a single reviewer
	// must approve the combined diff, told to hold the merge unless every
	// above-low finding is addressed. N findings cost one fix session and one
	// review instead of N of each; a rejection revises the whole batch.
	fixed, note := fixBatch(ctx, maker, dir, repo, group)
	if !fixed {
		// HELD. The batch could not be built, verified, or approved — dev must
		// not take a project whose serious findings are unresolved.
		for _, f := range group {
			resolveFinding(f.id, false, "auto mode worked this project as a batch; the merge to dev is HELD: "+note)
		}
		slog.Info("auto: batch not merged; held", "project", repo.project, "findings", len(group))
		return true, nil
	}

	// Approved and verified: push the batch and merge it to dev in one step.
	batch := "fix/batch-" + repo.run
	if pErr := pushBranch(ctx, dir, batch); pErr != nil {
		for _, f := range group {
			resolveFinding(f.id, false, "the batch was approved but could not be pushed: "+pErr.Error())
		}
		return true, nil
	}
	msg := fmt.Sprintf("merge: batch fix of %d findings for run/%s", len(group), repo.run)
	res, mErr := mergeBranchToDev(ctx, base, repo, batch, msg)
	if mErr != nil {
		for _, f := range group {
			resolveFinding(f.id, false, "the batch was approved; the merge to dev failed: "+firstLineOf(mErr.Error()))
		}
		return true, nil
	}
	for _, f := range group {
		resolveFinding(f.id, true, fmt.Sprintf(
			"auto mode fixed this in a batch of %d and merged to dev: %s", len(group), res))
	}
	slog.Info("auto: project merged to dev",
		"project", repo.project, "findings", len(group), "result", res)

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

// fixBatch fixes a project's WHOLE finding set in ONE developer run, then
// verifies it once: the source scanners must go quiet and a single reviewer
// must approve the combined diff. This is the fast path — N findings cost one
// fix session and one review instead of N of each, because the dominant cost is
// the number of separate model sessions, not the edits. The trade is
// granularity: a rejection revises the batch as a whole, and the merge gate
// moves into the review, which is told to withhold approval unless every
// ABOVE-LOW finding in the production code is addressed. So dev still never
// takes a batch that leaves a serious hole open.
//
// On success it commits the batch to the clone and returns true. On failure —
// the developer could not reach a passing tree, the scanner still flags an
// issue it can see, or the reviewer will not approve within the cycle budget —
// it returns false and the whole merge is held.
func fixBatch(ctx context.Context, maker agents.Creator, dir string, repo findingRepo, group []finding) (bool, string) {
	cur, err := readTree(dir)
	if err != nil {
		return false, "auto mode could not read the working tree: " + err.Error()
	}
	// Baseline for BOTH detector families over the whole tree — the batch fixes
	// security and quality findings together, so both must be measured.
	baseSec := scanSignatures(runKindScanners(ctx, maker.Sandbox, cur, "security"))
	baseQual := scanSignatures(runKindScanners(ctx, maker.Sandbox, cur, "quality"))

	feedback := ""
	for cycle := 1; cycle <= maxFixReviewCycles; cycle++ {
		task := batchFixTask(group)
		if feedback != "" {
			task += "\n\nA PREVIOUS ATTEMPT WAS SENT BACK IN REVIEW. Address exactly this and revise:\n" + feedback
		}

		agent := maker.FixFinding(cur)
		out, tree, runErr := runStage(ctx, func(map[string]string) (*agents.Agent, error) { return agent, nil }, cur, task)
		if runErr != nil {
			return false, "the batch fix errored: " + firstLineOf(runErr.Error())
		}
		if !out.Passed {
			return false, "the batch fix could not reach a passing build/test within its budget. Last check:\n" +
				truncate(out.LastCheck, 500)
		}
		cur = tree

		if err := replaceTree(dir, cur); err != nil {
			return false, "auto mode could not lay the batch down for review: " + err.Error()
		}
		diff, diffErr := stagedDiff(ctx, dir)
		if diffErr != nil {
			return false, "the batch could not be diffed for review: " + diffErr.Error()
		}
		if strings.TrimSpace(diff) == "" {
			// The developer produced NO change. For these findings a green build is
			// not evidence of a fix — they do not break the build — so an empty diff
			// means the developer ran the check, saw green, and stopped without
			// editing, not that the work is done. Push back and retry rather than
			// hold. Measured live: the batch developer's first act was the check, it
			// passed on the untouched tree, and the stage ended having fixed nothing.
			if cycle == maxFixReviewCycles {
				return false, "the developer made no changes across every cycle; a person should take these findings."
			}
			feedback = "You made NO changes, but the findings are NOT fixed. A passing build/test does " +
				"not resolve these issues — they do not break the build. EDIT the code to address every " +
				"finding listed above, THEN run the check. Do not stop on a green check you got without editing."
			slog.Info("auto: batch fix made no changes; pushing back", "cycle", cycle)
			continue
		}

		// THE SCANNER GATE, over the whole tree: the fixes must introduce no new
		// finding and leave the detectors quieter than the baseline.
		secV := scanVerify(baseSec, scanSignatures(runKindScanners(ctx, maker.Sandbox, cur, "security")))
		qualV := scanVerify(baseQual, scanSignatures(runKindScanners(ctx, maker.Sandbox, cur, "quality")))
		introduced := append(append([]string{}, secV.introduced...), qualV.introduced...)
		if len(introduced) > 0 {
			feedback = "Your changes INTRODUCED new scanner findings at: " + strings.Join(introduced, ", ") +
				". Remove them; do not trade one issue for another."
			slog.Info("auto: batch scanner regressed; revising", "cycle", cycle, "introduced", len(introduced))
			continue
		}

		// THE REVIEW GATE — and the merge gate. The reviewer is told to approve
		// only if every above-low finding in the production code is addressed.
		approved := false
		m := maker
		m.MergeFix = func(string) (string, error) {
			approved = true
			return "Approved. The batch will merge to dev.", nil
		}
		reviewAgent := m.MergeReviewer(cur)
		rout, _, rerr := runStage(ctx, func(map[string]string) (*agents.Agent, error) { return reviewAgent, nil }, cur, batchReviewTask(group, diff))
		if rerr != nil {
			return false, "the batch review errored: " + firstLineOf(rerr.Error())
		}
		if approved {
			if out, err := gitCmd(ctx, dir, "commit", "-q", "-m",
				fmt.Sprintf("fix: %d findings for run/%s", len(group), repo.run)); err != nil {
				return false, "the batch was approved but could not be committed: " + firstLineOf(out)
			}
			return true, ""
		}

		feedback = strings.TrimSpace(rout.Answer)
		slog.Info("auto: batch review rejected; revising", "cycle", cycle, "of", maxFixReviewCycles)
	}
	return false, fmt.Sprintf(
		"the batch went through %d review cycles without approval; a person should settle it.", maxFixReviewCycles)
}

// batchFixTask lists every finding for the developer to fix in one pass.
func batchFixTask(group []finding) string {
	var b strings.Builder
	b.WriteString("Fix ALL of the following reported findings in this code, in one pass. Each is a real " +
		"defect; address every one you can with a source-code change. You may NOT edit test files.\n")
	for i, f := range group {
		fmt.Fprintf(&b, "\n[%d] (%s / %s) %s\n%s\n", i+1, f.severity, f.kind, f.title, f.body)
	}
	return b.String()
}

// batchReviewTask asks the reviewer to judge the whole batch and holds the merge
// gate: approve only when every above-low finding in the production code is
// addressed. A finding that cannot be fixed by a source change — one about a
// test file, or one a sibling fix already covered — does not block approval.
func batchReviewTask(group []finding, diff string) string {
	var b strings.Builder
	b.WriteString("Review this batch of fixes. Call merge_fix to APPROVE only if EVERY finding above low " +
		"severity (critical, high, medium) is actually addressed by the diff — EXCEPT a finding that no " +
		"source-code change can fix (one about a *_test.go file, which may not be edited, or one already " +
		"satisfied by another change) does not block approval. If an above-low finding in the production " +
		"code remains genuinely unfixed, REJECT and name which ones. Low-severity findings need not all be fixed.\n\n" +
		"The findings:\n")
	for i, f := range group {
		fmt.Fprintf(&b, "\n[%d] (%s / %s) %s\n%s\n", i+1, f.severity, f.kind, f.title, f.body)
	}
	b.WriteString("\n\nThe combined diff:\n" + truncate(diff, 12000))
	return b.String()
}

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

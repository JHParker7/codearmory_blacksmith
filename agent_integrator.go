package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// The integrator: reviewed agent branches become the integration branch.
//
// It is the fourth pipeline stage and the first one that is NOT an agent. There
// is no model here at all, and that is the design rather than an omission: a
// merge is `git merge`, and whether the result works is `go test`. Both are
// deterministic, both are faster and more reliable than asking a model, and
// neither can be talked into the wrong answer. The model is kept for the one
// case a tool genuinely cannot settle — a conflict, where two changes disagree
// about intent — and even then this stage only reports it.
//
// WHY THIS STAGE HAS TO EXIST. Every agent branch is cut from the integration
// branch at the moment its sandbox cloned, and no agent can see any other. Four
// concurrent tickets are four branches from one base, each verified alone. Merged
// they have never been tested together: "all green" is four separate greens that
// never met. So the gates run again HERE, on the merged tree, and that run — not
// the per-branch one — is what says the integration branch works.
//
// SERIAL, ALWAYS. The dispatcher for this handler is configured with concurrency
// 1 because every merge moves the branch the next merge starts from. Two at once
// is two merges racing to push the same ref, and the loser either force-pushes
// over work or fails for reasons that look random.

// mergedMarker records that a ticket's branch reached the integration branch, so
// it is merged exactly once.
const mergedMarker = "**Merged to the integration branch.**"

func hasMerged(t Ticket) bool {
	for _, c := range t.Comments {
		if strings.Contains(c.Body, mergedMarker) {
			return true
		}
	}
	return false
}

// conflictMarker records a merge that could not be made mechanically.
const conflictMarker = "**Merge conflict — a person is needed.**"

// integrationFailMarker records a merge that APPLIED cleanly and then failed the
// gates. Separate from a conflict on purpose, because the two ask for opposite
// work: a conflict is two changes disagreeing about intent, and someone has to
// decide which wins. This is one change that is correct alone and wrong in
// combination — nothing to reconcile, a bug to fix. Reporting both under
// "merge conflict" would send a person looking for a disagreement that is not
// there.
const integrationFailMarker = "**Integration failed — the merged tree does not pass.**"

func hasIntegrationFailure(t Ticket) bool {
	for _, c := range t.Comments {
		if strings.Contains(c.Body, integrationFailMarker) {
			return true
		}
	}
	return false
}

// IntegratorAgent implements Handler.
type IntegratorAgent struct {
	api  *CodeArmory
	repo RepoConfig
	// branch is the integration branch these merges land on.
	branch string
}

func NewIntegratorAgent(api *CodeArmory, repo RepoConfig, branch string) *IntegratorAgent {
	if branch == "" {
		branch = defaultIntegrationBranch
	}
	return &IntegratorAgent{api: api, repo: repo, branch: branch}
}

func (a *IntegratorAgent) Role() string { return "integrator" }

// Class is required by Handler and unused: this stage calls no model. It names
// the small class so a host that serves only that one can still integrate.
func (a *IntegratorAgent) Class() Class { return ClassSmall }

// Wants takes tickets a reviewer has finished with that are not yet merged.
//
// AFTER the review deliberately. The reviewer is advisory and blocks nothing, so
// this is not a gate it passes — it is ordering. Merging first would mean the
// review lands on a branch already integrated, which is a report about the past
// rather than a look at a proposal.
func (a *IntegratorAgent) Wants(t Ticket) bool {
	// The column already says reviewed-and-not-yet-merged. The branch is still
	// checked for the same reason the reviewer checks it: there is nothing to
	// merge without one.
	return hasBranch(t)
}

func hasConflict(t Ticket) bool {
	for _, c := range t.Comments {
		if strings.Contains(c.Body, conflictMarker) {
			return true
		}
	}
	return false
}

// Handle merges one ticket's branch and verifies the result.
func (a *IntegratorAgent) Handle(ctx context.Context, t Ticket) (string, string, error) {
	if a.repo.URL == "" {
		return OutcomeFailed, "", errors.New("integrator: no repository configured")
	}
	branch := branchFromTicket(t)
	if branch == "" {
		return OutcomeFailed, "", fmt.Errorf("integrator: no branch recorded on %s", t.TicketID)
	}
	rec := recorderFrom(ctx)

	res, err := a.api.RunSandbox(ctx, rec, SandboxSpec{
		Image:       a.repo.Image,
		Command:     []string{"sh", "-c", a.mergeScript(branch)},
		RunnerClass: a.repo.RunnerClass,
		TimeoutSecs: a.repo.TimeoutSecs,
		SecretRefs:  a.secretRefs(),
	})
	if err != nil {
		return OutcomeFailed, "", fmt.Errorf("integrator: sandbox: %w", err)
	}
	out := res.Stdout + res.Stderr

	switch {
	case res.OK():
		a.comment(ctx, t, fmt.Sprintf("%s\n\n`%s` → `%s`, and the gates passed on the merged tree.\n\n"+
			"The per-branch run only proved this change works against the base it was cut from; this one proves it works with everything merged since.",
			mergedMarker, branch, a.branch))

		// THE RELEASE IS ITS OWN COMMENT, so the window can find it without
		// reading the merge note, and so a person scanning a board sees a version
		// rather than a branch name. Absent is not a failure: a merge that added
		// no commits cuts no tag, and a tag that could not be pushed has already
		// said so in the output.
		detail := "merged " + branch
		if version, subjects, ok := parseRelease(out); ok {
			a.comment(ctx, t, releaseNotes(version, subjects))
			detail = fmt.Sprintf("merged %s as %s", branch, version)
		}
		return OutcomeSuccess, detail, nil

	case strings.Contains(out, mergeConflictMarker):
		// Left for a person, and NOT retried: a conflict is a disagreement about
		// intent between two changes, and running the same mechanical merge again
		// produces the same conflict while spending another sandbox.
		a.comment(ctx, t, fmt.Sprintf("%s\n\n`%s` does not merge cleanly into `%s`.\n\n```\n%s\n```",
			conflictMarker, branch, a.branch, clip(conflictDetail(out), 1200)))
		// Routed to the resolver by OUTCOME, not by naming its column here.
		return OutcomeConflicted, "conflict on " + branch, nil

	default:
		// The merge applied but the merged tree fails. The branch is fine alone and
		// broken in combination, which is exactly what this stage exists to catch,
		// so it is reported against the ticket rather than pushed.
		a.comment(ctx, t, fmt.Sprintf("%s\n\n`%s` merges cleanly into `%s` but the gates FAIL on the result, so nothing was pushed.\n\n"+
			"This change passed alone; it does not pass combined with what has merged since. There is nothing to reconcile — the combination is wrong.\n\n```\n%s\n```",
			integrationFailMarker, branch, a.branch, clip(out, 2000)))
		// A tree that MERGED and then failed its gates is wrong in combination,
		// which no amount of reconciling text fixes — so it goes to a person
		// rather than to the resolver.
		return OutcomeBlocked, "merged tree fails on " + branch, nil
	}
}

// mergeConflictMarker is printed by the script so a conflict is distinguishable
// from a failing test without parsing git's own wording.
const mergeConflictMarker = "===MERGE-CONFLICT==="

// mergeScript merges the branch into the integration branch, runs the gates on
// the RESULT, and pushes only if they pass.
//
// The integration branch is created from the base on first use: a repository that
// has never been integrated has no such branch, and failing on that would make
// the stage unusable exactly once per repository, at the least convenient moment.
func (a *IntegratorAgent) mergeScript(branch string) string {
	base := a.repo.Branch
	if base == "" {
		base = "main"
	}
	s := sandboxPreamble + fmt.Sprintf(`URL="${GIT_CLONE_URL:-%s}"
git clone --filter=blob:none "$URL" repo >/dev/null 2>&1
cd repo
git config user.name integrator
git config user.email integrator@blacksmith.invalid
git checkout -q %q 2>/dev/null || { git checkout -q %q && git checkout -q -b %q; }
# REBASE BEFORE MERGING. An agent branch is cut from the integration branch at
# the moment its sandbox cloned, and this stage is serial, so by the time a
# branch reaches here the integration branch has usually moved on beneath it.
# Merging then compares the agent's work against a snapshot that no longer
# exists, and every ticket touching the same file conflicts BY CONSTRUCTION
# rather than because two changes actually disagree. Measured: three of four
# unfinished tickets in a run were correct, reviewed branches lost this way.
#
# Replaying the commits onto the current tip removes the ordering conflicts and
# leaves only the genuine overlaps — which are the ones worth sending to the
# resolver, and the ones a person would actually have to think about.
#
# "if !" rather than "||" because the preamble sets -e and a failing rebase must
# be handled here, not abort the script. The conflicted paths are captured BEFORE
# the abort, since aborting unwinds the state that names them.
git checkout -q -B blacksmith-replay origin/%s
if ! git rebase %q >/dev/null 2>&1; then
  echo '%s'
  git diff --name-only --diff-filter=U
  git rebase --abort >/dev/null 2>&1 || true
  exit 1
fi
git checkout -q %q
git merge --no-ff --no-edit blacksmith-replay || { echo '%s'; git diff --name-only --diff-filter=U; exit 1; }
`, a.repo.URL, a.branch, base, a.branch, branch, a.branch, mergeConflictMarker, a.branch, mergeConflictMarker)

	// The gates, on the merged tree. Lint stays advisory here for the same reason
	// it is advisory to the developer: it can report what this merge cannot fix.
	for _, cmd := range []string{a.repo.TestCommand, a.repo.CriticalCommand} {
		if cmd != "" {
			s += cmd + "\n"
		}
	}
	s += fmt.Sprintf("git push origin %s\n", a.branch)
	// AFTER the push, so a tag never names a tree that failed to build or that
	// never landed. See releaseTagScript.
	s += releaseTagScript()
	return s
}

func (a *IntegratorAgent) secretRefs() map[string]string {
	if a.repo.SecretRef == "" {
		return nil
	}
	return map[string]string{"GIT_CLONE_URL": a.repo.SecretRef}
}

// conflictDetail pulls the conflicted paths out of the script's output, which is
// what a person needs first; the rest is git's usual advice.
func conflictDetail(out string) string {
	_, after, ok := strings.Cut(out, mergeConflictMarker)
	if !ok {
		return clip(out, 800)
	}
	return strings.TrimSpace(after)
}

func (a *IntegratorAgent) comment(ctx context.Context, t Ticket, body string) {
	if a.api == nil {
		return
	}
	if _, err := a.api.AddComment(ctx, t.TicketID, body); err != nil {
		// LOUD, not merely recorded. A stage's whole visible output is its comment:
		// if the write fails, the work happened and left no trace anywhere a person
		// looks, and the ticket reads as though the stage never ran. That is the
		// hardest failure to diagnose, because there is nothing to diagnose from.
		recorderFrom(ctx).Action(ctx, "ticket-comment", t.TicketID, err)
		slog.ErrorContext(ctx, "could not write the stage's comment; its work is invisible on the ticket",
			"ticket_id", t.TicketID, "error", err)
	}
}

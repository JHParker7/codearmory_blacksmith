// Package integrate makes reviewed agent branches into the integration branch.
//
// THERE IS NO MODEL HERE AT ALL, and that is the design rather than an omission:
// a merge is `git merge`, and whether the result works is the test command. Both
// are deterministic, both are faster and more reliable than asking a model, and
// neither can be talked into the wrong answer. A model is kept for the one case a
// tool genuinely cannot settle — a conflict, where two changes disagree about
// intent — and even then this stage only reports it.
//
// WHY THE STAGE HAS TO EXIST. Every agent branch is cut from the integration
// branch at the moment its sandbox cloned, and no agent can see any other. Four
// concurrent tickets are four branches from one base, each verified ALONE.
// Merged, they have never been tested together: "all green" is four separate
// greens that never met. So the gates run again HERE, on the merged tree, and
// that run — not the per-branch one — is what says the integration branch works.
//
// SERIAL, ALWAYS. The dispatcher for this stage runs one at a time, because
// every merge moves the branch the next merge starts from. Two at once is two
// merges racing to push the same ref, and the loser either force-pushes over
// work or fails for reasons that look random.
package integrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/code-armory-app/blacksmith/internal/config"
	"github.com/code-armory-app/blacksmith/internal/forge"
	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/record"
	"github.com/code-armory-app/blacksmith/internal/release"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/transcript"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

// MergedMarker records that a ticket's branch reached the integration branch, so
// it is merged exactly once.
const MergedMarker = "**Merged to the integration branch.**"

// ConflictMarker records a merge that could not be made mechanically.
const ConflictMarker = "**Merge conflict — a person is needed.**"

// FailedMarker records a merge that APPLIED cleanly and then failed the gates.
//
// SEPARATE FROM A CONFLICT ON PURPOSE, because the two ask for opposite work. A
// conflict is two changes disagreeing about intent, and someone has to decide
// which wins. This is ONE change that is correct alone and wrong in combination
// — nothing to reconcile, a bug to fix. Reporting both under "merge conflict"
// would send a person looking for a disagreement that is not there.
const FailedMarker = "**Integration failed — the merged tree does not pass.**"

// ConflictOutput is printed by the script so a conflict is distinguishable from
// a failing test WITHOUT parsing git's own wording, which changes between
// versions and translations.
const ConflictOutput = "===MERGE-CONFLICT==="

// Sandbox runs a command in isolation.
type Sandbox interface {
	Run(ctx context.Context, rec forge.Recorder, spec forge.Spec) (forge.Result, error)
}

// Store is the board operations this stage needs.
type Store interface {
	AddComment(ctx context.Context, id, body string) (ticket.Comment, error)
}

// Agent merges one ticket's branch and verifies the result.
type Agent struct {
	sandbox Sandbox
	store   Store
	repo    config.Repo

	// branch is the integration branch these merges land on.
	branch string
}

// New builds the integrator.
func New(sandbox Sandbox, store Store, repo config.Repo, branch string) *Agent {
	if branch == "" {
		branch = config.DefaultIntegrationBranch
	}
	return &Agent{sandbox: sandbox, store: store, repo: repo, branch: branch}
}

func (a *Agent) Role() string { return workflow.RoleIntegrate }

// Class is ClassNone: this stage calls no model, and saying "small" instead
// would leave it unassembled on a host that does not serve small — stranding
// every ticket in its column.
func (a *Agent) Class() model.Class { return model.ClassNone }

// Wants takes tickets a reviewer has finished with that have something to merge.
//
// AFTER THE REVIEW, deliberately. The reviewer is advisory and blocks nothing, so
// this is not a gate it passes — it is ORDERING. Merging first would mean the
// review lands on a branch already integrated, which is a report about the past
// rather than a look at a proposal.
func (a *Agent) Wants(t ticket.Ticket) bool {
	// The column already says reviewed-and-not-yet-merged. The branch is still
	// checked for the same reason the reviewer checks it: there is nothing to
	// merge without one.
	return record.HasBranch(t)
}

// Handle merges one branch and verifies the merged tree.
func (a *Agent) Handle(ctx context.Context, t ticket.Ticket) (workflow.Outcome, string, error) {
	if a.repo.URL == "" {
		return workflow.OutcomeFailed, "", errors.New("integrator: no repository configured")
	}
	branch := record.BranchOf(t)
	if branch == "" {
		return workflow.OutcomeFailed, "", fmt.Errorf("integrator: no branch recorded on %s", t.ID)
	}

	res, err := a.sandbox.Run(ctx, recorderFrom(ctx), forge.Spec{
		Image:       a.repo.Image,
		Command:     []string{"sh", "-c", a.MergeScript(branch)},
		RunnerClass: a.repo.RunnerClass,
		TimeoutSecs: a.repo.TimeoutSecs,
		SecretRefs:  a.secretRefs(),
	})
	if err != nil {
		return workflow.OutcomeFailed, "", fmt.Errorf("integrator: sandbox: %w", err)
	}
	out := res.Stdout + res.Stderr

	switch {
	case res.OK():
		return a.merged(ctx, t, branch, out)

	case strings.Contains(out, ConflictOutput):
		// Left for the resolver and NOT retried: a conflict is a disagreement about
		// intent between two changes, and running the same mechanical merge again
		// produces the same conflict while spending another sandbox.
		a.comment(ctx, t, fmt.Sprintf("%s\n\n`%s` does not merge cleanly into `%s`.\n\n```\n%s\n```",
			ConflictMarker, branch, a.branch, clip(ConflictDetail(out), 1200)))
		// Routed by OUTCOME, not by naming a column here.
		return workflow.OutcomeConflicted, "conflict on " + branch, nil

	default:
		// The merge applied and the merged tree fails. The branch is fine alone and
		// broken in combination, which is exactly what this stage exists to catch.
		a.comment(ctx, t, fmt.Sprintf(
			"%s\n\n`%s` merges cleanly into `%s` but the gates FAIL on the result, so nothing was "+
				"pushed.\n\nThis change passed alone; it does not pass combined with what has merged "+
				"since. There is nothing to reconcile — the combination is wrong.\n\n```\n%s\n```",
			FailedMarker, branch, a.branch, clip(out, 2000)))
		// A tree that MERGED and then failed its gates is wrong in combination,
		// which no amount of reconciling text fixes — so it goes to a person rather
		// than to the resolver.
		return workflow.OutcomeBlocked, "merged tree fails on " + branch, nil
	}
}

// merged records a successful integration and the release it cut.
func (a *Agent) merged(ctx context.Context, t ticket.Ticket, branch, out string) (workflow.Outcome, string, error) {
	a.comment(ctx, t, fmt.Sprintf(
		"%s\n\n`%s` → `%s`, and the gates passed on the merged tree.\n\nThe per-branch run only "+
			"proved this change works against the base it was cut from; this one proves it works "+
			"with everything merged since.",
		MergedMarker, branch, a.branch))

	// THE RELEASE IS ITS OWN COMMENT, so the window can find it without reading
	// the merge note, and so a person scanning a board sees a VERSION rather than
	// a branch name.
	//
	// Absent is not a failure: a merge that added no commits cuts no tag, and a
	// tag that could not be pushed has already said so in the output.
	detail := "merged " + branch
	if version, subjects, ok := release.Parse(out); ok {
		a.comment(ctx, t, release.Notes(version, subjects))
		detail = fmt.Sprintf("merged %s as %s", branch, version)
	}
	return workflow.OutcomeSuccess, detail, nil
}

// MergeScript merges the branch into the integration branch, runs the gates on
// the RESULT, and pushes only if they pass.
//
// The integration branch is created from the base on first use: a repository
// that has never been integrated has no such branch, and failing on that would
// make the stage unusable exactly once per repository, at the least convenient
// moment.
func (a *Agent) MergeScript(branch string) string {
	base := a.repo.Branch
	if base == "" {
		base = "main"
	}

	s := forge.Preamble + fmt.Sprintf(`URL="${GIT_CLONE_URL:-%s}"
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
`, a.repo.URL, a.branch, base, a.branch, branch, a.branch, ConflictOutput, a.branch, ConflictOutput)

	// The gates, on the merged tree. Braced for the same reason every other
	// spliced command is: an operator's command may be a chain, and under the
	// preamble's set -e an unguarded earlier element aborts the script.
	for _, cmd := range []string{a.repo.TestCommand, a.repo.CriticalCommand} {
		if cmd != "" {
			s += "{ " + cmd + " ; }\n"
		}
	}

	s += fmt.Sprintf("git push origin %s\n", a.branch)
	// AFTER THE PUSH, so a tag never names a tree that failed to build or that
	// never landed.
	s += release.TagScript()
	return s
}

// ConflictDetail pulls the conflicted paths out of the script's output, which is
// what a person needs first; the rest is git's usual advice.
func ConflictDetail(out string) string {
	_, after, ok := strings.Cut(out, ConflictOutput)
	if !ok {
		return clip(out, 800)
	}
	return strings.TrimSpace(after)
}

// Merged reports whether a ticket has already reached the integration branch.
func Merged(t ticket.Ticket) bool { return hasMarker(t, MergedMarker) }

// Conflicted reports whether a merge was left for the resolver.
func Conflicted(t ticket.Ticket) bool { return hasMarker(t, ConflictMarker) }

// FailedIntegration reports whether a merged tree failed its gates.
func FailedIntegration(t ticket.Ticket) bool { return hasMarker(t, FailedMarker) }

func hasMarker(t ticket.Ticket, marker string) bool {
	for _, c := range t.Comments {
		if strings.Contains(c.Body, marker) {
			return true
		}
	}
	return false
}

func (a *Agent) secretRefs() map[string]string {
	if a.repo.SecretRef == "" {
		return nil
	}
	return map[string]string{"GIT_CLONE_URL": a.repo.SecretRef}
}

// comment writes the stage's visible output.
//
// A FAILURE HERE IS LOUD, not merely recorded. A stage's whole visible output is
// its comment: if the write fails, the work happened and left no trace anywhere a
// person looks, and the ticket reads as though the stage never ran. That is the
// hardest failure to diagnose, because there is nothing to diagnose from.
func (a *Agent) comment(ctx context.Context, t ticket.Ticket, body string) {
	if a.store == nil {
		return
	}
	if _, err := a.store.AddComment(ctx, t.ID, body); err != nil {
		recorderFrom(ctx).Action(ctx, "ticket-comment", t.ID, err)
		slog.ErrorContext(ctx, "could not write the stage's comment; its work is invisible on the ticket",
			"ticket_id", t.ID, "error", err)
	}
}

// recorderFrom is the transcript on this context, or nil — which every recorder
// method tolerates.
func recorderFrom(ctx context.Context) *transcript.Recorder {
	// Prefer the one Start attached; fall back to a locally attached recorder so
	// a test that wires its own still works.
	if r := transcript.RecorderFrom(ctx); r != nil {
		return r
	}
	r, _ := ctx.Value(recorderKey{}).(*transcript.Recorder)
	return r
}

type recorderKey struct{}

// WithRecorder attaches a transcript to a context for the stages that record
// their own actions.
func WithRecorder(ctx context.Context, r *transcript.Recorder) context.Context {
	return context.WithValue(ctx, recorderKey{}, r)
}

func clip(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

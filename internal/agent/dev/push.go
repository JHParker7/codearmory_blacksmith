package dev

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/code-armory-app/blacksmith/internal/forge"
	"github.com/code-armory-app/blacksmith/internal/ticket"
)

// GitCommit commits from the message file the preamble names.
//
// A MESSAGE FILE, NOT `-m`, so model-authored text never reaches a shell word.
const GitCommit = `git -c user.name=dev-agent -c user.email=dev-agent@platform.invalid ` +
	`commit -F "$COMMIT_MSG_FILE"`

// WriteMessageScript puts a commit message where GitCommit will read it.
//
// BASE64, for the same reason every other model-authored payload is: a commit
// subject is prose, prose contains apostrophes and newlines, and both end a
// shell word early. There is nothing to escape if nothing is quoted.
func WriteMessageScript(msg string) string {
	return fmt.Sprintf("echo %s | base64 -d > \"$COMMIT_MSG_FILE\"\n",
		forge.Quote(base64.StdEncoding.EncodeToString([]byte(msg))))
}

// ApplyScript writes the staged tree into the sandbox.
//
// BASE64 AGAIN, and here it is load-bearing rather than tidy: the payload is Go
// source, and a struct tag is `json:"id"`, so a type definition is almost all
// quotes. Asking a shell to carry that literally is asking for a quoting bug on
// every ticket that defines a type.
func ApplyScript(staged map[string]string) string {
	var b strings.Builder
	for _, p := range sortedKeys(staged) {
		fmt.Fprintf(&b, "\nmkdir -p \"$(dirname %s)\"\necho %s | base64 -d > %s",
			forge.Quote(p),
			forge.Quote(base64.StdEncoding.EncodeToString([]byte(staged[p]))),
			forge.Quote(p))
	}
	b.WriteString("\n")
	return b.String()
}

// SplitCommitScript commits each written file on its own, ahead of the catch-all
// commit that follows it.
//
// GENERATED FROM THE STAGED TREE, not discovered in the shell. These are the
// paths this agent wrote this attempt, known here, so the messages are built in
// Go with the same rules as any other commit rather than assembled by
// string-mashing inside a script. Anything the loop does not name — a
// formatter's rewrite, a lock file the dependency step resolved — is still
// caught by the `git add -A` after it, which is why that commit stays.
//
// A file that fails to commit is NOT an error: it may have been staged and
// committed by an earlier attempt, or reverted by a formatter. The catch-all
// behind it is the backstop, so each of these is allowed to be a no-op.
func SplitCommitScript(t ticket.Ticket, staged map[string]string, summary, commitType string) string {
	if !SplitCommits(staged) {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n")
	for _, p := range sortedKeys(staged) {
		fmt.Fprintf(&b, "git add -- %s 2>/dev/null || true\n", forge.Quote(p))
		// Nothing staged for this path means nothing to commit for it.
		b.WriteString("if ! git diff --cached --quiet; then\n")
		b.WriteString("  " + WriteMessageScript(PerFileCommitMessage(t, summary, commitType, p)))
		b.WriteString("  " + GitCommit + " >/dev/null || true\n")
		b.WriteString("fi\n")
	}
	return b.String()
}

// CommitAndPushScript commits what the agent wrote and pushes it.
//
// THE LAST COMMIT IS GUARDED, and leaving it unguarded destroyed a run. The
// per-file split above commits each written file on its own; when it has taken
// all of them there is nothing left for this one, `git commit` fails with
// "nothing to commit", the retry fails the same way, and the preamble's `set -e`
// aborts the script BEFORE the push. The work is committed inside the sandbox
// and thrown away with it.
//
// Measured on r110, and it is silent in exactly the way that costs most: the
// developer wrote ticket.go, store.go, main.go and handlers.go, all correct, and
// the branch ended up holding only ticket.go — the one write made while a single
// staged file meant the split did not run. Every turn after that the agent was
// shown its own correct main.go and told by the gate that func main was
// undeclared, because THE GATE BUILDS THE BRANCH and the branch never got it.
// Twenty turns of an agent being right and being told it was wrong.
//
// So the final commit runs only when something is staged for it, and THE PUSH
// RUNS EITHER WAY.
func CommitAndPushScript(t ticket.Ticket, staged map[string]string, summary, commitType, branch string) string {
	return SplitCommitScript(t, staged, summary, commitType) +
		"\ngit add -A\n" +
		"if ! git diff --cached --quiet; then\n" +
		"  " + WriteMessageScript(CommitMessage(t, summary, commitType)) +
		"  " + GitCommit + " || {\n" +
		// Formatters and linters that rewrite files fail the commit AND leave the
		// fixes in the working tree. Re-staging and retrying once turns that from
		// an unexplained failure into the no-op it should be; a second failure is
		// a real rejection and is reported.
		"    echo '--- hooks modified files or rejected the commit; retrying once ---'\n" +
		"    git add -A\n" +
		"    " + GitCommit + "\n" +
		"  }\n" +
		"fi\n" +
		fmt.Sprintf("git push --force origin %s\n", forge.Quote(branch))
}

// CheckoutScript puts the sandbox on this ticket's branch, creating it when the
// remote has none.
func CheckoutScript(branch string) string {
	q := forge.Quote(branch)
	return fmt.Sprintf("git fetch -q origin %s 2>/dev/null && git checkout -q -B %s FETCH_HEAD "+
		"2>/dev/null || git checkout -q -B %s\n", q, q, q)
}

// MergeIntegrationScript brings in what this ticket waited for.
//
// A TICKET THAT WAITED FOR ITS DEPENDENCY MUST ACTUALLY RECEIVE IT. Branches are
// cut at scoping time, all from the same base, before any sibling has merged.
// Scheduling then holds a ticket until its blockers finish — correctly — but
// nothing rebases, so the developer starts on a tree that predates the code it
// was waiting for. Measured: an agent writing handlers.go against a branch with
// no store.go, compiling to "undefined: Task, undefined: store", then reading 96
// times in a row looking for where Task was defined. It was not there, and the
// reading was rational and could never terminate.
//
// A CONFLICT IS NOT THIS STAGE'S TO RESOLVE: the merge is abandoned and the work
// continues on what the branch already had, because a developer stopped by a
// merge conflict has produced nothing at all, while one working from a stale
// tree has at least produced something the resolver can look at.
func MergeIntegrationScript(integrationBranch string) string {
	if integrationBranch == "" {
		return ""
	}
	q := forge.Quote(integrationBranch)
	return fmt.Sprintf(`
if git fetch -q origin %s 2>/dev/null; then
  if ! git merge -q --no-edit FETCH_HEAD 2>/dev/null; then
    echo "WARNING: could not merge %s cleanly; continuing on this branch alone"
    git merge --abort 2>/dev/null || true
  fi
fi
`, q, integrationBranch)
}

// BestEffortScript runs a command that MUST NOT be able to fail the push.
//
// LOUDLY BEST-EFFORT. A repository may not have the tool, and a tool that cannot
// run must not stop a change that is otherwise fine — but silently skipping one
// the project DOES define would let the agent commit to a standard its human
// contributors are held to. So the warning is printed rather than swallowed.
//
// Empty is a no-op: an operator who has not named a command has not asked for
// one.
func BestEffortScript(command, warning string) string {
	if command == "" {
		return ""
	}
	// BRACE-GROUPED, because the preamble sets -e and a bare `cmd || echo` only
	// protects the last command in a pipeline the operator wrote.
	return fmt.Sprintf("\n{ %s ; } || echo %s\n", command, forge.Quote("WARNING: "+warning))
}

// FormatWarning and DepsWarning are what a failed best-effort step says.
//
// DEPENDENCIES ARE NOT OPTIONAL POLISH. Without the resolve step an agent cannot
// add a dependency at all, and the way it fails is vicious: it writes `require
// github.com/x/y` into go.mod correctly, and every build then fails with
// "missing go.sum entry" — a file of CRYPTOGRAPHIC HASHES that cannot be
// produced by editing text. The agent has no execution tool that would fix it.
// Measured on a live ticket: six commits and 112 model turns spent circling
// go.mod while the actual compile errors sat untouched behind it.
const (
	FormatWarning = "the formatter failed; committing unformatted"
	DepsWarning   = "dependencies could not be resolved; the build may fail on a missing lock entry"
)

// HookSetupScript installs the repository's own hooks, then a Conventional
// Commits hook if nothing else claimed that slot.
//
// A HOOK RATHER THAN A PROMPT, which is this department's usual answer. The
// message is already built in Go, so an agent cannot normally get it wrong — but
// the delegate engine commits for itself, a repository may carry its own
// tooling, and A RULE ENFORCED ON ONE PATH STOPS BEING TRUE the moment a second
// path appears. The hook holds for every commit made in the sandbox however it
// was made.
//
// ONLY IF NOTHING ELSE CLAIMED THE HOOK. pre-commit installs its own commit-msg,
// and a repository running commitlint has said what it wants; overwriting that
// would replace a project's rules with this department's.
const HookSetupScript = `
if [ -f .pre-commit-config.yaml ]; then
  if command -v pre-commit >/dev/null 2>&1 || pip install --quiet pre-commit 2>/dev/null; then
    pre-commit install --hook-type pre-commit --hook-type commit-msg >/dev/null 2>&1 \
      || echo "WARNING: pre-commit install failed; hooks will not run"
  else
    echo "WARNING: this repository defines pre-commit hooks but pre-commit could not be installed"
  fi
fi
` + ConventionalHookScript

// ConventionalHookScript rejects a commit message that is not Conventional
// Commits, measuring the subject in BYTES exactly as the run that broke on it
// did. See MaxSubjectBytes.
const ConventionalHookScript = `
if [ ! -e .git/hooks/commit-msg ]; then
  mkdir -p .git/hooks
  cat > .git/hooks/commit-msg <<'BLACKSMITH_HOOK'
#!/bin/sh
# Conventional Commits, as the Angular project defines them.
subject=$(head -n 1 "$1")
case "$subject" in
  '#'*|'') exit 0 ;;
esac
if ! printf '%s' "$subject" | grep -Eq '^(feat|fix|chore|docs|refactor|test|perf|build|ci|style|revert)(\([a-z0-9._/-]+\))?!?: .+'; then
  echo "commit-msg: not Conventional Commits." >&2
  echo "  got:  $subject" >&2
  echo "  want: type(scope): subject   e.g. feat(store): add the in-memory store" >&2
  echo "  type is one of feat fix chore docs refactor test perf build ci style revert" >&2
  exit 1
fi
if [ "$(printf '%s' "$subject" | wc -c)" -gt 72 ]; then
  echo "commit-msg: subject is longer than 72 characters." >&2
  exit 1
fi
second=$(sed -n '2p' "$1")
if [ -n "$second" ]; then
  echo "commit-msg: leave a blank line between the subject and the body." >&2
  exit 1
fi
BLACKSMITH_HOOK
  chmod +x .git/hooks/commit-msg
fi
`

// PushOptions are the repository commands the push path splices in.
type PushOptions struct {
	IntegrationBranch string
	FormatCommand     string
	DepsCommand       string
}

// PushScript is the whole path from a bare sandbox to a pushed branch.
//
// THE ORDER IS THE ARGUMENT. The integration merge happens BEFORE the edits
// land, so the verification runs against the tree that includes this ticket's
// dependencies — and because the checkout resets to the remote branch, a merge
// done only in an earlier sandbox would be discarded here. Doing it again is
// what gets it committed and pushed.
//
// The formatter and the dependency resolve run at COMMIT time rather than in
// verification, because what LANDS has to be the formatted, resolved state. A
// formatter run afterwards reports on a branch that is already pushed, and a
// manifest naming a module the lock file has never heard of fails the very first
// build.
func PushScript(t ticket.Ticket, branch string, staged map[string]string, summary, commitType string, o PushOptions) string {
	return CheckoutScript(branch) +
		MergeIntegrationScript(o.IntegrationBranch) +
		ApplyScript(staged) +
		BestEffortScript(o.DepsCommand, DepsWarning) +
		BestEffortScript(o.FormatCommand, FormatWarning) +
		HookSetupScript +
		CommitAndPushScript(t, staged, summary, commitType, branch)
}

// TreeHash fingerprints the staged tree.
//
// IT REPLACES COMPARING WRITE COUNTERS, which desynchronised nine separate times
// across one day's runs — a no-op counted as a write, a push with nothing to
// commit returning early, a gate rejection skipping the update. Each was a
// different code path forgetting to keep two numbers in step.
//
// A FINGERPRINT CANNOT FORGET: it is derived from the tree rather than
// maintained alongside it, so there is nothing to keep in step. Paths are sorted
// so the same tree always yields the same value.
func TreeHash(staged map[string]string) string {
	h := sha256.New()
	for _, p := range sortedKeys(staged) {
		fmt.Fprintf(h, "%s\x00%s\x00", p, staged[p])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// TreeVerified reports whether the tree as it stands right now is the one the
// last verification ran against.
func (s *State) TreeVerified() bool {
	return s.LastTest != "" && s.VerifiedTree == TreeHash(s.Staged)
}

// ChangedFromBaseline reports whether anything staged actually differs from the
// branch.
//
// BASELINE, NOT READ. Read is deliberately updated with staged content so the
// agent sees its own edits, which makes it useless for this comparison — by the
// time a second write arrives there is nothing left to compare against.
func (s *State) ChangedFromBaseline() bool {
	for p, c := range s.Staged {
		if b, ok := s.Baseline[p]; !ok || b != c {
			return true
		}
	}
	return false
}

package main

// The run's git history: one commit per landed write, a mark per stage and
// per draw, pushed to the project's OWN repository on the control plane as a
// run/<name> branch.
//
// A repository per project, because the operator was right and the shared
// repo was wrong: branches share a clone, so pulling one project's run
// dragged every project's history along. What made per-project repos look
// hard was creation — git daemon cannot create one over the wire — and the
// answer is a one-liner, not a product: the first push to a new project runs
// the operator-configured mkrepo command, which on this plane is a kubectl
// exec of `git init --bare /srv/git/<project>.git`. Idempotent, because git
// init on an existing bare repo reinitialises harmlessly.
//
// EVERY FAILURE HERE IS A WARNING, NEVER A RUN FAILURE. The history is a
// record of the work, not part of it: a run whose only defect is an
// unreachable git server has still built the thing, and failing it would
// throw working code away over bookkeeping.

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// gitLog journals one run. A nil *gitLog is inert, so call sites need no
// guards.
type gitLog struct {
	dir string
	ok  bool
}

// gitEnvURL is the git server's BASE url — git://host:port — under which
// each project is its own <project>.git. Empty means local history only:
// commits still land in the run directory's own .git, nothing pushes.
const gitEnvURL = "AGENTS_WORKSHOP_GIT_URL"

// gitEnvMkrepo is a command template run once per push to ensure the
// project's repository exists, with %s replaced by the project name. On this
// plane: kubectl exec of git init --bare. Empty skips the step, for servers
// that auto-create or repos made by hand.
const gitEnvMkrepo = "AGENTS_WORKSHOP_MKREPO"

// newGitLog opens the run's history and anchors it with the request itself.
func newGitLog(dir, task string) *gitLog {
	g := &gitLog{dir: dir}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("run history disabled", "error", err)
		return g
	}
	if out, err := g.git("init", "-q"); err != nil {
		// No git on the host, most likely. Said once, then silent.
		slog.Warn("run history disabled: git init failed", "error", err, "output", out)
		return g
	}
	g.ok = true
	g.installCommitHook()
	// The request is the root commit, so `git log --reverse` reads as the
	// story of the run: what was asked, then every change made answering it.
	g.mark("request: " + task)
	return g
}

// installCommitHook drops the Conventional-Commits commit-msg hook into the
// run's repository. Best-effort: a repo whose hooks directory cannot be written
// still journals, just without the enforcement — the format is also guaranteed
// on blacksmith's own commits by ccNormalize, and the hook is what catches a
// human who clones and commits.
func (g *gitLog) installCommitHook() {
	hookPath := filepath.Join(g.dir, ".git", "hooks", "commit-msg")
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		return
	}
	if err := os.WriteFile(hookPath, []byte(commitMsgHook), 0o755); err != nil {
		slog.Warn("run history: could not install the commit-msg hook", "error", err)
	}
}

// git runs one command in the run directory, with identity supplied per-call
// so the run needs no global config and inherits nobody's.
func (g *gitLog) git(args ...string) (string, error) {
	full := append([]string{
		"-C", g.dir,
		"-c", "user.name=blacksmith",
		"-c", "user.email=blacksmith@workshop.invalid",
		"-c", "commit.gpgsign=false",
	}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// write records one landed edit: the file syncs to disk and becomes a commit
// under the message the edit itself declared.
func (g *gitLog) write(path, content string, deleted bool, message string) {
	if g == nil || !g.ok {
		return
	}
	full := filepath.Join(g.dir, filepath.FromSlash(path))
	if deleted {
		os.Remove(full)
	} else {
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			slog.Warn("run history: sync failed", "path", path, "error", err)
			return
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			slog.Warn("run history: sync failed", "path", path, "error", err)
			return
		}
	}
	// The edit declared a type and summary; add the scope of the file it touched
	// so the subject reads `fix(store): …` rather than `fix: …`.
	g.commitAll(ccWithScope(message, ccScope(path)))
}

// snapshot commits whatever the directory holds now — the seed tree, or a
// draw's revert — under one message.
func (g *gitLog) snapshot(message string) {
	if g == nil || !g.ok {
		return
	}
	g.commitAll(message)
}

// mark drops an empty commit: a stage boundary, a draw boundary, the verdict.
func (g *gitLog) mark(message string) {
	if g == nil || !g.ok {
		return
	}
	if out, err := g.git("commit", "-q", "--allow-empty", "-m", ccNormalize(message)); err != nil {
		slog.Warn("run history: mark failed", "error", err, "output", out)
	}
}

func (g *gitLog) commitAll(message string) {
	if out, err := g.git("add", "-A"); err != nil {
		slog.Warn("run history: add failed", "error", err, "output", out)
		return
	}
	// An edit that syncs to identical bytes has nothing to commit; that is a
	// no-op, not an error.
	if _, err := g.git("diff", "--cached", "--quiet"); err == nil {
		return
	}
	if out, err := g.git("commit", "-q", "-m", ccNormalize(message)); err != nil {
		slog.Warn("run history: commit failed", "error", err, "output", out)
	}
}

// pushRun sends the run's history to ITS PROJECT's repository as a
// run/<name> branch, creating the repository first through the operator's
// mkrepo command when one is configured. Forced, because a RERUN of the same
// run directory is the same branch's newer truth.
func (g *gitLog) pushRun(baseURL, mkrepo, dir string) {
	if g == nil || !g.ok || baseURL == "" {
		return
	}
	project, run := projectAndRun(dir)
	if mkrepo != "" {
		// The project name is a shell word in someone's command template, so
		// only the slug alphabet may pass — slugOf guarantees [a-z0-9-] and
		// this guard is what makes that a security property rather than a
		// naming convention.
		cmd := fmt.Sprintf(mkrepo, project)
		if out, err := exec.Command("sh", "-c", cmd).CombinedOutput(); err != nil {
			slog.Warn("run history: mkrepo failed — pushing anyway in case the repo exists",
				"project", project, "error", err, "output", strings.TrimSpace(string(out)))
		}
	}
	url := strings.TrimRight(baseURL, "/") + "/" + project + ".git"
	branch := "run/" + run
	if out, err := g.git("push", "-q", "--force", url, "HEAD:refs/heads/"+branch); err != nil {
		slog.Warn("run history: push failed — the run itself is unaffected",
			"url", url, "branch", branch, "error", err, "output", out)
		return
	}
	// THE VERSION TAGS RIDE ALONG. semantic-release reads the history to name a
	// release; pushing the tags is what makes that name visible to a clone and
	// what pins the image's tag to a commit. Forced, because a rerun of the same
	// run moves the tag to the new tip.
	if out, err := g.git("push", "-q", "--force", "--tags", url); err != nil {
		slog.Warn("run history: tag push failed", "url", url, "error", err, "output", out)
	}
	slog.Info("run history pushed", "project", project, "branch", branch)
}

// projectAndRun names a run from its directory: the workspace root is the
// project, the run directory is the run. A bare batch -repo with no workshop
// above it is its own project, with one rolling "run/latest" branch.
func projectAndRun(dir string) (project, run string) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	run = slugOf(filepath.Base(abs))
	project = slugOf(filepath.Base(filepath.Dir(abs)))
	if project == "" || project == "request" {
		return run, "latest"
	}
	return project, run
}

// curGit is the run currently being journalled. A package variable for the
// same reason announce is: one run holds the GPU at a time, and the write
// hook reaches here from inside the tool layer without threading a journal
// through every signature the tests pin.
var curGit *gitLog

func fmtDraw(n int) string { return fmt.Sprintf("reroll: draw %d — fresh tree", n) }

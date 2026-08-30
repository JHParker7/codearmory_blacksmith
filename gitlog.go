package main

// The run's git history: one commit per landed write, a mark per stage and
// per draw, pushed as a NAMESPACED BRANCH of one shared repository on the
// control plane.
//
// One repository with <project>/<run> branches, rather than a repository per
// project, because git daemon cannot create repositories over the wire — a
// repo per project means plane surgery for every new project, which is
// exactly the "reconfigure to use it elsewhere" this exists to remove. The
// plane already carries ~120 tracker-*.git repos from the department era as
// the cautionary listing. Branches are free, clone -b gets any run onto any
// VM, and a new project is nothing but a new prefix.
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

// gitEnvURL names the shared workshop repository. Empty means local history
// only: commits still land in the run directory's own .git, nothing pushes.
const gitEnvURL = "AGENTS_WORKSHOP_GIT_URL"

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
	// The request is the root commit, so `git log --reverse` reads as the
	// story of the run: what was asked, then every change made answering it.
	g.mark("request: " + task)
	return g
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
	g.commitAll(message)
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
	if out, err := g.git("commit", "-q", "--allow-empty", "-m", message); err != nil {
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
	if out, err := g.git("commit", "-q", "-m", message); err != nil {
		slog.Warn("run history: commit failed", "error", err, "output", out)
	}
}

// push sends the run's history to the shared repository as its namespaced
// branch. Forced, because a RERUN of the same run directory is the same
// branch's newer truth, and the local history is always the authority.
func (g *gitLog) push(url, branch string) {
	if g == nil || !g.ok || url == "" {
		return
	}
	if out, err := g.git("push", "-q", "--force", url, "HEAD:refs/heads/"+branch); err != nil {
		slog.Warn("run history: push failed — the run itself is unaffected",
			"url", url, "branch", branch, "error", err, "output", out)
		return
	}
	slog.Info("run history pushed", "branch", branch)
}

// branchFor names a run's branch <project>/<run> from its directory: the
// workspace root is the project, the run directory is the run. A bare batch
// -repo with no parent context is just its own name.
func branchFor(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	run := slugOf(filepath.Base(abs))
	project := slugOf(filepath.Base(filepath.Dir(abs)))
	if project == "" || project == "request" {
		return run
	}
	return project + "/" + run
}

// curGit is the run currently being journalled. A package variable for the
// same reason announce is: one run holds the GPU at a time, and the write
// hook reaches here from inside the tool layer without threading a journal
// through every signature the tests pin.
var curGit *gitLog

func fmtDraw(n int) string { return fmt.Sprintf("reroll: draw %d — fresh tree", n) }

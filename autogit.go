package main

// The git glue auto-mode needs: clone a run branch to work on, and push the
// fix back as its own branch. Kept apart from gitlog.go, which journals a
// forward run; this reaches BACKWARD, pulling a past run's branch to refine
// it.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// gitCmd runs one git command in a directory with the workshop identity, the
// same identity gitlog.go commits under.
func gitCmd(ctx context.Context, dir string, args ...string) (string, error) {
	full := []string{"-C", dir,
		"-c", "user.name=blacksmith",
		"-c", "user.email=blacksmith@workshop.invalid",
		"-c", "commit.gpgsign=false"}
	out, err := exec.CommandContext(ctx, "git", append(full, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// cloneRunBranch clones a project's run branch into a fresh directory under
// base, and returns that directory. The clone is shallow — one branch, no
// history depth — because auto-mode edits the tip and pushes a new branch; it
// never needs the run's full commit story.
func cloneRunBranch(ctx context.Context, base string, repo findingRepo) (string, error) {
	baseURL := os.Getenv(gitEnvURL)
	if baseURL == "" {
		return "", fmt.Errorf("no workshop git url configured")
	}
	url := strings.TrimRight(baseURL, "/") + "/" + repo.project + ".git"
	branch := "run/" + repo.run
	dir := filepath.Join(base, "auto-"+repo.project+"-"+repo.run)
	_ = os.RemoveAll(dir)

	if out, err := gitCmd(ctx, base, "clone", "-q", "--depth", "1", "--branch", branch, url, dir); err != nil {
		return "", fmt.Errorf("clone %s@%s: %s", url, branch, firstLineOf(out))
	}
	return dir, nil
}

// pushFixBranch writes the fixed tree over the clone, commits it, and pushes a
// fix/<finding> branch — a NEW branch, never a force over the run branch,
// because the run's history is a record that must not be rewritten and the
// fix is a proposal a person reviews and merges.
func pushFixBranch(ctx context.Context, files map[string]string, dir string, repo findingRepo, f finding) error {
	if err := writeTree(dir, files); err != nil {
		return err
	}
	if out, err := gitCmd(ctx, dir, "add", "-A"); err != nil {
		return fmt.Errorf("add: %s", out)
	}
	msg := "fix: " + strings.TrimPrefix(strings.TrimPrefix(f.title, "security: "), "quality: ")
	if out, err := gitCmd(ctx, dir, "commit", "-q", "-m", msg); err != nil {
		// Nothing staged means the fix was a no-op edit; not an error worth
		// failing the resolve over, but worth saying.
		if strings.Contains(out, "nothing to commit") {
			return fmt.Errorf("the fix changed no files")
		}
		return fmt.Errorf("commit: %s", out)
	}
	baseURL := os.Getenv(gitEnvURL)
	url := strings.TrimRight(baseURL, "/") + "/" + repo.project + ".git"
	branch := "fix/" + shortID(f.id)
	if out, err := gitCmd(ctx, dir, "push", "-q", "--force", url, "HEAD:refs/heads/"+branch); err != nil {
		return fmt.Errorf("push %s: %s", branch, out)
	}
	return nil
}

// shortID trims a ticket UUID to its first segment, enough to name a branch
// without the whole thing.
func shortID(id string) string {
	if i := strings.IndexByte(id, '-'); i > 0 {
		return id[:i]
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// autoIdleDelay is how long auto-mode waits after draining the board before
// looking again — long, because findings arrive only when a request runs, and
// a tight poll against the board is noise.
const autoIdleDelay = 60 * time.Second

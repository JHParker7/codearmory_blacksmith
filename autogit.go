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

// replaceTree makes the working tree exactly `files`: it removes tracked files
// the fix dropped, then writes what it kept. writeTree alone only adds and
// overwrites, so a deletion would silently survive and the review diff would
// lie about what the fix does.
func replaceTree(dir string, files map[string]string) error {
	entries, err := gitLsFiles(dir)
	if err != nil {
		return err
	}
	for _, rel := range entries {
		if _, keep := files[rel]; !keep {
			_ = os.Remove(filepath.Join(dir, filepath.FromSlash(rel)))
		}
	}
	return writeTree(dir, files)
}

func gitLsFiles(dir string) ([]string, error) {
	out, err := exec.Command("git", "-C", dir, "ls-files").Output()
	if err != nil {
		return nil, err
	}
	var files []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" {
			files = append(files, l)
		}
	}
	return files, nil
}

// pushBranch force-pushes the working clone's current HEAD to a named branch on
// origin. Auto-mode accumulates a project's approved fixes as commits in one
// clone and pushes them as a single batch branch, which mergeBranchToDev then
// brings onto dev.
func pushBranch(ctx context.Context, dir, branch string) error {
	baseURL := os.Getenv(gitEnvURL)
	if baseURL == "" {
		return fmt.Errorf("no workshop git url configured")
	}
	if out, err := gitCmd(ctx, dir, "push", "-q", "--force", "origin", "HEAD:refs/heads/"+branch); err != nil {
		return fmt.Errorf("push %s: %s", branch, out)
	}
	return nil
}

// stagedDiff stages the working tree and returns its diff against HEAD — the
// delta ONE finding's fix makes on top of the fixes already committed in this
// clone. Reviewing that delta, not the whole accumulated tree, keeps each
// finding's review about its own change.
func stagedDiff(ctx context.Context, dir string) (string, error) {
	if out, err := gitCmd(ctx, dir, "add", "-A"); err != nil {
		return "", fmt.Errorf("add: %s", out)
	}
	out, err := gitCmd(ctx, dir, "diff", "--cached", "HEAD")
	if err != nil {
		return "", fmt.Errorf("diff: %s", firstLineOf(out))
	}
	return out, nil
}

// restoreToHead throws away an uncommitted, unapproved fix so the next finding
// starts from the clone's committed state — the run tip plus the fixes that
// WERE approved, and nothing a reviewer rejected.
func restoreToHead(ctx context.Context, dir string) {
	_, _ = gitCmd(ctx, dir, "reset", "-q", "--hard", "HEAD")
	_, _ = gitCmd(ctx, dir, "clean", "-qfd")
}

// mergeBranchToDev merges a pushed branch into the project's dev branch and
// pushes dev — the review's approval made real. A CLIENT-SIDE MERGE, because
// the plane's git server is a bare git-daemon with no server-side merge;
// blacksmith clones, merges, and pushes dev back.
//
// The first merge CREATES dev at the branch tip (the branch already carries the
// run's whole tree plus the fixes). Later ones merge onto it. A merge that
// conflicts is aborted and reported — two changes touching one file are a
// human's call, not a machine's — and the branch stays put.
//
// This runs ONLY after a project's above-low findings are all fixed: the batch
// branch is the accumulated set of approved fixes, so dev never sees a partial
// project where a critical hole is still open.
func mergeBranchToDev(ctx context.Context, base string, repo findingRepo, branch, msg string) (string, error) {
	baseURL := os.Getenv(gitEnvURL)
	if baseURL == "" {
		return "", fmt.Errorf("no workshop git url configured")
	}
	url := strings.TrimRight(baseURL, "/") + "/" + repo.project + ".git"

	dir := filepath.Join(base, "merge-"+repo.project+"-"+repo.run)
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if out, err := gitCmd(ctx, dir, "init", "-q"); err != nil {
		return "", fmt.Errorf("init: %s", out)
	}
	if out, err := gitCmd(ctx, dir, "remote", "add", "origin", url); err != nil {
		return "", fmt.Errorf("remote: %s", out)
	}
	if out, err := gitCmd(ctx, dir, "fetch", "-q", "origin", branch); err != nil {
		return "", fmt.Errorf("fetch %s: %s", branch, out)
	}
	branchSHA, err := gitCmd(ctx, dir, "rev-parse", "FETCH_HEAD")
	if err != nil {
		return "", fmt.Errorf("rev-parse: %s", branchSHA)
	}

	// Does dev already exist on the server?
	devExists := false
	if out, _ := gitCmd(ctx, dir, "ls-remote", "--heads", "origin", "dev"); strings.Contains(out, "refs/heads/dev") {
		devExists = true
	}
	if devExists {
		if out, err := gitCmd(ctx, dir, "fetch", "-q", "origin", "dev"); err != nil {
			return "", fmt.Errorf("fetch dev: %s", out)
		}
		if out, err := gitCmd(ctx, dir, "checkout", "-q", "-B", "dev", "origin/dev"); err != nil {
			return "", fmt.Errorf("checkout dev: %s", out)
		}
		if out, err := gitCmd(ctx, dir, "merge", "--no-ff", "-m", msg, branchSHA); err != nil {
			_, _ = gitCmd(ctx, dir, "merge", "--abort")
			return "", fmt.Errorf("the fixes conflict with dev and need a person: %s", firstLineOf(out))
		}
	} else {
		// dev starts at the batch tip.
		if out, err := gitCmd(ctx, dir, "checkout", "-q", "-B", "dev", branchSHA); err != nil {
			return "", fmt.Errorf("create dev: %s", out)
		}
	}
	if out, err := gitCmd(ctx, dir, "push", "-q", "origin", "HEAD:refs/heads/dev"); err != nil {
		return "", fmt.Errorf("push dev: %s", out)
	}
	return branch + " → dev", nil
}

// autoIdleDelay is how long auto-mode waits after draining the board before
// looking again — long, because findings arrive only when a request runs, and
// a tight poll against the board is noise.
const autoIdleDelay = 60 * time.Second

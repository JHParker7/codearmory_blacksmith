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
	// RESET TO THE RUN TIP FIRST, so each push is ONE commit of the whole fix
	// against the base — not a delta on the previous attempt. The review loop
	// re-pushes a revised fix each cycle, and the reviewer must see the entire
	// change every time, not just what the latest revision moved.
	if out, err := gitCmd(ctx, dir, "reset", "-q", "--hard", "origin/run/"+repo.run); err != nil {
		return fmt.Errorf("reset to run tip: %s", out)
	}
	if err := replaceTree(dir, files); err != nil {
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

// diffOfHead returns the diff of the fix commit — the tip against its parent
// — for the reviewer to read. A clone of the fix branch has the fix as HEAD,
// so HEAD~1..HEAD is exactly the change under review.
func diffOfHead(ctx context.Context, dir string) (string, error) {
	out, err := gitCmd(ctx, dir, "diff", "HEAD~1", "HEAD")
	if err != nil {
		return "", fmt.Errorf("diff: %s", firstLineOf(out))
	}
	return out, nil
}

// mergeFixToDev merges a pushed fix branch into the project's dev branch and
// pushes dev — the review agent's approval made real. A CLIENT-SIDE MERGE,
// because the plane's git server is a bare git-daemon with no server-side
// merge; blacksmith clones, merges, and pushes dev back.
//
// The first approved fix CREATES dev at the fix tip (the fix branch already
// carries the run's whole tree plus the fix). Later ones merge onto it. A
// merge that conflicts is aborted and reported — two fixes touching one file
// are a human's call, not a machine's — and the fix stays on its branch.
func mergeFixToDev(ctx context.Context, base string, repo findingRepo, f finding) (string, error) {
	baseURL := os.Getenv(gitEnvURL)
	if baseURL == "" {
		return "", fmt.Errorf("no workshop git url configured")
	}
	url := strings.TrimRight(baseURL, "/") + "/" + repo.project + ".git"
	fixBranch := "fix/" + shortID(f.id)

	dir := filepath.Join(base, "merge-"+repo.project+"-"+shortID(f.id))
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
	if out, err := gitCmd(ctx, dir, "fetch", "-q", "origin", fixBranch); err != nil {
		return "", fmt.Errorf("fetch %s: %s", fixBranch, out)
	}
	fixSHA, err := gitCmd(ctx, dir, "rev-parse", "FETCH_HEAD")
	if err != nil {
		return "", fmt.Errorf("rev-parse: %s", fixSHA)
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
		msg := "merge: " + strings.TrimPrefix(strings.TrimPrefix(f.title, "security: "), "quality: ")
		if out, err := gitCmd(ctx, dir, "merge", "--no-ff", "-m", msg, fixSHA); err != nil {
			_, _ = gitCmd(ctx, dir, "merge", "--abort")
			return "", fmt.Errorf("the fix conflicts with dev and needs a person: %s", firstLineOf(out))
		}
	} else {
		// dev starts at the fix tip.
		if out, err := gitCmd(ctx, dir, "checkout", "-q", "-B", "dev", fixSHA); err != nil {
			return "", fmt.Errorf("create dev: %s", out)
		}
	}
	if out, err := gitCmd(ctx, dir, "push", "-q", "origin", "HEAD:refs/heads/dev"); err != nil {
		return "", fmt.Errorf("push dev: %s", out)
	}
	return fixBranch + " → dev", nil
}

// autoIdleDelay is how long auto-mode waits after draining the board before
// looking again — long, because findings arrive only when a request runs, and
// a tight poll against the board is noise.
const autoIdleDelay = 60 * time.Second

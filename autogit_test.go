package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// cloneRunBranch must return a path that ACTUALLY EXISTS, even when base is
// relative. It once returned base/auto-… while the clone (run with `-C base`)
// landed at base/base/auto-…, so every readTree afterward failed and auto-mode
// abandoned every finding before a fix. This drives the exact relative-base
// case that broke live.
func TestCloneRunBranchReturnsAReadableTree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()
	root := t.TempDir()

	// A bare "remote" holding project proj.git with branch run/r carrying one file.
	remote := filepath.Join(root, "remote")
	bare := filepath.Join(remote, "proj.git")
	mustGit(t, root, "init", "-q", "--bare", bare)
	work := filepath.Join(root, "seed")
	mustGit(t, root, "clone", "-q", bare, work)
	if err := os.WriteFile(filepath.Join(work, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, work, "checkout", "-q", "-b", "run/r")
	mustGit(t, work, "-c", "user.name=t", "-c", "user.email=t@t", "add", "-A")
	mustGit(t, work, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "seed")
	mustGit(t, work, "push", "-q", "origin", "run/r")

	t.Setenv(gitEnvURL, "file://"+remote)

	// Run with a RELATIVE base from a fresh working directory, exactly as
	// `blacksmith -auto` does when -repo is unset (base defaults to "workshop").
	cwd := filepath.Join(root, "cwd")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	old, _ := os.Getwd()
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)
	if err := os.MkdirAll("workshop", 0o755); err != nil {
		t.Fatal(err)
	}

	dir, err := cloneRunBranch(ctx, "workshop", findingRepo{project: "proj", run: "r"})
	if err != nil {
		t.Fatalf("cloneRunBranch: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("returned dir does not exist: %v", err)
	}
	tree, err := readTree(dir)
	if err != nil {
		t.Fatalf("readTree on the returned dir failed — the exact live bug: %v", err)
	}
	if _, ok := tree["main.go"]; !ok {
		t.Fatalf("the cloned tree is missing main.go; got %d files", len(tree))
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

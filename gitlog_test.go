package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitOut runs git in a directory and fails the test on error.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// THE COMMIT STREAM FINALLY DRAINS INTO A REPOSITORY. Every write already
// declared a type and a summary; now each landed write is a commit under that
// message, anchored by the request and closed by the verdict, so
// `git log --reverse` reads as the story of the run.
func TestEveryWriteIsACommitUnderItsOwnMessage(t *testing.T) {
	dir := t.TempDir()
	g := newGitLog(dir, "build a task api")
	if !g.ok {
		t.Skip("no git on this host")
	}

	g.write("store.go", "package main\n", false, "feat: add the store")
	g.write("store.go", "package main\n\nfunc New() {}\n", false, "feat: add the constructor")
	g.mark("stage(plan-dev): passed")
	g.write("store.go", "package main\n", true, "revert: undo the last write to store.go")

	log := gitOut(t, dir, "log", "--reverse", "--format=%s")
	want := []string{
		"request: build a task api",
		"feat: add the store",
		"feat: add the constructor",
		"stage(plan-dev): passed",
		"revert: undo the last write to store.go",
	}
	got := strings.Split(log, "\n")
	if len(got) != len(want) {
		t.Fatalf("history is %d commits, want %d:\n%s", len(got), len(want), log)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("commit %d = %q, want %q", i, got[i], want[i])
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "store.go")); !os.IsNotExist(err) {
		t.Error("the undone file survived on disk")
	}
}

// The push lands the run as its NAMESPACED branch of the shared repository —
// one repo, <project>/<run> branches, so a new project is a new prefix and
// never a new repository on the plane.
func TestTheRunPushesAsItsNamespacedBranch(t *testing.T) {
	remote := t.TempDir()
	gitOut(t, remote, "init", "--bare", "-q")

	work := filepath.Join(t.TempDir(), "myproject", "003-build-api")
	g := newGitLog(work, "build it")
	if !g.ok {
		t.Skip("no git on this host")
	}
	g.write("main.go", "package main\n", false, "feat: the entry point")
	g.mark("run: passed")
	g.push(remote, branchFor(work))

	refs := gitOut(t, remote, "for-each-ref", "--format=%(refname:short)")
	if refs != "myproject/003-build-api" {
		t.Fatalf("the branch is %q, want myproject/003-build-api", refs)
	}
}

// A rerun of the same run directory is the same branch's newer truth: the
// forced push replaces it rather than failing on non-fast-forward.
func TestARerunReplacesItsOwnBranch(t *testing.T) {
	remote := t.TempDir()
	gitOut(t, remote, "init", "--bare", "-q")

	work := filepath.Join(t.TempDir(), "proj", "001-x")
	g := newGitLog(work, "first")
	if !g.ok {
		t.Skip("no git on this host")
	}
	g.write("a.md", "one\n", false, "docs: one")
	g.push(remote, branchFor(work))

	os.RemoveAll(work)
	g2 := newGitLog(work, "second")
	g2.write("a.md", "two\n", false, "docs: two")
	g2.push(remote, branchFor(work))

	subjects := gitOut(t, remote, "log", "proj/001-x", "--format=%s")
	if !strings.Contains(subjects, "request: second") || strings.Contains(subjects, "request: first") {
		t.Fatalf("the rerun did not replace the branch:\n%s", subjects)
	}
}

// A nil journal and a journal with no git are both inert: history is a record
// of the work, never a reason the work fails.
func TestAMissingJournalNeverPanics(t *testing.T) {
	var g *gitLog
	g.write("a.go", "x", false, "feat: x")
	g.mark("m")
	g.snapshot("s")
	g.push("url", "b")

	dead := &gitLog{dir: "/nonexistent/nowhere"}
	dead.write("a.go", "x", false, "feat: x")
	dead.mark("m")
}

// The project reader hands a run the PROJECT, not the workshop's bookkeeping:
// earlier runs' directories and the TUI log stay out of the seed.
func TestTheSeedSkipsRunDirectoriesAndTheLog(t *testing.T) {
	base := t.TempDir()
	files := map[string]string{
		"go.mod":           "module myproject\n",
		"store.go":         "package main\n",
		"docs/design.md":   "# design\n",
		"001-old-run/x.go": "package old\n",
		"002-another/y.go": "package old\n",
		"tui.log":          "noise\n",
	}
	if err := writeTree(base, files); err != nil {
		t.Fatal(err)
	}

	seed, err := readProjectTree(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"go.mod", "store.go", "docs/design.md"} {
		if _, ok := seed[want]; !ok {
			t.Errorf("the project file %s is missing from the seed", want)
		}
	}
	for p := range seed {
		if strings.HasPrefix(p, "001-") || strings.HasPrefix(p, "002-") || p == "tui.log" {
			t.Errorf("workshop bookkeeping leaked into the seed: %s", p)
		}
	}
}

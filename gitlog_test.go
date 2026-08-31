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
	// Each edit's type keeps its meaning and gains the scope of the file it
	// touched; the journal's own marks are normalised to a chore so the whole
	// history is Conventional Commits and machine-readable for versioning.
	want := []string{
		"chore: request: build a task api",
		"feat(store): add the store",
		"feat(store): add the constructor",
		"chore: stage(plan-dev): passed",
		"revert(store): undo the last write to store.go",
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
	// EVERY subject must satisfy the format the installed hook enforces —
	// otherwise blacksmith's own commits would trip its own hook.
	for _, s := range got {
		if !ccSubject.MatchString(s) {
			t.Errorf("subject is not Conventional Commits: %q", s)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "store.go")); !os.IsNotExist(err) {
		t.Error("the undone file survived on disk")
	}
}

// The commit-msg hook blacksmith installs actually rejects a non-conforming
// message and passes a conforming one — the enforcement a human cloning the
// project hits, and the backstop on blacksmith's own commits.
func TestTheInstalledCommitHookEnforcesTheFormat(t *testing.T) {
	dir := t.TempDir()
	g := newGitLog(dir, "build a task api")
	if !g.ok {
		t.Skip("no git on this host")
	}
	hook := filepath.Join(dir, ".git", "hooks", "commit-msg")
	if info, err := os.Stat(hook); err != nil || info.Mode()&0o100 == 0 {
		t.Fatalf("commit-msg hook not installed executable: %v", err)
	}
	// A commit through the hook with a bad message must fail; a good one passes.
	if out, err := g.git("commit", "--allow-empty", "-m", "just some words"); err == nil {
		t.Errorf("the hook accepted a non-conventional subject:\n%s", out)
	}
	if out, err := g.git("commit", "--allow-empty", "-m", "feat(x): a real change"); err != nil {
		t.Errorf("the hook rejected a valid subject: %v\n%s", err, out)
	}
}

// A releasable history is tagged with the version its commits imply, and the
// tag lands on the run tip.
func TestTagReleaseVersionsFromHistory(t *testing.T) {
	dir := t.TempDir()
	g := newGitLog(dir, "build a task api")
	if !g.ok {
		t.Skip("no git on this host")
	}
	g.write("store.go", "package main\n", false, "feat: add the store")
	if v := g.tagRelease(); v != "v1.0.0" {
		t.Fatalf("first releasable history tagged %q, want v1.0.0", v)
	}
	// A fix on top bumps the patch.
	g.write("store.go", "package main\n// fixed\n", false, "fix: correct the store")
	if v := g.tagRelease(); v != "v1.0.1" {
		t.Fatalf("a fix bumped to %q, want v1.0.1", v)
	}
	if got := gitOut(t, dir, "tag", "--list"); !strings.Contains(got, "v1.0.0") || !strings.Contains(got, "v1.0.1") {
		t.Errorf("tags missing: %q", got)
	}
}

// EACH PROJECT IS ITS OWN REPOSITORY — the operator's correction of the
// shared-repo design, whose branches dragged every project's history into
// every clone. The first push creates the repo through the operator's mkrepo
// one-liner (here: a plain git init --bare, exactly the "mkdir x; git init"
// the operator described) and lands the run as a run/<name> branch.
func TestTheRunPushesToItsProjectsOwnRepository(t *testing.T) {
	server := t.TempDir() // stands in for /srv/git on the plane

	work := filepath.Join(t.TempDir(), "myproject", "003-build-api")
	g := newGitLog(work, "build it")
	if !g.ok {
		t.Skip("no git on this host")
	}
	g.write("main.go", "package main\n", false, "feat: the entry point")
	g.mark("run: passed")
	g.pushRun(server, "git init --bare -q "+server+"/%s.git", work)

	refs := gitOut(t, filepath.Join(server, "myproject.git"),
		"for-each-ref", "--format=%(refname:short)")
	if refs != "run/003-build-api" {
		t.Fatalf("the branch is %q, want run/003-build-api", refs)
	}
	// And a second project is a second repository, not a neighbouring branch.
	work2 := filepath.Join(t.TempDir(), "otherproj", "001-thing")
	g2 := newGitLog(work2, "other")
	g2.write("a.md", "x\n", false, "docs: x")
	g2.pushRun(server, "git init --bare -q "+server+"/%s.git", work2)
	if _, err := os.Stat(filepath.Join(server, "otherproj.git")); err != nil {
		t.Fatalf("the second project did not get its own repository: %v", err)
	}
	refs = gitOut(t, filepath.Join(server, "myproject.git"),
		"for-each-ref", "--format=%(refname:short)")
	if strings.Contains(refs, "otherproj") {
		t.Fatalf("the second project leaked into the first repository: %q", refs)
	}
}

// A rerun of the same run directory is the same branch's newer truth: the
// forced push replaces it rather than failing on non-fast-forward.
func TestARerunReplacesItsOwnBranch(t *testing.T) {
	base := t.TempDir()
	remote := filepath.Join(base, "proj.git")
	if err := os.MkdirAll(remote, 0o755); err != nil {
		t.Fatal(err)
	}
	gitOut(t, remote, "init", "--bare", "-q")

	work := filepath.Join(t.TempDir(), "proj", "001-x")
	g := newGitLog(work, "first")
	if !g.ok {
		t.Skip("no git on this host")
	}
	g.write("a.md", "one\n", false, "docs: one")
	g.pushRun(filepath.Dir(remote), "", work)

	os.RemoveAll(work)
	g2 := newGitLog(work, "second")
	g2.write("a.md", "two\n", false, "docs: two")
	g2.pushRun(filepath.Dir(remote), "", work)

	subjects := gitOut(t, remote, "log", "run/001-x", "--format=%s")
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
	g.pushRun("url", "", "/x/y")

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

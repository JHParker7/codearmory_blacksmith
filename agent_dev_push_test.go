package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// EVERY FILE THE AGENT WROTE MUST REACH THE BRANCH.
//
// The per-file split commits each written file on its own. When it takes all of
// them there is nothing left for the final commit, which then fails with
// "nothing to commit" — and under the sandbox preamble's `set -e` that aborts the
// script BEFORE the push. The work is committed inside the sandbox and thrown
// away with it.
//
// Measured on r110. The developer wrote ticket.go, store.go, main.go and
// handlers.go, all four correct, and the branch ended up holding only ticket.go:
// the single write made while one staged file meant the split did not run. For
// twenty turns afterwards the agent was shown its own correct main.go and told by
// the gate that func main was undeclared, because the gate builds the BRANCH.
//
// This runs the generated script against a real repository with a real remote,
// because the failure was in shell control flow and no amount of reading the
// string caught it.
func TestEveryWrittenFileReachesTheBranch(t *testing.T) {
	integrationTest(t)
	for _, bin := range []string{"sh", "git"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("no %s", bin)
		}
	}

	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	work := filepath.Join(root, "work")

	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	git(root, "init", "--bare", "-q", remote)
	git(root, "init", "-q", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "go.mod"), []byte("module tracker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(work, "add", "-A")
	git(work, "commit", "-q", "-m", "chore: base")
	git(work, "remote", "add", "origin", remote)
	git(work, "push", "-q", "origin", "main")

	// Four files, as the developer wrote four. More than one, so the split runs.
	written := map[string]string{
		"ticket.go":   "package main\n\ntype Ticket struct{}\n",
		"store.go":    "package main\n\ntype Store struct{}\n",
		"main.go":     "package main\n\nfunc main() {}\n",
		"handlers.go": "package main\n\nfunc handlers() {}\n",
	}
	for path, body := range written {
		if err := os.WriteFile(filepath.Join(work, path), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	a := &DevAgent{}
	s := &devState{summary: "add the ticketing system", commitType: "feat", staged: written}
	// set -e, exactly as the sandbox runs it — that is the half that aborted.
	script := "set -e\nexport COMMIT_MSG_FILE=" + filepath.Join(root, ".commit-msg") + "\n" +
		a.commitAndPushScript(Ticket{TicketID: "t-1", Title: "x"}, s, "main")

	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = work
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the commit-and-push script failed: %v\n%s", err, out)
	}

	// THE REMOTE IS THE AUTHORITY. The gate builds what was pushed, not what the
	// sandbox happens to be holding.
	listed := exec.Command("git", "--git-dir="+remote, "ls-tree", "--name-only", "-r", "main")
	out, err := listed.CombinedOutput()
	if err != nil {
		t.Fatalf("ls-tree: %v\n%s", err, out)
	}
	got := string(out)
	for path := range written {
		if !strings.Contains(got, path) {
			t.Errorf("%s never reached the branch; the gate would build without it.\non the branch:\n%s",
				path, got)
		}
	}
}

// One file is not split, and must still land.
func TestASingleWrittenFileReachesTheBranch(t *testing.T) {
	integrationTest(t)
	for _, bin := range []string{"sh", "git"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("no %s", bin)
		}
	}

	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	work := filepath.Join(root, "work")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(root, "init", "--bare", "-q", remote)
	git(root, "init", "-q", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "go.mod"), []byte("module tracker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(work, "add", "-A")
	git(work, "commit", "-q", "-m", "chore: base")
	git(work, "remote", "add", "origin", remote)
	git(work, "push", "-q", "origin", "main")

	if err := os.WriteFile(filepath.Join(work, "only.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &DevAgent{}
	s := &devState{summary: "add one file", commitType: "feat", staged: map[string]string{"only.go": "package main\n"}}
	script := "set -e\nexport COMMIT_MSG_FILE=" + filepath.Join(root, ".commit-msg") + "\n" +
		a.commitAndPushScript(Ticket{TicketID: "t-1"}, s, "main")
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = work
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	out, _ := exec.Command("git", "--git-dir="+remote, "ls-tree", "--name-only", "-r", "main").CombinedOutput()
	if !strings.Contains(string(out), "only.go") {
		t.Errorf("the single written file never reached the branch:\n%s", out)
	}
}

package dev

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/forge"
	"github.com/code-armory-app/blacksmith/internal/ticket"
)

func needShell(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not available")
	}
}

func needGit(t *testing.T) {
	t.Helper()
	needShell(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available")
	}
}

// runScript executes a script under the preamble's `set -e` in a temporary
// directory, which is what the sandbox does.
func runScript(t *testing.T, dir, script string) (string, error) {
	t.Helper()
	return runScriptWithMsgFile(t, dir, filepath.Join(t.TempDir(), ".commit-msg"), script)
}

func runScriptWithMsgFile(t *testing.T, dir, msgFile, script string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", "-c", "set -e\n"+script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "COMMIT_MSG_FILE="+msgFile)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// EVERY SPLICED COMMAND MUST BE VALID SHELL, whatever a person put in the
// configuration or a model put in a path.
func TestThePushScriptIsValidShellForAwkwardInput(t *testing.T) {
	needShell(t)

	staged := map[string]string{
		// Go source is almost all quotes once a struct tag appears.
		"store.go": "package main\n\ntype Task struct {\n\tID string `json:\"id\"`\n}\n",
		// A path with an apostrophe in it. It cost a real outage elsewhere.
		"it's a file.go": "package main\n",
	}
	// Prose with an apostrophe, which is what closed a quote early before.
	summary := "add the project's own store — with a filter"

	for _, o := range []PushOptions{
		{},
		{IntegrationBranch: "integration", FormatCommand: "gofmt -w .", DepsCommand: "go mod tidy"},
		// A command that fails is the NORMAL case for a best-effort step.
		{FormatCommand: "false", DepsCommand: "(exit 3)"},
		{FormatCommand: "echo working; false"},
	} {
		script := PushScript(job2(), "agent/t-42", staged, summary, "feat", o)
		if out, err := exec.Command("sh", "-n", "-c", "set -e\n"+script).CombinedOutput(); err != nil {
			t.Errorf("%+v: not valid shell: %v\n%s", o, err, out)
		}
	}
}

// A BEST-EFFORT STEP MUST NOT BE ABLE TO FAIL THE PUSH, and under `set -e` a
// bare `cmd || echo` only protects the last command an operator wrote.
func TestAFailingFormatterDoesNotStopTheScript(t *testing.T) {
	needShell(t)

	// Commands whose line FAILS overall: the push continues and the failure is
	// reported.
	failing := []string{
		"false",
		"(exit 3)",
		"echo one; false",
		"echo one; echo two; false",
		"false && echo unreachable",
	}
	for _, cmd := range failing {
		out, err := runScript(t, t.TempDir(), BestEffortScript(cmd, FormatWarning)+"echo REACHED\n")
		if err != nil {
			t.Errorf("%q aborted the script: %v\n%s", cmd, err, out)
		}
		if !strings.Contains(out, "REACHED") {
			t.Errorf("%q stopped the script before the commit:\n%s", cmd, out)
		}
		// LOUDLY best-effort: silently skipping a tool the project defines would
		// let the agent commit to a standard its contributors are held to.
		if !strings.Contains(out, "WARNING") {
			t.Errorf("%q failed silently:\n%s", cmd, out)
		}
	}

	// AND THE CASE THAT NEEDS THE BRACES. An early failure inside the operator's
	// line is what `set -e` kills: without the group, `false; echo two || echo W`
	// leaves `false` unguarded and the script dies before the commit. The line
	// SUCCEEDS overall, so there is nothing to warn about — only something to
	// survive.
	for _, cmd := range []string{"false; echo two", "false; true"} {
		out, err := runScript(t, t.TempDir(), BestEffortScript(cmd, FormatWarning)+"echo REACHED\n")
		if err != nil {
			t.Errorf("%q aborted the script: %v\n%s", cmd, err, out)
		}
		if !strings.Contains(out, "REACHED") {
			t.Errorf("%q stopped the script before the commit:\n%s", cmd, out)
		}
	}

	// An unset command is a no-op: an operator who named none asked for none.
	if got := BestEffortScript("", FormatWarning); got != "" {
		t.Errorf("an unset command produced %q", got)
	}
}

// THE PAYLOAD IS GO SOURCE, and a struct tag is `json:"id"` — so a type
// definition is almost all quotes. Asking a shell to carry that literally is a
// quoting bug on every ticket that defines a type.
func TestTheStagedTreeIsWrittenThroughBase64(t *testing.T) {
	needShell(t)

	content := "package main\n\ntype Task struct {\n\tID string `json:\"id\"`\n}\n\n" +
		"// $(rm -rf /) && `whoami` 'quoted'\n"
	dir := t.TempDir()

	script := ApplyScript(map[string]string{"a/b/store.go": content})
	if strings.Contains(script, "rm -rf /") {
		t.Error("model-authored content reached the script as text")
	}
	if out, err := runScript(t, dir, script); err != nil {
		t.Fatalf("the apply script failed: %v\n%s", err, out)
	}

	got, err := os.ReadFile(filepath.Join(dir, "a/b/store.go"))
	if err != nil {
		t.Fatalf("the file was not written: %v", err)
	}
	if string(got) != content {
		t.Errorf("the content did not survive the round trip:\n%q", got)
	}
}

// A COMMIT MESSAGE IS PROSE, and prose contains apostrophes and newlines. Both
// end a shell word early.
func TestTheCommitMessageIsWrittenThroughBase64(t *testing.T) {
	needShell(t)

	msg := "feat(store): add the project's own store\n\nTicket: t-42"
	dir := t.TempDir()
	msgFile := filepath.Join(t.TempDir(), ".commit-msg")
	if out, err := runScriptWithMsgFile(t, dir, msgFile, WriteMessageScript(msg)); err != nil {
		t.Fatalf("the message script failed: %v\n%s", err, out)
	}
	got, err := os.ReadFile(msgFile)
	if err != nil {
		t.Fatalf("the message was not written: %v", err)
	}
	if string(got) != msg {
		t.Errorf("the message did not survive:\n%q", got)
	}
}

// initRepo makes a real repository with a real origin, so the push path is
// exercised rather than asserted on.
func initRepo(t *testing.T) (work, origin string) {
	t.Helper()
	needGit(t)

	origin = t.TempDir()
	work = t.TempDir()
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(origin, "init", "--bare", "-q", "-b", "main")
	git(work, "init", "-q", "-b", "main")
	git(work, "remote", "add", "origin", origin)
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("# x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(work, "add", "-A")
	git(work, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "chore: init")
	git(work, "push", "-q", "origin", "main")
	return work, origin
}

// THE LAST COMMIT IS GUARDED, AND LEAVING IT UNGUARDED DESTROYED A RUN.
//
// The per-file split commits each written file on its own; when it has taken all
// of them there is nothing left for the catch-all, `git commit` fails with
// "nothing to commit", the retry fails the same way, and `set -e` aborts the
// script BEFORE the push. The work is committed inside the sandbox and thrown
// away with it.
//
// r110: the developer wrote ticket.go, store.go, main.go and handlers.go, all
// correct, and the branch ended up holding only ticket.go. Every turn after that
// it was shown its own correct main.go and told by the gate that func main was
// undeclared, because the gate builds the BRANCH. Twenty turns of an agent being
// right and being told it was wrong.
func TestEverythingWrittenReachesTheBranch(t *testing.T) {
	work, origin := initRepo(t)

	staged := map[string]string{
		"ticket.go":   "package main\n\ntype Ticket struct{}\n",
		"store.go":    "package main\n\ntype Store struct{}\n",
		"main.go":     "package main\n\nfunc main() {}\n",
		"handlers.go": "package main\n\nfunc handle() {}\n",
	}
	script := CheckoutScript("agent/t-42") + ApplyScript(staged) +
		CommitAndPushScript(job2(), staged, "add the store", "feat", "agent/t-42")

	if out, err := runScript(t, work, script); err != nil {
		t.Fatalf("the push failed: %v\n%s", err, out)
	}

	// Read the branch back out of the ORIGIN, which is what the gate builds.
	cmd := exec.Command("git", "ls-tree", "-r", "--name-only", "agent/t-42")
	cmd.Dir = origin
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the branch was not pushed: %v\n%s", err, out)
	}
	for p := range staged {
		if !strings.Contains(string(out), p) {
			t.Errorf("%s never reached the branch; the gate would call it undeclared:\n%s", p, out)
		}
	}
}

// AND THE PUSH RUNS EITHER WAY: a change the split committed in full leaves the
// catch-all with nothing, which is a no-op rather than a failure.
func TestAChangeFullyTakenByTheSplitStillPushes(t *testing.T) {
	work, origin := initRepo(t)

	staged := map[string]string{
		"a.go": "package main\n\nfunc a() {}\n",
		"b.go": "package main\n\nfunc b() {}\n",
	}
	out, err := runScript(t, work, CheckoutScript("agent/t-42")+ApplyScript(staged)+
		CommitAndPushScript(job2(), staged, "add a and b", "feat", "agent/t-42"))
	if err != nil {
		t.Fatalf("a fully-split change failed to push: %v\n%s", err, out)
	}

	cmd := exec.Command("git", "log", "--oneline", "agent/t-42")
	cmd.Dir = origin
	log, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the branch was not pushed: %v\n%s", err, log)
	}
	// ONE COMMIT PER FILE, each subject short enough to be true.
	for _, want := range []string{"feat(a):", "feat(b):"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("the history does not carry %q:\n%s", want, log)
		}
	}
}

// A SINGLE-FILE CHANGE IS NOT SPLIT, and must still commit and push.
func TestASingleFileChangePushes(t *testing.T) {
	work, origin := initRepo(t)

	staged := map[string]string{"only.go": "package main\n\nfunc only() {}\n"}
	out, err := runScript(t, work, CheckoutScript("agent/t-42")+ApplyScript(staged)+
		CommitAndPushScript(job2(), staged, "add only", "feat", "agent/t-42"))
	if err != nil {
		t.Fatalf("a single-file change failed to push: %v\n%s", err, out)
	}

	cmd := exec.Command("git", "ls-tree", "-r", "--name-only", "agent/t-42")
	cmd.Dir = origin
	tree, _ := cmd.CombinedOutput()
	if !strings.Contains(string(tree), "only.go") {
		t.Errorf("the file never reached the branch:\n%s", tree)
	}

	// AND IT IS ONE COMMIT WITHOUT A SCOPE. Splitting a single-file change adds a
	// per-file commit that says the same thing as the catch-all it precedes.
	log := exec.Command("git", "log", "--oneline", "agent/t-42")
	log.Dir = origin
	history, _ := log.CombinedOutput()
	if strings.Contains(string(history), "feat(only):") {
		t.Errorf("a single-file change was split into per-file commits:\n%s", history)
	}
	if !strings.Contains(string(history), "feat: add only") {
		t.Errorf("the catch-all commit is missing:\n%s", history)
	}
}

// THE HOOK HOLDS FOR EVERY COMMIT MADE IN THE SANDBOX HOWEVER IT WAS MADE — the
// delegate engine commits for itself, and a rule enforced on one path stops
// being true the moment a second path appears.
func TestTheConventionalCommitHookRejectsWhatTheLinterWould(t *testing.T) {
	work, _ := initRepo(t)

	if out, err := runScript(t, work, HookSetupScript); err != nil {
		t.Fatalf("installing the hook failed: %v\n%s", err, out)
	}
	hook := filepath.Join(work, ".git/hooks/commit-msg")
	if _, err := os.Stat(hook); err != nil {
		t.Fatalf("the hook was not installed: %v", err)
	}

	check := func(msg string) error {
		f := filepath.Join(t.TempDir(), "msg")
		if err := os.WriteFile(f, []byte(msg), 0o644); err != nil {
			t.Fatal(err)
		}
		return exec.Command("sh", hook, f).Run()
	}

	for _, ok := range []string{
		"feat: add the store",
		"feat(store): add the store",
		"fix(api-v2)!: reject an empty title",
		"chore: x\n\nTicket: t-42",
	} {
		if err := check(ok); err != nil {
			t.Errorf("the hook rejected a valid message %q: %v", ok, err)
		}
	}

	for _, bad := range []string{
		"added the store",
		"Feat: add the store",
		"feat add the store",
		"improvement: add the store",
		// IN BYTES: 72 em dashes is 24 runes and 216 bytes.
		"feat: " + strings.Repeat("—", 72),
		"feat: x\nTicket: t-42",
	} {
		if err := check(bad); err == nil {
			t.Errorf("the hook accepted %q, which commitlint would reject", bad)
		}
	}
}

// EVERY MESSAGE THIS DEPARTMENT BUILDS PASSES ITS OWN HOOK. The two were written
// to the same rule and only a test can keep them to it — r104 is what happens
// when they disagree.
func TestTheMessagesWeBuildPassTheHookWeInstall(t *testing.T) {
	work, _ := initRepo(t)
	if out, err := runScript(t, work, HookSetupScript); err != nil {
		t.Fatalf("installing the hook failed: %v\n%s", err, out)
	}
	hook := filepath.Join(work, ".git/hooks/commit-msg")

	summaries := []string{
		"add a filter — matching on done, title and owner — to the task store's List method",
		"Added the project's own store.",
		strings.Repeat("a very long summary indeed ", 20),
		"日本語 support",
		"",
	}
	for _, summary := range summaries {
		for _, msg := range []string{
			CommitMessage(job2(), summary, "feat"),
			CommitMessage(job2(), summary, "not-a-type"),
			PerFileCommitMessage(job2(), summary, "feat", "some/deep/a_very_long_file_name.go"),
			PerFileCommitMessage(job2(), summary, "test", "store_test.go"),
		} {
			f := filepath.Join(t.TempDir(), "msg")
			if err := os.WriteFile(f, []byte(msg), 0o644); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command("sh", hook, f).CombinedOutput(); err != nil {
				t.Errorf("our own message was rejected by our own hook:\n%q\n%s", msg, out)
			}
		}
	}
}

// A REPOSITORY THAT RUNS COMMITLINT HAS SAID WHAT IT WANTS; overwriting that
// would replace a project's rules with this department's.
func TestAnExistingCommitMsgHookIsNotOverwritten(t *testing.T) {
	work, _ := initRepo(t)
	if err := os.MkdirAll(filepath.Join(work, ".git/hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	theirs := "#!/bin/sh\n# the project's own\nexit 0\n"
	path := filepath.Join(work, ".git/hooks/commit-msg")
	if err := os.WriteFile(path, []byte(theirs), 0o755); err != nil {
		t.Fatal(err)
	}

	if out, err := runScript(t, work, HookSetupScript); err != nil {
		t.Fatalf("hook setup failed: %v\n%s", err, out)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != theirs {
		t.Errorf("the project's own hook was replaced:\n%s", got)
	}
}

// A TICKET THAT WAITED FOR ITS DEPENDENCY MUST ACTUALLY RECEIVE IT — and a
// CONFLICT IS NOT THIS STAGE'S TO RESOLVE.
func TestTheIntegrationMergeIsBestEffort(t *testing.T) {
	needShell(t)

	if got := MergeIntegrationScript(""); got != "" {
		t.Errorf("an unconfigured integration branch produced %q", got)
	}
	script := MergeIntegrationScript("integration")
	if out, err := exec.Command("sh", "-n", "-c", "set -e\n"+script).CombinedOutput(); err != nil {
		t.Errorf("the merge script is not valid shell: %v\n%s", err, out)
	}
	// A repository with no such branch must not stop the run.
	work, _ := initRepo(t)
	if out, err := runScript(t, work, script+"echo REACHED\n"); err != nil {
		t.Errorf("a missing integration branch aborted the push: %v\n%s", err, out)
	} else if !strings.Contains(out, "REACHED") {
		t.Errorf("the script stopped early:\n%s", out)
	}
	if !strings.Contains(script, "merge --abort") {
		t.Error("a conflicted merge is not abandoned; the developer would produce nothing")
	}
}

// THE ORDER IS THE ARGUMENT: the merge lands before the edits so verification
// runs against the tree that includes this ticket's dependencies, and the
// formatter and dependency resolve run at COMMIT time so what LANDS is the
// formatted, resolved state.
func TestThePushScriptOrdersItsStepsTheWayTheyHaveToHappen(t *testing.T) {
	s := PushScript(job2(), "agent/t-42",
		map[string]string{"store.go": "package main\n"}, "x", "feat",
		PushOptions{IntegrationBranch: "integration", FormatCommand: "gofmt -w .", DepsCommand: "go mod tidy"})

	order := []string{
		"git fetch -q origin 'agent/t-42'", // checkout
		"'integration'",                    // merge what we waited for
		"base64 -d > 'store.go'",           // then the edits
		"go mod tidy",                      // then resolve
		"gofmt -w .",                       // then format
		".git/hooks/commit-msg",            // then hooks
		"git push --force origin",          // then push
	}
	at := -1
	for _, want := range order {
		i := strings.Index(s, want)
		if i < 0 {
			t.Fatalf("the script is missing %q:\n%s", want, s)
		}
		if i < at {
			t.Errorf("%q comes out of order in the script", want)
		}
		at = i
	}
}

// A FINGERPRINT CANNOT FORGET. Comparing write counters desynchronised nine
// separate times in one day of runs — a no-op counted as a write, a push with
// nothing to commit returning early, a gate rejection skipping the update.
func TestTheVerifiedTreeIsDerivedRatherThanMaintained(t *testing.T) {
	s := &State{Staged: map[string]string{"a.go": "x", "b.go": "y"}}

	if s.TreeVerified() {
		t.Error("an unverified tree reported as verified")
	}
	s.LastTest = "ok"
	s.VerifiedTree = TreeHash(s.Staged)
	if !s.TreeVerified() {
		t.Error("the tree that was just verified reported as unverified")
	}

	// ANY change to the tree invalidates it, without anything having to remember.
	s.Staged["a.go"] = "changed"
	if s.TreeVerified() {
		t.Error("a changed tree still reported as verified")
	}

	// THE HASH IS ORDER-INDEPENDENT BY CONSTRUCTION. Go randomises map iteration,
	// so a hash that walked the map directly would differ between two runs over
	// the same tree — and the verified-tree check would then say "changed" on
	// every turn.
	tree := map[string]string{}
	for i := range 64 {
		tree[fmt.Sprintf("file_%02d.go", i)] = fmt.Sprintf("package p%d\n", i)
	}
	want := TreeHash(tree)
	for range 50 {
		// A fresh map with the same contents, built in a different insertion order.
		other := map[string]string{}
		for i := 63; i >= 0; i-- {
			other[fmt.Sprintf("file_%02d.go", i)] = fmt.Sprintf("package p%d\n", i)
		}
		if got := TreeHash(other); got != want {
			t.Fatalf("the same tree hashed two ways:\n%s\n%s", got, want)
		}
	}
}

// BASELINE, NOT READ. Read is deliberately updated with staged content so the
// agent sees its own edits, which makes it useless for this comparison.
func TestAWriteIsComparedAgainstTheBranchNotAgainstItself(t *testing.T) {
	s := &State{
		Staged:   map[string]string{"store.go": "package main\n"},
		Baseline: map[string]string{"store.go": "package main\n"},
		Read:     map[string]string{"store.go": "package main\n"},
	}
	if s.ChangedFromBaseline() {
		t.Error("a write identical to the branch was reported as a change")
	}

	s.Staged["store.go"] = "package main\n\nfunc List() {}\n"
	s.Read["store.go"] = s.Staged["store.go"] // as Apply does
	if !s.ChangedFromBaseline() {
		t.Error("a real change was reported as a no-op")
	}

	// A file the branch never had is a change.
	s2 := &State{Staged: map[string]string{"new.go": "x"}, Baseline: map[string]string{}}
	if !s2.ChangedFromBaseline() {
		t.Error("a newly created file was not reported as a change")
	}
}

func TestTheBranchNameIsQuotedWhereverItReachesTheShell(t *testing.T) {
	needShell(t)
	// A branch name cannot normally contain an apostrophe, but the quoting must
	// not depend on that being true.
	for _, branch := range []string{"agent/t-42", "agent/it's-odd", "agent/with space"} {
		s := CheckoutScript(branch) +
			CommitAndPushScript(job2(), map[string]string{"a.go": "x"}, "x", "feat", branch)
		if out, err := exec.Command("sh", "-n", "-c", "set -e\n"+s).CombinedOutput(); err != nil {
			t.Errorf("branch %q produced invalid shell: %v\n%s", branch, err, out)
		}
		if !strings.Contains(s, forge.Quote(branch)) {
			t.Errorf("branch %q was not quoted", branch)
		}
	}
}

func TestAnEmptyTreeStillProducesAValidScript(t *testing.T) {
	needShell(t)
	s := PushScript(ticket.Ticket{ID: "t-1", Title: "x"}, "agent/t-1", nil, "", "", PushOptions{})
	if out, err := exec.Command("sh", "-n", "-c", "set -e\n"+s).CombinedOutput(); err != nil {
		t.Errorf("an empty tree produced invalid shell: %v\n%s", err, out)
	}
}

// THE COMMIT MESSAGE FILE LIVES OUTSIDE THE WORKING TREE, which is why the
// preamble puts it in /tmp. Inside the repository it is a file the catch-all
// `git add -A` commits, so every branch would carry a stray .commit-msg.
func TestTheCommitMessageFileNeverBecomesPartOfTheChange(t *testing.T) {
	work, origin := initRepo(t)

	staged := map[string]string{"only.go": "package main\n"}
	script := CheckoutScript("agent/t-42") + ApplyScript(staged) +
		CommitAndPushScript(job2(), staged, "add only", "feat", "agent/t-42")

	if out, err := runScript(t, work, script); err != nil {
		t.Fatalf("the push failed: %v\n%s", err, out)
	}

	cmd := exec.Command("git", "ls-tree", "-r", "--name-only", "agent/t-42")
	cmd.Dir = origin
	tree, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the branch was not pushed: %v\n%s", err, tree)
	}
	if strings.Contains(string(tree), ".commit-msg") {
		t.Errorf("the commit message file was committed to the branch:\n%s", tree)
	}
}

// A FORMATTER OR LINTER HOOK THAT REWRITES FILES FAILS THE COMMIT AND LEAVES THE
// FIXES IN THE WORKING TREE. Re-staging and retrying once turns that from an
// unexplained failure into the no-op it should be; a second failure is a real
// rejection and is reported.
func TestAHookThatRewritesFilesGetsOneRetry(t *testing.T) {
	work, origin := initRepo(t)

	// A pre-commit hook that reformats the file and fails, once.
	hooks := filepath.Join(work, ".git/hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	hook := "#!/bin/sh\n" +
		"if [ -e .rewritten ]; then exit 0; fi\n" +
		"touch .rewritten\n" +
		"printf 'package main\\n\\nfunc only() {}\\n' > only.go\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}

	staged := map[string]string{"only.go": "package main\nfunc only(){}\n"}
	out, err := runScript(t, work, CheckoutScript("agent/t-42")+ApplyScript(staged)+
		CommitAndPushScript(job2(), staged, "add only", "feat", "agent/t-42"))
	if err != nil {
		t.Fatalf("a rewriting hook lost the change: %v\n%s", err, out)
	}
	if !strings.Contains(out, "retrying once") {
		t.Errorf("the retry did not happen or did not say so:\n%s", out)
	}

	cmd := exec.Command("git", "ls-tree", "-r", "--name-only", "agent/t-42")
	cmd.Dir = origin
	tree, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("nothing was pushed: %v\n%s", err, tree)
	}
	if !strings.Contains(string(tree), "only.go") {
		t.Errorf("the rewritten file never reached the branch:\n%s", tree)
	}
}

// A PER-FILE COMMIT THAT FAILS IS NOT AN ERROR: it may have been committed by an
// earlier attempt, or reverted by a formatter. The catch-all behind it is the
// backstop, so each of these is allowed to be a no-op — and under `set -e` an
// unguarded failure would abort the script before the push.
func TestAPerFileCommitThatIsRejectedDoesNotStopTheRest(t *testing.T) {
	work, origin := initRepo(t)

	hooks := filepath.Join(work, ".git/hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	// A hook that rejects the commit naming one particular scope.
	hook := "#!/bin/sh\ngrep -q 'feat(rejected)' \"$1\" && exit 1\nexit 0\n"
	if err := os.WriteFile(filepath.Join(hooks, "commit-msg"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}

	staged := map[string]string{
		"rejected.go": "package main\n\nfunc rejected() {}\n",
		"kept.go":     "package main\n\nfunc kept() {}\n",
	}
	out, err := runScript(t, work, CheckoutScript("agent/t-42")+ApplyScript(staged)+
		CommitAndPushScript(job2(), staged, "add them", "feat", "agent/t-42"))
	if err != nil {
		t.Fatalf("one rejected per-file commit aborted the push: %v\n%s", err, out)
	}

	cmd := exec.Command("git", "ls-tree", "-r", "--name-only", "agent/t-42")
	cmd.Dir = origin
	tree, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("nothing was pushed: %v\n%s", err, tree)
	}
	// BOTH still reach the branch: the catch-all picks up what the split could
	// not commit.
	for _, p := range []string{"kept.go", "rejected.go"} {
		if !strings.Contains(string(tree), p) {
			t.Errorf("%s never reached the branch:\n%s", p, tree)
		}
	}
}

// THE HOOK COUNTS BYTES, and it must not be changed to count characters.
//
// This one cannot be caught by running the hook: `wc -m` counts BYTES in a C
// locale, which is what a sandbox has, so the two spellings behave identically
// here and differ only where a locale happens to be set. That is precisely the
// reason the code may not depend on `wc -m` meaning characters — a rule that
// holds on the test machine and not in production is worse than no rule.
func TestTheHookCountsBytesRatherThanTrustingTheLocale(t *testing.T) {
	if !strings.Contains(ConventionalHookScript, "wc -c") {
		t.Error("the hook does not count bytes")
	}
	if strings.Contains(ConventionalHookScript, "wc -m") {
		t.Error("the hook counts characters, which means bytes in a C locale and " +
			"characters elsewhere — the same message would pass on one host and fail on another")
	}
	// And the department's own budget is the same number the hook enforces.
	if !strings.Contains(ConventionalHookScript, "-gt 72") || MaxSubjectBytes != 72 {
		t.Errorf("the hook and MaxSubjectBytes (%d) disagree; r104 is what that costs", MaxSubjectBytes)
	}
}

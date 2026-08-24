package integrate

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/config"
	"github.com/code-armory-app/blacksmith/internal/forge"
	"github.com/code-armory-app/blacksmith/internal/record"
	"github.com/code-armory-app/blacksmith/internal/release"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

// sandbox answers with whatever a test says the merge produced.
type sandbox struct {
	res  forge.Result
	err  error
	spec forge.Spec
}

func (s *sandbox) Run(_ context.Context, _ forge.Recorder, spec forge.Spec) (forge.Result, error) {
	s.spec = spec
	return s.res, s.err
}

type board struct {
	comments []string
	fail     bool
}

func (b *board) AddComment(_ context.Context, _, body string) (ticket.Comment, error) {
	if b.fail {
		return ticket.Comment{}, errors.New("the store could not be reached")
	}
	b.comments = append(b.comments, body)
	return ticket.Comment{Body: body}, nil
}

func (b *board) saidAny(want string) bool {
	for _, c := range b.comments {
		if strings.Contains(c, want) {
			return true
		}
	}
	return false
}

func repo() config.Repo {
	return config.Repo{
		URL: "https://git.example/org/repo", Branch: "main",
		Image: "golang:1.25", RunnerClass: "agent-dev",
		TestCommand: "go test ./...", SecretRef: "git:https://git.example/org/repo",
	}
}

func reviewed() ticket.Ticket {
	return ticket.Ticket{
		ID: "t-1", Status: workflow.ColReadyForIntegration,
		Comments: []ticket.Comment{{ID: "c-1", Body: record.PublishBranch(record.BranchMarker, "agent/t-1")}},
	}
}

func passed(stdout string) forge.Result {
	return forge.Result{Status: forge.StatusCompleted, ExitCode: 0, Stdout: stdout}
}

func failed(stdout string) forge.Result {
	return forge.Result{Status: forge.StatusCompleted, ExitCode: 1, Stdout: stdout}
}

// THE GATES RUN ON THE MERGED TREE, and that run — not the per-branch one — is
// what says the integration branch works.
func TestACleanMergeIsRecordedAndReported(t *testing.T) {
	sb := &sandbox{res: passed("Merge made by the 'ort' strategy.\nok  store 0.2s")}
	b := &board{}

	status, detail, err := New(sb, b, repo(), "dev").Handle(context.Background(), reviewed())
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if status != workflow.OutcomeSuccess {
		t.Errorf("status = %q", status)
	}
	if !strings.Contains(detail, "agent/t-1") {
		t.Errorf("detail = %q, want it to name the branch", detail)
	}
	if !b.saidAny(MergedMarker) {
		t.Error("nothing on the ticket says it was merged")
	}
	// It must say WHY this run matters, because "all green" before it was four
	// separate greens that never met.
	if !b.saidAny("everything merged since") {
		t.Error("the comment does not explain what the merged run proved")
	}
}

// THE RELEASE IS ITS OWN COMMENT, so a person scanning the board sees a VERSION
// rather than a branch name.
func TestAReleaseIsRecordedSeparatelyFromTheMerge(t *testing.T) {
	out := "merged\n" + release.Marker + "\nv0.2.0\nfeat(store): add an index\nfix: handle a nil map\n"
	sb := &sandbox{res: passed(out)}
	b := &board{}

	_, detail, err := New(sb, b, repo(), "dev").Handle(context.Background(), reviewed())
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(detail, "v0.2.0") {
		t.Errorf("detail = %q, want the version", detail)
	}
	if len(b.comments) != 2 {
		t.Fatalf("wrote %d comments, want the merge note and the release", len(b.comments))
	}
	notes := b.comments[1]
	if !strings.Contains(notes, "v0.2.0") || !strings.Contains(notes, "add an index") {
		t.Errorf("the release notes are %q", notes)
	}
	// The merge note itself must not carry them, or the window cannot tell them
	// apart.
	if strings.Contains(b.comments[0], "add an index") {
		t.Error("the release notes were folded into the merge comment")
	}
}

// ABSENT IS NOT A FAILURE: a merge that added no commits cuts no tag.
func TestAMergeThatCutNoTagIsStillASuccess(t *testing.T) {
	sb := &sandbox{res: passed("Already up to date.")}
	b := &board{}

	status, detail, err := New(sb, b, repo(), "dev").Handle(context.Background(), reviewed())
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if status != workflow.OutcomeSuccess {
		t.Errorf("status = %q", status)
	}
	if strings.Contains(detail, " as ") {
		t.Errorf("detail = %q, want no version claimed", detail)
	}
	if len(b.comments) != 1 {
		t.Errorf("wrote %d comments, want only the merge note", len(b.comments))
	}
}

// A CONFLICT AND A FAILING MERGED TREE ASK FOR OPPOSITE WORK, so they are
// reported differently and routed differently.
func TestAConflictGoesToTheResolverAndIsNotRetried(t *testing.T) {
	out := "rebasing...\n" + ConflictOutput + "\nstore.go\nhandlers.go\n"
	sb := &sandbox{res: failed(out)}
	b := &board{}

	status, detail, err := New(sb, b, repo(), "dev").Handle(context.Background(), reviewed())
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if status != workflow.OutcomeConflicted {
		t.Errorf("status = %q, want a conflict", status)
	}
	if !strings.Contains(detail, "agent/t-1") {
		t.Errorf("detail = %q", detail)
	}
	if !b.saidAny(ConflictMarker) {
		t.Error("the ticket does not say it conflicted")
	}
	// THE CONFLICTED PATHS ARE WHAT A PERSON NEEDS FIRST.
	if !b.saidAny("store.go") || !b.saidAny("handlers.go") {
		t.Error("the comment does not name the conflicted files")
	}
}

func TestAMergedTreeThatFailsItsGatesGoesToAPerson(t *testing.T) {
	sb := &sandbox{res: failed("Merge made by the 'ort' strategy.\n--- FAIL: TestAdd\nstore_test.go:12: want 2")}
	b := &board{}

	status, detail, err := New(sb, b, repo(), "dev").Handle(context.Background(), reviewed())
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// NOT the resolver: there is nothing to reconcile, the combination is wrong.
	if status != workflow.OutcomeBlocked {
		t.Errorf("status = %q, want it blocked rather than sent to the resolver", status)
	}
	if !strings.Contains(detail, "merged tree fails") {
		t.Errorf("detail = %q", detail)
	}
	if !b.saidAny(FailedMarker) {
		t.Error("the ticket does not distinguish this from a conflict")
	}
	if b.saidAny(ConflictMarker) {
		t.Error("a failing merged tree was reported as a conflict; a person would look for a disagreement")
	}
	if !b.saidAny("nothing to reconcile") {
		t.Error("the comment does not say what kind of failure this is")
	}
}

// NOTHING TO MERGE WITHOUT A BRANCH, and saying so plainly beats a sandbox that
// checks out nothing.
func TestATicketWithNoBranchIsRefusedBeforeASandbox(t *testing.T) {
	sb := &sandbox{res: passed("")}
	a := New(sb, &board{}, repo(), "dev")

	bare := ticket.Ticket{ID: "t-1", Status: workflow.ColReadyForIntegration}
	if a.Wants(bare) {
		t.Error("the stage wanted a ticket with nothing pushed")
	}
	status, _, err := a.Handle(context.Background(), bare)
	if err == nil {
		t.Fatal("a ticket with no branch was merged")
	}
	if status != workflow.OutcomeFailed {
		t.Errorf("status = %q", status)
	}
	if sb.spec.Image != "" {
		t.Error("a sandbox was started for a ticket with nothing to merge")
	}
}

func TestAHostWithNoRepositoryCannotIntegrate(t *testing.T) {
	a := New(&sandbox{}, &board{}, config.Repo{}, "dev")
	if _, _, err := a.Handle(context.Background(), reviewed()); err == nil {
		t.Error("a host with no repository configured merged something")
	}
}

func TestASandboxFailureIsAFailureNotAConflict(t *testing.T) {
	sb := &sandbox{err: errors.New("the sandbox never booted")}
	status, _, err := New(sb, &board{}, repo(), "dev").Handle(context.Background(), reviewed())
	if err == nil {
		t.Fatal("a sandbox that never ran reported a merge")
	}
	if status != workflow.OutcomeFailed {
		t.Errorf("status = %q", status)
	}
}

// THE MOST RECENT BRANCH IS THE ONE MERGED. A ticket reworked after a hand-back
// carries more than one.
func TestTheBranchMergedIsTheLastOnePublished(t *testing.T) {
	sb := &sandbox{res: passed("")}
	tk := reviewed()
	tk.Comments = append(tk.Comments,
		ticket.Comment{ID: "c-2", Body: record.PublishBranch(record.BranchMarker, "agent/t-1-rework")})

	_, detail, err := New(sb, &board{}, repo(), "dev").Handle(context.Background(), tk)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(detail, "agent/t-1-rework") {
		t.Errorf("detail = %q, want the reworked branch", detail)
	}
	if !strings.Contains(strings.Join(sb.spec.Command, " "), "agent/t-1-rework") {
		t.Error("the script merges a branch other than the one last published")
	}
}

// THE CREDENTIAL NEVER PASSES THROUGH THIS PROCESS: forge resolves the reference
// at dispatch.
func TestTheCloneCredentialIsAReferenceNotAValue(t *testing.T) {
	sb := &sandbox{res: passed("")}
	if _, _, err := New(sb, &board{}, repo(), "dev").Handle(context.Background(), reviewed()); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	ref, ok := sb.spec.SecretRefs["GIT_CLONE_URL"]
	if !ok {
		t.Fatal("no clone credential was requested")
	}
	if !strings.HasPrefix(ref, "git:") {
		t.Errorf("the secret ref is %q, which looks like a value rather than a reference", ref)
	}
	// A repository with no secret reference asks for none rather than an empty one.
	plain := repo()
	plain.SecretRef = ""
	sb2 := &sandbox{res: passed("")}
	New(sb2, &board{}, plain, "dev").Handle(context.Background(), reviewed())
	if len(sb2.spec.SecretRefs) != 0 {
		t.Errorf("secret refs = %v on a repository with none configured", sb2.spec.SecretRefs)
	}
}

func TestTheMergeScriptIsValidShell(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not available")
	}
	for name, r := range map[string]config.Repo{
		"a plain repository": repo(),
		"with a critical command": func() config.Repo {
			r := repo()
			r.CriticalCommand = "gosec -severity=high ./..."
			return r
		}(),
		"with no commands": {URL: "https://git.example/x", Branch: "main"},
		// A command carrying an apostrophe: an unbalanced quote makes the shell exit
		// on a syntax error with nothing run and nothing to explain it.
		"with prose in a command": func() config.Repo {
			r := repo()
			r.TestCommand = "echo \"this project's tests\" && go test ./..."
			return r
		}(),
	} {
		s := New(&sandbox{}, &board{}, r, "dev").MergeScript("agent/t-1")
		if out, err := exec.Command("sh", "-n", "-c", s).CombinedOutput(); err != nil {
			t.Errorf("%s: the merge script is not valid shell: %v\n%s", name, err, out)
		}
	}
}

// THE REBASE COMES BEFORE THE MERGE. Without it every ticket touching the same
// file conflicts BY CONSTRUCTION rather than because two changes disagree —
// measured at three of four correct, reviewed branches lost.
func TestTheScriptRebasesBeforeMerging(t *testing.T) {
	s := New(&sandbox{}, &board{}, repo(), "dev").MergeScript("agent/t-1")

	rebase := strings.Index(s, "git rebase")
	merge := strings.Index(s, "git merge")
	if rebase < 0 || merge < 0 {
		t.Fatalf("the script does not both rebase and merge")
	}
	if rebase > merge {
		t.Error("the script merges before rebasing; ordering conflicts would reach the resolver")
	}
	// The conflicted paths are captured BEFORE the abort, since aborting unwinds
	// the state that names them.
	abort := strings.Index(s, "git rebase --abort")
	names := strings.Index(s, "--diff-filter=U")
	if abort > 0 && names > abort {
		t.Error("the conflicted paths are read after the abort has unwound them")
	}
}

// THE TAG IS CUT AFTER THE PUSH, so it never names a tree that failed to build
// or that never landed.
func TestTheTagIsCutAfterTheGatesAndThePush(t *testing.T) {
	s := New(&sandbox{}, &board{}, repo(), "dev").MergeScript("agent/t-1")

	tests := strings.Index(s, "go test ./...")
	push := strings.Index(s, "git push origin dev")
	tag := strings.Index(s, "blacksmith_next_version")
	if tests < 0 || push < 0 || tag < 0 {
		t.Fatalf("the script is missing a step: tests=%d push=%d tag=%d", tests, push, tag)
	}
	if !(tests < push && push < tag) {
		t.Error("the order is not gates, push, tag")
	}
}

// THE INTEGRATION BRANCH IS CREATED ON FIRST USE, or the stage is unusable
// exactly once per repository, at the least convenient moment.
func TestTheScriptCreatesTheIntegrationBranchIfItIsMissing(t *testing.T) {
	s := New(&sandbox{}, &board{}, repo(), "dev").MergeScript("agent/t-1")
	if !strings.Contains(s, `git checkout -q -b "dev"`) {
		t.Errorf("the script does not create the integration branch:\n%s", s)
	}
	if !strings.Contains(s, `git checkout -q "main"`) {
		t.Error("the script does not cut the integration branch from the base")
	}
}

func TestConflictDetailNamesThePathsAndNothingElse(t *testing.T) {
	out := "cloning...\nrebasing...\n" + ConflictOutput + "\nstore.go\nhandlers.go\n"
	got := ConflictDetail(out)
	if strings.Contains(got, "cloning") {
		t.Errorf("ConflictDetail() = %q, want only what follows the marker", got)
	}
	if !strings.Contains(got, "store.go") || !strings.Contains(got, "handlers.go") {
		t.Errorf("ConflictDetail() = %q", got)
	}
	// Output with no marker still yields something rather than nothing.
	if got := ConflictDetail("something else went wrong"); got == "" {
		t.Error("ConflictDetail() dropped output that carried no marker")
	}
}

func TestTheMarkersAreReadableBackOffATicket(t *testing.T) {
	merged := ticket.Ticket{Comments: []ticket.Comment{{Body: MergedMarker + "\n\ndetails"}}}
	if !Merged(merged) || Conflicted(merged) || FailedIntegration(merged) {
		t.Error("a merged ticket did not read back as merged, or read back as something else")
	}
	conflicted := ticket.Ticket{Comments: []ticket.Comment{{Body: ConflictMarker}}}
	if !Conflicted(conflicted) || Merged(conflicted) {
		t.Error("a conflicted ticket read back wrongly")
	}
	failedTree := ticket.Ticket{Comments: []ticket.Comment{{Body: FailedMarker}}}
	if !FailedIntegration(failedTree) || Conflicted(failedTree) {
		t.Error("a failed integration read back as a conflict")
	}
	if Merged(ticket.Ticket{}) {
		t.Error("a ticket with no comments read back as merged")
	}
}

// A COMMENT THAT CANNOT BE WRITTEN LEAVES THE WORK INVISIBLE, but must not lose
// the outcome: the merge happened either way.
func TestAFailedCommentDoesNotLoseTheOutcome(t *testing.T) {
	sb := &sandbox{res: passed("")}
	status, _, err := New(sb, &board{fail: true}, repo(), "dev").Handle(context.Background(), reviewed())
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if status != workflow.OutcomeSuccess {
		t.Errorf("status = %q; the merge happened whether or not the comment landed", status)
	}
}

func TestTheStageIdentifiesItself(t *testing.T) {
	a := New(&sandbox{}, &board{}, repo(), "")
	if a.Role() != workflow.RoleIntegrate {
		t.Errorf("Role() = %q", a.Role())
	}
	if a.Class() == "" {
		t.Error("the stage names no class; a host could not wire it")
	}
	// An unnamed integration branch falls back rather than pushing to "".
	if !strings.Contains(a.MergeScript("agent/t-1"), config.DefaultIntegrationBranch) {
		t.Error("an unnamed integration branch did not fall back to the default")
	}
}

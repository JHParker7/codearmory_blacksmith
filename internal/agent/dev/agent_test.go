package dev

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/config"
	"github.com/code-armory-app/blacksmith/internal/edit"
	"github.com/code-armory-app/blacksmith/internal/forge"
	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/record"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/transcript"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

// box answers the three things the loop asks a sandbox for, by reading what the
// script wants. That is enough to run a whole attempt without a network.
type box struct {
	tree  []string
	files map[string]string

	scripts   []string
	branchRun []string
	released  int
	adopted   string
	merged    string

	// verdicts are the check results in order; the last one repeats.
	verdicts []forge.Result

	pushFails  bool
	surveyFail bool
	readFail   bool

	// recorders keeps the recorder each call was given. The VALUE, not a bool: a
	// nil *transcript.Recorder inside a forge.Recorder interface is not equal to
	// nil, so "was one passed" answers yes to exactly the bug this catches.
	recorders []forge.Recorder
}

func (b *box) AdoptBranch(branch string) { b.adopted = branch }
func (b *box) AlsoMerge(script string)   { b.merged = script }
func (b *box) Release(context.Context)   { b.released++ }

func (b *box) Run(_ context.Context, rec forge.Recorder, script string) (forge.Result, error) {
	b.scripts = append(b.scripts, script)
	b.recorders = append(b.recorders, rec)

	switch {
	case strings.Contains(script, "git ls-files"):
		if b.surveyFail {
			return forge.Result{Status: forge.StatusCompleted, ExitCode: 1, Stderr: "not a repository"}, nil
		}
		return forge.Result{Status: forge.StatusCompleted, Stdout: strings.Join(b.tree, "\n")}, nil

	case strings.Contains(script, FileBlockMarker):
		if b.readFail {
			return forge.Result{Status: forge.StatusCompleted, ExitCode: 2, Stderr: "read failed"}, nil
		}
		var out strings.Builder
		for p, content := range b.files {
			if strings.Contains(script, forge.Quote(p)) {
				fmt.Fprintf(&out, "%s%s\n%s\n", FileBlockMarker, p,
					base64.StdEncoding.EncodeToString([]byte(content)))
			}
		}
		return forge.Result{Status: forge.StatusCompleted, Stdout: out.String()}, nil

	default: // the push
		if b.pushFails {
			return forge.Result{Status: forge.StatusCompleted, ExitCode: 1,
				Stdout: "commit-msg: not Conventional Commits."}, nil
		}
		return forge.Result{Status: forge.StatusCompleted}, nil
	}
}

func (b *box) RunOnBranch(_ context.Context, rec forge.Recorder, branch, _ string) (forge.Result, error) {
	b.recorders = append(b.recorders, rec)
	b.branchRun = append(b.branchRun, branch)
	i := min(len(b.branchRun)-1, len(b.verdicts)-1)
	if i < 0 {
		return forge.Result{Status: forge.StatusCompleted}, nil
	}
	return b.verdicts[i], nil
}

type boxes struct {
	box *box
	err error
}

func (s *boxes) Acquire(context.Context, forge.SandboxSpec) (Box, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.box, nil
}

// gw answers each turn from a list; every later turn repeats the last reply,
// which is what an agent that will not stop actually looks like.
type gw struct {
	replies []model.ChatResult
	errs    []error
	reqs    []model.ChatRequest
}

func (g *gw) Chat(_ context.Context, _ model.Class, req model.ChatRequest) (model.ChatResult, error) {
	g.reqs = append(g.reqs, req)
	i := len(g.reqs) - 1
	if i < len(g.errs) && g.errs[i] != nil {
		return model.ChatResult{}, g.errs[i]
	}
	if i < len(g.replies) {
		return g.replies[i], nil
	}
	if len(g.replies) > 0 {
		return g.replies[len(g.replies)-1], nil
	}
	return model.ChatResult{Content: "not an action"}, nil
}

func call(name string, args any) model.ChatResult {
	b, _ := json.Marshal(args)
	return model.ChatResult{
		Calls:        []model.ToolCall{{Name: name, Arguments: string(b)}},
		FinishReason: model.FinishStop, CompletionTokens: 60,
	}
}

func readCall(paths ...string) model.ChatResult {
	return call(ActionReadFiles, map[string]any{"paths": paths})
}

func writeCall(edits ...edit.Edit) model.ChatResult {
	return call(ActionWriteFiles, map[string]any{
		"edits": edits, "summary": "add the filter", "type": "feat",
	})
}

type board struct{ comments []string }

func (b *board) AddComment(_ context.Context, _, body string) (ticket.Comment, error) {
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

func repoConfig() config.Repo {
	return config.Repo{
		URL: "https://git.example/org/repo", SecretRef: "git:repo",
		Branch: "main", IntegrationBranch: "integration", BranchPrefix: "agent/",
		Image: "golang:1.25", RunnerClass: "agent-dev", TestCommand: "go test ./...",
	}
}

func devTicket() ticket.Ticket {
	return ticket.Ticket{ID: "t-42", Title: "Add filtering to the store", Priority: "high",
		Description: "List(Filter{Done:true}) must return only completed tasks."}
}

func green() forge.Result {
	return forge.Result{Status: forge.StatusCompleted, Stdout: "ok  store 0.2s"}
}

func red() forge.Result {
	return forge.Result{Status: forge.StatusCompleted, ExitCode: 1,
		Stdout: "--- FAIL: TestListFiltersDone\n    store_test.go:12: got 2 want 1"}
}

func devAgent(g *gw, b *box, brd *board, opts Options) *Agent {
	if opts.Role == "" {
		opts.Role = workflow.RoleDev
	}
	if opts.MaxTurns == 0 {
		opts.MaxTurns = 8
	}
	opts.Tools = true
	return New(g, &boxes{box: b}, brd, model.ClassLarge, repoConfig(), opts)
}

// The ordinary path, end to end: survey, read, write, verify, finish.
func TestAnAttemptThatWorksReadsWritesVerifiesAndStops(t *testing.T) {
	b := &box{
		tree:     []string{"store.go", "store_test.go"},
		files:    map[string]string{"store.go": storeGo},
		verdicts: []forge.Result{green()},
	}
	g := &gw{replies: []model.ChatResult{
		readCall("store.go"),
		writeCall(edit.Edit{Path: "store.go", OldStr: "return nil", Replace: "return []Task{}"}),
	}}
	brd := &board{}

	status, detail, err := devAgent(g, b, brd, Options{}).Handle(context.Background(), devTicket())
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if status != workflow.OutcomeSuccess {
		t.Fatalf("status = %q, detail = %q", status, detail)
	}

	// IT STOPPED ON ITS OWN SUCCESS CONDITION rather than being told to: across
	// 3,498 turns the agents called finish once.
	if len(g.reqs) != 2 {
		t.Errorf("%d turns for a two-action job; the stage did not end itself", len(g.reqs))
	}
	// THE BRANCH IS PUBLISHED, or nothing downstream can find the work.
	if !brd.saidAny(record.BranchMarker) && !brd.saidAny("agent/t-42") {
		t.Errorf("the branch was not recorded on the ticket: %v", brd.comments)
	}
	// AND THE SANDBOX WAS GIVEN BACK.
	if b.released != 1 {
		t.Errorf("the sandbox was released %d times", b.released)
	}
}

// THE CHECKS RUN AGAINST THE BRANCH, not the working tree, and the push comes
// first — a verification against an unpushed tree reports on something no other
// stage can see.
func TestTheWorkIsPushedBeforeItIsChecked(t *testing.T) {
	b := &box{
		tree: []string{"store.go"}, files: map[string]string{"store.go": storeGo},
		verdicts: []forge.Result{green()},
	}
	g := &gw{replies: []model.ChatResult{
		readCall("store.go"),
		writeCall(edit.Edit{Path: "store.go", OldStr: "return nil", Replace: "return []Task{}"}),
	}}

	if _, _, err := devAgent(g, b, &board{}, Options{}).Handle(context.Background(), devTicket()); err != nil {
		t.Fatal(err)
	}

	var pushed bool
	for _, s := range b.scripts {
		if strings.Contains(s, "git push --force origin") {
			pushed = true
		}
	}
	if !pushed {
		t.Error("nothing was pushed before the checks ran")
	}
	if len(b.branchRun) != 1 || b.branchRun[0] != "agent/t-42" {
		t.Errorf("the checks ran against %v, want the ticket's branch", b.branchRun)
	}
}

// THE SANDBOX STARTS ON THIS TICKET'S BRANCH AND ON WHAT IT WAITED FOR. The
// lease clones the base, where the tests the developer must satisfy are not.
func TestTheSandboxAdoptsTheBranchAndMergesWhatItWaitedFor(t *testing.T) {
	b := &box{tree: []string{"store.go"}, files: map[string]string{"store.go": storeGo}}
	g := &gw{replies: []model.ChatResult{{Content: "no action here"}}}

	devAgent(g, b, &board{}, Options{MaxTurns: 1}).Handle(context.Background(), devTicket())

	if b.adopted != "agent/t-42" {
		t.Errorf("the sandbox adopted %q", b.adopted)
	}
	if !strings.Contains(b.merged, "integration") {
		t.Errorf("the sandbox does not merge what the ticket waited for: %q", b.merged)
	}
}

// A RED VERIFICATION IS NOT THE END: the failure goes back into the prompt and
// the agent gets another turn, which is the whole loop.
func TestAFailingCheckGoesBackIntoThePromptAndTheLoopContinues(t *testing.T) {
	b := &box{
		tree: []string{"store.go"}, files: map[string]string{"store.go": storeGo},
		verdicts: []forge.Result{red(), green()},
	}
	g := &gw{replies: []model.ChatResult{
		readCall("store.go"),
		writeCall(edit.Edit{Path: "store.go", OldStr: "return nil", Replace: "return []Task{}"}),
		writeCall(edit.Edit{Path: "store.go", OldStr: "return []Task{}", Replace: "return s.tasks"}),
	}}

	status, _, err := devAgent(g, b, &board{}, Options{}).Handle(context.Background(), devTicket())
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if status != workflow.OutcomeSuccess {
		t.Fatalf("status = %q", status)
	}
	if len(b.branchRun) != 2 {
		t.Errorf("%d verifications, want a red one and then a green one", len(b.branchRun))
	}

	// THE FAILURE REACHED THE AGENT. A failure it cannot see is one it cannot fix.
	var sawFailure bool
	for _, req := range g.reqs {
		for _, m := range req.Messages {
			if strings.Contains(m.Content, "TestListFiltersDone") {
				sawFailure = true
			}
		}
	}
	if !sawFailure {
		t.Error("the agent was never shown what failed")
	}
}

// A PUSH THAT FAILED IS THE AGENT'S PROBLEM TO SEE, not the stage's to die on:
// a rejected commit hook is something it can act on, and r104 ended a whole run
// reporting it as "could not push the branch".
func TestAFailedPushIsReportedToTheAgentRatherThanEndingTheStage(t *testing.T) {
	b := &box{
		tree: []string{"store.go"}, files: map[string]string{"store.go": storeGo},
		pushFails: true,
	}
	g := &gw{replies: []model.ChatResult{
		readCall("store.go"),
		writeCall(edit.Edit{Path: "store.go", OldStr: "return nil", Replace: "return []Task{}"}),
	}}

	status, _, err := devAgent(g, b, &board{}, Options{MaxTurns: 3}).
		Handle(context.Background(), devTicket())
	if err != nil {
		t.Fatalf("a failed push ended the stage with an error: %v", err)
	}
	if status != workflow.OutcomeFailed {
		t.Errorf("status = %q", status)
	}
	// The agent was shown the hook's own words.
	var sawReason bool
	for _, req := range g.reqs {
		for _, m := range req.Messages {
			if strings.Contains(m.Content, "not Conventional Commits") {
				sawReason = true
			}
		}
	}
	if !sawReason {
		t.Error("the agent was never told why the push failed")
	}
	// AND NO CHECK RAN, because there is nothing on the branch to check.
	if len(b.branchRun) != 0 {
		t.Errorf("the checks ran against a branch that was never pushed: %v", b.branchRun)
	}
}

// AN EXHAUSTED ATTEMPT SAYS WHAT IT DID. Otherwise the ticket reads "stopped
// after 8 iterations" and nothing else, which is unusable.
func TestAnExhaustedAttemptReportsWhatItActuallyDid(t *testing.T) {
	b := &box{tree: []string{"store.go"}, files: map[string]string{"store.go": storeGo}}
	// A model that only ever reads.
	g := &gw{replies: []model.ChatResult{readCall("store.go")}}
	brd := &board{}

	status, detail, err := devAgent(g, b, brd, Options{MaxTurns: 3}).
		Handle(context.Background(), devTicket())
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if status != workflow.OutcomeFailed {
		t.Errorf("status = %q", status)
	}
	if !brd.saidAny("read_files") {
		t.Errorf("the ticket does not say what the attempt did: %v", brd.comments)
	}
	if detail == "" {
		t.Error("the failure has no detail")
	}
}

// READS ARE REFUNDED, so an agent that only reads is stopped by the read run
// rather than by the budget — which is what names the actual failure.
func TestAnAgentThatOnlyReadsIsStoppedByTheReadRun(t *testing.T) {
	b := &box{tree: []string{"store.go"}, files: map[string]string{"store.go": storeGo}}
	g := &gw{replies: []model.ChatResult{readCall("store.go")}}
	brd := &board{}

	_, detail, err := devAgent(g, b, brd, Options{MaxTurns: 3}).
		Handle(context.Background(), devTicket())
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(detail, "read") {
		t.Errorf("detail = %q, want it to name the read run", detail)
	}
	// It ran far past its nominal budget of three, because reads do not spend it.
	if len(g.reqs) <= 3 {
		t.Errorf("%d turns; reads were charged against the budget", len(g.reqs))
	}
}

// A RUN OF UNUSABLE REPLIES ENDS THE ATTEMPT, and the ticket carries both the
// error and the reply it came from — either alone is unactionable.
func TestARunOfUnparseableRepliesStopsWithTheEvidence(t *testing.T) {
	b := &box{tree: []string{"store.go"}}
	g := &gw{replies: []model.ChatResult{{Content: "I would rather write prose."}}}
	brd := &board{}

	status, detail, err := devAgent(g, b, brd, Options{MaxTurns: 20}).
		Handle(context.Background(), devTicket())
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if status != workflow.OutcomeFailed || detail != "unparseable model output" {
		t.Errorf("status = %q detail = %q", status, detail)
	}
	if len(g.reqs) != MaxParseFailures {
		t.Errorf("%d turns, want %d", len(g.reqs), MaxParseFailures)
	}
	if !brd.saidAny("I would rather write prose") {
		t.Error("the ticket does not carry the reply that could not be read")
	}
}

// A GATEWAY THAT WILL NOT ANSWER IS NOT THE AGENT'S FAULT and cannot be
// reasoned about, so it ends the stage rather than burning the budget.
func TestAGatewayFailureEndsTheStage(t *testing.T) {
	b := &box{tree: []string{"store.go"}}
	g := &gw{errs: []error{errors.New("no slot became free")}}

	status, _, err := devAgent(g, b, &board{}, Options{}).Handle(context.Background(), devTicket())
	if err == nil {
		t.Fatal("a gateway failure was swallowed")
	}
	if status != workflow.OutcomeFailed {
		t.Errorf("status = %q", status)
	}
	if b.released != 1 {
		t.Error("the sandbox was not released when the stage failed")
	}
}

// A SANDBOX THAT WILL NOT BOOT IS REPORTED rather than worked around.
func TestAFailedAcquireEndsTheStage(t *testing.T) {
	a := New(&gw{}, &boxes{err: errors.New("no capacity")}, &board{},
		model.ClassLarge, repoConfig(), Options{Role: workflow.RoleDev})

	status, _, err := a.Handle(context.Background(), devTicket())
	if err == nil {
		t.Fatal("a sandbox that never booted was worked around")
	}
	if status != workflow.OutcomeFailed {
		t.Errorf("status = %q", status)
	}
}

// A SURVEY THAT FAILS MEANS THERE IS NO TREE TO REASON ABOUT.
func TestAFailedSurveyEndsTheStage(t *testing.T) {
	b := &box{surveyFail: true}
	status, _, err := devAgent(&gw{}, b, &board{}, Options{}).Handle(context.Background(), devTicket())
	if err == nil {
		t.Fatal("a failed survey was ignored")
	}
	if status != workflow.OutcomeFailed {
		t.Errorf("status = %q", status)
	}
	if b.released != 1 {
		t.Error("the sandbox was not released after a failed survey")
	}
}

// A FAILED READ IS A REFUSAL, NOT A DEAD STAGE: the file may simply be
// unreadable, and the agent can ask for a different one.
func TestAFailedReadIsARefusalRatherThanTheEnd(t *testing.T) {
	b := &box{tree: []string{"store.go"}, readFail: true}
	g := &gw{replies: []model.ChatResult{readCall("store.go")}}

	status, _, err := devAgent(&gw{replies: g.replies}, b, &board{}, Options{MaxTurns: 3}).
		Handle(context.Background(), devTicket())
	if err != nil {
		t.Fatalf("a failed read ended the stage: %v", err)
	}
	if status != workflow.OutcomeFailed {
		t.Errorf("status = %q", status)
	}
}

// WHY IT CAME BACK REACHES THE PROMPT. Without it the send-back cannot converge:
// the prompt is rebuilt from the ticket every attempt, so a returned ticket
// arrives looking exactly like a fresh one.
func TestASentBackTicketCarriesItsFindingsIntoTheFirstTurn(t *testing.T) {
	b := &box{tree: []string{"store.go"}, files: map[string]string{"store.go": storeGo}}
	g := &gw{replies: []model.ChatResult{readCall("store.go")}}

	t2 := devTicket()
	t2.Comments = []ticket.Comment{
		{Body: record.ReturnedMarker + "\n\nthe filter is case-sensitive"},
		{Body: record.ReturnedMarker + "\n\nthe filter still drops empty titles"},
	}

	devAgent(g, b, &board{}, Options{MaxTurns: 1}).Handle(context.Background(), t2)

	if len(g.reqs) == 0 {
		t.Fatal("the model was never called")
	}
	var joined string
	for _, m := range g.reqs[0].Messages {
		joined += m.Content
	}
	// THE LAST ONE WINS: everything before it has been fixed or superseded.
	if !strings.Contains(joined, "still drops empty titles") {
		t.Error("the newest finding did not reach the prompt")
	}
	if strings.Contains(joined, "case-sensitive") {
		t.Error("a superseded finding was re-litigated in the prompt")
	}
}

// THE SPECIFICATION AUTHOR IS CHECKED DIFFERENTLY: its tests are supposed to
// FAIL, so the developer's gate would pass it for writing nothing.
func TestTheAuthorIsCheckedAgainstItsOwnDefinitionOfDone(t *testing.T) {
	author := New(&gw{}, &boxes{box: &box{}}, &board{}, model.ClassLarge, repoConfig(),
		Options{Mode: ModeTest, Role: workflow.RoleSpec})
	dev := New(&gw{}, &boxes{box: &box{}}, &board{}, model.ClassLarge, repoConfig(),
		Options{Mode: ModeDevelop, Role: workflow.RoleDev})

	if !strings.Contains(author.checkScript(), "must fail against the current code") {
		t.Error("the author's check does not require a red suite")
	}
	if strings.Contains(dev.checkScript(), "must fail against the current code") {
		t.Error("the developer's check requires its tests to fail")
	}
}

// A SECTION WRITES ONTO ITS TASK'S BRANCH: that is the whole assembly
// mechanism, and giving each section its own would leave three specifications in
// three places with nothing to develop against.
//
// ONLY A SECTION. A TASK also has a parent — the request the breakdown came from
// — so applying the redirect to development sent every developer to the
// REQUEST's branch, where nothing was written. r73 is silent and total: eight
// sections wrote their tests to four task branches, all four developers were
// pointed at agent/<request>, each finished in seven records against an empty
// tree, the review reported "empty diff" and the integrator merged it. The board
// reported success in 10.3 minutes having delivered one struct.
func TestOnlyASectionIsRedirectedToItsParentsBranch(t *testing.T) {
	parent := "task-9"
	section := ticket.Ticket{ID: "sec-1", ParentID: &parent}
	taskWithRequest := ticket.Ticket{ID: "task-9", ParentID: strPtr("req-1")}

	author := New(nil, nil, nil, model.ClassSmall, repoConfig(), Options{Mode: ModeTest})
	if got := author.BranchFor(section); got != "agent/task-9" {
		t.Errorf("a section writes to %q, want its task's branch", got)
	}

	dev := New(nil, nil, nil, model.ClassSmall, repoConfig(), Options{Mode: ModeDevelop})
	if got := dev.BranchFor(taskWithRequest); got != "agent/task-9" {
		t.Errorf("a developer works %q, want the task's own branch", got)
	}
	if got := dev.BranchFor(ticket.Ticket{ID: "t-1"}); got != "agent/t-1" {
		t.Errorf("BranchFor = %q", got)
	}

	// An unconfigured prefix still produces a namespaced branch rather than a
	// bare ticket id at the repository root.
	bare := New(nil, nil, nil, model.ClassSmall, config.Repo{}, Options{})
	if got := bare.BranchFor(ticket.Ticket{ID: "t-1"}); got != "agent/t-1" {
		t.Errorf("BranchFor with no prefix = %q", got)
	}
}

func strPtr(s string) *string { return &s }

func TestAStageWithNoRepositoryStops(t *testing.T) {
	a := New(&gw{}, nil, nil, model.ClassSmall, config.Repo{}, Options{Role: workflow.RoleDev})
	status, _, err := a.Handle(context.Background(), devTicket())
	if err == nil {
		t.Fatal("a stage with no repository ran")
	}
	if status != workflow.OutcomeFailed {
		t.Errorf("status = %q", status)
	}
}

func TestTheStageIdentifiesItself(t *testing.T) {
	a := New(nil, nil, nil, model.ClassLarge, repoConfig(), Options{Role: workflow.RoleDev})
	if a.Role() != workflow.RoleDev {
		t.Errorf("Role() = %q", a.Role())
	}
	if a.Class() != model.ClassLarge {
		t.Errorf("Class() = %q", a.Class())
	}
	if !a.Wants(ticket.Ticket{}) {
		t.Error("the stage refused a ticket in its own queue")
	}
}

// AN UNSET BUDGET BECOMES THE DEFAULT rather than zero, which would end every
// attempt before its first turn.
func TestAnUnsetTurnBudgetBecomesTheDefault(t *testing.T) {
	if got := New(nil, nil, nil, model.ClassSmall, repoConfig(), Options{}).maxTurns; got != DefaultMaxIterations {
		t.Errorf("maxTurns = %d, want %d", got, DefaultMaxIterations)
	}
	if got := New(nil, nil, nil, model.ClassSmall, repoConfig(), Options{MaxTurns: 7}).maxTurns; got != 7 {
		t.Errorf("maxTurns = %d, want the configured 7", got)
	}
}

// A RE-READ IS SERVED AND REFUNDED, and the agent is TOLD it learned nothing —
// it cannot tell otherwise, because the contents look the same either way.
func TestARepeatedReadIsAnsweredAndReportedAsTeachingNothing(t *testing.T) {
	b := &box{tree: []string{"store.go"}, files: map[string]string{"store.go": storeGo}}
	g := &gw{replies: []model.ChatResult{readCall("store.go")}}

	devAgent(g, b, &board{}, Options{MaxTurns: 3}).Handle(context.Background(), devTicket())

	// ONE SANDBOX READ FOR MANY ASKS: the rest were served from what was held.
	var reads int
	for _, s := range b.scripts {
		if strings.Contains(s, FileBlockMarker) {
			reads++
		}
	}
	if reads != 1 {
		t.Errorf("%d sandbox reads for one file asked for repeatedly", reads)
	}

	// AND THE AGENT WAS TOLD. Without this it has no way to know the turn was
	// wasted.
	var told bool
	for _, req := range g.reqs {
		for _, m := range req.Messages {
			if strings.Contains(m.Content, "told you nothing") {
				told = true
			}
		}
	}
	if !told {
		t.Error("a stale read was served silently")
	}

	// THE RUN IS COUNTED, which is what warms the sampler — a refunded read never
	// touches the refusal count, so without this the model deterministically
	// re-emits the same read forever.
	var warmed bool
	for _, req := range g.reqs {
		if req.Temperature > 0 {
			warmed = true
		}
	}
	if !warmed {
		t.Error("a run of identical reads left the sampler deterministic")
	}
}

// A FRESH READ ENDS THE STALE RUN, or the sampler stays warm on an agent that is
// making progress.
func TestAFreshReadClearsTheStaleRun(t *testing.T) {
	b := &box{
		tree:  []string{"store.go", "server.go"},
		files: map[string]string{"store.go": storeGo, "server.go": "package main\n"},
	}
	g := &gw{replies: []model.ChatResult{
		readCall("store.go"),
		readCall("store.go"),  // stale
		readCall("store.go"),  // stale
		readCall("server.go"), // fresh again
		writeCall(edit.Edit{Path: "store.go", OldStr: "return nil", Replace: "return []Task{}"}),
	}}

	devAgent(g, b, &board{}, Options{MaxTurns: 6}).Handle(context.Background(), devTicket())

	// The turn after the fresh read is decoded greedily again.
	if len(g.reqs) < 5 {
		t.Fatalf("only %d turns ran", len(g.reqs))
	}
	if g.reqs[4].Temperature != 0 {
		t.Errorf("the sampler stayed warm after a fresh read: %v", g.reqs[4].Temperature)
	}
}

// A FAILED READ IS A REFUSAL, NOT A DEAD STAGE — and the agent has to be told,
// or it asks for the same unreadable file again.
func TestAFailedReadTellsTheAgentRatherThanEndingTheStage(t *testing.T) {
	b := &box{tree: []string{"store.go"}, readFail: true}
	g := &gw{replies: []model.ChatResult{readCall("store.go")}}

	_, _, err := devAgent(g, b, &board{}, Options{MaxTurns: 3}).Handle(context.Background(), devTicket())
	if err != nil {
		t.Fatalf("a failed read ended the stage: %v", err)
	}

	var told bool
	for _, req := range g.reqs {
		for _, m := range req.Messages {
			if strings.Contains(m.Content, "Could not read") {
				told = true
			}
		}
	}
	if !told {
		t.Error("the agent was never told its read failed")
	}
}

// EVERY SANDBOX CALL IS GIVEN THE STAGE'S OWN RECORDER.
//
// All four passed nil, so this loop — which serves the developer, the
// specification author, the coverage author and the spec merger, four of the
// eleven stages — recorded no actions at all. The board could say which column a
// ticket sat in and never what was being done to it, because the activity feed
// was rendering from a stream with no producer. Found by reading a stuck run's
// transcript by hand: 119 turns, zero actions.
//
// The recorder is compared BY IDENTITY. A nil *transcript.Recorder wrapped in a
// forge.Recorder interface is not equal to nil, so a "was one passed" check
// would pass on the very bug this exists to catch. The recording itself happens
// in the forge client, which has its own tests; what is pinned here is the call
// site, which is where the nil was.
func TestEverySandboxCallIsGivenTheRecorder(t *testing.T) {
	b := &box{
		tree:     []string{"store.go", "store_test.go"},
		files:    map[string]string{"store.go": storeGo},
		verdicts: []forge.Result{green()},
	}
	g := &gw{replies: []model.ChatResult{
		readCall("store.go"),
		writeCall(edit.Edit{Path: "store.go", OldStr: "return nil", Replace: "return []Task{}"}),
	}}

	rec := transcript.New(nil, "test-host")
	ctx := rec.Start(context.Background(), "tr-1", "t-42", "dev-agent")

	if _, _, err := devAgent(g, b, &board{}, Options{}).Handle(ctx, devTicket()); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// The survey, the read, the push and the check.
	if len(b.recorders) < 3 {
		t.Fatalf("only %d sandbox calls were made; the attempt did not run far "+
			"enough to prove anything", len(b.recorders))
	}
	for i, got := range b.recorders {
		if got != forge.Recorder(rec) {
			t.Errorf("sandbox call %d of %d was given %v, want the stage's own "+
				"recorder — whatever it did is otherwise absent from the transcript",
				i+1, len(b.recorders), got)
		}
	}
}

// A STAGE OUTSIDE A TRANSCRIPT STILL RUNS. Capture is optional, and a nil
// recorder is what every method on it already expects.
func TestTheDevLoopSurvivesNoTranscript(t *testing.T) {
	if got := recorderFrom(context.Background()); got != nil {
		t.Fatalf("got %v, want nil outside a transcript", got)
	}

	b := &box{
		tree:     []string{"store.go"},
		files:    map[string]string{"store.go": storeGo},
		verdicts: []forge.Result{green()},
	}
	g := &gw{replies: []model.ChatResult{
		writeCall(edit.Edit{Path: "store.go", OldStr: "return nil", Replace: "return []Task{}"}),
	}}
	if _, _, err := devAgent(g, b, &board{}, Options{}).Handle(context.Background(), devTicket()); err != nil {
		t.Fatalf("Handle without a transcript: %v", err)
	}
}

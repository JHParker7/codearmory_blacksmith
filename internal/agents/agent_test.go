package agents

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/tools"
)

// fakeGateway replays a script of replies and records what it was asked.
type fakeGateway struct {
	replies []model.ChatResult
	err     error
	seen    []model.ChatRequest
}

func (f *fakeGateway) Chat(_ context.Context, _ model.Class, req model.ChatRequest) (model.ChatResult, error) {
	f.seen = append(f.seen, req)
	if f.err != nil {
		return model.ChatResult{}, f.err
	}
	if len(f.replies) == 0 {
		return model.ChatResult{Content: "nothing left to say"}, nil
	}
	r := f.replies[0]
	f.replies = f.replies[1:]
	return r, nil
}

func calls(name, args string) model.ChatResult {
	return model.ChatResult{Calls: []model.ToolCall{{Name: name, Arguments: args}}}
}

type fakeSandbox struct{ exit int }

func (f fakeSandbox) Run(context.Context, map[string]string, string) (tools.Output, error) {
	return tools.Output{ExitCode: f.exit, Stdout: "output\n"}, nil
}

// checked is a minimal stage that gates on a command, for the loop's own tests.
// Written here rather than borrowing a real stage so that changing the
// developer's budget does not silently change what these prove.
func checked() Options {
	return Options{
		Name:          "checked",
		Class:         model.ClassLarge,
		Prompt:        "make it work",
		Guard:         tools.AllowAll,
		Tools:         []string{tools.ReadFiles, tools.ListFiles, tools.WriteFile, tools.RunCommand},
		Check:         "go test ./...",
		MaxIterations: 5,
		MaxTokens:     100,
	}
}

func unchecked() Options {
	o := checked()
	o.Name, o.Check, o.Tools = "unchecked", "", []string{tools.ReadFiles, tools.ListFiles}
	return o
}

func TestACheckThatPassesEndsTheStage(t *testing.T) {
	gw := &fakeGateway{replies: []model.ChatResult{calls(tools.RunCommand, `{}`)}}
	a := Creator{Gateway: gw, Sandbox: fakeSandbox{exit: 0}}.
		New(map[string]string{"a.go": "package p\n"}, checked())

	out, err := a.Run(context.Background(), "make it work")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !out.Passed {
		t.Fatal("a check exiting zero did not end the stage as passed")
	}
	if out.Iterations != 1 {
		t.Fatalf("it took %d turns, want 1", out.Iterations)
	}
}

func TestAFailingCheckKeepsGoingAndCarriesItsOutputForward(t *testing.T) {
	gw := &fakeGateway{replies: []model.ChatResult{
		calls(tools.RunCommand, `{}`),
		calls(tools.RunCommand, `{}`),
	}}
	a := Creator{Gateway: gw, Sandbox: fakeSandbox{exit: 1}}.
		New(map[string]string{"a.go": "package p\n"}, checked())

	out, err := a.Run(context.Background(), "make it work")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Passed {
		t.Fatal("a check exiting non-zero was reported as passed")
	}
	if !strings.Contains(out.LastCheck, "exit 1") {
		t.Fatalf("the check output was not carried out: %q", out.LastCheck)
	}
	// The second turn must have been shown what the first check said — that is
	// the whole reason the stage got another turn.
	if len(gw.seen) < 2 {
		t.Fatalf("only %d turns were taken", len(gw.seen))
	}
	if !strings.Contains(gw.seen[1].Messages[1].Content, "what the check last said") {
		t.Fatalf("the second turn was not shown the check output:\n%s", gw.seen[1].Messages[1].Content)
	}
}

// A stage whose product is prose ends when the model stops calling tools. A
// stage with a check does not get to declare itself finished.
func TestAStageWithoutACheckEndsOnItsAnswer(t *testing.T) {
	gw := &fakeGateway{replies: []model.ChatResult{{Content: "here is the design"}}}
	a := Creator{Gateway: gw}.New(map[string]string{"a.go": "package p\n"}, unchecked())

	out, err := a.Run(context.Background(), "review it")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !out.Passed || out.Answer != "here is the design" {
		t.Fatalf("the answer did not end the stage: %+v", out)
	}
}

func TestAStageWithACheckCannotDeclareItselfFinished(t *testing.T) {
	gw := &fakeGateway{replies: []model.ChatResult{
		{Content: "I am done"},
		calls(tools.RunCommand, `{}`),
	}}
	a := Creator{Gateway: gw, Sandbox: fakeSandbox{exit: 0}}.
		New(map[string]string{"a.go": "package p\n"}, checked())

	out, err := a.Run(context.Background(), "make it work")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Iterations != 2 {
		t.Fatalf("the prose answer did not cost a turn: %d", out.Iterations)
	}
	if !out.Passed {
		t.Fatal("the stage never reached its passing check")
	}
	if !strings.Contains(gw.seen[1].Messages[1].Content, tools.RunCommand) {
		t.Fatalf("the nudge does not name the tool to call:\n%s", gw.seen[1].Messages[1].Content)
	}
}

func TestARunOutOfBudgetReportsWhatTheCheckLastSaid(t *testing.T) {
	o := checked()
	o.MaxIterations = 3
	gw := &fakeGateway{replies: []model.ChatResult{
		calls(tools.RunCommand, `{}`),
		calls(tools.RunCommand, `{}`),
		calls(tools.RunCommand, `{}`),
	}}
	a := Creator{Gateway: gw, Sandbox: fakeSandbox{exit: 1}}.
		New(map[string]string{"a.go": "package p\n"}, o)

	out, err := a.Run(context.Background(), "make it work")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Passed || out.Iterations != 3 {
		t.Fatalf("the budget was not spent as expected: %+v", out)
	}
	if out.LastCheck == "" {
		t.Fatal("a stage that ran out of budget reported nothing actionable")
	}
}

func TestAModelFailureEndsTheStage(t *testing.T) {
	gw := &fakeGateway{err: errors.New("endpoint refused the connection")}
	a := Creator{Gateway: gw, Sandbox: fakeSandbox{}}.New(map[string]string{}, checked())

	if _, err := a.Run(context.Background(), "go"); err == nil {
		t.Fatal("an unreachable model did not end the stage")
	}
}

// A refusal is content the model is meant to read and act on; it must not end
// the run. This is the difference between the tool returning a string and
// returning an error.
func TestARefusedEditIsFedBackRatherThanEndingTheStage(t *testing.T) {
	o := checked()
	o.Guard = tools.NoTests
	gw := &fakeGateway{replies: []model.ChatResult{
		calls(tools.WriteFile, `{"path":"a_test.go","replace":"package q\n","summary":"x","type":"fix"}`),
		calls(tools.RunCommand, `{}`),
	}}
	a := Creator{Gateway: gw, Sandbox: fakeSandbox{exit: 0}}.
		New(map[string]string{"a_test.go": "package p\n"}, o)

	out, err := a.Run(context.Background(), "make it work")
	if err != nil {
		t.Fatalf("a refusal ended the run: %v", err)
	}
	if !out.Passed {
		t.Fatal("the stage did not continue past the refusal")
	}
	if len(out.Trail) == 0 || !strings.Contains(out.Trail[0].Result, "not yours to change") {
		t.Fatalf("the refusal is not in the trail: %+v", out.Trail)
	}
}

// The prompt is rebuilt every turn rather than accumulated. Two messages, always
// — a system instruction and one user turn holding the task and the trail.
func TestThePromptIsRebuiltEachTurnRatherThanAccumulated(t *testing.T) {
	o := checked()
	o.MaxIterations = 4
	gw := &fakeGateway{replies: []model.ChatResult{
		calls(tools.ListFiles, `{}`),
		calls(tools.ListFiles, `{}`),
		calls(tools.ListFiles, `{}`),
		calls(tools.ListFiles, `{}`),
	}}
	a := Creator{Gateway: gw, Sandbox: fakeSandbox{exit: 1}}.
		New(map[string]string{"a.go": "package p\n"}, o)

	if _, err := a.Run(context.Background(), "look around"); err != nil {
		t.Fatalf("run: %v", err)
	}
	for i, req := range gw.seen {
		if len(req.Messages) != 2 {
			t.Fatalf("turn %d sent %d messages, want 2", i+1, len(req.Messages))
		}
		if req.Messages[0].Role != "system" || req.Messages[1].Role != "user" {
			t.Fatalf("turn %d has the wrong message roles: %+v", i+1, req.Messages)
		}
	}
}

// The agent owns its workspace, so what a stage produced is read back from it
// rather than from a tree the caller kept a handle on.
func TestTheTreeIsReadBackFromTheAgent(t *testing.T) {
	gw := &fakeGateway{replies: []model.ChatResult{
		calls(tools.WriteFile, `{"path":"new.md","replace":"hello\n","summary":"add","type":"docs"}`),
		{Content: "done"},
	}}
	o := unchecked()
	o.Guard = tools.AllowAll
	o.Tools = []string{tools.ReadFiles, tools.ListFiles, tools.WriteFile}
	a := Creator{Gateway: gw}.New(map[string]string{}, o)

	if _, err := a.Run(context.Background(), "write it"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := a.Files()["new.md"]; got != "hello\n" {
		t.Fatalf("the agent's tree does not hold the write: %q", got)
	}
}

// A nil guard is far more likely to be an omission than an intention, and the
// cost of guessing wrong is a stage that may write anything.
func TestAnOmittedGuardRefusesRatherThanPermitting(t *testing.T) {
	gw := &fakeGateway{replies: []model.ChatResult{
		calls(tools.WriteFile, `{"path":"a.go","replace":"package p\n","summary":"x","type":"fix"}`),
		{Content: "done"},
	}}
	o := unchecked()
	o.Guard = nil
	o.Tools = []string{tools.ReadFiles, tools.WriteFile}
	a := Creator{Gateway: gw}.New(map[string]string{}, o)

	out, err := a.Run(context.Background(), "write it")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, ok := a.Files()["a.go"]; ok {
		t.Fatal("a stage created with no guard was allowed to write")
	}
	if len(out.Trail) == 0 || !strings.Contains(out.Trail[0].Result, "Error") {
		t.Fatalf("the write was not refused: %+v", out.Trail)
	}
}

// "Does this work" is a property of the repository, not of the stage looking at
// it, so an operator's command wins over the stage's default.
func TestTheOperatorsCheckOverridesTheStagesDefault(t *testing.T) {
	box := &recordingSandbox{}
	a := Creator{Gateway: &fakeGateway{}, Sandbox: box, Check: "make verify"}.
		New(map[string]string{}, checked())

	if a.Check() != "make verify" {
		t.Fatalf("the override was not applied: %q", a.Check())
	}
}

// ... but a stage with no check must not acquire one from the override. A
// reviewer that suddenly gates on a command is a reviewer that can fail.
func TestAnOverrideDoesNotGiveACheckToAStageWithoutOne(t *testing.T) {
	a := Creator{Gateway: &fakeGateway{}, Check: "make verify"}.New(map[string]string{}, unchecked())

	if a.Check() != "" {
		t.Fatalf("a stage with no check acquired one: %q", a.Check())
	}
}

type recordingSandbox struct{ command string }

func (r *recordingSandbox) Run(_ context.Context, _ map[string]string, command string) (tools.Output, error) {
	r.command = command
	return tools.Output{}, nil
}

// A stage may run for a hundred turns and the prompt has to stay bounded.
func TestTheTrailIsWindowedSoThePromptStaysBounded(t *testing.T) {
	var trail []Step
	for i := 0; i < TrailWindow*3; i++ {
		trail = append(trail, Step{Tool: "list_files", Result: "a.go"})
	}

	got := RenderTrail(trail)
	if strings.Count(got, "list_files") > TrailWindow {
		t.Fatalf("more than %d steps were rendered:\n%s", TrailWindow, got)
	}
	if !strings.Contains(got, "earlier steps not shown") {
		t.Fatalf("the omission is not declared:\n%s", got)
	}
}

// A compiler or test runner puts the summary and the first real error at the
// end. Keeping the head keeps the banner and throws away the diagnosis.
func TestALongOutputKeepsItsTail(t *testing.T) {
	long := strings.Repeat("noise\n", 500) + "FAIL: TestTheThingThatMatters"

	got := trim(long, 200)
	if !strings.Contains(got, "TestTheThingThatMatters") {
		t.Fatalf("the tail was discarded: %q", got)
	}
	if !strings.Contains(got, "omitted") {
		t.Fatalf("the truncation is not declared: %q", got)
	}
}

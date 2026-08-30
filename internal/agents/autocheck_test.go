package agents

import (
	"context"
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/tools"
)

// countingSandbox reports green and counts how often it was asked.
type countingSandbox struct {
	exit int
	runs int
}

func (c *countingSandbox) Run(context.Context, map[string]string, string) (tools.Output, error) {
	c.runs++
	return tools.Output{ExitCode: c.exit, Stdout: "out\n"}, nil
}

// THE POLISH PROBLEM. The check only ran when the model asked, so after the
// write that made the tree green the model could spend every remaining turn
// polishing — nothing ever told it it was done. The check now runs by itself
// after a writing turn, and green ends the stage on the spot.
func TestAGreenTreeEndsTheStageWithoutTheModelAsking(t *testing.T) {
	o := checked()
	o.Guard = tools.AllowAll
	o.MaxIterations = 50
	gw := &fakeGateway{replies: []model.ChatResult{
		calls(tools.WriteFile, `{"path":"a.md","replace":"done\n","summary":"x","type":"docs"}`),
		// The polish turns that must never happen:
		calls(tools.ReadFiles, `{"paths":["a.md"]}`),
		calls(tools.ReadFiles, `{"paths":["a.md"]}`),
	}}
	box := &countingSandbox{exit: 0}
	a := Creator{Gateway: gw, Sandbox: box}.New(map[string]string{}, o)

	out, err := a.Run(context.Background(), "build it")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !out.Passed {
		t.Fatal("a green auto-check did not pass the stage")
	}
	if out.Iterations != 1 {
		t.Fatalf("the stage polished for %d turns after going green", out.Iterations)
	}
	if box.runs != 1 {
		t.Fatalf("the check ran %d times, want 1", box.runs)
	}
}

// One check per WRITING TURN, not per write: a burst of files costs one
// execution.
func TestABurstOfWritesCostsOneCheck(t *testing.T) {
	o := checked()
	o.Guard = tools.AllowAll
	o.MaxIterations = 3
	gw := &fakeGateway{replies: []model.ChatResult{
		{Calls: []model.ToolCall{
			{Name: tools.WriteFile, Arguments: `{"path":"a.md","replace":"a\n","summary":"x","type":"docs"}`},
			{Name: tools.WriteFile, Arguments: `{"path":"b.md","replace":"b\n","summary":"x","type":"docs"}`},
			{Name: tools.WriteFile, Arguments: `{"path":"c.md","replace":"c\n","summary":"x","type":"docs"}`},
		}},
	}}
	box := &countingSandbox{exit: 0}
	a := Creator{Gateway: gw, Sandbox: box}.New(map[string]string{}, o)

	if _, err := a.Run(context.Background(), "build it"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if box.runs != 1 {
		t.Fatalf("three writes in one turn cost %d checks, want 1", box.runs)
	}
}

// A turn whose writes were all REFUSED runs nothing: a refusal loop must not
// burn sandbox executions.
func TestRefusedWritesDoNotTriggerTheAutoCheck(t *testing.T) {
	o := checked()
	o.Guard = tools.NoTests
	o.MaxIterations = 3
	gw := &fakeGateway{replies: []model.ChatResult{
		calls(tools.WriteFile, `{"path":"a_test.go","replace":"package q\n","summary":"x","type":"fix"}`),
		calls(tools.WriteFile, `{"path":"a_test.go","replace":"package q\n","summary":"x","type":"fix"}`),
		calls(tools.WriteFile, `{"path":"a_test.go","replace":"package q\n","summary":"x","type":"fix"}`),
	}}
	box := &countingSandbox{exit: 0}
	a := Creator{Gateway: gw, Sandbox: box}.
		New(map[string]string{"a_test.go": "package p\n"}, o)

	if _, err := a.Run(context.Background(), "build it"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if box.runs != 0 {
		t.Fatalf("refused writes triggered %d checks, want 0", box.runs)
	}
}

// A turn where the model ran the check itself is not checked twice.
func TestAModelRunCheckIsNotDoubled(t *testing.T) {
	o := checked()
	o.Guard = tools.AllowAll
	o.MaxIterations = 2
	gw := &fakeGateway{replies: []model.ChatResult{
		{Calls: []model.ToolCall{
			{Name: tools.WriteFile, Arguments: `{"path":"a.md","replace":"a\n","summary":"x","type":"docs"}`},
			{Name: tools.RunCommand, Arguments: `{}`},
		}},
	}}
	box := &countingSandbox{exit: 0}
	a := Creator{Gateway: gw, Sandbox: box}.New(map[string]string{}, o)

	out, err := a.Run(context.Background(), "build it")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !out.Passed || box.runs != 1 {
		t.Fatalf("passed=%v runs=%d, want passed with exactly one check", out.Passed, box.runs)
	}
}

// A red auto-check does not end the stage — its output lands in front of the
// model like any check result, and the work continues.
func TestARedAutoCheckFeedsBackAndContinues(t *testing.T) {
	o := checked()
	o.Guard = tools.AllowAll
	o.MaxIterations = 2
	gw := &fakeGateway{replies: []model.ChatResult{
		calls(tools.WriteFile, `{"path":"a.md","replace":"a\n","summary":"x","type":"docs"}`),
		calls(tools.ReadFiles, `{"paths":["a.md"]}`),
	}}
	box := &countingSandbox{exit: 1}
	a := Creator{Gateway: gw, Sandbox: box}.New(map[string]string{}, o)

	out, err := a.Run(context.Background(), "build it")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Passed {
		t.Fatal("a red auto-check passed the stage")
	}
	if out.LastCheck == "" {
		t.Fatal("the auto-check's output was not carried forward")
	}
	if len(gw.seen) < 2 || !strings.Contains(gw.seen[1].Messages[1].Content, "what the check last said") {
		t.Fatal("the next turn was not shown the auto-check output")
	}
}

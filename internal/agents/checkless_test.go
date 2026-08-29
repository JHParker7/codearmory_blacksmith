package agents

import (
	"context"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/tools"
)

// A STAGE WITH NO CHECK IS DONE WHEN IT STOPS CHANGING THINGS.
//
// Measured: the planning architect wrote a 424-line plan and a 298-line test
// plan, then circled without ever answering in prose. The idle bound stopped it
// — correctly — and then reported the stage FAILED, which threw all 722 lines
// away and killed the run. Its deliverable was already on disk.
func TestAChecklessStageThatStopsChangingThingsHasFinished(t *testing.T) {
	o := unchecked()
	o.Guard = tools.AllowAll
	o.MaxIterations = 200
	o.Tools = []string{tools.ReadFiles, tools.ListFiles, tools.WriteFile}

	replies := []model.ChatResult{
		calls(tools.WriteFile, `{"path":"PLAN.md","replace":"the plan\n","summary":"x","type":"docs"}`),
	}
	for i := 0; i < 200; i++ {
		replies = append(replies, calls(tools.ListFiles, `{}`))
	}
	gw := &fakeGateway{replies: replies}
	a := Creator{Gateway: gw}.New(map[string]string{}, o)

	out, err := a.Run(context.Background(), "plan it")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !out.Passed {
		t.Fatal("a checkless stage that produced its deliverable was reported as failed")
	}
	if !out.Stalled {
		t.Error("the stall was not recorded; a tidy finish and a loop should be tellable apart")
	}
	if got := a.Files()["PLAN.md"]; got != "the plan\n" {
		t.Fatalf("the deliverable was lost: %q", got)
	}
}

// A stage WITH a check must not get the same treatment: stopping without a green
// check is a failure, and calling it done would report broken code as finished.
func TestACheckedStageThatStallsStillFails(t *testing.T) {
	o := checked()
	o.MaxIterations = 200
	var replies []model.ChatResult
	for i := 0; i < 200; i++ {
		replies = append(replies, calls(tools.ReadFiles, `{"paths":["a.go"]}`))
	}
	gw := &fakeGateway{replies: replies}
	a := Creator{Gateway: gw, Sandbox: fakeSandbox{exit: 1}}.
		New(map[string]string{"a.go": "package p\n"}, o)

	out, err := a.Run(context.Background(), "build it")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Passed {
		t.Fatal("a checked stage that stalled was reported as passed")
	}
	if !out.Stalled {
		t.Fatal("the stall was not recorded")
	}
}

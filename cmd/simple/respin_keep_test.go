package main

import (
	"context"
	"testing"
	"time"

	"github.com/code-armory-app/blacksmith/internal/agents"
	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/tools"
)

// writeThenWedgeGateway lands one real write on its first attempt and THEN
// wedges; from the second attempt on it answers promptly.
type writeThenWedgeGateway struct {
	attempt *int
	calls   int
}

func (w *writeThenWedgeGateway) Chat(ctx context.Context, _ model.Class, _ model.ChatRequest) (model.ChatResult, error) {
	w.calls++
	if *w.attempt == 1 {
		if w.calls == 1 {
			return model.ChatResult{Calls: []model.ToolCall{{
				Name:      tools.WriteFile,
				Arguments: `{"path":"WORK.md","replace":"the killed attempt wrote this\n","summary":"x","type":"docs"}`,
			}}}, nil
		}
		<-ctx.Done()
		return model.ChatResult{}, ctx.Err()
	}
	return model.ChatResult{Calls: []model.ToolCall{{Name: tools.RunCommand, Arguments: "{}"}}}, nil
}

// redThenGreenBox is red while the killed attempt's marker is the only file —
// its auto-check after the write must not accidentally pass the first attempt,
// which has to die on the CLOCK for this test to test anything — and green for
// the fresh agent's own check.
type redThenGreenBox struct{}

func (redThenGreenBox) Run(_ context.Context, files map[string]string, _ string) (tools.Output, error) {
	if len(files) == 1 {
		return tools.Output{ExitCode: 1, Stdout: "not yet\n"}, nil
	}
	return tools.Output{ExitCode: 0, Stdout: "ok\n"}, nil
}

// THE PROPERTY THE RESPIN EXISTS FOR, pinned exactly: the code the killed
// model wrote is NOT reverted. The fresh agent inherits it and only the
// conversation is thrown away. The neighbouring respin test cannot prove this
// — its wedge never writes before dying — and the gap was found by the
// operator asking the question the test should have answered.
func TestTheKilledAttemptsWritesSurviveTheRespin(t *testing.T) {
	attempt := 0
	gw := &writeThenWedgeGateway{attempt: &attempt}
	c := agents.Creator{Gateway: gw, Sandbox: redThenGreenBox{}}

	var secondAttemptSaw map[string]string
	build := func(files map[string]string) (*agents.Agent, error) {
		attempt++
		if attempt == 2 {
			secondAttemptSaw = files
			// The fresh agent writes its own marker so the box sees two files
			// and its check can go green.
			files["FRESH.md"] = "the fresh attempt is here\n"
		}
		o := wedgeOptions(80*time.Millisecond, 1)
		o.Guard = tools.AllowAll
		return c.New(files, o), nil
	}

	out, files, err := runStage(context.Background(), build, map[string]string{}, "work")
	if err != nil {
		t.Fatalf("runStage: %v", err)
	}
	if !out.Passed {
		t.Fatal("the fresh agent did not finish")
	}
	if got := secondAttemptSaw["WORK.md"]; got != "the killed attempt wrote this\n" {
		t.Fatalf("the fresh agent did not inherit the killed attempt's write: %q", got)
	}
	if got := files["WORK.md"]; got != "the killed attempt wrote this\n" {
		t.Fatalf("the final tree lost the killed attempt's write: %q", got)
	}
}

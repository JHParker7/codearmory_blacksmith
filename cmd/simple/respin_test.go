package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/code-armory-app/blacksmith/internal/agents"
	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/tools"
)

// wedgeGateway blocks until its context dies on the first attempt — the shape
// of a dev re-running an unchanging check forever — and answers promptly from
// the second attempt on.
type wedgeGateway struct {
	attempt  *int
	rescueOn int
}

func (w wedgeGateway) Chat(ctx context.Context, _ model.Class, _ model.ChatRequest) (model.ChatResult, error) {
	if *w.attempt < w.rescueOn {
		<-ctx.Done()
		return model.ChatResult{}, ctx.Err()
	}
	return model.ChatResult{Calls: []model.ToolCall{{Name: tools.RunCommand, Arguments: "{}"}}}, nil
}

type greenBox struct{}

func (greenBox) Run(context.Context, map[string]string, string) (tools.Output, error) {
	return tools.Output{ExitCode: 0, Stdout: "ok\n"}, nil
}

func wedgeOptions(timeout time.Duration, respins int) agents.Options {
	return agents.Options{
		Name:           "wedge",
		Class:          model.ClassLarge,
		Prompt:         "work",
		Guard:          tools.AllowAll,
		Tools:          []string{tools.WriteFile, tools.RunCommand},
		Check:          "true",
		AttemptTimeout: timeout,
		Respins:        respins,
		MaxIterations:  5,
		MaxTokens:      100,
	}
}

// THE OPERATOR'S RULE: after the timeout, kill the dev and spin up a new one.
// The fresh agent keeps the tree, loses the trail, and here — unwedged —
// finishes on its first check.
func TestATimedOutStageIsRespunAndTheFreshAgentFinishes(t *testing.T) {
	attempt := 0
	c := agents.Creator{Gateway: wedgeGateway{attempt: &attempt, rescueOn: 2}, Sandbox: greenBox{}}

	build := func(files map[string]string) (*agents.Agent, error) {
		attempt++
		return c.New(files, wedgeOptions(50*time.Millisecond, 2)), nil
	}

	out, files, err := runStage(context.Background(),
		build, map[string]string{"kept.go": "package main\n"}, "work")
	if err != nil {
		t.Fatalf("runStage: %v", err)
	}
	if !out.Passed {
		t.Fatal("the fresh agent did not finish")
	}
	if attempt != 2 {
		t.Fatalf("took %d attempts, want 2", attempt)
	}
	if _, ok := files["kept.go"]; !ok {
		t.Fatal("the tree did not survive the respin")
	}
}

// Respins are bounded: a stage that wedges every attempt fails with the
// timeout in the error, not a fourth quiet attempt.
func TestRespinsAreBounded(t *testing.T) {
	attempt := 0
	c := agents.Creator{Gateway: wedgeGateway{attempt: &attempt, rescueOn: 99}, Sandbox: greenBox{}}

	build := func(files map[string]string) (*agents.Agent, error) {
		attempt++
		return c.New(files, wedgeOptions(30*time.Millisecond, 1)), nil
	}

	_, _, err := runStage(context.Background(), build, map[string]string{}, "work")
	if err == nil {
		t.Fatal("a permanently wedged stage did not fail")
	}
	if attempt != 2 {
		t.Fatalf("took %d attempts, want exactly 2 (one respin)", attempt)
	}
	if !strings.Contains(err.Error(), "deadline") && !strings.Contains(err.Error(), "context") {
		t.Fatalf("the error does not carry the timeout: %v", err)
	}
}

// The operator pressing ctrl-C is not a stage timeout, and must not be
// answered with a respin.
func TestACancelledRunIsNotRespun(t *testing.T) {
	attempt := 0
	c := agents.Creator{Gateway: wedgeGateway{attempt: &attempt, rescueOn: 99}, Sandbox: greenBox{}}

	ctx, cancel := context.WithCancel(context.Background())
	build := func(files map[string]string) (*agents.Agent, error) {
		attempt++
		return c.New(files, wedgeOptions(time.Hour, 5)), nil
	}
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()

	_, _, err := runStage(ctx, build, map[string]string{}, "work")
	if err == nil {
		t.Fatal("a cancelled run returned no error")
	}
	if attempt != 1 {
		t.Fatalf("a ctrl-C was answered with %d attempts, want 1", attempt)
	}
}

// A stage with no timeout runs exactly once, unbounded, as before.
func TestAStageWithoutATimeoutIsNotWrapped(t *testing.T) {
	attempt := 0
	c := agents.Creator{Gateway: wedgeGateway{attempt: &attempt, rescueOn: 1}, Sandbox: greenBox{}}

	build := func(files map[string]string) (*agents.Agent, error) {
		attempt++
		return c.New(files, wedgeOptions(0, 0)), nil
	}
	out, _, err := runStage(context.Background(), build, map[string]string{}, "work")
	if err != nil || !out.Passed || attempt != 1 {
		t.Fatalf("the untimed path changed: err=%v passed=%v attempts=%d", err, out.Passed, attempt)
	}
}

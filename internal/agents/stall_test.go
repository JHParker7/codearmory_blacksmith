package agents

import (
	"context"
	"fmt"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/tools"
)

// THE FAILURE THAT COST THE FIRST LIVE RUN of this loop: 76 read_files calls
// against 2 writes, run_command never once called, and the whole budget spent
// re-reading the same two files. Reading is not progress, however much of it
// happens.
func TestAnAgentThatOnlyReadsIsStoppedRatherThanSpendingItsBudget(t *testing.T) {
	o := checked()
	o.MaxIterations = 200
	var replies []model.ChatResult
	for i := 0; i < 200; i++ {
		replies = append(replies, calls(tools.ReadFiles, `{"paths":["a.go"]}`))
	}
	gw := &fakeGateway{replies: replies}
	a := Creator{Gateway: gw, Sandbox: fakeSandbox{exit: 1}}.
		New(map[string]string{"a.go": "package p\n"}, o)

	out, err := a.Run(context.Background(), "make it work")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !out.Stalled {
		t.Fatal("a looping agent was not reported as stalled")
	}
	if out.Iterations > MaxIdleTurns+1 {
		t.Fatalf("it took %d turns to notice, want at most %d", out.Iterations, MaxIdleTurns+1)
	}
	if out.Passed {
		t.Fatal("a stalled stage was reported as passed")
	}
}

// A stage that is working must not be stopped by the idle bound however long it
// runs. An accepted write resets it.
func TestAnAgentThatKeepsWritingIsNotStopped(t *testing.T) {
	o := checked()
	o.MaxIterations = 40
	o.Guard = tools.AllowAll
	var replies []model.ChatResult
	for i := 0; i < 40; i++ {
		replies = append(replies, calls(tools.WriteFile, fmt.Sprintf(
			`{"path":"f%d.md","replace":"line %d\n","summary":"x","type":"docs"}`, i, i)))
	}
	gw := &fakeGateway{replies: replies}
	a := Creator{Gateway: gw, Sandbox: fakeSandbox{exit: 1}}.
		New(map[string]string{"a.go": "package p\n"}, o)

	out, err := a.Run(context.Background(), "work")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Stalled {
		t.Fatalf("a working agent was stopped as stalled after %d turns", out.Iterations)
	}
	if out.Iterations != 40 {
		t.Fatalf("it stopped early at %d turns", out.Iterations)
	}
}

// An agent repeating an edit the guard refuses is exactly as stuck as one
// re-reading, and counting the attempt as progress would hide it.
func TestARepeatedlyRefusedWriteCountsAsStalled(t *testing.T) {
	o := checked()
	o.MaxIterations = 200
	o.Guard = tools.NoTests
	var replies []model.ChatResult
	for i := 0; i < 200; i++ {
		replies = append(replies, calls(tools.WriteFile,
			`{"path":"a_test.go","replace":"package q\n","summary":"x","type":"fix"}`))
	}
	gw := &fakeGateway{replies: replies}
	a := Creator{Gateway: gw, Sandbox: fakeSandbox{exit: 1}}.
		New(map[string]string{"a_test.go": "package p\n"}, o)

	out, err := a.Run(context.Background(), "make it work")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !out.Stalled {
		t.Fatal("an agent repeating a refused write was not reported as stalled")
	}
}

// A check that keeps failing is a stage that is working, not one that is stuck.
// Running the check is progress even when it comes back red.
func TestAFailingCheckIsNotStalling(t *testing.T) {
	o := checked()
	o.MaxIterations = MaxIdleTurns + 5
	var replies []model.ChatResult
	for i := 0; i < o.MaxIterations; i++ {
		replies = append(replies, calls(tools.RunCommand, `{}`))
	}
	gw := &fakeGateway{replies: replies}
	a := Creator{Gateway: gw, Sandbox: fakeSandbox{exit: 1}}.
		New(map[string]string{"a.go": "package p\n"}, o)

	out, err := a.Run(context.Background(), "make it work")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Stalled {
		t.Fatal("a stage running its check was reported as stalled")
	}
}

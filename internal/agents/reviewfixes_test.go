package agents

import (
	"context"
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/tools"
)

// A model that answers in prose every turn is exactly as stuck as one
// re-reading a file, and the prose branch used to `continue` past the idle
// counter — so it burned the whole budget instead of stalling at the bound.
func TestAProseLoopOnACheckedStageTripsTheStallBound(t *testing.T) {
	o := checked()
	o.MaxIterations = 200
	gw := &fakeGateway{} // no scripted replies: every turn is prose
	a := Creator{Gateway: gw, Sandbox: fakeSandbox{exit: 1}}.
		New(map[string]string{"a.go": "package p\n"}, o)

	out, err := a.Run(context.Background(), "build it")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !out.Stalled {
		t.Fatal("a prose loop was not reported as stalled")
	}
	if out.Iterations > MaxIdleTurns+1 {
		t.Fatalf("it took %d turns to notice, want at most %d", out.Iterations, MaxIdleTurns+1)
	}
}

// A checkless stage whose deliverable is FILES must not pass on prose alone.
// The plan would exist only in stdout, and the next stage starts from an empty
// tree — the same empty-tree pass the stall path already refuses.
func TestACheckessFileStageCannotPassByProseAlone(t *testing.T) {
	o := unchecked()
	o.Guard = tools.AllowAll
	o.Tools = []string{tools.ReadFiles, tools.ListFiles, tools.WriteFile}
	o.MaxIterations = 3
	gw := &fakeGateway{replies: []model.ChatResult{
		{Content: "Here is my plan: build a store, then handlers."},
		calls(tools.WriteFile, `{"path":"PLAN.md","replace":"the plan\n","summary":"x","type":"docs"}`),
		{Content: "done"},
	}}
	a := Creator{Gateway: gw}.New(map[string]string{}, o)

	out, err := a.Run(context.Background(), "plan it")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !out.Passed {
		t.Fatalf("the stage did not pass once the file was written: %+v", out)
	}
	if out.Iterations != 3 {
		t.Fatalf("the prose-only answer on turn 1 was accepted (took %d turns, want 3)", out.Iterations)
	}
	if !strings.Contains(gw.seen[1].Messages[1].Content, "deliverable is FILES") {
		t.Fatal("the turn after the refused answer does not say why it was refused")
	}
}

// A stage that CANNOT write — the reviewer — still finishes on its answer.
// Requiring writes of a stage with no write tool would make it unfinishable.
func TestAReadOnlyStageStillFinishesOnItsAnswer(t *testing.T) {
	gw := &fakeGateway{replies: []model.ChatResult{{Content: "no findings"}}}
	a := Creator{Gateway: gw}.New(map[string]string{"a.go": "package p\n"},
		maker().Sec(nil).opts)

	out, err := a.Run(context.Background(), "review")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !out.Passed || out.Answer != "no findings" {
		t.Fatalf("the reviewer could not finish on its answer: %+v", out)
	}
}

// A reply cut off at the token ceiling is half an answer, not a finished one.
// Accepting it ships half a plan and tells the model nothing about why.
func TestATruncatedAnswerIsNotAcceptedAsFinished(t *testing.T) {
	o := unchecked() // read-only tools, so the answer would otherwise pass
	o.MaxIterations = 2
	gw := &fakeGateway{replies: []model.ChatResult{
		{Content: "the plan is to", FinishReason: model.FinishLength},
		{Content: "short and complete"},
	}}
	a := Creator{Gateway: gw}.New(map[string]string{}, o)

	out, err := a.Run(context.Background(), "plan it")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Answer != "short and complete" {
		t.Fatalf("the truncated fragment was accepted as the answer: %q", out.Answer)
	}
	if !strings.Contains(gw.seen[1].Messages[1].Content, "CUT OFF") {
		t.Fatal("the model was not told it was cut off")
	}
}

// Write, undo, stall: the tree is empty and Writes() must say so, or the
// checkless completion hands the next stage an empty tree with a straight face.
func TestAFullyUndoneWriteIsNotADeliverable(t *testing.T) {
	o := unchecked()
	o.Guard = tools.AllowAll
	o.Tools = []string{tools.ListFiles, tools.WriteFile, tools.UndoEdit}
	o.MaxIterations = 200
	replies := []model.ChatResult{
		calls(tools.WriteFile, `{"path":"PLAN.md","replace":"the plan\n","summary":"x","type":"docs"}`),
		calls(tools.UndoEdit, `{}`),
	}
	gw := &fakeGateway{replies: replies} // then prose forever
	a := Creator{Gateway: gw}.New(map[string]string{}, o)

	out, err := a.Run(context.Background(), "plan it")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Passed {
		t.Fatal("a stage whose only write was undone passed over an empty tree")
	}
}

// The specification's gate points the other way: green when the tests FAIL.
func TestTheSpecCheckExpectsRedAndKeepsItUnderAnOverride(t *testing.T) {
	spec := maker().Spec(nil)
	if !strings.Contains(spec.Check(), "! go test") {
		t.Fatalf("the spec check does not expect red: %q", spec.Check())
	}
	// go vet is the piece that typechecks the TEST FILES, which go build does
	// not; without it a test file full of syntax errors reads as the expected red.
	if !strings.Contains(spec.Check(), "go vet") {
		t.Fatalf("the spec check cannot tell broken tests from failing ones: %q", spec.Check())
	}

	// The operator override says what "the tests pass" means; substituting it
	// here would re-invert the one stage whose gate points the other way.
	overridden := Creator{Gateway: &fakeGateway{}, Check: "make verify"}.Spec(nil)
	if overridden.Check() != spec.Check() {
		t.Fatalf("the operator override replaced the expected-red check: %q", overridden.Check())
	}
	// ...while a green-check stage still takes the override.
	dev := Creator{Gateway: &fakeGateway{}, Check: "make verify"}.Dev(nil)
	if dev.Check() != "make verify" {
		t.Fatalf("the override no longer reaches ordinary stages: %q", dev.Check())
	}
}

// A file too large for the whole prompt budget is shown head and tail with the
// elided range NAMED — omission's remedy is "read it", and for this file
// reading changes nothing, which was an unbreakable loop.
func TestAFileTooLargeForTheBudgetIsElidedNotInvisible(t *testing.T) {
	var big strings.Builder
	big.WriteString("package main // FIRST LINE\n")
	for i := 0; i < 3000; i++ {
		big.WriteString("// padding line that makes this file larger than the whole prompt budget\n")
	}
	big.WriteString("// LAST LINE\n")

	o := checked()
	o.MaxIterations = 2
	gw := &fakeGateway{replies: []model.ChatResult{
		calls(tools.ReadFiles, `{"paths":["big.go"]}`),
		calls(tools.ListFiles, `{}`),
	}}
	a := Creator{Gateway: gw, Sandbox: fakeSandbox{exit: 1}}.
		New(map[string]string{"big.go": big.String()}, o)

	if _, err := a.Run(context.Background(), "look"); err != nil {
		t.Fatalf("run: %v", err)
	}
	prompt := gw.seen[1].Messages[1].Content
	if !strings.Contains(prompt, "FIRST LINE") || !strings.Contains(prompt, "LAST LINE") {
		t.Fatal("the oversized file's head and tail did not reach the prompt")
	}
	if !strings.Contains(prompt, "NOT SHOWN") {
		t.Fatal("the elision is not declared")
	}
}

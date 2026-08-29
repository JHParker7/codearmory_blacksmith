package agents

import (
	"context"
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/tools"
)

// THE BUG THAT COST THE FIRST TWO LIVE RUNS.
//
// model.go rendered to 2,348 characters. The trail trimmed every tool result to
// 1,200 and kept the TAIL, so `type Comment struct` — which lives at the top —
// was never once visible to the model. It wrote a Comment literal with a field
// the struct does not have, and then re-read the same file fifteen times trying
// to see the half it was never shown.
//
// A DECLARATION AT THE TOP OF A LONG FILE MUST REACH THE PROMPT. That is the
// whole property, and it is asserted on a file long enough that the old trail
// would have cut it.
func TestADeclarationAtTheTopOfALongFileReachesThePrompt(t *testing.T) {
	var body strings.Builder
	body.WriteString("package main\n\ntype Comment struct {\n\tID int\n\tBody string\n}\n")
	// Push the declaration far above any tail window.
	for i := 0; i < 400; i++ {
		body.WriteString("\n// filler line that pushes the declaration out of any tail window\n")
	}
	body.WriteString("\nfunc Last() {}\n")

	gw := &fakeGateway{replies: []model.ChatResult{
		calls(tools.ReadFiles, `{"paths":["model.go"]}`),
		calls(tools.ReadFiles, `{"paths":["model.go"]}`),
	}}
	o := checked()
	o.MaxIterations = 2
	a := Creator{Gateway: gw, Sandbox: fakeSandbox{exit: 1}}.
		New(map[string]string{"model.go": body.String()}, o)

	if _, err := a.Run(context.Background(), "build it"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(gw.seen) < 2 {
		t.Fatalf("only %d turns were taken", len(gw.seen))
	}

	// The turn AFTER the read is the one that matters: by then the read's own
	// result is history, and the file must still be in front of the model.
	prompt := gw.seen[1].Messages[1].Content
	if !strings.Contains(prompt, "type Comment struct") {
		t.Fatal("the declaration at the top of the file never reached the prompt")
	}
	if !strings.Contains(prompt, "func Last()") {
		t.Fatal("the end of the file did not reach the prompt either")
	}
}

// A file the agent WROTE must stay in front of it, or it will write the next
// file against a half-remembered version of the first — which is exactly how
// run 1 produced a store.go that used string ids against an int-keyed model.
func TestAFileTheAgentWroteStaysInThePrompt(t *testing.T) {
	o := checked()
	o.Guard = tools.AllowAll
	o.MaxIterations = 3
	gw := &fakeGateway{replies: []model.ChatResult{
		calls(tools.WriteFile,
			`{"path":"model.go","replace":"package main\n\ntype Comment struct{ ID int }\n","summary":"x","type":"feat"}`),
		calls(tools.ListFiles, `{}`),
		calls(tools.ListFiles, `{}`),
	}}
	a := Creator{Gateway: gw, Sandbox: fakeSandbox{exit: 1}}.New(map[string]string{}, o)

	if _, err := a.Run(context.Background(), "build it"); err != nil {
		t.Fatalf("run: %v", err)
	}
	last := gw.seen[len(gw.seen)-1].Messages[1].Content
	if !strings.Contains(last, "type Comment struct{ ID int }") {
		t.Fatalf("the file the agent wrote is not in the prompt two turns later:\n%s", last)
	}
}

// The contents shown must be the CURRENT ones. Showing what a file held when it
// was first read would be worse than showing nothing: the agent would address
// edits against text that is no longer there.
func TestTheContentsShownAreTheCurrentOnes(t *testing.T) {
	o := checked()
	o.Guard = tools.AllowAll
	o.MaxIterations = 3
	gw := &fakeGateway{replies: []model.ChatResult{
		calls(tools.ReadFiles, `{"paths":["a.go"]}`),
		calls(tools.WriteFile,
			`{"path":"a.go","old_str":"\tOLD","replace":"\tNEW","summary":"x","type":"fix"}`),
		calls(tools.ListFiles, `{}`),
	}}
	a := Creator{Gateway: gw, Sandbox: fakeSandbox{exit: 1}}.
		New(map[string]string{"a.go": "package p\n\nfunc F() {\n\tOLD\n}\n"}, o)

	if _, err := a.Run(context.Background(), "change it"); err != nil {
		t.Fatalf("run: %v", err)
	}
	// Only the files block. The trail legitimately still quotes the old text as
	// the argument the write was called with, and that is a record of what was
	// tried rather than a claim about the file.
	known := filesBlock(t, gw.seen[len(gw.seen)-1].Messages[1].Content)
	if strings.Contains(known, "OLD") {
		t.Fatalf("the prompt still shows the file as it was before the edit:\n%s", known)
	}
	if !strings.Contains(known, "NEW") {
		t.Fatalf("the prompt does not show the edited contents:\n%s", known)
	}
}

// filesBlock extracts just the rendered file contents from a prompt.
func filesBlock(t *testing.T, prompt string) string {
	t.Helper()
	const header = "--- the files you have read or written, as they are NOW ---"
	i := strings.Index(prompt, header)
	if i < 0 {
		t.Fatalf("the prompt has no files section:\n%s", prompt)
	}
	rest := prompt[i+len(header):]
	if j := strings.Index(rest, "\n--- "); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// A file that is dropped for budget is NAMED rather than truncated. A name is an
// instruction the agent can act on; half a file reads as a whole one.
func TestAFileDroppedForBudgetIsNamedRatherThanTruncated(t *testing.T) {
	files := map[string]string{}
	big := strings.Repeat("x x x x x x x x x x x x x x x x\n", 1500) // ~48k each
	files["one.go"] = "package p\n" + big
	files["two.go"] = "package p\n" + big

	o := checked()
	o.MaxIterations = 2
	gw := &fakeGateway{replies: []model.ChatResult{
		calls(tools.ReadFiles, `{"paths":["one.go","two.go"]}`),
		calls(tools.ListFiles, `{}`),
	}}
	a := Creator{Gateway: gw, Sandbox: fakeSandbox{exit: 1}}.New(files, o)

	if _, err := a.Run(context.Background(), "look"); err != nil {
		t.Fatalf("run: %v", err)
	}
	last := gw.seen[len(gw.seen)-1].Messages[1].Content
	if !strings.Contains(last, "not shown, read them if you need them") {
		t.Fatal("a file dropped for budget was not declared")
	}
	if !strings.Contains(last, "one.go") {
		t.Fatal("the dropped file was not named")
	}
}

// Numbered, because every edit refusal tells the agent to copy from "the
// numbered contents" and to disambiguate repeated lines by number. Showing the
// file without them makes that advice unfollowable.
func TestTheFilesInThePromptAreNumbered(t *testing.T) {
	o := checked()
	o.MaxIterations = 2
	gw := &fakeGateway{replies: []model.ChatResult{
		calls(tools.ReadFiles, `{"paths":["a.go"]}`),
		calls(tools.ListFiles, `{}`),
	}}
	a := Creator{Gateway: gw, Sandbox: fakeSandbox{exit: 1}}.
		New(map[string]string{"a.go": "package p\nfunc F() {}\n"}, o)

	if _, err := a.Run(context.Background(), "look"); err != nil {
		t.Fatalf("run: %v", err)
	}
	last := gw.seen[len(gw.seen)-1].Messages[1].Content
	if !strings.Contains(last, "1\tpackage p") || !strings.Contains(last, "2\tfunc F() {}") {
		t.Fatalf("the files in the prompt are not numbered:\n%s", last)
	}
}

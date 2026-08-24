package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// A returned ticket must arrive DIFFERENT from a fresh one. The prompt is
// rebuilt from the ticket each attempt and carries no comment history, so
// without the findings the agent writes the same change again and the reviewer
// rejects it again — a loop that only the return ceiling stops.
func TestReturnedTicketCarriesTheReviewFindingsIntoThePrompt(t *testing.T) {
	fresh := Ticket{TicketID: "t1", Title: "add filtering", Description: "do the thing"}
	if got := returnReason(fresh); got != "" {
		t.Errorf("returnReason on a fresh ticket = %q, want empty", got)
	}
	out := renderDevState(fresh, &devState{budget: 8})
	if strings.Contains(out, "SENT BACK") {
		t.Error("a ticket that was never returned is described as sent back")
	}

	returned := fresh
	returned.Comments = []Comment{
		{CommentID: "c1", Body: "**Security review** — first round\n\n- **high** `a.go` — an OLD finding\n" + returnedMarker,
			CreatedAt: time.Unix(10, 0)},
		{CommentID: "c2", Body: "**Security review** — second round\n\n- **high** `a.go` — hardcoded password\n" +
			returnedMarker + "\n\n<sub>model · 1/2 tokens · 3ms</sub>", CreatedAt: time.Unix(20, 0)},
	}

	why := returnReason(returned)
	if !strings.Contains(why, "hardcoded password") {
		t.Errorf("the latest finding is missing: %q", why)
	}
	// The round that is over must not be re-litigated.
	if strings.Contains(why, "an OLD finding") {
		t.Errorf("a superseded round is quoted back: %q", why)
	}
	// Neither the machine marker nor the token footer is something to act on.
	if strings.Contains(why, returnedMarker) || strings.Contains(why, "<sub>") {
		t.Errorf("machine detail leaked into the reason: %q", why)
	}

	out = renderDevState(returned, &devState{budget: 8})
	if !strings.Contains(out, "SENT BACK") || !strings.Contains(out, "hardcoded password") {
		t.Errorf("the prompt does not tell the agent what to fix:\n%s", out)
	}
	// It must be framed as a correction, not a fresh start.
	if !strings.Contains(out, "do not start over") {
		t.Error("the prompt does not say the change already exists; the agent may rewrite it from scratch")
	}
}

// RED IS SUCCESS for the test author, and every instinct in a coding model says
// otherwise. The one "fix" available to it — weakening the test until it passes —
// is exactly what this stage exists to prevent, so the brief has to invert the
// usual goal explicitly rather than leave it implied.
func TestTesterPromptInvertsTheUsualGoal(t *testing.T) {
	p := testerSystemPrompt()
	for _, want := range []string{
		"BEFORE the implementation exists",
		"WILL NOT COMPILE OR PASS",
		"ONLY edit *_test.go",
		"Do NOT weaken a test",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("the test author's brief is missing %q", want)
		}
	}
	// It must not be told to make anything pass.
	if strings.Contains(p, "Must pass before you can finish") {
		t.Error("the test author is told its tests must pass; they cannot, and it would weaken them trying")
	}
}

// The developer's brief is the other half of the same rule: the tests are
// already there, they are the specification, and they are not its to edit.
func TestDevPromptSaysTestsAreAlreadyWrittenAndNotItsToEdit(t *testing.T) {
	p := devSystemPrompt()
	for _, want := range []string{
		"TESTS ARE ALREADY WRITTEN",
		"may NOT edit any *_test.go",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("the developer's brief is missing %q", want)
		}
	}
}

// The two modes must not be handed the same brief: a test author told to write
// the implementation would write it, and the split would exist only on paper.
func TestEachModeGetsItsOwnBrief(t *testing.T) {
	dev := NewDevAgent(nil, nil, ClassLarge, RepoConfig{}, 8)
	tester := NewTesterAgent(nil, nil, ClassSmall, RepoConfig{}, 8)
	if dev.systemPrompt(Ticket{}) == tester.systemPrompt(Ticket{}) {
		t.Fatal("both modes share one system prompt")
	}
	if dev.systemPrompt(Ticket{}) != devSystemPrompt() {
		t.Error("the developer is not given the developer brief")
	}
	if tester.systemPrompt(Ticket{}) != testerSystemPrompt() {
		t.Error("the test author is not given the test-author brief")
	}
}

// TOOL CALLS ARE THE INTERFACE NOW. A typed call cannot put a file body in the
// wrong field, omit a required argument, or carry a delimiter copied from the
// prompt — the three failures that cost whole runs under the hand-written
// envelope.
func TestReplyFromAToolCallBecomesAnAction(t *testing.T) {
	res := ChatResult{Calls: []ToolCall{{
		Name:      actionWriteFiles,
		Arguments: `{"edits":[{"path":"main.go","start_line":3,"end_line":4,"replace":"new"}],"summary":"s","type":"fix"}`,
	}}}
	act, err := parseDevReply(res, modeDevelop)
	if err != nil {
		t.Fatalf("parseDevReply() = %v", err)
	}
	if act.Action != actionWriteFiles {
		t.Errorf("action = %q, want %q", act.Action, actionWriteFiles)
	}
	if len(act.Edits) != 1 || act.Edits[0].StartLine != 3 || act.Edits[0].EndLine != 4 || act.Edits[0].Replace != "new" {
		t.Errorf("edits = %+v", act.Edits)
	}
	if act.Summary != "s" || act.Type != "fix" {
		t.Errorf("summary/type lost: %+v", act)
	}
}

// Backends disagree about whether "no arguments" is "{}", "" or "null", and a
// reply must not be lost to that. Parsing is deliberately separate from
// validating what the arguments contain.
func TestToolCallWithNoArgumentsIsAccepted(t *testing.T) {
	for _, args := range []string{"", "{}", "null", "  "} {
		act, err := parseDevReply(ChatResult{Calls: []ToolCall{{Name: actionReadFiles, Arguments: args}}}, modeDevelop)
		if err != nil {
			t.Fatalf("arguments %q: %v", args, err)
		}
		if act.Action != actionReadFiles {
			t.Errorf("arguments %q gave action %q", args, act.Action)
		}
	}
}

// The arguments must never be able to redefine which tool was called — that
// would reintroduce the envelope through the back door.
func TestToolCallArgumentsCannotOverrideTheAction(t *testing.T) {
	act, err := parseDevReply(ChatResult{Calls: []ToolCall{{
		Name:      actionReadFiles,
		Arguments: `{"action":"write_files"}`,
	}}}, modeDevelop)
	if err != nil {
		t.Fatalf("parseDevReply() = %v", err)
	}
	if act.Action != actionReadFiles {
		t.Errorf("action = %q; the arguments overrode the tool that was called", act.Action)
	}
}

func TestUnknownToolIsRejected(t *testing.T) {
	_, err := parseDevReply(ChatResult{Calls: []ToolCall{{Name: "rm_rf", Arguments: "{}"}}}, modeDevelop)
	if err == nil {
		t.Fatal("an unknown tool was accepted")
	}
}

// A backend that ignores tools and answers with content must still work — not
// every server the department may talk to supports function calling.
func TestReplyFallsBackToContentWhenNoToolIsCalled(t *testing.T) {
	res := ChatResult{Content: `{"action":"read_files","paths":["main.go"]}`}
	act, err := parseDevReply(res, modeDevelop)
	if err != nil {
		t.Fatalf("parseDevReply() = %v", err)
	}
	if act.Action != actionReadFiles || len(act.Paths) != 1 {
		t.Errorf("act = %+v", act)
	}
}

// Both modes must offer the same actions — a tester with no run_tests could
// never finish — while differing in what they say about editing.
func TestBothModesOfferEveryActionWithTheRightEditRule(t *testing.T) {
	for _, mode := range []agentMode{modeDevelop, modeTest} {
		names := map[string]string{}
		for _, tool := range devTools(mode) {
			names[tool.Name] = tool.Description
		}
		for _, want := range devActions {
			if _, ok := names[want]; !ok {
				t.Errorf("mode %v does not offer %q", mode, want)
			}
		}
		write := names[actionWriteFiles]
		if mode == modeTest && !strings.Contains(write, "ONLY edit *_test.go") {
			t.Errorf("the test author's write tool does not restrict it to tests:\n%s", write)
		}
		if mode == modeDevelop && !strings.Contains(write, "NOT edit *_test.go") {
			t.Errorf("the developer's write tool does not forbid editing tests:\n%s", write)
		}
	}
}

// Offering tools and a response_format together confuses backends that support
// both: the grammar forces a JSON document, which is not the shape a tool call
// comes back in.
func TestSchemaIsDroppedWhenToolsAreOffered(t *testing.T) {
	if got := wireTools(nil); got != nil {
		t.Error("an empty tool list still produced a tools field")
	}
	if got := wireTools([]Tool{{Name: "x", Parameters: map[string]any{}}}); len(got) != 1 || got[0].Type != "function" {
		t.Errorf("wireTools = %+v", got)
	}
}

// SUCCESS NO LONGER NEEDS NAMING, because the model cannot act on it: a passing
// verification ends the stage in the loop itself. The prompt's job here is to
// report the result, not to instruct — the previous version told the agent to
// "call finish now", which is advice for a tool that no longer exists.
func TestAPassingVerificationIsReportedWithoutAskingForAnAction(t *testing.T) {
	passed := &devState{
		read: map[string]string{}, staged: map[string]string{"main.go": "package main\n"},
		missing: map[string]bool{}, tree: []string{"main.go"},
		lastTest: "Tests passed on branch agent/t1.", testsPass: true,
		writes: 1, verifiedWrites: 1, iteration: 4, budget: 50,
	}
	out := renderDevState(Ticket{TicketID: "T-1"}, passed)
	if !strings.Contains(out, "PASSED") {
		t.Errorf("the verification result is not reported at all:\n%s", out)
	}
	for _, gone := range []string{"call finish", "run_tests", "give_up"} {
		if strings.Contains(out, gone) {
			t.Errorf("the prompt still names %q, which is no longer an action:\n%s", gone, out)
		}
	}
}

// A TOOL CALL EMITTED AS TEXT IS STILL A TOOL CALL. Not every chat template is
// parsed into tool_calls by the server: qwen2.5-coder writes the call inside
// <tools> tags and llama.cpp returns it as content. Rejecting that would mean
// refusing a model that did exactly what it was asked, for a reason it cannot
// see — and it is why a 14B on the second GPU could not be used at all.
func TestAToolCallWrittenAsTextIsAccepted(t *testing.T) {
	// The exact shape qwen2.5-coder:14b returned.
	res := ChatResult{Content: "<tools>\n{\n  \"name\": \"read_files\",\n  \"arguments\": {\n    \"paths\": [\"main.go\", \"go.mod\"]\n  }\n}\n</tools>\n\n\n"}
	act, err := parseDevReply(res, modeDevelop)
	if err != nil {
		t.Fatalf("parseDevReply() = %v", err)
	}
	if act.Action != actionReadFiles {
		t.Errorf("action = %q, want %q", act.Action, actionReadFiles)
	}
	if len(act.Paths) != 2 || act.Paths[0] != "main.go" {
		t.Errorf("paths = %v", act.Paths)
	}
}

// The wrapper varies by model family; the contents do not.
func TestToolCallTextAcceptsTheCommonWrappers(t *testing.T) {
	for _, tag := range []string{"tools", "tool_call", "tool_calls", "function_call"} {
		body := fmt.Sprintf("<%s>{\"name\":\"read_files\",\"arguments\":{\"paths\":[\"main.go\"]}}</%s>", tag, tag)
		act, err := parseDevReply(ChatResult{Content: body}, modeDevelop)
		if err != nil || act.Action != actionReadFiles {
			t.Errorf("<%s> wrapper not understood: %v / %+v", tag, err, act)
		}
	}
	// Bare, with no wrapper at all.
	act, err := parseDevReply(ChatResult{Content: `{"name":"read_files","arguments":{"paths":["main.go"]}}`}, modeDevelop)
	if err != nil || act.Action != actionReadFiles {
		t.Errorf("an unwrapped tool call was not understood: %v / %+v", err, act)
	}
	// And inside a code fence, which models add unprompted.
	act, err = parseDevReply(ChatResult{Content: "```json\n{\"name\":\"read_files\",\"arguments\":{\"paths\":[\"a.go\"]}}\n```"}, modeDevelop)
	if err != nil || act.Action != actionReadFiles || len(act.Paths) != 1 {
		t.Errorf("a fenced tool call was not understood: %v / %+v", err, act)
	}
}

// The hand-written envelope must still work: it is a different shape, and both
// are things models actually emit.
func TestTheEnvelopeShapeStillParses(t *testing.T) {
	act, err := parseDevReply(ChatResult{Content: `{"action":"read_files","paths":["a.go"]}`}, modeDevelop)
	if err != nil || act.Action != actionReadFiles {
		t.Errorf("the envelope shape stopped working: %v / %+v", err, act)
	}
	// An envelope must NOT be mistaken for a tool call: it has no "name".
	if _, ok := toolCallFromText(`{"action":"read_files","paths":["a.go"]}`); ok {
		t.Error("an envelope was read as a tool call")
	}
	// Nor prose.
	if _, ok := toolCallFromText("I will now read the files."); ok {
		t.Error("prose was read as a tool call")
	}
}

// THE SAME FENCE BUG THAT BROKE THE ARCHITECT, on the developer's hot path.
// This reply carries file CONTENT, so a fence inside the payload is ordinary —
// a README, a doc comment with an example. extractFenced takes the first ```
// in the string, so running it over a well-formed action returned the payload's
// snippet and threw the action away.
func TestDevActionSurvivesFencesInsideFileContent(t *testing.T) {
	raw := `{"action":"write_files","edits":[{"path":"README.md","search":"","replace":"# Tracker\n\n` +
		"```" + `bash\ngo build ./...\n` + "```" + `\n"}],"summary":"add readme"}`
	act, err := parseDevAction(raw)
	if err != nil {
		t.Fatalf("a valid action containing a markdown fence was rejected: %v", err)
	}
	if act.Action != actionWriteFiles {
		t.Fatalf("action = %q, want %q", act.Action, actionWriteFiles)
	}
	if len(act.Edits) != 1 || !strings.Contains(act.Edits[0].Replace, "go build ./...") {
		t.Errorf("the fenced snippet was lost from the payload: %+v", act.Edits)
	}
}

// The unwrapping extractFenced exists for must still work: a model that writes a
// preamble and then a fenced object is the case it was written for.
func TestFencedDevActionIsStillUnwrapped(t *testing.T) {
	raw := "Here is my next step:\n\n```json\n{\"action\":\"read_files\",\"paths\":[\"main.go\"]}\n```"
	act, err := parseDevAction(raw)
	if err != nil {
		t.Fatalf("a fenced action must still parse: %v", err)
	}
	if act.Action != actionReadFiles {
		t.Errorf("action = %q, want %q", act.Action, actionReadFiles)
	}
}

// A spec that asserts a length and then indexes the result panics whenever the
// length is wrong, and the panic buries the assertion that explains it behind
// twenty lines of runtime stack. Measured twice: the developer read a crash
// instead of "expected 2 items, got 0", and both times failed the ticket.
func TestTestAuthorIsToldToStopOnAssertionsTheRestDependsOn(t *testing.T) {
	p := testerSystemPrompt()
	for _, want := range []string{"require.Len", "require.NoError", "panic"} {
		if !strings.Contains(p, want) {
			t.Errorf("the test author is not told about %q", want)
		}
	}
	// And it must still know when plain assert is right, or it will use require
	// for everything and stop the suite at the first cosmetic difference.
	if !strings.Contains(p, "Use assert for a check the following lines do not depend on") {
		t.Error("the prompt does not say when assert is still the right choice")
	}
}

// THE HAND-BACK MUST BE READABLE, not merely countable. endOnBrokenSpec posts a
// comment naming the file and the fault, and specRepairsSoFar counts those
// comments to bound the round trips — so the ceiling worked while the message
// went nowhere, because returnReason matched the reviewer's marker alone.
//
// Measured on r68: a section panicked in its own fixture, the developer handed it
// back twice, and both times the author opened "I'll write the unit tests for the
// status parameter filtering functionality" — it did not know it was a repair. It
// rewrote all 215 lines and reproduced the identical bug, twice.
func TestTheSpecHandBackReachesTheAuthorsPrompt(t *testing.T) {
	tk := Ticket{
		TicketID: "t1", Title: "Implement status parameter filtering",
		Comments: []Comment{{
			CommentID: "c1",
			Body: specRepairMarker + "\n\nThe tests in `handlers_status_test.go` PANIC before their " +
				"assertions run, from the tests' own setup.\n\n```\nmalformed HTTP version \"2 HTTP/1.0\"\n```\n<sub>420 tokens</sub>",
		}},
	}
	got := renderDevState(tk, &devState{})

	if !strings.Contains(got, "handlers_status_test.go") {
		t.Error("the author is never told which file it must fix")
	}
	if !strings.Contains(got, "malformed HTTP version") {
		t.Error("the failing output is not in the prompt, so the author cannot see the fault")
	}
	// The operative instruction: the observed failure was a full-file rewrite.
	if !strings.Contains(got, "do not rewrite the file") {
		t.Error("nothing tells the author to correct rather than start over")
	}
	// Machine markers and the footer are not something to act on.
	if strings.Contains(got, specRepairMarker) {
		t.Error("the raw marker leaked into the prompt")
	}
	if strings.Contains(got, "<sub>") {
		t.Error("the token footer leaked into the prompt")
	}
}

// A ticket nobody sent back must not claim it was.
func TestNoHandBackNoRepairBanner(t *testing.T) {
	got := renderDevState(Ticket{TicketID: "t2", Title: "Fresh"}, &devState{})
	if strings.Contains(got, "SENT THIS SPECIFICATION BACK") {
		t.Error("a fresh ticket is announced as a repair")
	}
}

// The reviewer's send-back and the developer's must not be confused: they go to
// different agents and say opposite things about who owns the file.
func TestReviewerAndSpecHandBacksStaySeparate(t *testing.T) {
	tk := Ticket{TicketID: "t3", Comments: []Comment{
		{CommentID: "c1", Body: returnedMarker + "\n\nreviewer found a nil deref"},
	}}
	got := renderDevState(tk, &devState{})
	if !strings.Contains(got, "SENT BACK BY THE REVIEWER") {
		t.Error("the reviewer's send-back stopped rendering")
	}
	if strings.Contains(got, "SENT THIS SPECIFICATION BACK") {
		t.Error("a reviewer send-back was rendered as a specification repair")
	}
}

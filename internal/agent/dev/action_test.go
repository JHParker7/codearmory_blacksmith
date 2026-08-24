package dev

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/model"
)

const fence = "```"

// THREE CHANNELS, ALL ACCEPTED. Refusing any of them would mean rejecting a
// model that did exactly what it was asked, for a reason it cannot see.
func TestATurnIsReadFromWhicheverChannelTheModelUsed(t *testing.T) {
	want := func(a Action, err error) func(*testing.T) {
		return func(t *testing.T) {
			t.Helper()
			if err != nil {
				t.Fatalf("the turn was refused: %v", err)
			}
			if a.Action != ActionReadFiles || len(a.Paths) != 2 || a.Paths[0] != "store.go" {
				t.Fatalf("read the turn as %+v", a)
			}
		}
	}

	t.Run("a parsed tool call", want(ParseReply(model.ChatResult{
		Calls: []model.ToolCall{{
			Name: ActionReadFiles, Arguments: `{"paths":["store.go","server.go"]}`,
		}},
	}, ModeDevelop)))

	// qwen2.5-coder writes {"name":…,"arguments":{…}} inside <tools> tags and
	// llama.cpp hands it back as content.
	t.Run("a tool call written into the content", want(ParseReply(model.ChatResult{
		Content: `<tools>{"name":"read_files","arguments":{"paths":["store.go","server.go"]}}</tools>`,
	}, ModeDevelop)))

	t.Run("a hand-written envelope", want(ParseReply(model.ChatResult{
		Content: `{"action":"read_files","paths":["store.go","server.go"]}`,
	}, ModeDevelop)))

	// A PARSED CALL WINS over content, because a backend that produced one has
	// already decided what the model meant.
	t.Run("both, with the call preferred", want(ParseReply(model.ChatResult{
		Calls:   []model.ToolCall{{Name: ActionReadFiles, Arguments: `{"paths":["store.go","server.go"]}`}},
		Content: `{"action":"undo_edit"}`,
	}, ModeDevelop)))
}

func TestEveryWrapperModelsUseIsUnwrapped(t *testing.T) {
	for _, tag := range ToolCallWrappers {
		body := `{"name":"undo_edit","arguments":{}}`
		got, ok := ToolCallFromText("<" + tag + ">" + body + "</" + tag + ">")
		if !ok || got.Name != ActionUndoEdit {
			t.Errorf("<%s> was not unwrapped: %+v ok=%v", tag, got, ok)
		}
	}
	// And a fenced one, which is the other thing models do.
	got, ok := ToolCallFromText(fence + "json\n" + `{"name":"undo_edit","arguments":null}` + "\n" + fence)
	if !ok || got.Name != ActionUndoEdit {
		t.Errorf("a fenced tool call was not read: %+v ok=%v", got, ok)
	}
	// Null arguments become an empty object rather than a decode failure.
	if got.Arguments != "{}" {
		t.Errorf("Arguments = %q, want an empty object", got.Arguments)
	}
}

// AN ENVELOPE IS NOT A TOOL CALL. Without the name check, {"action":"read_files"}
// would be read as a tool call named "" and refused, when ParseAction reads it
// perfectly well.
func TestAnEnvelopeIsNotMistakenForAToolCall(t *testing.T) {
	if _, ok := ToolCallFromText(`{"action":"read_files","paths":["a.go"]}`); ok {
		t.Error("a hand-written envelope was read as a tool call")
	}
	for _, raw := range []string{"", "   ", "I will read the store.", "not json {"} {
		if _, ok := ToolCallFromText(raw); ok {
			t.Errorf("%q was read as a tool call", raw)
		}
	}
}

// ARGUMENTS MUST NEVER REDEFINE WHICH TOOL WAS CALLED: the name is the one field
// the caller already knows, and a model that repeats it wrongly would otherwise
// run a different action than the one it selected.
func TestTheToolNameWinsOverItsArguments(t *testing.T) {
	got, err := ActionFromCall(model.ToolCall{
		Name:      ActionUndoEdit,
		Arguments: `{"action":"write_files","edits":[{"path":"store.go","decl":"main","replace":"x"}]}`,
	}, ModeDevelop)
	if err != nil {
		t.Fatalf("ActionFromCall: %v", err)
	}
	if got.Action != ActionUndoEdit {
		t.Errorf("Action = %q; the arguments redefined the tool that was called", got.Action)
	}
}

// A REFUSAL MUST BE ACTIONABLE: naming the symptom and not the cause sends a
// correct agent to the wrong place.
func TestAnUnknownToolIsRefusedWithTheOnesThatExist(t *testing.T) {
	_, err := ActionFromCall(model.ToolCall{Name: "delete_repository"}, ModeDevelop)
	if err == nil {
		t.Fatal("an unknown tool was accepted")
	}
	for _, want := range Actions {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

func TestAToolCallWithNoArgumentsIsStillAnAction(t *testing.T) {
	for _, args := range []string{"", "   ", "null", "{}"} {
		got, err := ActionFromCall(model.ToolCall{Name: ActionUndoEdit, Arguments: args}, ModeDevelop)
		if err != nil {
			t.Errorf("arguments %q: %v", args, err)
			continue
		}
		if got.Action != ActionUndoEdit {
			t.Errorf("arguments %q gave %+v", args, got)
		}
	}
}

func TestBrokenArgumentsAreRefusedByName(t *testing.T) {
	_, err := ActionFromCall(model.ToolCall{
		Name: ActionWriteFiles, Arguments: `{"edits": [`,
	}, ModeDevelop)
	if err == nil {
		t.Fatal("unparseable arguments were accepted")
	}
	if !strings.Contains(err.Error(), ActionWriteFiles) {
		t.Errorf("the refusal does not say which tool: %v", err)
	}
}

// THIS PATH CARRIES FILE CONTENTS, so a fence inside the payload is ordinary.
// Unwrapping a reply that is already an object would return the payload's
// snippet and throw the action away.
func TestAnEnvelopeCarryingAFenceIsNotMangled(t *testing.T) {
	content := "# Title\n\n" + fence + "bash\ngo build ./...\n" + fence + "\n"
	raw, err := json.Marshal(Action{
		Action:  ActionWriteFiles,
		Summary: "document it",
		Type:    "docs",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Splice the fenced content into the replace field the way a model would.
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	obj["edits"] = []map[string]any{{"path": "README.md", "decl": "", "replace": content}}
	spliced, _ := json.Marshal(obj)

	got, err := ParseAction(string(spliced))
	if err != nil {
		t.Fatalf("an envelope containing a fence was refused: %v", err)
	}
	if len(got.Edits) != 1 || !strings.Contains(got.Edits[0].Replace, "go build ./...") {
		t.Errorf("the payload was cut at its fence: %+v", got.Edits)
	}
}

func TestEveryWayAnEnvelopeCanBeUnusable(t *testing.T) {
	cases := map[string]string{
		"no object at all":     "I will now edit the store.",
		"empty":                "",
		"no action field":      `{"paths":["a.go"]}`,
		"a blank action":       `{"action":"   "}`,
		"an action nobody has": `{"action":"delete_everything"}`,
	}
	for name, raw := range cases {
		if _, err := ParseAction(raw); err == nil {
			t.Errorf("%s: %q was accepted", name, raw)
		}
	}
}

// finish IS ACCEPTED THOUGH IT IS NOT OFFERED: a model asking to finish has said
// something meaningful and the loop decides what to do with it. An unknown word
// has not.
func TestFinishIsUnderstoodEvenThoughItIsNotOffered(t *testing.T) {
	got, err := ParseAction(`{"action":"finish","summary":"done","type":"feat"}`)
	if err != nil {
		t.Fatalf("finish was refused: %v", err)
	}
	if got.Action != ActionFinish {
		t.Errorf("Action = %q", got.Action)
	}
	// But it is not in the offered vocabulary, so a tool CALL naming it is
	// refused: the model was never given it.
	if _, err := ActionFromCall(model.ToolCall{Name: ActionFinish}, ModeDevelop); err == nil {
		t.Error("a tool call named finish was accepted; it is not offered")
	}
}

// THE MODEL CHOOSES WHAT TO CHANGE, NOT WHEN TO CHECK IT. In the last fixture
// pass 28 of 31 refusals were the harness telling the agent it could not run the
// tests just now — every one existing only because asking was possible.
func TestTheLoopOffersNoWayToAskForVerificationOrToQuit(t *testing.T) {
	for _, mode := range []Mode{ModeDevelop, ModeTest, ModeCoverage} {
		for _, gone := range []string{ActionRunTests, ActionFinish, ActionGiveUp} {
			for _, name := range ActionsFor(mode) {
				if name == gone {
					t.Errorf("mode %d still offers %q", mode, gone)
				}
			}
			for _, tool := range Tools(mode) {
				if tool.Name == gone {
					t.Errorf("mode %d offers the %q tool", mode, gone)
				}
			}
		}
	}
}

// THE OFFERED TOOLS AND THE ACCEPTED ACTIONS CANNOT DRIFT APART. A tool offered
// but not accepted is a trap of exactly the kind removing them was meant to end.
func TestEveryOfferedToolIsAnAcceptedAction(t *testing.T) {
	for _, mode := range []Mode{ModeDevelop, ModeTest, ModeCoverage} {
		offered := Tools(mode)
		if len(offered) != len(ActionsFor(mode)) {
			t.Errorf("mode %d offers %d tools for %d actions", mode, len(offered), len(ActionsFor(mode)))
		}
		for _, tool := range offered {
			if _, err := ActionFromCall(model.ToolCall{Name: tool.Name}, mode); err != nil {
				if !strings.Contains(err.Error(), "not valid JSON") {
					t.Errorf("mode %d offers %q but refuses it: %v", mode, tool.Name, err)
				}
			}
		}
	}
}

// EACH STAGE'S RULE IS ON THE TOOL, where the model is choosing arguments,
// rather than two thousand tokens earlier in a system prompt.
func TestEachStageIsToldWhichFilesItMayWrite(t *testing.T) {
	rules := map[Mode][]string{
		ModeDevelop:  {"may NOT edit *_test.go"},
		ModeTest:     {"ONLY edit *_test.go"},
		ModeCoverage: {"ONLY create NEW *_test.go", "cannot be edited"},
	}
	for mode, wants := range rules {
		var write model.Tool
		for _, tool := range Tools(mode) {
			if tool.Name == ActionWriteFiles {
				write = tool
			}
		}
		if write.Name == "" {
			t.Fatalf("mode %d offers no way to write", mode)
		}
		for _, want := range wants {
			if !strings.Contains(write.Description, want) {
				t.Errorf("mode %d does not say %q:\n%s", mode, want, write.Description)
			}
		}
	}

	// THE CHECK MEANS SOMETHING DIFFERENT PER STAGE, and assuming otherwise is
	// the mistake: the author's tests CANNOT pass, because the code they describe
	// does not exist yet.
	if !strings.Contains(ModeTest.CheckDescription(), "does not run them") {
		t.Error("the author is not told its check does not run its tests")
	}
	if !strings.Contains(ModeCoverage.CheckDescription(), "coverage") {
		t.Error("the coverage stage is not told what its check measures")
	}
}

// NEVER PUT THE SAME TEXT IN old_str AND replace — stated on the tool because
// that is where the copy happens.
func TestTheWriteToolNamesTheCopyItMustNotMake(t *testing.T) {
	var write model.Tool
	for _, tool := range Tools(ModeDevelop) {
		if tool.Name == ActionWriteFiles {
			write = tool
		}
	}
	for _, want := range []string{
		"NEVER put the same text in old_str and replace",
		"must appear exactly once",
		"What you do not name, you do not change",
	} {
		if !strings.Contains(write.Description, want) {
			t.Errorf("the write tool does not say %q", want)
		}
	}
}

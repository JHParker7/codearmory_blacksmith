package dev

import (
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/model"
)

// toolCall is the shape a gateway hands back.
func toolCall(name, args string) model.ToolCall {
	return model.ToolCall{Name: name, Arguments: args}
}

// THE SHAPE THAT KILLED A STAGE ON A LIVE RUN.
//
// Every hosted tool-calling API takes the arguments object as a string, so a
// model that has learned to stringify its arguments stringifies the values
// inside them too. A spec-agent emitted this on three consecutive attempts at
// ~3,500 completion tokens each, was told only "unparseable model output", and
// the stage died having thrown away work that was correct.
func TestEditsArriveAsAStringifiedArray(t *testing.T) {
	args := `{"edits":"[{\"path\": \"store_test.go\", \"decl\": \"TestX\", \"replace\": \"func TestX(t *testing.T) {}\"}]"}`

	act, err := ActionFromCall(toolCall(ActionWriteFiles, args), ModeDevelop)
	if err != nil {
		t.Fatalf("a stringified edits array was refused: %v", err)
	}
	if len(act.Edits) != 1 {
		t.Fatalf("got %d edits, want the one inside the string", len(act.Edits))
	}
	if act.Edits[0].Path != "store_test.go" || act.Edits[0].Decl != "TestX" {
		t.Errorf("the edit came back as %+v", act.Edits[0])
	}
}

// The ordinary shape must be untouched by the tolerance.
func TestEditsArriveAsAnArray(t *testing.T) {
	args := `{"edits":[{"path":"store.go","old_str":"return nil","replace":"return []Task{}"}]}`

	act, err := ActionFromCall(toolCall(ActionWriteFiles, args), ModeDevelop)
	if err != nil {
		t.Fatalf("an ordinary edits array was refused: %v", err)
	}
	if len(act.Edits) != 1 || act.Edits[0].OldStr != "return nil" {
		t.Fatalf("the edit came back as %+v", act.Edits)
	}
}

func TestPathsArriveAsAStringifiedArray(t *testing.T) {
	act, err := ActionFromCall(
		toolCall(ActionReadFiles, `{"paths":"[\"main.go\", \"store.go\"]"}`), ModeDevelop)
	if err != nil {
		t.Fatalf("a stringified paths array was refused: %v", err)
	}
	if len(act.Paths) != 2 || act.Paths[0] != "main.go" {
		t.Fatalf("paths came back as %v", act.Paths)
	}
}

// A BARE PATH IS NOT AN ERROR. "main.go" is a string that is not a JSON array,
// and it plainly means one file — refusing it would be refusing a reply whose
// intent is unambiguous.
func TestASinglePathArrivesAsABareString(t *testing.T) {
	act, err := ActionFromCall(toolCall(ActionReadFiles, `{"paths":"main.go"}`), ModeDevelop)
	if err != nil {
		t.Fatalf("a single bare path was refused: %v", err)
	}
	if len(act.Paths) != 1 || act.Paths[0] != "main.go" {
		t.Fatalf("paths came back as %v", act.Paths)
	}
}

// TOLERANCE IS NOT SILENCE. A string that is neither an array nor a path must
// still be refused, and the refusal must name what was wrong with it — a
// message that says only "unparseable" is what made the model repeat itself.
func TestAnUnreadableStringifiedArrayIsStillRefused(t *testing.T) {
	_, err := ActionFromCall(
		toolCall(ActionWriteFiles, `{"edits":"[{not json at all"}`), ModeDevelop)
	if err == nil {
		t.Fatal("a string that is not an edits array was accepted")
	}
	if !strings.Contains(err.Error(), "edits") {
		t.Errorf("the refusal does not name the field at fault: %v", err)
	}
}

// The content fallback reads the same shapes, because a model that writes its
// tool call into the content double-encodes there too.
func TestTheContentFallbackAlsoReadsAStringifiedArray(t *testing.T) {
	act, err := ParseAction(`{"action":"write_files","edits":"[{\"path\":\"a.go\",\"decl\":\"X\",\"replace\":\"func X(){}\"}]"}`)
	if err != nil {
		t.Fatalf("the content fallback refused a stringified edits array: %v", err)
	}
	if len(act.Edits) != 1 || act.Edits[0].Path != "a.go" {
		t.Fatalf("edits came back as %+v", act.Edits)
	}
}

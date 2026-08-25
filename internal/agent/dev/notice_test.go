package dev

import (
	"errors"
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/model"
)

// THE REFUSAL MUST NAME THE SHAPE, NOT THE SYMPTOM.
//
// A model that stringifies its own arguments escapes Go source three levels
// deep, and a small model does not get through a test file's worth of that
// without breaking it. Measured on a live run at 2,334 and 3,033 completion
// tokens against a 4,000 ceiling — so the reply ended on its own, malformed,
// and it was NOT truncation. Told only "your previous reply was rejected", the
// model sent the identical shape again and the stage died on the third attempt.
func TestABrokenStringifiedArgumentIsToldWhatShapeToUse(t *testing.T) {
	res := model.ChatResult{
		Calls: []model.ToolCall{{
			Name:      ActionWriteFiles,
			Arguments: `{"edits":"[{\"path\": \"store_test.go\", \"replace\": \"package ma`,
		}},
		CompletionTokens: 3033,
	}

	got := ParseFailureNotice(res, errors.New("unexpected EOF"))

	if !strings.Contains(got, "JSON ARRAY") {
		t.Errorf("the notice does not say what shape to use instead:\n%s", got)
	}
	if !strings.Contains(got, "RIGHT") || !strings.Contains(got, "WRONG") {
		t.Errorf("the notice does not show the two shapes side by side:\n%s", got)
	}
	if strings.Contains(got, "CUT OFF") {
		t.Errorf("a reply that ended well inside the ceiling was called truncated:\n%s", got)
	}
}

// TRUNCATION IS STILL TRUNCATION. The two read the same and need opposite
// fixes, and the finish reason is what separates them — a stringified argument
// that really was cut off must be told to write less, not to change its shape.
func TestARealTruncationIsStillReportedAsOne(t *testing.T) {
	res := model.ChatResult{
		Content:      `{"action":"write_files","edits":[{"path":"a.go"`,
		FinishReason: model.FinishLength,
	}

	got := ParseFailureNotice(res, errors.New("unexpected EOF"))
	if !strings.Contains(got, "CUT OFF") {
		t.Errorf("a reply that hit the token limit was not told so:\n%s", got)
	}
}

// AN ORDINARY MALFORMED REPLY STILL GETS THE ERROR. The specific notices are
// for shapes worth naming; everything else must still say what went wrong
// rather than falling through to silence.
func TestAnOrdinaryParseFailureStillQuotesTheError(t *testing.T) {
	res := model.ChatResult{Content: `not json at all`}

	got := ParseFailureNotice(res, errors.New("reply is not valid JSON"))
	if !strings.Contains(got, "not valid JSON") {
		t.Errorf("the notice dropped the error:\n%s", got)
	}
	if strings.Contains(got, "JSON ARRAY") {
		t.Errorf("an ordinary failure was blamed on stringified arguments:\n%s", got)
	}
}

// A WELL-FORMED STRINGIFIED ARRAY IS ACCEPTED, not lectured at — the decoder
// reads it, so it never reaches a notice at all. This pins the pair: tolerate
// what can be read, and explain only what cannot.
func TestAReadableStringifiedArrayNeverReachesTheNotice(t *testing.T) {
	act, err := ActionFromCall(toolCall(ActionWriteFiles,
		`{"edits":"[{\"path\": \"a.go\", \"decl\": \"X\", \"replace\": \"func X(){}\"}]"}`),
		ModeDevelop)
	if err != nil {
		t.Fatalf("a readable stringified array was refused: %v", err)
	}
	if len(act.Edits) != 1 {
		t.Fatalf("got %d edits, want the one inside the string", len(act.Edits))
	}
}

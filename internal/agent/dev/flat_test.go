package dev

import (
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/edit"
	"github.com/code-armory-app/blacksmith/internal/model"
)

// THE SHAPE THE MODEL CAN ACTUALLY CLOSE.
//
// The batched tool asked for an array of objects each carrying a whole file.
// Measured on a live run: the model produced 8,236 characters of triple-escaped
// JSON and transposed the final two brackets — "]}"  where "}]" belonged — three
// attempts running, complete envelope, finish_reason tool_calls, not truncation.
// One edit at the top level has no inner array to close.
func TestAFlatWriteBecomesTheOneEditItDescribes(t *testing.T) {
	act, err := ActionFromCall(toolCall(ActionWriteFile,
		`{"path":"store.go","old_str":"return nil","replace":"return []Task{}",`+
			`"summary":"return a slice","type":"fix"}`), ModeDevelop)
	if err != nil {
		t.Fatalf("a flat write was refused: %v", err)
	}
	if len(act.Edits) != 1 {
		t.Fatalf("got %d edits, want the single one it described", len(act.Edits))
	}
	got := act.Edits[0]
	if got.Path != "store.go" || got.OldStr != "return nil" || got.Replace != "return []Task{}" {
		t.Errorf("the edit came back as %+v", got)
	}
	if act.Summary != "return a slice" || act.Type != "fix" {
		t.Errorf("summary/type were lost: %q %q", act.Summary, act.Type)
	}
}

// EVERY WAY OF SAYING WHERE STILL WORKS, flat.
func TestTheFlatWriteCarriesEachAddressingMode(t *testing.T) {
	cases := map[string]string{
		"old_str":    `{"path":"a.go","old_str":"x","replace":"y","summary":"s","type":"fix"}`,
		"decl":       `{"path":"a.go","decl":"main","replace":"func main(){}","summary":"s","type":"fix"}`,
		"lines":      `{"path":"a.go","start_line":2,"end_line":3,"replace":"y","summary":"s","type":"fix"}`,
		"whole file": `{"path":"a.go","replace":"package main\n","summary":"s","type":"feat"}`,
	}
	for want, args := range cases {
		act, err := ActionFromCall(toolCall(ActionWriteFile, args), ModeDevelop)
		if err != nil {
			t.Errorf("%s: refused: %v", want, err)
			continue
		}
		mode, ok := act.Edits[0].Address()
		if !ok || mode != want {
			t.Errorf("%s: read back as %q (ok=%v)", want, mode, ok)
		}
	}
}

// THE SCHEMA NO LONGER SAYS "EXACTLY ONE ADDRESS", so the refusal has to — and
// it has to name which ones were given. "Invalid edit" sends a correct agent to
// the wrong place, which is this repository's most expensive failure class.
func TestAFlatWriteNamingTwoAddressesIsRefusedByName(t *testing.T) {
	_, err := ActionFromCall(toolCall(ActionWriteFile,
		`{"path":"a.go","old_str":"x","decl":"main","replace":"y","summary":"s","type":"fix"}`),
		ModeDevelop)
	if err == nil {
		t.Fatal("an edit naming both old_str and decl was accepted; which one wins is a coin toss")
	}
	for _, want := range []string{"old_str", "decl", "exactly one"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

func TestAFlatWriteWithNoPathIsRefused(t *testing.T) {
	_, err := ActionFromCall(toolCall(ActionWriteFile,
		`{"replace":"y","summary":"s","type":"fix"}`), ModeDevelop)
	if err == nil || !strings.Contains(err.Error(), "path") {
		t.Fatalf("a write with no file to write to was accepted or refused vaguely: %v", err)
	}
}

// THE ARRAY FORM IS STILL ACCEPTED. A backend whose tool calls really are
// schema-constrained emits it correctly, and refusing a reply that did what was
// once asked is the failure this package exists to avoid.
func TestTheArrayFormIsStillAccepted(t *testing.T) {
	act, err := ActionFromCall(toolCall(ActionWriteFiles,
		`{"edits":[{"path":"a.go","decl":"main","replace":"func main(){}"}],`+
			`"summary":"s","type":"fix"}`), ModeDevelop)
	if err != nil {
		t.Fatalf("the array form was refused: %v", err)
	}
	if len(act.Edits) != 1 || act.Edits[0].Decl != "main" {
		t.Fatalf("the array form came back as %+v", act.Edits)
	}
}

// ...AND IT IS NEVER OFFERED, so nothing learns to build the nesting again.
func TestTheArrayFormIsNotOffered(t *testing.T) {
	for _, mode := range []Mode{ModeDevelop, ModeTest, ModeCoverage} {
		for _, tool := range Tools(mode) {
			if tool.Name == ActionWriteFiles {
				t.Errorf("mode %d offers the array write form", mode)
			}
		}
	}
}

// THE COPY THE FLAT SHAPE INVITES IS STILL CAUGHT.
//
// Flattening puts old_str and replace side by side again, and that arrangement
// is what once produced an identical pair in 39 of 47 turns. The branching never
// made it unrepresentable either — 11 of 26 refusals in a later window were the
// same copy — so what catches it is this refusal, and it has to survive.
func TestAnIdenticalPairIsStillRefusedOnTheFlatPath(t *testing.T) {
	act, err := ActionFromCall(toolCall(ActionWriteFile,
		`{"path":"a.go","old_str":"return nil","replace":"return nil","summary":"s","type":"fix"}`),
		ModeDevelop)
	if err != nil {
		t.Fatalf("decoding refused it too early, before Apply could name the cause: %v", err)
	}

	s := &State{Staged: map[string]string{}, Read: map[string]string{"a.go": "return nil\n"}}
	applyErr := Apply(s, []edit.Edit(act.Edits), ModeDevelop)
	if applyErr == nil {
		t.Fatal("an edit whose old_str equals its replace was applied; it changes nothing")
	}
	if !strings.Contains(applyErr.Error(), "IDENTICAL") {
		t.Errorf("the refusal does not name the cause: %v", applyErr)
	}
}

var _ = model.ToolCall{}

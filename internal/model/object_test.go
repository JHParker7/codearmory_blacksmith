package model

import (
	"encoding/json"
	"strings"
	"testing"
)

const fence = "```"

type doc struct {
	Overview string `json:"overview"`
	Files    []struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	} `json:"files"`
}

// THE CLOSING FENCE IS THE LAST ONE, NOT THE NEXT ONE.
//
// This is the whole reason Unfence exists as its own function with a test on it.
// A model that wraps its reply in ```json, answering with a document that itself
// contains a ```bash block, is the ORDINARY case for the design stage — and
// cutting at the inner fence returns a fragment with no closing brace, which
// then reports "no JSON object in the model output" against a reply that was
// perfectly well formed.
//
// It was invisible on the model that emitted objects bare and fatal on the one
// that fenced them, so the same harness bug would have been scored as one model
// being worse than the other.
func TestAFencedReplyCarryingAnInnerFenceSurvives(t *testing.T) {
	inner := "# Title\n\n" + fence + "bash\ngo build ./...\n" + fence + "\n"
	obj, err := json.Marshal(map[string]any{
		"overview": "a task manager",
		"files":    []map[string]string{{"path": "README.md", "content": inner}},
	})
	if err != nil {
		t.Fatal(err)
	}
	reply := fence + "json\n" + string(obj) + "\n" + fence

	var got doc
	if err := DecodeObject(reply, &got); err != nil {
		t.Fatalf("DecodeObject: %v", err)
	}
	if len(got.Files) != 1 {
		t.Fatalf("kept %d files, want the one the model sent", len(got.Files))
	}
	if !strings.Contains(got.Files[0].Content, "go build ./...") {
		t.Errorf("the payload was cut at its inner fence: %q", got.Files[0].Content)
	}
}

// A REPLY THAT ALREADY IS AN OBJECT IS NEVER UNFENCED, which is the other half
// of the same rule: unwrapping a reply that needs no unwrapping is what
// destroys a payload containing a fence.
func TestABareObjectIsNotUnfenced(t *testing.T) {
	inner := "# Title\n\n" + fence + "sh\nmake test\n" + fence + "\n"
	obj, _ := json.Marshal(map[string]any{
		"overview": "x",
		"files":    []map[string]string{{"path": "README.md", "content": inner}},
	})

	var got doc
	if err := DecodeObject(string(obj), &got); err != nil {
		t.Fatalf("DecodeObject: %v", err)
	}
	if !strings.Contains(got.Files[0].Content, "make test") {
		t.Errorf("a bare object was mangled: %q", got.Files[0].Content)
	}
}

func TestWhatDecodeObjectTolerates(t *testing.T) {
	cases := map[string]string{
		"prose either side":  "Sure, here you go:\n{\"overview\":\"x\"}\nHope that helps!",
		"a plain fence":      fence + "\n{\"overview\":\"x\"}\n" + fence,
		"a language tag":     fence + "json\n{\"overview\":\"x\"}\n" + fence,
		"leading whitespace": "\n\n  {\"overview\":\"x\"}  \n",
		// A raw newline inside a string is the most common way model JSON is
		// invalid; DecodeJSON repairs it and DecodeObject must not lose that.
		"a raw newline in a string": "{\"overview\":\"two\nlines\"}",
	}
	for name, raw := range cases {
		var got doc
		if err := DecodeObject(raw, &got); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got.Overview == "" {
			t.Errorf("%s: decoded to nothing", name)
		}
	}
}

func TestAReplyWithNoObjectIsRefusedRatherThanGuessed(t *testing.T) {
	for _, raw := range []string{
		"",
		"I cannot do that.",
		fence + "\nnot json at all\n" + fence,
		"}{",
	} {
		var got doc
		if err := DecodeObject(raw, &got); err == nil {
			t.Errorf("%q was accepted as an object", raw)
		}
	}
}

// AN UNCLOSED FENCE IS A TRUNCATED REPLY, and what arrived is still the best
// available reading of it: returning "" would throw away an object that is
// complete even though the fence around it is not.
func TestAnUnclosedFenceKeepsWhatArrived(t *testing.T) {
	var got doc
	if err := DecodeObject(fence+"json\n{\"overview\":\"x\"}", &got); err != nil {
		t.Fatalf("an object inside an unclosed fence was thrown away: %v", err)
	}
	if got.Overview != "x" {
		t.Errorf("overview = %q", got.Overview)
	}
}

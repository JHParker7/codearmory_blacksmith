package main

import (
	"strings"
	"testing"
)

// THESE TWO GO THROUGH parseDevAction, so they live with the parser rather than
// with the decoder it calls. The rest of the repair tests moved to
// internal/model, which is where the repairing happens.

func TestModelJSONSurvivesRawNewlinesInStrings(t *testing.T) {
	raw := "{\"action\":\"write_files\",\"edits\":[{\"path\":\"main.go\",\"search\":\"\",\"replace\":\"package main\nfunc Greet(name string) string {\n\treturn \\\"Hello, \\\" + name\n}\n\"}]}"

	act, err := parseDevAction(raw)
	if err != nil {
		t.Fatalf("parseDevAction() = %v; the reply is unambiguous and must be recovered", err)
	}
	if act.Action != actionWriteFiles || len(act.Edits) != 1 {
		t.Fatalf("action = %+v, want one write_files edit", act)
	}
	got := act.Edits[0].Replace
	if !strings.Contains(got, "package main\nfunc Greet") {
		t.Errorf("content lost its line breaks: %q", got)
	}
	// The escaped quotes in the Go source must survive as quotes, not as escapes.
	if !strings.Contains(got, `return "Hello, " + name`) {
		t.Errorf("content mangled the quoted string: %q", got)
	}
}

// Repair must not touch a reply that was already valid, and must not reformat
// the document around the strings.
func TestModelJSONLeavesValidRepliesAlone(t *testing.T) {
	const raw = `{
  "action": "read_files",
  "paths": ["main.go", "main_test.go"]
}`
	act, err := parseDevAction(raw)
	if err != nil {
		t.Fatalf("parseDevAction() = %v", err)
	}
	if act.Action != actionReadFiles || len(act.Paths) != 2 {
		t.Fatalf("action = %+v, want read_files with two paths", act)
	}
}

// Escapes are copied whole. If \" were read as the end of a string, everything
// after it would be treated as document structure and the repair would corrupt
// the very content it is meant to save.

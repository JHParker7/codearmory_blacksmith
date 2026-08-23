package model

import (
	"strings"
	"testing"
)

// The failure this exists for, in the shape it actually arrived in: a developer
// agent asked for a Go file and given one, with real line breaks, inside a JSON
// string. Eight iterations and a whole ticket's attempt budget went in twenty
// seconds without a single sandbox running, because every reply failed here.
// decodeTarget is the shape these replies land in.
//
// A LOCAL STRUCT, not the developer's devAction. What is under test is the
// repair, not the type it decodes into, and reaching for the agent's type here
// would make a JSON decoder depend on an agent — which is the coupling this
// package exists to remove.
type decodeTarget struct {
	Action string `json:"action"`
	Edits  []struct {
		Path    string `json:"path"`
		Search  string `json:"search"`
		Replace string `json:"replace"`
	} `json:"edits"`
}

func TestControlCharRepairRespectsEscapedQuotes(t *testing.T) {
	in := "{\"a\":\"say \\\"hi\\\"\nthen go\",\"b\":1}"
	out := escapeControlCharsInStrings(in)
	if strings.Count(out, "\n") != 0 {
		t.Errorf("a raw newline survived inside a string: %q", out)
	}
	if !strings.Contains(out, `say \"hi\"`) {
		t.Errorf("escaped quotes were altered: %q", out)
	}
	var got struct {
		A string `json:"a"`
		B int    `json:"b"`
	}
	if err := DecodeJSON(in, &got); err != nil {
		t.Fatalf("DecodeJSON() = %v", err)
	}
	if got.A != "say \"hi\"\nthen go" || got.B != 1 {
		t.Errorf("decoded = %+v, want the newline and the quotes both preserved", got)
	}
}

// Whitespace BETWEEN tokens is structure, not content, and must be left as it
// is — escaping it would produce a document that no longer parses.
func TestControlCharRepairIgnoresWhitespaceOutsideStrings(t *testing.T) {
	const in = "{\n\t\"a\": \"x\"\n}"
	if got := escapeControlCharsInStrings(in); got != in {
		t.Errorf("formatting whitespace was escaped:\n got %q\nwant %q", got, in)
	}
}

// Ambiguity is NOT repaired. An unescaped quote inside a string could end it or
// belong to it, and guessing would risk silently altering code an agent is about
// to commit. A parse error is the correct answer.
func TestModelJSONStillRejectsWhatItCannotKnow(t *testing.T) {
	var into map[string]any
	if err := DecodeJSON(`{"a": "x" "y"}`, &into); err == nil {
		t.Error("accepted a genuinely ambiguous reply; a wrong guess here reaches a commit")
	}
}

// AN UNKNOWN ESCAPE MUST NOT THROW THE REPLY AWAY. A model writing source code
// into a JSON string produces backslashes JSON does not recognise, and one of
// them invalidates the whole object. Observed live: "invalid character ' ' in
// string escape code" killed a developer attempt four replies running, on a
// reply that otherwise carried a perfectly good file.
func TestInvalidEscapesAreRepairedNotRejected(t *testing.T) {
	// The exact shape that failed: a backslash before a space.
	raw := `{"action":"write_files","edits":[{"path":"a.go","search":"","replace":"a\ b"}]}`
	var act decodeTarget
	if err := DecodeJSON(raw, &act); err != nil {
		t.Fatalf("DecodeJSON() = %v; the reply is unambiguous and must be recovered", err)
	}
	if len(act.Edits) != 1 {
		t.Fatalf("edits = %+v", act.Edits)
	}
	// PRESERVED as a literal backslash, not dropped. Dropping is tempting here —
	// it was probably meant as a space — but the same rule would silently rewrite
	// \d to d and corrupt a regex in code about to be committed.
	if got := act.Edits[0].Replace; got != `a\ b` {
		t.Errorf("replace = %q, want the backslash preserved", got)
	}
}

// The case that makes preserving the right choice: a regex written into code.
func TestARegexEscapeSurvivesTheRepair(t *testing.T) {
	raw := `{"action":"write_files","edits":[{"path":"a.go","search":"","replace":"re := \"\d+\""}]}`
	var act decodeTarget
	if err := DecodeJSON(raw, &act); err != nil {
		t.Fatalf("DecodeJSON() = %v", err)
	}
	got := act.Edits[0].Replace
	if !strings.Contains(got, `\d`) {
		t.Errorf("replace = %q; the regex lost its backslash and would silently match the wrong thing", got)
	}
}

// Valid escapes must be untouched, or the repair corrupts well-formed replies —
// which are the majority.
func TestValidEscapesAreUntouched(t *testing.T) {
	raw := `{"action":"write_files","edits":[{"path":"a.go","search":"","replace":"line\nquote \" tab\t done"}]}`
	var act decodeTarget
	if err := DecodeJSON(raw, &act); err != nil {
		t.Fatalf("DecodeJSON() = %v", err)
	}
	got := act.Edits[0].Replace
	for _, want := range []string{"line\nquote", "\" tab\t done"} {
		if !strings.Contains(got, want) {
			t.Errorf("replace = %q, want it to contain %q", got, want)
		}
	}
}

// A trailing backslash must not run off the end of the buffer.
func TestATrailingBackslashDoesNotPanic(t *testing.T) {
	for _, raw := range []string{`{"a":"x\`, `{"a":"x\"`, `{"a":"\`} {
		var into map[string]any
		_ = DecodeJSON(raw, &into) // must not panic; an error is fine
	}
}

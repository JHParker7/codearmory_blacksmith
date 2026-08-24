package model

import (
	"strings"
	"testing"
)

type reply struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func TestAWellFormedReplyDecodesUnchanged(t *testing.T) {
	var got reply
	if err := DecodeJSON(`{"path":"main.go","content":"package main\n"}`, &got); err != nil {
		t.Fatalf("DecodeJSON: %v", err)
	}
	if got.Path != "main.go" || got.Content != "package main\n" {
		t.Errorf("decoded %+v", got)
	}
}

// A RAW NEWLINE INSIDE A STRING is the reply this package exists for: a model
// asked to put a file into a "content" field writes the file, with real line
// breaks. Eight iterations and a ticket's whole attempt budget went in twenty
// seconds to this one habit.
func TestARawNewlineInsideAStringIsRepaired(t *testing.T) {
	var got reply
	payload := "{\"path\":\"main.go\",\"content\":\"package main\n\nfunc main() {}\n\"}"
	if err := DecodeJSON(payload, &got); err != nil {
		t.Fatalf("DecodeJSON: %v", err)
	}
	want := "package main\n\nfunc main() {}\n"
	if got.Content != want {
		t.Errorf("content = %q, want %q — the line breaks are the file, not noise", got.Content, want)
	}
}

func TestOtherRawControlCharactersAreRepaired(t *testing.T) {
	cases := map[string]struct{ payload, want string }{
		"tab":             {"{\"content\":\"a\tb\"}", "a\tb"},
		"carriage return": {"{\"content\":\"a\rb\"}", "a\rb"},
		"null":            {"{\"content\":\"a\x00b\"}", "a\x00b"},
		"bell":            {"{\"content\":\"a\x07b\"}", "a\x07b"},
	}
	for name, c := range cases {
		var got reply
		if err := DecodeJSON(c.payload, &got); err != nil {
			t.Errorf("%s: DecodeJSON: %v", name, err)
			continue
		}
		if got.Content != c.want {
			t.Errorf("%s: content = %q, want %q", name, got.Content, c.want)
		}
	}
}

// WHAT COMES AFTER A COMPLETE OBJECT IS NOT THE OBJECT'S PROBLEM. An architect
// returned 4,135 bytes of correct JSON with one closing brace too many, the
// stage reported "unparseable model output", and the project got no architecture
// document.
func TestTrailingJunkAfterACompleteObjectIsIgnored(t *testing.T) {
	for _, suffix := range []string{"}", "\n", " ", "}]}}", "some trailing prose"} {
		var got reply
		if err := DecodeJSON(`{"path":"a.go","content":"x"}`+suffix, &got); err != nil {
			t.Errorf("a complete object followed by %q was rejected: %v", suffix, err)
			continue
		}
		if got.Path != "a.go" {
			t.Errorf("suffix %q: decoded %+v", suffix, got)
		}
	}
}

// A TRUNCATED VALUE GENUINELY CANNOT BE RECOVERED, and must not be quietly
// accepted as a partial object — half an edit applied is worse than none.
func TestATruncatedReplyIsStillAnError(t *testing.T) {
	for _, payload := range []string{
		`{"path":"a.go","content":`,
		`{"path":"a.go"`,
		`{`,
		"",
	} {
		var got reply
		if err := DecodeJSON(payload, &got); err == nil {
			t.Errorf("DecodeJSON(%q) succeeded, decoding to %+v", payload, got)
		}
	}
}

// AN UNESCAPED QUOTE HAS TWO READINGS — it could end the string or belong in it
// — so it is refused rather than guessed at. Guessing would silently change code
// the agent is about to commit.
func TestAnAmbiguousReplyIsRefusedRatherThanGuessedAt(t *testing.T) {
	var got reply
	payload := `{"path":"a.go","content":"he said "hi" there"}`
	if err := DecodeJSON(payload, &got); err == nil {
		t.Errorf("an unescaped quote was repaired into %+v; the repair must only take what has one reading", got)
	}
}

// A BACKSLASH THAT DOES NOT START AN ESCAPE IS KEPT, VISIBLY. Dropping it
// rewrites \d to d and corrupts a regex in code about to be committed;
// preserving it leaves a literal backslash that fails to compile, which the
// agent can see and fix.
func TestAnUnknownEscapeIsPreservedRatherThanDropped(t *testing.T) {
	cases := map[string]struct{ payload, want string }{
		"regex class":  {`{"content":"\d+"}`, `\d+`},
		"stray space":  {`{"content":"a\ b"}`, `a\ b`},
		"windows path": {`{"content":"C:\users\dev"}`, `C:\users\dev`},
		"usr prose":    {`{"content":"see \usr for it"}`, `see \usr for it`},
	}
	for name, c := range cases {
		var got reply
		if err := DecodeJSON(c.payload, &got); err != nil {
			t.Errorf("%s: DecodeJSON: %v — a reply carrying a good file was thrown away for one character", name, err)
			continue
		}
		if got.Content != c.want {
			t.Errorf("%s: content = %q, want %q", name, got.Content, c.want)
		}
	}
}

// THE LIMIT, PINNED SO IT IS NOT DISCOVERED IN A RUN. A backslash followed by a
// single-letter escape is genuinely ambiguous, and JSON's reading wins: `C:\bin`
// decodes with a backspace in it. Nothing here can tell that from a model that
// meant the escape, and this test exists to state the trade rather than to argue
// the behaviour is desirable.
func TestASingleLetterEscapeIsReadAsJSONMeansIt(t *testing.T) {
	var got reply
	if err := DecodeJSON(`{"content":"C:\bin"}`, &got); err != nil {
		t.Fatalf("DecodeJSON: %v", err)
	}
	if got.Content != "C:\bin" {
		t.Errorf("content = %q, want the JSON reading %q", got.Content, "C:\bin")
	}
	if strings.Contains(got.Content, `\b`) {
		t.Error("the escape was preserved literally; ambiguity here resolves to JSON's meaning, not the model's")
	}
}

// The mirror image: a \u that IS well formed must keep meaning what it means. A
// repair that mangled real escapes would be worse than no repair.
func TestARealUnicodeEscapeStillDecodes(t *testing.T) {
	cases := map[string]struct{ payload, want string }{
		"lowercase hex": {`{"content":"valid \u00e9 here"}`, "valid é here"},
		"uppercase hex": {`{"content":"\u00C9"}`, "É"},
		"digits":        {`{"content":"\u0041"}`, "A"},
	}
	for name, c := range cases {
		var got reply
		if err := DecodeJSON(c.payload, &got); err != nil {
			t.Errorf("%s: DecodeJSON: %v", name, err)
			continue
		}
		if got.Content != c.want {
			t.Errorf("%s: content = %q, want %q", name, got.Content, c.want)
		}
	}
}

// The repair only runs when the strict parse has already failed, so a valid \u
// alone proves nothing. This forces it down the repair path by putting a raw
// newline in the same reply.
func TestARealUnicodeEscapeSurvivesTheRepairPath(t *testing.T) {
	var got reply
	payload := "{\"content\":\"caf\\u00e9\nbar\"}"
	if err := DecodeJSON(payload, &got); err != nil {
		t.Fatalf("DecodeJSON: %v", err)
	}
	if got.Content != "café\nbar" {
		t.Errorf("content = %q, want the escape decoded and the newline kept", got.Content)
	}
}

// EVERYTHING OUTSIDE A STRING IS LEFT ALONE, including the whitespace that
// formats the document — a repair that touched it would be rewriting the reply
// rather than fixing it.
func TestTheRepairTouchesNothingOutsideAString(t *testing.T) {
	for _, s := range []string{
		"{\n  \"a\": 1,\n\t\"b\": [2, 3]\n}",
		`{"a":"plain"}`,
		"",
		"not json at all",
	} {
		if got := escapeControlCharsInStrings(s); got != s {
			t.Errorf("the repair rewrote text with no control character inside a string:\n in: %q\nout: %q", s, got)
		}
	}
}

// A quote that ENDS a string must be seen as ending it — otherwise the repair
// escapes the whitespace of the rest of the document and produces something
// further from valid JSON than what it started with.
func TestTheRepairTracksWhereStringsEnd(t *testing.T) {
	in := "{\"a\":\"x\ny\", \"b\":\n2}"
	got := escapeControlCharsInStrings(in)
	if !strings.Contains(got, `"x\ny"`) {
		t.Errorf("the newline inside the string was not escaped: %q", got)
	}
	if !strings.Contains(got, "\"b\":\n2") {
		t.Errorf("the newline between the string and the next key was escaped: %q", got)
	}
}

// An escaped quote must not be read as the end of the string, or everything
// after it is treated as document text and its control characters are left raw.
func TestAnEscapedQuoteDoesNotEndTheString(t *testing.T) {
	var got reply
	payload := "{\"content\":\"he said \\\"hi\\\"\nand left\"}"
	if err := DecodeJSON(payload, &got); err != nil {
		t.Fatalf("DecodeJSON: %v", err)
	}
	if got.Content != "he said \"hi\"\nand left" {
		t.Errorf("content = %q", got.Content)
	}
}

// A trailing backslash escapes nothing and must not run off the end of the
// input; the same visible-beats-silent rule applies.
func TestATrailingBackslashDoesNotRunOffTheEnd(t *testing.T) {
	got := escapeControlCharsInStrings(`{"a":"end\`)
	if !strings.HasSuffix(got, `\\`) {
		t.Errorf("a trailing backslash came out as %q, want it preserved", got)
	}
}

// THE ERROR MUST NAME THE REPLY THE MODEL WROTE. The repaired text is an
// internal detail, and an error quoting an offset in a string that never existed
// sends whoever reads it to the wrong place — which is this repository's most
// expensive failure class.
func TestTheErrorDescribesTheOriginalReply(t *testing.T) {
	// A raw newline forces the repair, and the unescaped quote means the repaired
	// text fails too — so both paths have an error to report.
	payload := "{\"content\":\"a\nb\"c\"}"
	var got reply
	err := DecodeJSON(payload, &got)
	if err == nil {
		t.Fatal("DecodeJSON accepted an unrepairable reply")
	}

	var strict reply
	want := decodeFirstValue(payload, &strict)
	if want == nil {
		t.Fatal("this test needs a payload the strict parse rejects")
	}
	if err.Error() != want.Error() {
		t.Errorf("DecodeJSON reported %q; want the strict parse's own %q", err, want)
	}
}

package tools

import (
	"context"
	"os"
	"strings"
	"testing"
)

// THE BINDING TEST. ConventionalTypes claims to be the list hooks/commit-msg
// enforces, and it had silently drifted to seven entries against the hook's
// eleven — "perf" and "ci" were refused here with a message that spoke for a
// hook that would have accepted them. A comment citing evidence has to be held
// to it; internal/agent/dev binds its copy the same way.
func TestConventionalTypesMatchTheHook(t *testing.T) {
	raw, err := os.ReadFile("../../hooks/commit-msg")
	if err != nil {
		t.Fatalf("reading the hook: %v", err)
	}
	hook := string(raw)

	// The hook's alternation: type is one of the words in its grep pattern.
	start := strings.Index(hook, "'^(")
	end := strings.Index(hook[start:], ")")
	if start < 0 || end < 0 {
		t.Fatalf("the hook's type alternation was not found; the pattern may have moved")
	}
	hookTypes := strings.Split(hook[start+3:start+end], "|")

	ours := map[string]bool{}
	for _, c := range ConventionalTypes {
		ours[c] = true
	}
	for _, h := range hookTypes {
		if !ours[h] {
			t.Errorf("the hook accepts %q and this list refuses it", h)
		}
	}
	if len(ConventionalTypes) != len(hookTypes) {
		t.Errorf("this list has %d types, the hook has %d: %v vs %v",
			len(ConventionalTypes), len(hookTypes), ConventionalTypes, hookTypes)
	}
}

// The natural reading of a parameter named glob: "*.go" matches Go files. It
// was a literal suffix match, so that exact input matched nothing and the tool
// reported a confident "No matches found" for symbols that existed.
func TestAGlobActuallyGlobs(t *testing.T) {
	s := newSet(map[string]string{
		"store.go":  "package p\nfunc NewStore() {}\n",
		"README.md": "NewStore is documented here\n",
	}, AllowAll, nil)

	got, err := s.Invoke(context.Background(), SearchFiles, `{"pattern":"NewStore","glob":"*.go"}`)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !strings.Contains(got, "store.go:2:") {
		t.Fatalf("\"*.go\" did not match a Go file: %q", got)
	}
	if strings.Contains(got, "README.md") {
		t.Fatalf("\"*.go\" matched a markdown file: %q", got)
	}
}

// A bare suffix keeps working — it has no metacharacters, and suffix is what it
// says.
func TestABareSuffixStillFilters(t *testing.T) {
	s := newSet(map[string]string{
		"store.go":  "package p\nfunc NewStore() {}\n",
		"README.md": "NewStore\n",
	}, AllowAll, nil)

	got, err := s.Invoke(context.Background(), SearchFiles, `{"pattern":"NewStore","glob":".go"}`)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !strings.Contains(got, "store.go") || strings.Contains(got, "README.md") {
		t.Fatalf("the bare suffix filter broke: %q", got)
	}
}

// The summary limit speaks in characters, so it must count them — a CJK or
// accented summary inside the limit was refused with byte counts the model
// could not reconcile against what it sent.
func TestAMultibyteSummaryWithinTheLimitIsAccepted(t *testing.T) {
	s := newSet(map[string]string{}, AllowAll, nil)

	summary := strings.Repeat("功", 150) // 150 characters, 450 bytes
	got, err := s.Invoke(context.Background(), WriteFile,
		`{"path":"a.md","replace":"hi\n","summary":"`+summary+`","type":"docs"}`)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if strings.Contains(got, "Error") {
		t.Fatalf("a 150-character summary was refused: %q", got)
	}
}

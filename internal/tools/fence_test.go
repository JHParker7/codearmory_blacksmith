package tools

import (
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/edit"
)

// THE LOOP THAT KILLED PLAN RUN 1 ON THE ONE-SHOT HARNESS. The model wrapped
// its replacement in a ```go fence — how code is written everywhere it learned
// from — and backticks are not Go, so the syntax gate refused it and the model
// resent the same edit every 24 seconds until the stall bound fired. The old
// developer loop unfences every reply for exactly this reason; this package had
// dropped that.
func TestAFencedReplacementIsRepairedNotRefused(t *testing.T) {
	w := NewWorkspace(map[string]string{
		"a.go": "package p\n\nfunc F() int {\n\treturn 1\n}\n",
	}, AllowAll)

	line, err := w.ApplyEdit(edit.Edit{
		Path: "a.go", Decl: "F",
		Replace: "```go\nfunc F() int {\n\treturn 2\n}\n```",
	})
	if err != nil {
		t.Fatalf("a fenced replacement was refused: %v", err)
	}
	got, _ := w.Read("a.go")
	if strings.Contains(got, "```") {
		t.Fatalf("the fence reached the file: %q", got)
	}
	if !strings.Contains(got, "return 2") {
		t.Fatalf("the replacement did not land: %q", got)
	}
	// Said out loud, so the model learns the fence was never needed rather than
	// concluding it worked.
	if !strings.Contains(line, "fence") {
		t.Fatalf("the repair is not reported: %q", line)
	}
}

func TestAFencedWholeFileCreateIsRepaired(t *testing.T) {
	w := NewWorkspace(map[string]string{}, AllowAll)

	if _, err := w.ApplyEdit(edit.Edit{
		Path:    "new.go",
		Replace: "```go\npackage p\n\nfunc F() {}\n```",
	}); err != nil {
		t.Fatalf("a fenced new file was refused: %v", err)
	}
	got, _ := w.Read("new.go")
	if strings.Contains(got, "```") || !strings.Contains(got, "package p") {
		t.Fatalf("the fence was not stripped from the new file: %q", got)
	}
}

// ``` inside a quoted Go string is legal Go, so content that already passes the
// gate must never be touched — a legitimate backtick cannot be mangled.
func TestALegitimateFenceInsideAGoStringIsLeftAlone(t *testing.T) {
	w := NewWorkspace(map[string]string{
		"a.go": "package p\n\nvar s = \"old\"\n",
	}, AllowAll)

	if _, err := w.ApplyEdit(edit.Edit{
		Path: "a.go", OldStr: "var s = \"old\"",
		Replace: "var s = \"a ``` fence in prose\"",
	}); err != nil {
		t.Fatalf("legal Go containing backticks was refused: %v", err)
	}
	got, _ := w.Read("a.go")
	if !strings.Contains(got, "a ``` fence in prose") {
		t.Fatalf("the legitimate backticks were mangled: %q", got)
	}
}

// Genuinely broken Go — no fence to strip — is still refused; the repair must
// not become a hole in the gate.
func TestGenuinelyBrokenGoIsStillRefused(t *testing.T) {
	w := NewWorkspace(map[string]string{
		"a.go": "package p\n\nfunc F() int {\n\treturn 1\n}\n",
	}, AllowAll)

	if _, err := w.ApplyEdit(edit.Edit{
		Path: "a.go", Decl: "F", Replace: "func F() int {{{ return",
	}); err == nil {
		t.Fatal("broken Go was accepted")
	}
	// And a fence around broken Go does not launder it.
	if _, err := w.ApplyEdit(edit.Edit{
		Path: "a.go", Decl: "F", Replace: "```go\nfunc F() int {{{ return\n```",
	}); err == nil {
		t.Fatal("a fence around broken Go laundered it through the gate")
	}
}

// Markdown never runs the gate, so fences in prose land literally — a plan that
// QUOTES code keeps its fences.
func TestFencesInMarkdownLandLiterally(t *testing.T) {
	w := NewWorkspace(map[string]string{"PLAN.md": "# plan\n\nold\n"}, AllowAll)

	if _, err := w.ApplyEdit(edit.Edit{
		Path: "PLAN.md", OldStr: "old",
		Replace: "```go\nfunc Example() {}\n```",
	}); err != nil {
		t.Fatalf("a fenced example in markdown was refused: %v", err)
	}
	got, _ := w.Read("PLAN.md")
	if !strings.Contains(got, "```go") {
		t.Fatalf("the markdown lost its fence: %q", got)
	}
}

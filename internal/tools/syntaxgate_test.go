package tools

import (
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/edit"
)

// THE SYNTAX GATES ARE FOR GO AND ONLY FOR GO.
//
// edit.ReplacementIsMalformed decides whether text parses as Go declarations or
// Go statements, and it was applied to every targeted edit whatever the file.
// A markdown heading parses as neither, so an architect editing its own plan was
// told "the replacement is not valid in any position" — and once that plan grew
// past the whole-rewrite threshold it could not replace the file either. Boxed
// in from both sides, it spent its budget circling; every earlier diagnosis of
// that stall was of a symptom.
func TestProseIsNotParsedAsGo(t *testing.T) {
	w := NewWorkspace(map[string]string{
		"PLAN.md": "# plan\n\nsome prose\n\n## section\n\nmore prose\n",
	}, AllowAll)

	for _, replacement := range []string{
		"## a new heading",
		"- a bullet point",
		"| a | table | row |",
		"1. numbered item",
		"just a sentence with no code in it at all",
	} {
		w2 := NewWorkspace(map[string]string{"PLAN.md": "# plan\n\nsome prose\n"}, AllowAll)
		_, err := w2.ApplyEdit(edit.Edit{Path: "PLAN.md", OldStr: "some prose", Replace: replacement})
		if err != nil {
			t.Errorf("prose replacement %q was refused: %v", replacement, err)
		}
	}
	_ = w
}

// Go is still gated, or the check that catches a broken replacement before it
// reaches the compiler would be gone.
func TestGoIsStillParsed(t *testing.T) {
	w := NewWorkspace(map[string]string{"a.go": "package p\n\nfunc F() {\n\tx := 1\n\t_ = x\n}\n"}, AllowAll)

	_, err := w.ApplyEdit(edit.Edit{Path: "a.go", OldStr: "\tx := 1", Replace: "\tx := {{{"})
	if err == nil {
		t.Fatal("a broken Go replacement was accepted")
	}
	if !strings.Contains(err.Error(), "not valid") {
		t.Errorf("the refusal does not name the cause: %v", err)
	}
}

// A markdown file whose content happens to contain braces must not be judged on
// whether those braces balance as Go.
func TestProseWithBracesIsFine(t *testing.T) {
	w := NewWorkspace(map[string]string{"PLAN.md": "# plan\n\nold\n"}, AllowAll)

	_, err := w.ApplyEdit(edit.Edit{
		Path: "PLAN.md", OldStr: "old",
		Replace: "Use `Task{` to build one, and note the unmatched brace in this sentence: }",
	})
	if err != nil {
		t.Fatalf("prose containing braces was refused: %v", err)
	}
}

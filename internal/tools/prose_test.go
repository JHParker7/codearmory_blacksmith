package tools

import (
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/edit"
)

// Revising a document by rewriting it is the normal way to revise one.
//
// The refusal that protects source code was telling an architect to "quote a
// short snippet" of a 424-line plan it had written itself; it could not, and
// spent the rest of its budget circling. Prose has no delayed failure: a plan
// missing a section is missing it visibly, to the next reader.
func TestAProseFileCanBeRewrittenWhole(t *testing.T) {
	w := NewWorkspace(map[string]string{"PLAN.md": "# old plan\n\nlots of prose\n"}, AllowAll)

	_, err := w.ApplyEdit(edit.Edit{Path: "PLAN.md", Replace: "# new plan\n\ndifferent prose\n"})
	if err != nil {
		t.Fatalf("a markdown file could not be rewritten: %v", err)
	}
	got, _ := w.Read("PLAN.md")
	if !strings.Contains(got, "new plan") || strings.Contains(got, "old plan") {
		t.Fatalf("the rewrite did not take: %q", got)
	}
}

// Source is still protected, because a declaration dropped in a rewrite does not
// announce itself: it surfaces a whole verification round later as a compile
// error somewhere else.
func TestSourceIsStillProtectedFromAWholesaleRewrite(t *testing.T) {
	w := NewWorkspace(map[string]string{"a.go": "package p\n\nfunc F() {}\n"}, AllowAll)

	if _, err := w.ApplyEdit(edit.Edit{Path: "a.go", Replace: "package p\n"}); err == nil {
		t.Fatal("a Go file was rewritten wholesale")
	}
}

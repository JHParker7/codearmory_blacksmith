package tools

import (
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/edit"
)

func longPlan() string {
	var b strings.Builder
	b.WriteString("# plan\n")
	for i := 0; i < MaxWholeRewriteLines+50; i++ {
		b.WriteString("a line of the plan\n")
	}
	return b.String()
}

// A LONG DOCUMENT IS REFUSED FOR COST, NOT FOR SAFETY, and the message says so.
//
// Measured: eleven whole-file writes to a 363-line plan took 705 of one run's
// 831 seconds, at a median of 60s each against 10s for a read on the same
// prompt — so it is output, not context. The last of them reproduced the file
// byte for byte and was refused as a no-op.
func TestALongProseFileIsNotRewrittenWhole(t *testing.T) {
	w := NewWorkspace(map[string]string{"PLAN.md": longPlan()}, AllowAll)

	_, err := w.ApplyEdit(edit.Edit{Path: "PLAN.md", Replace: "# a whole new plan\n"})
	if err == nil {
		t.Fatal("a long plan was rewritten whole")
	}
	// The advice has to be usable on prose: no talk of declarations.
	if strings.Contains(err.Error(), "decl") {
		t.Errorf("the refusal offers a Go-only address for a markdown file: %v", err)
	}
	for _, want := range []string{"old_str", "start_line"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// A long document must still be EDITABLE, or the refusal above is a trap — which
// is what the previous version of this rule turned out to be.
func TestALongProseFileCanStillBeEditedInPlace(t *testing.T) {
	w := NewWorkspace(map[string]string{"PLAN.md": longPlan()}, AllowAll)

	if _, err := w.ApplyEdit(edit.Edit{
		Path: "PLAN.md", OldStr: "# plan", Replace: "# the revised plan",
	}); err != nil {
		t.Fatalf("a long plan could not be edited in place: %v", err)
	}
	got, _ := w.Read("PLAN.md")
	if !strings.Contains(got, "# the revised plan") {
		t.Fatal("the targeted edit did not take")
	}
}

// A SHORT document may still be rewritten whole. Refusing that would be pedantry:
// regenerating it is cheaper than addressing it, and revising a short note by
// rewriting it is how anyone would do it.
func TestAShortProseFileMayStillBeRewrittenWhole(t *testing.T) {
	w := NewWorkspace(map[string]string{"NOTE.md": "# old\n\nshort note\n"}, AllowAll)

	if _, err := w.ApplyEdit(edit.Edit{Path: "NOTE.md", Replace: "# new\n\nrewritten\n"}); err != nil {
		t.Fatalf("a short note could not be rewritten: %v", err)
	}
}

// Source is refused at ANY size, and for the other reason: a declaration dropped
// in a rewrite does not announce itself, it surfaces a verification round later
// as a compile error somewhere else.
func TestSourceIsRefusedAtAnySize(t *testing.T) {
	w := NewWorkspace(map[string]string{"a.go": "package p\n\nfunc F() {}\n"}, AllowAll)

	_, err := w.ApplyEdit(edit.Edit{Path: "a.go", Replace: "package p\n"})
	if err == nil {
		t.Fatal("a short Go file was rewritten wholesale")
	}
	if !strings.Contains(err.Error(), "decl") {
		t.Errorf("the source refusal lost its declaration advice: %v", err)
	}
}

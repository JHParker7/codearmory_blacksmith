package agents

import (
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/tools"
)

// The security reviewer's write surface is EXACTLY its report. A reviewer that
// can write anything else stops reviewing and starts rewriting, and an
// extension guard could not say this — ".md" would hand it the plan too.
func TestTheSecurityReviewerMayWriteOnlyItsReport(t *testing.T) {
	a := maker().PlanSec(nil)

	if err := a.opts.Guard("SECURITY.md"); err != nil {
		t.Fatalf("the reviewer may not write its own report: %v", err)
	}
	for _, p := range []string{"store.go", "store_test.go", "PLAN.md", "go.mod", "scan/sast.txt"} {
		if err := a.opts.Guard(p); err == nil {
			t.Errorf("the reviewer may write %s", p)
		}
	}
}

// Advisory, not a gate: the review has no check, cannot run commands, and
// cannot undo — it reads, and it reports.
func TestTheSecurityReviewerIsAdvisoryAndReadOnlyOtherwise(t *testing.T) {
	a := maker().PlanSec(nil)

	if a.Check() != "" {
		t.Fatalf("the reviewer gates on a command: %q", a.Check())
	}
	for _, name := range []string{tools.RunCommand, tools.UndoEdit} {
		if offers(a, name) {
			t.Errorf("the reviewer is offered %s", name)
		}
	}
	for _, name := range []string{tools.ReadFiles, tools.ListFiles, tools.SearchFiles, tools.WriteFile} {
		if !offers(a, name) {
			t.Errorf("the reviewer cannot call %s", name)
		}
	}
}

// The scanner seam is the TREE: reports under scan/ are read like any file,
// as leads to verify rather than verdicts to copy. The prompt is where that
// contract lives until the scanners exist, so it is pinned.
func TestTheSecurityReviewerIsPointedAtTheScannerSeam(t *testing.T) {
	p := maker().PlanSec(nil).opts.Prompt

	for _, want := range []string{"scan/", "false positives", "SECURITY.md", "FILE", "LINE"} {
		if !strings.Contains(p, want) {
			t.Errorf("the reviewer's prompt is missing %q", want)
		}
	}
}

// The review runs LAST: it reads the finished change, so everything else must
// already have happened.
func TestTheSecurityReviewRunsLast(t *testing.T) {
	got := PlanStages()
	if got[len(got)-1] != StagePlanSec {
		t.Fatalf("the security review is not the last stage: %v", got)
	}
}

package agents

import (
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/tools"
)

// FINDINGS GO TO THE BOARD, NEVER THE TREE. A findings file committed to the
// repository is a curated vulnerability list handed to anyone with clone
// access — the operator drew that line — so the reviewer can write NOTHING:
// its work is tickets, its verdict is its answer.
func TestTheSecurityReviewerWritesNothingIntoTheTree(t *testing.T) {
	a := maker().PlanSec(nil)

	for _, p := range []string{"SECURITY.md", "store.go", "PLAN.md", "scan/sast.txt"} {
		if err := a.opts.Guard(p); err == nil {
			t.Errorf("the reviewer may write %s into the tree", p)
		}
	}
	if offers(a, tools.WriteFile) {
		t.Error("the reviewer is offered write_file at all")
	}
}

// Its work goes through file_ticket; everything else about it is read-only
// and advisory — no check, no commands, no undo.
func TestTheSecurityReviewerFilesTicketsAndNothingElse(t *testing.T) {
	a := maker().PlanSec(nil)

	if !offers(a, tools.FileTicket) {
		t.Fatal("the reviewer cannot file tickets")
	}
	if a.Check() != "" {
		t.Fatalf("the reviewer gates on a command: %q", a.Check())
	}
	for _, name := range []string{tools.RunCommand, tools.UndoEdit} {
		if offers(a, name) {
			t.Errorf("the reviewer is offered %s", name)
		}
	}
	for _, name := range []string{tools.ReadFiles, tools.ListFiles, tools.SearchFiles} {
		if !offers(a, name) {
			t.Errorf("the reviewer cannot call %s", name)
		}
	}
}

// The scanner seam is the TREE — reports under scan/ are leads to verify, and
// the board rule is stated where the model reads it. Pinned, because the
// prompt is where both contracts live until the scanners exist.
func TestTheSecurityReviewerPromptCarriesBothContracts(t *testing.T) {
	p := maker().PlanSec(nil).opts.Prompt

	for _, want := range []string{"scan/", "false positives", "NEVER write findings into the repository",
		"file_ticket", "FILE", "LINE"} {
		if !strings.Contains(p, want) {
			t.Errorf("the reviewer's prompt is missing %q", want)
		}
	}
}

// The review runs LAST: it reads the finished change.
func TestTheSecurityReviewRunsLast(t *testing.T) {
	got := PlanStages()
	if got[len(got)-1] != StagePlanSec {
		t.Fatalf("the security review is not the last stage: %v", got)
	}
}

// Filing a ticket is progress: a reviewer steadily filing findings is the
// opposite of stuck, and must neither heat up nor stall.
func TestFilingATicketCountsAsProgress(t *testing.T) {
	if !progressed(tools.FileTicket, "Filed abc [high]: something") {
		t.Fatal("a filed ticket did not count as progress")
	}
	if progressed(tools.FileTicket, "Error: the board refused the ticket") {
		t.Fatal("a refused ticket counted as progress")
	}
}

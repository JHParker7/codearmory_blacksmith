package agents

import (
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/tools"
)

// The code reviewer is the security reviewer's twin: reads, files quality
// tickets, writes nothing, runs no commands.
func TestTheCodeReviewerFilesQualityTicketsAndWritesNothing(t *testing.T) {
	a := maker().PlanReview(nil)

	if a.opts.TicketKind != "quality" {
		t.Fatalf("the code reviewer files %q findings, want quality", a.opts.TicketKind)
	}
	for _, p := range []string{"store.go", "PLAN.md", "SECURITY.md"} {
		if err := a.opts.Guard(p); err == nil {
			t.Errorf("the code reviewer may write %s", p)
		}
	}
	if !offers(a, tools.FileTicket) {
		t.Fatal("the code reviewer cannot file tickets")
	}
	for _, name := range []string{tools.WriteFile, tools.RunCommand, tools.UndoEdit} {
		if offers(a, name) {
			t.Errorf("the code reviewer is offered %s", name)
		}
	}
}

// The two reviewers file DIFFERENT kinds, which is what lets auto-mode order
// security before quality.
func TestTheReviewersFileDistinctKinds(t *testing.T) {
	c := maker()
	if sec := c.PlanSec(nil).opts.TicketKind; sec != "security" {
		t.Errorf("the security reviewer files %q, want security", sec)
	}
	if q := c.PlanReview(nil).opts.TicketKind; q != "quality" {
		t.Errorf("the code reviewer files %q, want quality", q)
	}
}

// It reviews for QUALITY, not security, and is told the security reviewer
// already covered that ground — so the two do not file the same finding under
// two kinds.
func TestTheCodeReviewerStaysOnQuality(t *testing.T) {
	p := maker().PlanReview(nil).opts.Prompt
	for _, want := range []string{"QUALITY", "scan/lint.txt", "do NOT refile", "file_ticket"} {
		if !strings.Contains(p, want) {
			t.Errorf("the code reviewer prompt is missing %q", want)
		}
	}
	if strings.Contains(p, "attacker") {
		t.Error("the code reviewer is straying into security")
	}
}

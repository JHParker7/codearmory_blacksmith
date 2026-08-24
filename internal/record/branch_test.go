package record

import (
	"testing"

	"github.com/code-armory-app/blacksmith/internal/ticket"
)

func withComments(bodies ...string) ticket.Ticket {
	var t ticket.Ticket
	for i, b := range bodies {
		t.Comments = append(t.Comments, ticket.Comment{ID: string(rune('a' + i)), Body: b})
	}
	return t
}

// EVERY STAGE THAT PUSHES PUBLISHES THE SAME WAY, and every stage that acts on a
// branch reads it the same way. A reader that knew only about the developer's
// marker would find nothing on a ticket that went straight from its author to
// review.
func TestABranchIsFoundWhicheverStagePublishedIt(t *testing.T) {
	for _, marker := range []string{BranchMarker, TestsWrittenMarker, CoverageMarker} {
		tk := withComments(PublishBranch(marker, "agent/t-1"))
		if got := BranchOf(tk); got != "agent/t-1" {
			t.Errorf("a branch published under %q read back as %q", marker, got)
		}
		if !HasBranch(tk) {
			t.Errorf("HasBranch = false for a branch published under %q", marker)
		}
	}
}

// THE MOST RECENT WINS. A ticket reworked after a hand-back carries more than
// one, and the branch to act on is the LAST one published — taking the first
// would send the reviewer to the tests without the implementation.
func TestTheLastBranchPublishedIsTheOneToActOn(t *testing.T) {
	tk := withComments(
		PublishBranch(TestsWrittenMarker, "agent/t-1"),
		"a review comment in between",
		PublishBranch(BranchMarker, "agent/t-1-rework"),
	)
	if got := BranchOf(tk); got != "agent/t-1-rework" {
		t.Errorf("BranchOf() = %q, want the branch published last", got)
	}
}

// A ticket with nothing pushed has nothing to review, merge or resolve.
func TestNoBranchIsReportedWhenNothingWasPushed(t *testing.T) {
	cases := map[string]ticket.Ticket{
		"no comments at all": {},
		"ordinary comments":  withComments("triaged", "reviewed: clean"),
		// A marker with no branch line is not a publication: a stage that failed to
		// push must not look as though it succeeded.
		"a marker with no branch": withComments(BranchMarker + "\n\nsomething went wrong"),
		"an empty branch name":    withComments(PublishBranch(BranchMarker, "")),
		// A branch line without a marker is prose, not a publication — an agent
		// quoting a branch name in a comment must not redirect a later stage.
		"prose that mentions a branch": withComments("I looked at **Branch:** `agent/t-9` earlier"),
	}
	for name, tk := range cases {
		if got := BranchOf(tk); got != "" {
			t.Errorf("%s: BranchOf() = %q, want empty", name, got)
		}
		if HasBranch(tk) {
			t.Errorf("%s: HasBranch() = true", name)
		}
	}
}

// An unterminated branch line names nothing, and must not hand a later stage a
// half-read name to check out.
func TestAnUnterminatedBranchLineIsIgnored(t *testing.T) {
	tk := withComments(BranchMarker + "\n\n" + branchIntro + "agent/t-1 with no closing tick")
	if got := BranchOf(tk); got != "" {
		t.Errorf("BranchOf() = %q, want empty", got)
	}
}

// A later comment that publishes nothing must not erase what was published
// before it: the reviewer's own note follows the developer's push.
func TestALaterCommentWithoutABranchDoesNotEraseOne(t *testing.T) {
	tk := withComments(
		PublishBranch(BranchMarker, "agent/t-1"),
		BranchMarker+"\n\nthe push failed and nothing was published",
	)
	if got := BranchOf(tk); got != "agent/t-1" {
		t.Errorf("BranchOf() = %q, want the branch that was actually published", got)
	}
}

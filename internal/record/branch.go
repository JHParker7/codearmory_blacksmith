package record

import (
	"strings"

	"github.com/code-armory-app/blacksmith/internal/ticket"
)

// branchIntro is how every stage publishes the branch it pushed.
//
// ONE SPELLING, in one place. Three stages read this back — the reviewer, the
// integrator and the resolver — and a fourth writes it; agreeing on the prose by
// accident is a bug waiting for someone to reword one of them.
const branchIntro = "**Branch:** `"

// BranchOf reports the branch a stage should act on, or "" when none has been
// published.
//
// THE MOST RECENT WINS. A ticket reworked after a hand-back carries more than
// one — the author's, then the developer's, then the coverage stage's — and the
// branch to act on is the LAST one published. Taking the first would send the
// reviewer to the tests without the implementation.
//
// It looks for any of the branch markers rather than one, because which stage
// published last depends on the route the ticket took through the pipeline, and
// a reader that knew only about the developer's would find nothing on a ticket
// that went straight from its author to review.
func BranchOf(t ticket.Ticket) string {
	branch := ""
	for _, c := range t.Comments {
		if !publishesBranch(c.Body) {
			continue
		}
		_, rest, ok := strings.Cut(c.Body, branchIntro)
		if !ok {
			continue
		}
		found, _, ok := strings.Cut(rest, "`")
		if !ok {
			continue
		}
		if found = strings.TrimSpace(found); found != "" {
			branch = found
		}
	}
	return branch
}

// HasBranch reports whether anything has been pushed for this ticket. There is
// nothing to review, merge or resolve without one.
func HasBranch(t ticket.Ticket) bool { return BranchOf(t) != "" }

// PublishBranch renders the line a stage writes to announce what it pushed.
//
// The same function the readers are tested against, so the two cannot drift.
func PublishBranch(marker, branch string) string {
	return marker + "\n\n" + branchIntro + branch + "`"
}

func publishesBranch(body string) bool {
	for _, m := range []string{BranchMarker, TestsWrittenMarker, CoverageMarker} {
		if strings.Contains(body, m) {
			return true
		}
	}
	return false
}

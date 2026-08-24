// Package record is the vocabulary a stage writes into a ticket's comments, and
// the two questions asked back of it: who holds this ticket, and how many
// attempts has a role already spent on it.
//
// MARKERS RECORD, THEY DO NOT ROUTE. Selection used to read these — a triage
// comment meant triaged, a "pushed a branch" comment meant ready for review —
// and that made the wording of a comment part of the pipeline's control flow.
// Routing moved to the board columns (see internal/workflow); what stayed here
// is the audit trail, plus the one thing a column genuinely cannot express:
// WHICH HOST and which role took the work, when one role runs on several
// machines.
package record

// The markers. Two shapes, and the difference is deliberate.
//
// An HTML COMMENT is invisible on the board: it is machinery, and a person
// reading the ticket should not have to skim past it. BOLD PROSE is visible,
// because it is the sentence a person wants — what a stage did and what happens
// next. Anything a person would want to read is written the second way.
const (
	// ClaimMarker prefixes the comment recording a claim. It is matched EXACTLY,
	// so it must not change without a migration story for claims already standing
	// on in-flight tickets: a renamed marker makes every one of them invisible,
	// which resets attempt counts and un-holds work that is being worked.
	ClaimMarker = "<!-- blacksmith:claim "

	// ReturnedMarker records work handed back to an earlier stage.
	ReturnedMarker = "<!-- blacksmith:returned -->"

	// SpecRepairMarker records the developer handing a specification back as
	// unsatisfiable. Prose, not a comment, because it is the one hand-back a
	// person most often wants to see on the board.
	SpecRepairMarker = "**Returned: the specification does not compile.**"

	// MergedIntoMarker is left on a ticket folded into another, so the original
	// stays on the board saying where its work went rather than simply going
	// quiet.
	MergedIntoMarker = "**Merged into another ticket.**"

	// The branch markers. Each names the stage that pushed, because a later stage
	// finds the branch to work on by reading them back — and LAST ONE WINS, so a
	// coverage push supersedes the developer's, which supersedes the author's.
	BranchMarker       = "**Developer agent pushed a branch.**"
	TestsWrittenMarker = "**Test author pushed the tests.**"
	CoverageMarker     = "**Coverage tests added.**"
)

// resetsAttempts lists the markers that restart a role's attempt count.
//
// A RETURN RESTARTS THE COUNT, whichever direction it came from. The reviewer's
// send-back has always reset it — the ticket goes back a stage and comes forward
// again, and the attempts it spent getting rejected should not be charged
// against the corrected version. A specification handed back by the developer is
// the same journey and was not resetting anything, so every round trip cost one
// developer attempt AND one author attempt out of three.
//
// Measured: a ticket reached three claims for each role and the dispatcher then
// refused to give it to either — sitting in ready_for_dev, unclaimable, with
// nothing in any counter to say why. The hand-back loop was consuming the budget
// it needed to converge.
//
// Safe because the round trips are bounded elsewhere, by the developer's repair
// ceiling: this cannot yield more resets than that ceiling allows.
var resetsAttempts = []string{ReturnedMarker, SpecRepairMarker}

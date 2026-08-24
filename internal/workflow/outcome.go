package workflow

// Outcome is what a stage reports when it puts a ticket down.
//
// OUTCOMES ARE NOT COLUMNS, and keeping them apart is what leaves the routing
// table as the ONE place that knows column names. A stage says what happened and
// the table says where that sends the ticket; an agent naming its own
// destination would be a second routing table, and the two would disagree the
// day either changed.
type Outcome = string

const (
	OutcomeSuccess = "success"
	OutcomeFailed  = "failed"

	// OutcomeAbandoned is work the host was shut down under. Deliberately
	// distinct from failed: it is not the agent getting it wrong, so it must not
	// count against the ticket — and a transcript that recorded it as a failure
	// would teach the wrong lesson to anything later trained on one.
	OutcomeAbandoned = "abandoned"

	// OutcomeConflicted is work that is finished but does not merge. It belongs
	// to the resolver, and it is not a failure of the stage reporting it.
	OutcomeConflicted = "conflicted"

	// OutcomeBlocked is work no agent can carry further. It goes straight to the
	// column a person watches, rather than spending the remaining attempts on a
	// retry that cannot succeed.
	OutcomeBlocked = "blocked"

	// OutcomeReturned is work handed BACK to an earlier stage: the reviewer
	// rejecting a change so the developer fixes it, or the resolver finding the
	// conflict gone so the integrator merges it normally.
	//
	// Not a failure — the stage did its job and the answer was "not yet" — so it
	// does not count against that stage's attempts.
	OutcomeReturned = "returned"

	// OutcomeHandled means the stage PLACED THE TICKET ITSELF and routing must not
	// move it again. Rare and deliberate: the product manager sends a request that
	// needs no breakdown straight to the author, which is neither its stage's
	// success destination nor a failure. Without this the dispatcher's move would
	// silently undo the handler's.
	OutcomeHandled = "handled"
)

// Destination is the column an outcome sends the ticket to, or "" for one that
// must not be moved.
//
// THE ONLY PLACE THAT DECIDES. Every stage reports what happened and this reads
// the routing table, so adding a column is one edit and no agent has to be
// taught about it.
//
// retriesLeft answers the one question this package cannot: whether the ticket
// has attempts remaining for this role. That is counted from the claims on the
// ticket, which is the claim protocol's business — see docs/claiming.md — and
// passing it in keeps the routing table free of the platform's types.
func (s Stage) Destination(outcome Outcome, retriesLeft bool) string {
	switch outcome {
	case OutcomeSuccess:
		return s.Success

	case OutcomeHandled:
		// The stage placed it. Empty means "do not move it", which the caller
		// checks for.
		return ""

	case OutcomeConflicted:
		return ColConflicted

	case OutcomeBlocked:
		return s.Exhausted

	case OutcomeReturned:
		// A stage with no Returns column cannot hand work back, and falling through
		// to "do not move it" would strand the ticket in the working column with
		// nothing coming to collect it — so that case goes where every other dead
		// end goes, in front of a person.
		if s.Returns == "" {
			return s.Exhausted
		}
		return s.Returns

	case OutcomeAbandoned:
		// The host went away mid-task. Back to its queue to be picked up again.
		return s.Ready
	}

	// Failed. Retry while attempts remain, and otherwise put it where a person
	// will see it rather than cycling it through a queue it cannot leave.
	if retriesLeft {
		return s.Ready
	}
	return s.Exhausted
}

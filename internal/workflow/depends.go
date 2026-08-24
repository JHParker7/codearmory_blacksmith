package workflow

import "github.com/code-armory-app/blacksmith/internal/ticket"

// Ready reports whether every ticket this one waits for has reached ColDone.
//
// The platform deliberately returns each blocker's RAW STATUS rather than a
// "satisfied" flag, because boards define their own columns and only the
// consumer knows which of its own means finished. This is that decision, made
// here, once.
//
// A blocker the caller cannot see comes back with an empty status and is treated
// as NOT met. That is the safe direction: working a ticket whose prerequisite
// might be unfinished produces a branch built on something that does not exist,
// while waiting merely means a person has to look.
func Ready(t ticket.Ticket) bool {
	return len(Blockers(t)) == 0
}

// Blockers lists the prerequisites that have not finished, for a message a
// person can act on. Empty when the ticket is ready.
func Blockers(t ticket.Ticket) []ticket.Dependency {
	var out []ticket.Dependency
	for _, d := range t.DependsOn {
		if d.Status != ColDone {
			out = append(out, d)
		}
	}
	return out
}

// Deadlocked reports whether this ticket waits on a prerequisite that can never
// finish, and names the first such blocker.
//
// Ready requires every blocker to reach ColDone, and ColBlocked is terminal
// WITHOUT being done — so a ticket behind a blocked one is not waiting, it is
// stranded. Observed on a real batch: a chain of six tickets lost its second and
// the remaining four sat in ready_for_dev indefinitely, looking queued and
// consuming nothing. Nothing swept them and nothing said why, which is the worst
// shape a stall can take: a queue that is not moving reads exactly like a queue
// that is busy.
//
// ONLY ColBlocked COUNTS. ColConflicted is deliberately excluded — that is the
// resolver's queue, so a conflicted blocker is still on its way to done.
//
// It returns the DEPENDENCY, not a label. An earlier version returned the
// blocker's title, which reads well in a comment and is useless to a caller that
// has to act on the blocker: the revival path passed that title where an id
// belonged, failed on every ticket, and logged a warning nobody read while the
// board sat deadlocked.
func Deadlocked(t ticket.Ticket) (ticket.Dependency, bool) {
	for _, d := range t.DependsOn {
		if d.Status == ColBlocked {
			return d, true
		}
	}
	return ticket.Dependency{}, false
}

// BlockerName is how a dependency is named to a person: its title when the
// caller can see one, and its id when it cannot — which is exactly the case an
// invisible blocker produces, and the one a message most needs to be specific
// about.
func BlockerName(d ticket.Dependency) string {
	if d.Title != "" {
		return d.Title
	}
	return d.ID
}

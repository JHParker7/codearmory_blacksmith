package workflow

import (
	"testing"

	"github.com/code-armory-app/blacksmith/internal/ticket"
)

func dep(id, status string) ticket.Dependency {
	return ticket.Dependency{ID: id, Status: status}
}

func TestReadyOnlyWhenEveryPrerequisiteIsDone(t *testing.T) {
	cases := []struct {
		name string
		deps []ticket.Dependency
		want bool
	}{
		{"nothing to wait for", nil, true},
		{"all done", []ticket.Dependency{dep("a", ColDone), dep("b", ColDone)}, true},
		{"one still in dev", []ticket.Dependency{dep("a", ColDone), dep("b", ColInDev)}, false},
		{"one queued", []ticket.Dependency{dep("a", ColReadyForDev)}, false},
	}
	for _, c := range cases {
		if got := Ready(ticket.Ticket{DependsOn: c.deps}); got != c.want {
			t.Errorf("%s: Ready() = %v, want %v", c.name, got, c.want)
		}
	}
}

// AN INVISIBLE PREREQUISITE IS UNMET. A blocker this account cannot see comes
// back with an empty status, and treating that as finished builds a branch on
// something that does not exist — where waiting only means a person has to look.
func TestAnInvisiblePrerequisiteIsNotFinished(t *testing.T) {
	t.Run("empty status blocks", func(t *testing.T) {
		if Ready(ticket.Ticket{DependsOn: []ticket.Dependency{dep("a", "")}}) {
			t.Error("Ready() = true with a prerequisite of unknown status")
		}
	})
	// The mirror image, and the reason this is a test rather than a comment: the
	// platform's own "closed"/"resolved" are NOT this board's done column, and a
	// readiness check that accepted them would start work on a ticket no stage
	// here has finished.
	t.Run("the platform's closed is not this board's done", func(t *testing.T) {
		for _, s := range []string{ticket.StatusClosed, ticket.StatusResolved} {
			if Ready(ticket.Ticket{DependsOn: []ticket.Dependency{dep("a", s)}}) {
				t.Errorf("Ready() = true with a prerequisite at %q; only %q means this board finished it", s, ColDone)
			}
		}
	})
}

// A TICKET BEHIND A BLOCKED ONE IS NOT WAITING, IT IS STRANDED. Blocked is
// terminal without being done, so nothing will ever move it — observed as four
// tickets sitting in ready_for_dev indefinitely, looking queued and consuming
// nothing.
func TestDeadlockedNamesTheBlockerThatCanNeverFinish(t *testing.T) {
	got, ok := Deadlocked(ticket.Ticket{DependsOn: []ticket.Dependency{
		dep("a", ColDone), dep("b", ColBlocked), dep("c", ColInDev),
	}})
	if !ok {
		t.Fatal("Deadlocked() = false behind a blocked prerequisite")
	}
	// The DEPENDENCY, not a label: a caller that has to revive the blocker needs
	// its id. Returning the title instead failed on every ticket while the board
	// sat deadlocked.
	if got.ID != "b" {
		t.Errorf("Deadlocked() named %q, want the blocked prerequisite b", got.ID)
	}
}

// CONFLICTED IS NOT DEADLOCK. That is the resolver's queue, so a conflicted
// prerequisite is still on its way to done — sweeping it would kill live work.
func TestAConflictedPrerequisiteIsNotDeadlock(t *testing.T) {
	if _, ok := Deadlocked(ticket.Ticket{DependsOn: []ticket.Dependency{dep("a", ColConflicted)}}); ok {
		t.Error("Deadlocked() = true behind a conflicted prerequisite, which the resolver is still working")
	}
	if _, ok := Deadlocked(ticket.Ticket{DependsOn: []ticket.Dependency{dep("a", ColInDev)}}); ok {
		t.Error("Deadlocked() = true behind a prerequisite that is merely unfinished")
	}
}

func TestBlockersListsOnlyTheUnfinished(t *testing.T) {
	got := Blockers(ticket.Ticket{DependsOn: []ticket.Dependency{
		dep("done", ColDone), dep("dev", ColInDev), dep("unseen", ""),
	}})
	if len(got) != 2 {
		t.Fatalf("Blockers() returned %d entries, want the two unfinished ones: %+v", len(got), got)
	}
	if got[0].ID != "dev" || got[1].ID != "unseen" {
		t.Errorf("Blockers() = %+v, want dev and unseen in that order", got)
	}
	if len(Blockers(ticket.Ticket{DependsOn: []ticket.Dependency{dep("a", ColDone)}})) != 0 {
		t.Error("Blockers() reported a finished prerequisite")
	}
}

// The id is the fallback precisely because an INVISIBLE blocker has no title —
// which is the case a message most needs to be specific about, since a person
// has to go and find it.
func TestBlockerNameFallsBackToTheIDWhenThereIsNoTitle(t *testing.T) {
	if got := BlockerName(ticket.Dependency{ID: "t-9", Title: "Store"}); got != "Store" {
		t.Errorf("BlockerName() = %q, want the title", got)
	}
	if got := BlockerName(ticket.Dependency{ID: "t-9"}); got != "t-9" {
		t.Errorf("BlockerName() = %q, want the id when the blocker cannot be seen", got)
	}
}

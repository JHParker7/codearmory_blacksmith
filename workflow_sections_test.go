package main

import "testing"

// ONE DEVELOPER PER TASK, AND A SPECIFICATION WRITTEN A SLICE AT A TIME.
//
// A section is a slice of a task's tests, not a unit of work: it ends when its
// specification is written. The task is developed once, against every slice
// assembled by the reconciler.
//
// This reverses a change made for a weaker model. Per-section developers were
// introduced because the assembled task was the size that model could not manage
// — section SPECS took 3, 5 and 11 turns while the assembled task took 10 once
// and failed at 49 and 80 twice. That ceiling moved: measured on the current
// model against the same shape of work, a store, a JSON API and an HTML page
// with 37 tests were built in five turns.
//
// What the split bought is kept — each slice still gets its own author, with only
// its own criteria in front of it — and what it cost is dropped: a branch, a
// developer, a merge and a place in the dependency queue for every slice.

func TestASectionEndsWhenItsSpecificationIsWritten(t *testing.T) {
	spec, ok := stageFor(roleSpec)
	if !ok {
		t.Fatal("no stage for the section author")
	}
	if spec.Success != ColDone {
		t.Errorf("a finished section goes to %q; a section is specification, not work", spec.Success)
	}
}

func TestTheTaskIsDevelopedAfterItsSectionsAreAssembled(t *testing.T) {
	merge, ok := stageFor(roleSpecMerge)
	if !ok {
		t.Fatal("no stage for the reconciler")
	}
	if merge.Success != ColReadyForDev {
		t.Errorf("the reconciler hands the task to %q, want it developed", merge.Success)
	}
	if !merge.RequiresDependencies {
		t.Error("the reconciler runs before its sections are written")
	}
}

func TestOnlyTasksReachTheDeveloper(t *testing.T) {
	// The point of the change: a developer runs once per TASK. If a SECTION could
	// reach ready_for_dev there would be a developer, a branch and a merge per
	// slice again, which is what this removed.
	//
	// Two stages feed it and both are tasks: the test author, for a request small
	// enough that it was never broken down, and the reconciler, for one that was.
	feeders := map[string]bool{}
	for role, st := range stages {
		if st.Success == ColReadyForDev {
			feeders[role] = true
		}
	}
	if feeders[roleSpec] {
		t.Error("a section reaches the developer; that is a branch and a merge per slice again")
	}
	for _, want := range []string{roleTest, roleSpecMerge} {
		if !feeders[want] {
			t.Errorf("%s does not reach the developer, so its tasks are never built", want)
		}
	}
	if len(feeders) != 2 {
		t.Errorf("stages feeding the developer = %v, want exactly the two task stages", feeders)
	}
}

func TestEverySuccessPathStillTerminates(t *testing.T) {
	// Three edges moved. A cycle here is a ticket that circulates forever looking
	// busy, and a dead end is a column nothing collects from — which reads on a
	// board exactly like a queue that is merely busy.
	terminal := map[string]bool{ColDone: true, ColBlocked: true, ColTracking: true}
	for role, start := range stages {
		seen := map[string]bool{}
		column := start.Success
		for !terminal[column] {
			if seen[column] {
				t.Fatalf("success loop from %s at %q", role, column)
			}
			seen[column] = true

			var next Stage
			found := false
			for _, st := range stages {
				if st.Ready == column {
					next, found = st, true
					break
				}
			}
			if !found {
				t.Fatalf("from %s: nothing takes from %q, so a ticket stops there", role, column)
			}
			column = next.Success
		}
	}
}

func TestAHandBackAlwaysReachesAStageThatPollsIt(t *testing.T) {
	// A return column nothing polls is the worst shape of stall: the ticket looks
	// queued and nothing is coming for it.
	for role, st := range stages {
		if st.Returns == "" {
			continue
		}
		polled := false
		for _, other := range stages {
			if other.Ready == st.Returns {
				polled = true
				break
			}
		}
		if !polled {
			t.Errorf("%s hands back to %q, which no stage takes from", role, st.Returns)
		}
	}
}

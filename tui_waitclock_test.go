package main

import (
	"testing"
	"time"
)

// A WAITING TICKET IS NOT RUNNING, AND ITS CLOCK MUST NOT SAY IT IS.
//
// The runtime counted from CreatedAt for anything unfinished, so a ticket queued
// behind its dependencies climbed while nothing whatever was being spent on it.
// On a board where every task waits for every task before it, that is most of the
// board most of the time, and the figure shown was the age of the run rather than
// the cost of the work.
//
// Reported from the window on r85: four tickets sat at fifteen minutes each while
// one dev agent worked, and the obvious reading was that the run had got slower.
// It had not — they had not started.

func TestAQueuedTicketShowsNoRuntime(t *testing.T) {
	// Waiting for a stage to take it, created a quarter of an hour ago.
	for _, status := range []string{ColReadyForDev, ColReadyForSpec, ColReadyForSpecMerge, ColInbox} {
		tk := Ticket{TicketID: "t1000000", Status: status, CreatedAt: time.Now().Add(-15 * time.Minute)}
		if d, _ := ticketRuntime(tk); d != 0 {
			t.Errorf("a ticket queued in %q reports %s of runtime; nothing is being spent on it", status, runtimeText(d))
		}
	}
}

// A ticket a stage is holding DOES tick, from when that stage took it.
func TestAHeldTicketCountsFromWhenTheStageTookIt(t *testing.T) {
	created := time.Now().Add(-15 * time.Minute)
	tk := Ticket{
		TicketID:  "t1000000",
		Status:    ColInDev,
		CreatedAt: created,
		UpdatedAt: time.Now().Add(-90 * time.Second), // the dev stage claimed it 90s ago
	}

	d, final := ticketRuntime(tk)
	if final {
		t.Error("a ticket in development was reported as finished")
	}
	if d > 5*time.Minute {
		t.Errorf("runtime = %s; it is counting the fourteen minutes it spent queued, not the ninety seconds of work", runtimeText(d))
	}
	if d < 30*time.Second {
		t.Errorf("runtime = %s, want about 90s", runtimeText(d))
	}
}

// Every working column must tick, or a stage added later is silently invisible.
func TestEveryStagesWorkingColumnCounts(t *testing.T) {
	for role, st := range stages {
		if st.Working == "" {
			continue
		}
		if !heldByAStage(st.Working) {
			t.Errorf("%s holds tickets in %q and that column does not count as work", role, st.Working)
		}
	}
	// And a queue column must not.
	for role, st := range stages {
		if st.Ready == "" || st.Ready == st.Working {
			continue
		}
		if heldByAStage(st.Ready) {
			t.Errorf("%s queues tickets in %q and that column counts as work", role, st.Ready)
		}
	}
}

// Finished work still reports what it took, end to end — that is the one case
// where the queue time is part of the honest answer.
func TestAFinishedTicketStillReportsTheWholeThing(t *testing.T) {
	created := time.Now().Add(-3 * time.Hour)
	tk := Ticket{
		TicketID:  "t1000000",
		Status:    ColDone,
		CreatedAt: created,
		UpdatedAt: created.Add(4*time.Minute + 54*time.Second),
	}

	d, final := ticketRuntime(tk)
	if !final {
		t.Error("a done ticket is still counting")
	}
	if got := runtimeText(d); got != "4m54s" {
		t.Errorf("runtime = %q, want %q", got, "4m54s")
	}
}

// Blocked stops too: the department has given up and is waiting for a person.
func TestABlockedTicketStaysStopped(t *testing.T) {
	created := time.Now().Add(-2 * time.Hour)
	tk := Ticket{TicketID: "t1000000", Status: ColBlocked, CreatedAt: created,
		UpdatedAt: created.Add(3 * time.Minute)}

	if _, final := ticketRuntime(tk); !final {
		t.Error("a blocked ticket is counting again")
	}
}

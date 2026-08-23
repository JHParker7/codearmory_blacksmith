package main

import (
	"strings"
	"testing"
	"time"
)

// A FINISHED TICKET MUST STOP COUNTING.
//
// time.Since(CreatedAt) is the right clock for work in flight and the wrong one
// for work that is over: it keeps climbing for as long as the board is open, so a
// task that took four minutes yesterday reads as eighteen hours today and answers
// nothing. The point of showing a runtime is to compare runtimes.
func TestAFinishedTicketReportsWhatItTookNotHowOldItIs(t *testing.T) {
	created := time.Now().Add(-18 * time.Hour)
	tk := Ticket{
		TicketID:  "t1000000",
		Status:    ColDone,
		CreatedAt: created,
		UpdatedAt: created.Add(4*time.Minute + 54*time.Second),
	}

	d, final := ticketRuntime(tk)
	if !final {
		t.Error("a done ticket's runtime is not reported as final; it will keep climbing")
	}
	if got := runtimeText(d); got != "4m54s" {
		t.Errorf("runtime = %q, want %q — it is reporting age, not duration", got, "4m54s")
	}
}

// Work in flight is still measured from now, because that is the live answer.
func TestARunningTicketCountsUp(t *testing.T) {
	tk := Ticket{TicketID: "t1000000", Status: ColInDev, CreatedAt: time.Now().Add(-90 * time.Second)}

	d, final := ticketRuntime(tk)
	if final {
		t.Error("a ticket still in development was reported as finished")
	}
	if d < 80*time.Second || d > 200*time.Second {
		t.Errorf("runtime = %s, want about 90s", d)
	}
}

// SECONDS ARE THE POINT. shortDuration rounds to whole minutes, which is right
// for "how stale is this" and useless for "which of these was slow". Measured on
// r77: two sections took 5m44s and 5m00s and both render as "5m"; two more took
// 1m50s and 1m12s and both render as "1m".
func TestRuntimeKeepsTheSecondsThatDistinguishTickets(t *testing.T) {
	// Every duration here is one r77 actually produced.
	cases := map[time.Duration]string{
		33 * time.Second:                "33s",
		time.Minute + 12*time.Second:    "1m12s",
		time.Minute + 50*time.Second:    "1m50s",
		5 * time.Minute:                 "5m00s",
		5*time.Minute + 44*time.Second:  "5m44s",
		10*time.Minute + 46*time.Second: "10m46s",
		time.Hour + 3*time.Minute:       "1h03m",
	}
	for d, want := range cases {
		if got := runtimeText(d); got != want {
			t.Errorf("runtimeText(%s) = %q, want %q", d, got, want)
		}
	}
	// The distinction shortDuration cannot make, pinned so nobody "simplifies"
	// this back to it. Both of these are real r77 sections.
	for _, pair := range [][2]time.Duration{
		{5*time.Minute + 44*time.Second, 5 * time.Minute},
		{time.Minute + 50*time.Second, time.Minute + 12*time.Second},
	} {
		if shortDuration(pair[0]) != shortDuration(pair[1]) {
			t.Errorf("shortDuration now separates %s from %s; this guard is obsolete", pair[0], pair[1])
		}
		if runtimeText(pair[0]) == runtimeText(pair[1]) {
			t.Errorf("%s and %s render identically; the column cannot be compared", pair[0], pair[1])
		}
	}
}

// A ticket with no creation time must not render a bogus duration.
func TestATicketWithNoClockShowsNoRuntime(t *testing.T) {
	if d, _ := ticketRuntime(Ticket{TicketID: "t1000000", Status: ColDone}); d != 0 {
		t.Errorf("a ticket with no CreatedAt reported a runtime of %s", d)
	}
}

// The row has to make room for the column, or the title runs into it — the same
// width arithmetic that once put a negative into clip and panicked the window.
func TestTheRuntimeColumnIsPaidForOutOfTheTitle(t *testing.T) {
	m := tuiModel{width: 120}
	if got := m.titleWidth(); got < 20 {
		t.Fatalf("titleWidth = %d, which cannot be right", got)
	}
	narrow := tuiModel{width: 40}
	if got := narrow.titleWidth(); got < 20 {
		t.Errorf("titleWidth on a narrow window = %d; it must clamp, not go negative", got)
	}
}

// A negative duration cannot reach the formatter as a negative string: the clock
// arithmetic here subtracts timestamps that arrive from a service, and one of
// them being behind the other is not a reason to render "-3m".
func TestANegativeRuntimeRendersAsZero(t *testing.T) {
	if got := runtimeText(-5 * time.Minute); !strings.HasPrefix(got, "0") {
		t.Errorf("runtimeText(-5m) = %q, want a zero", got)
	}
}

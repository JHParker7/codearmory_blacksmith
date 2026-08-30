package main

import (
	"testing"

	"github.com/code-armory-app/blacksmith/internal/ticket"
)

func tk(id, title, priority string) ticket.Ticket {
	return ticket.Ticket{ID: id, Title: title, Priority: priority, Status: ticket.StatusOpen}
}

// SECURITY BEFORE QUALITY, ALWAYS — a low security hole outranks a critical
// quality nit, because the kinds are not on one severity ladder. This is the
// ordering the whole feature exists to guarantee.
func TestSecurityFindingsAreWorkedBeforeQuality(t *testing.T) {
	open := []ticket.Ticket{
		tk("q1", "quality: duplicated validation", "high"),
		tk("s1", "security: missing rate limit", "low"),
		tk("req", "request: build the thing", "medium"), // not a finding, skipped
		tk("q2", "quality: unclear name", "critical"),
		tk("s2", "security: SQL injection", "critical"),
	}
	got := pickFindings(open)

	if len(got) != 4 {
		t.Fatalf("picked %d findings, want 4 (the request skipped): %+v", len(got), got)
	}
	// Both security findings come first, in severity order.
	if got[0].id != "s2" || got[1].id != "s1" {
		t.Fatalf("security not first, by severity: %s then %s", got[0].id, got[1].id)
	}
	if got[2].kind != "quality" || got[3].kind != "quality" {
		t.Fatalf("quality did not follow security: %+v", got)
	}
	// Quality block by severity: critical before high.
	if got[2].id != "q2" || got[3].id != "q1" {
		t.Fatalf("quality not by severity: %s then %s", got[2].id, got[3].id)
	}
}

// Only reviewer findings are worked: requests and unkinded tickets are not
// code to refine.
func TestNonFindingTicketsAreSkipped(t *testing.T) {
	open := []ticket.Ticket{
		tk("r", "request: something", "high"),
		tk("x", "a hand-written ticket", "high"),
	}
	if got := pickFindings(open); len(got) != 0 {
		t.Fatalf("picked non-findings: %+v", got)
	}
}

// An unknown severity sorts LAST within its kind, so a mis-severitied ticket
// does not jump the queue ahead of ones that named a real level.
func TestUnknownSeveritySortsLast(t *testing.T) {
	open := []ticket.Ticket{
		tk("weird", "security: odd", "banana"),
		tk("real", "security: real", "medium"),
	}
	got := pickFindings(open)
	if got[0].id != "real" {
		t.Fatalf("the unknown severity jumped the queue: %+v", got)
	}
}

// The fix branch name is derived from the finding id, short enough to be a
// branch and stable per ticket.
func TestFixBranchNameIsStableAndShort(t *testing.T) {
	id := "0f402588-1922-4b73-a1e9-c3a03a8ec6d0"
	if got := shortID(id); got != "0f402588" {
		t.Fatalf("shortID = %q", got)
	}
}

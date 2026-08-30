package main

import (
	"testing"

	"github.com/code-armory-app/blacksmith/internal/ticket"
)

// A finding already attempted this session is skipped, so an unfixable one
// left open for a person does not send the loop back to it forever. This is
// the guard against the risk the first live run surfaced: findings that could
// not be fixed without changing the locked tests, re-picked every cycle.
func TestAttemptedFindingsAreSkipped(t *testing.T) {
	open := []ticket.Ticket{
		tk("s1", "security: cannot fix without breaking tests", "critical"),
		tk("s2", "security: real fixable thing", "high"),
	}
	skip := map[string]bool{"s1": true}
	got := pickFindings(open, skip)
	if len(got) != 1 || got[0].id != "s2" {
		t.Fatalf("the attempted finding was not skipped: %+v", got)
	}
}

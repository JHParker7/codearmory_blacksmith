package tools

import (
	"context"
	"strings"
	"testing"
)

// The tool lands on the callback and reports the ticket back to the model, so
// the reviewer knows the finding EXISTS somewhere rather than concluding the
// call vanished.
func TestFileTicketLandsOnTheBoardAndReportsTheID(t *testing.T) {
	var gotTitle, gotSeverity string
	s := newSet(map[string]string{}, DenyAll, nil, ReadFiles, FileTicket)
	s.FileTicket = func(kind, title, body, severity string) (string, error) {
		gotTitle, gotSeverity = title, severity
		return "tk-42", nil
	}

	out, err := s.Invoke(context.Background(), FileTicket,
		`{"title":"SQL injection in listTasks","body":"store.go:88 — user input reaches the query; parameterise it","severity":"high"}`)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !strings.Contains(out, "tk-42") || !strings.Contains(out, "high") {
		t.Fatalf("the result does not name the ticket: %q", out)
	}
	if gotTitle != "SQL injection in listTasks" || gotSeverity != "high" {
		t.Fatalf("the callback saw %q/%q", gotTitle, gotSeverity)
	}
}

// No store wired: the tool says so and points at the answer — and NEVER at
// the repository, which is the whole reason the board exists.
func TestFileTicketWithoutAStoreSaysWhereFindingsGoInstead(t *testing.T) {
	s := newSet(map[string]string{}, DenyAll, nil, FileTicket)

	out, err := s.Invoke(context.Background(), FileTicket,
		`{"title":"x","body":"y","severity":"low"}`)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !strings.Contains(out, "final answer") || !strings.Contains(out, "NOT") {
		t.Fatalf("the refusal does not redirect the finding safely: %q", out)
	}
}

// An empty finding is refused: a ticket is work for a person.
func TestAnEmptyFindingIsRefused(t *testing.T) {
	s := newSet(map[string]string{}, DenyAll, nil, FileTicket)
	s.FileTicket = func(string, string, string, string) (string, error) { return "tk", nil }

	out, err := s.Invoke(context.Background(), FileTicket, `{"title":" ","body":"","severity":"low"}`)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !strings.Contains(out, "Error") {
		t.Fatalf("an empty ticket was filed: %q", out)
	}
}

// The stage's kind reaches the callback, not the model's choosing — a security
// stage files "security", a quality stage files "quality", and neither can
// mislabel the other's severity ladder.
func TestTheStagesKindReachesTheCallback(t *testing.T) {
	var gotKind string
	s := newSet(map[string]string{}, DenyAll, nil, FileTicket)
	s.TicketKind = "security"
	s.FileTicket = func(kind, title, body, severity string) (string, error) {
		gotKind = kind
		return "tk", nil
	}

	if _, err := s.Invoke(context.Background(), FileTicket,
		`{"title":"t","body":"b","severity":"high"}`); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if gotKind != "security" {
		t.Fatalf("the callback saw kind %q, want security", gotKind)
	}
}

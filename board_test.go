package main

import (
	"strings"
	"testing"
)

// The TUI board shows the findings themselves, security first then by severity,
// with a summary and a fixed count — the refiner's queue, drawn as one.
func TestBoardRenderShowsFindingsInQueueOrder(t *testing.T) {
	v := boardView{
		reachable:      true,
		openSecurity:   1,
		openQuality:    1,
		resolved:       3,
		activeRequests: []string{"build a task API"},
		findings: []boardFinding{
			// Deliberately out of order; render relies on snapshotBoard's sort,
			// so hand it pre-sorted (security first, then severity) as it would be.
			{kind: "security", severity: "critical", title: "nil MaxBytesReader panics"},
			{kind: "quality", severity: "medium", title: "PUT cannot clear Description"},
		},
	}
	out := v.render()

	if !strings.Contains(out, "1 security · 1 quality open") {
		t.Errorf("summary line missing counts:\n%s", out)
	}
	if !strings.Contains(out, "3 fixed") {
		t.Errorf("summary should show the fixed count:\n%s", out)
	}
	if !strings.Contains(out, "build a task API") || !strings.Contains(out, "building") {
		t.Errorf("in-flight request not shown:\n%s", out)
	}
	if !strings.Contains(out, "nil MaxBytesReader panics") || !strings.Contains(out, "PUT cannot clear Description") {
		t.Errorf("findings not listed:\n%s", out)
	}
	// Security must render above quality.
	if strings.Index(out, "MaxBytesReader") > strings.Index(out, "clear Description") {
		t.Errorf("security finding should render before quality:\n%s", out)
	}
}

func TestBoardRenderCapsRowsAndCountsTheRest(t *testing.T) {
	v := boardView{reachable: true}
	for i := 0; i < 12; i++ {
		v.findings = append(v.findings, boardFinding{kind: "quality", severity: "low", title: "nit"})
	}
	out := v.render()
	if strings.Count(out, "nit") != 8 {
		t.Errorf("expected 8 finding rows, got %d:\n%s", strings.Count(out, "nit"), out)
	}
	if !strings.Contains(out, "…and 4 more") {
		t.Errorf("expected the overflow count line:\n%s", out)
	}
}

func TestBoardRenderEmptyIsHonest(t *testing.T) {
	out := boardView{reachable: true}.render()
	if !strings.Contains(out, "no open findings") {
		t.Errorf("an empty board should say so:\n%s", out)
	}
}

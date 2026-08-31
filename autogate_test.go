package main

import "testing"

// The merge gate is defined by severity: critical, high, and medium findings
// HOLD a project's merge to dev until they are fixed; low and unrecognized
// severities do not. This pins that boundary — the rule the user set is
// "don't merge till all findings above low severity are fixed."
func TestAboveLowSeverityGatesTheMerge(t *testing.T) {
	gates := map[string]bool{
		"critical": true,
		"high":     true,
		"medium":   true,
		"low":      false,
		"":         false, // unrecognized: does not hold the merge
		"trivial":  false,
	}
	for sev, want := range gates {
		if got := isAboveLow(sev); got != want {
			t.Errorf("isAboveLow(%q) = %v, want %v", sev, got, want)
		}
	}
	// Case-insensitive, like the rest of the severity handling.
	if !isAboveLow("HIGH") {
		t.Error("isAboveLow should be case-insensitive: HIGH must gate")
	}
}

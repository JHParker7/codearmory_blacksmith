package main

import (
	"strings"
	"testing"
)

// Ordering, not gating: the reviewer blocks nothing, so this stage runs after it
// so the review lands on a proposal rather than on something already integrated.
//
// That ordering is now the BOARD's: this stage takes from ready_for_integration,
// which is where the reviewer's success moves a ticket, so an unreviewed branch
// never reaches it. The "merged once, never again" and "do not retry a conflict"
// rules moved the same way — a merged ticket is in done and a conflicted one is
// in conflicted, and neither is a column this stage reads.
func TestIntegratorRunsAfterReview(t *testing.T) {
	review, ok := stageFor(roleReview)
	if !ok {
		t.Fatal("no stage for the reviewer")
	}
	integrate, ok := stageFor(roleIntegrate)
	if !ok {
		t.Fatal("no stage for the integrator")
	}
	if review.Success != integrate.Ready {
		t.Errorf("the reviewer hands to %q but the integrator reads %q; reviewed work would never be merged",
			review.Success, integrate.Ready)
	}
	// And a merge that has happened must not be re-read: done is nobody's queue.
	for role, st := range stages {
		if st.Ready == ColDone {
			t.Errorf("%s takes from %q; finished work would be worked again", role, ColDone)
		}
	}

	// What is left for the predicate is the part the column cannot say.
	a := NewIntegratorAgent(nil, RepoConfig{URL: "git://host/demo.git"}, "dev")
	branch := Comment{Body: branchMarker + "\n\n- **Branch:** `agent/t1`\n"}
	if !a.Wants(Ticket{Comments: []Comment{branch}}) {
		t.Error("does not take a ticket with a branch to merge")
	}
	if a.Wants(Ticket{}) {
		t.Error("takes a ticket with no branch; there is nothing to merge")
	}
}

// The gates must run on the MERGED tree and the push must come last. Each agent
// branch was verified against the base it cloned; merged, none of them has been
// tested in the combined state.
func TestMergeScriptVerifiesTheResultBeforePushing(t *testing.T) {
	a := NewIntegratorAgent(nil, RepoConfig{
		URL: "git://host/demo.git", Branch: "main",
		TestCommand: "go test ./...", CriticalCommand: "govulncheck ./...",
	}, "dev")
	s := a.mergeScript("agent/t1")

	merge := strings.Index(s, "git merge --no-ff")
	test := strings.Index(s, "go test ./...")
	crit := strings.Index(s, "govulncheck ./...")
	push := strings.Index(s, "git push origin dev")
	if merge < 0 || test < 0 || crit < 0 || push < 0 {
		t.Fatalf("script is missing a step:\n%s", s)
	}
	if !(merge < test && test < push) {
		t.Error("the tests do not run between the merge and the push")
	}
	if crit > push {
		t.Error("the critical gate runs after the push; a finding would already be on the branch")
	}
	// A conflict must be distinguishable from a failing test without reading
	// git's wording, and must not push.
	if !strings.Contains(s[:push], mergeConflictMarker) {
		t.Error("a conflict is not marked distinctly")
	}
	// First use of a repository has no integration branch yet.
	if !strings.Contains(s, `git checkout -q "dev" 2>/dev/null ||`) {
		t.Error("the integration branch is not created when absent; the stage would fail once per repo")
	}
}

// A conflict report is only useful if it names the files.
func TestConflictDetailNamesThePaths(t *testing.T) {
	out := "merging\n" + mergeConflictMarker + "\nmain.go\ninternal/api.go\n"
	got := conflictDetail(out)
	if !strings.Contains(got, "main.go") || !strings.Contains(got, "internal/api.go") {
		t.Errorf("conflictDetail = %q, want the conflicted paths", got)
	}
	if strings.Contains(got, "merging") {
		t.Error("conflictDetail includes output from before the conflict")
	}
}

// A conflict and a broken merge ask for OPPOSITE work: one is two changes
// disagreeing about intent and someone must decide which wins; the other is a
// change correct alone and wrong in combination, with nothing to reconcile.
// Reporting both as "merge conflict" sends a person hunting for a disagreement
// that is not there.
func TestConflictAndIntegrationFailureAreDistinct(t *testing.T) {
	if conflictMarker == integrationFailMarker {
		t.Fatal("the two failures share a marker")
	}
	base := []Comment{
		{Body: branchMarker + "\n\n- **Branch:** `agent/t1`\n"},
		{Body: reviewMarker + " (automated)\n\nclean"},
	}
	conflicted := Ticket{Comments: append(append([]Comment{}, base...), Comment{Body: conflictMarker})}
	broken := Ticket{Comments: append(append([]Comment{}, base...), Comment{Body: integrationFailMarker})}

	if !hasConflict(conflicted) || hasIntegrationFailure(conflicted) {
		t.Error("a conflict is misread as an integration failure")
	}
	if !hasIntegrationFailure(broken) || hasConflict(broken) {
		t.Error("an integration failure is misread as a conflict")
	}
	// Neither is retried, and the ROUTING is what guarantees it: a conflict is
	// reported as OutcomeConflicted and a broken merge as OutcomeBlocked, which
	// send the ticket to the resolver's column and to a person's column
	// respectively — neither of which the integrator reads.
	integrate, _ := stageFor(roleIntegrate)
	if ColConflicted == integrate.Ready || integrate.Exhausted == integrate.Ready {
		t.Error("a settled failure lands back in the integrator's own queue and is picked up again")
	}
	if ColConflicted != stages[roleResolve].Ready {
		t.Errorf("a conflict goes to %q but the resolver reads %q; conflicts would never be settled",
			ColConflicted, stages[roleResolve].Ready)
	}
}

// REBASE BEFORE MERGE. An agent branch is cut from the integration branch when
// its sandbox clones, and this stage is serial, so by the time a branch arrives
// the integration branch has usually moved beneath it. Merging then compares the
// work against a snapshot that no longer exists and every ticket touching the
// same file conflicts by construction — measured at three of four unfinished
// tickets in a run, all of them correct and already reviewed.
func TestIntegratorReplaysTheBranchOntoTheCurrentTip(t *testing.T) {
	a := NewIntegratorAgent(nil, RepoConfig{
		URL: "git://host/demo.git", Branch: "main", TestCommand: "go test ./...",
	}, "dev")
	s := a.mergeScript("agent/t1")

	rebase := strings.Index(s, "git rebase")
	merge := strings.Index(s, "git merge")
	push := strings.Index(s, "git push")
	if rebase < 0 {
		t.Fatalf("the branch is merged without being replayed onto the current tip:\n%s", s)
	}
	if !(rebase < merge && merge < push) {
		t.Errorf("rebase/merge/push are out of order (%d/%d/%d):\n%s", rebase, merge, push, s)
	}

	// The preamble sets -e, so a failing rebase must be HANDLED rather than
	// killing the script — that is the case this exists to report.
	line := s[rebase:]
	if end := strings.IndexByte(line, '\n'); end >= 0 {
		line = line[:end]
	}
	if !strings.Contains(s[:rebase], "if !") {
		t.Errorf("the rebase is unguarded under `set -e`, so a conflict ends the script silently:\n  %s", line)
	}
	// A conflict must still be reported to the resolver, and the conflicted paths
	// captured BEFORE the abort unwinds the state that names them.
	conflict := strings.Index(s, mergeConflictMarker)
	abort := strings.Index(s, "git rebase --abort")
	if conflict < 0 || abort < 0 {
		t.Fatal("a rebase conflict is not reported and aborted")
	}
	if conflict > abort {
		t.Error("the conflict is reported after the abort, by which point the paths are gone")
	}
	if strings.Index(s, "--diff-filter=U") > abort {
		t.Error("the conflicted paths are collected after the abort, so they will be empty")
	}
}

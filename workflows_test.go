package main

import (
	"context"
	"strings"
	"testing"
)

// ── unit ──────────────────────────────────────────────────────────────────────

// awaiting_approval is terminal for the agent though the run is unfinished: it
// is waiting on a person, and blocking on it holds a slot for as long as they
// take. Getting this wrong either spins forever or treats a paused run as a pass.
func TestIsTerminalRun(t *testing.T) {
	for _, s := range []string{RunCompleted, RunFailed, RunCancelled, RunAwaitingApproval} {
		if !isTerminalRun(s) {
			t.Errorf("isTerminalRun(%q) = false, want true", s)
		}
	}
	for _, s := range []string{RunPending, RunRunning, RunWaitingResources, "", "unknown"} {
		if isTerminalRun(s) {
			t.Errorf("isTerminalRun(%q) = true, want false", s)
		}
	}
}

// Only a completed run is a pass. An approval gate is emphatically not one.
func TestRunPassed(t *testing.T) {
	if !(Run{Status: RunCompleted}).Passed() {
		t.Error("a completed run is not Passed()")
	}
	for _, s := range []string{RunFailed, RunCancelled, RunAwaitingApproval, RunRunning, RunPending} {
		if (Run{Status: s}).Passed() {
			t.Errorf("status %q reported Passed()", s)
		}
	}
}

func TestTriggerRunRequiresAPipeline(t *testing.T) {
	c := &CodeArmory{}
	if _, err := c.TriggerRun(context.Background(), "", nil); err == nil {
		t.Error("TriggerRun with no pipeline = nil error")
	}
}

// ── integration ───────────────────────────────────────────────────────────────

func TestWaitForRunPollsToTerminal(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.runSteps = 3

	run, err := api.TriggerRun(context.Background(), "pipe-1", map[string]string{"branch": "agent/x"})
	if err != nil {
		t.Fatalf("TriggerRun() = %v", err)
	}
	final, err := api.WaitForRun(context.Background(), run.RunID)
	if err != nil {
		t.Fatalf("WaitForRun() = %v", err)
	}
	if !final.Passed() {
		t.Errorf("run = %+v, want completed", final)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.runInputs) != 1 || f.runInputs[0]["branch"] != "agent/x" {
		t.Errorf("inputs = %v, want the branch passed to the pipeline", f.runInputs)
	}
}

// The pipeline path must carry conductor's service prefix, like every other one.
func TestWorkflowsPathsCarryTheServicePrefix(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	if _, err := api.TriggerRun(context.Background(), "pipe-1", nil); err != nil {
		t.Fatalf("TriggerRun() = %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var ok bool
	for _, c := range f.calls {
		if strings.Contains(c, "/workflows/pipelines/pipe-1/runs") {
			ok = true
		}
	}
	if !ok {
		t.Errorf("no request went to /workflows/pipelines/...; calls were %v", f.calls)
	}
}

// A run belongs to the platform and stays visible there, so the agent going away
// must NOT cancel it — the opposite of the sandbox rule, and deliberately so.
func TestWaitForRunDoesNotCancelTheRunOnShutdown(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.runSteps = 10000 // never finishes on its own

	run, err := api.TriggerRun(context.Background(), "pipe-1", nil)
	if err != nil {
		t.Fatalf("TriggerRun() = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := api.WaitForRun(ctx, run.RunID)
		done <- err
	}()
	waitFor(t, "the run to be polled", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.runPolls[run.RunID] > 0
	})
	cancel()

	if err := <-done; err == nil {
		t.Fatal("WaitForRun() = nil error after cancellation")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.runs[run.RunID].Status == RunCancelled {
		t.Error("the run was cancelled when the agent stopped waiting; a pipeline run should finish and stay reviewable")
	}
}

// The agent must not treat a human approval gate as a verdict it can act on.
func TestDevAgentStopsAtAnApprovalGate(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x", CreatedBy: "alice"})
	f.runOutcome = RunAwaitingApproval

	gw, _ := scriptedModel(t,
		`{"action":"write_files","edits":[{"path":"a.go","search":"","replace":"package main"}]}`,
		`{"action":"run_tests"}`,
		`{"action":"finish","summary":"done"}`,
		`{"action":"give_up","reason":"waiting on approval"}`,
	)
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "t1", "dev-agent")

	status, _, err := devTestAgent(t, gw, api).Handle(ctx, f.get("t1"))
	if err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	if status != OutcomeFailed {
		t.Errorf("status = %q, want failure: an approval gate is not a pass", status)
	}
	for _, b := range f.commentBodies("t1") {
		if strings.Contains(b, branchMarker) {
			t.Error("the agent reported success while a run was waiting for approval")
		}
	}
}

// Verification must go through the pipeline, and the branch must be pushed
// before it runs — a pipeline verifies a branch, not a working copy.
func TestDevAgentVerifiesViaPipelineOnAPushedBranch(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1abc999", Title: "x", CreatedBy: "alice"})

	gw, _ := scriptedModel(t,
		`{"action":"write_files","edits":[{"path":"a.go","search":"","replace":"package main"}]}`,
		`{"action":"run_tests"}`,
		`{"action":"finish","summary":"did the thing"}`,
	)
	rec, _ := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr", "t1abc999", "dev-agent")

	status, detail, err := devTestAgent(t, gw, api).Handle(ctx, f.get("t1abc999"))
	if err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	if status != OutcomeSuccess {
		t.Fatalf("status = %q (%s)", status, detail)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.runInputs) != 1 {
		t.Fatalf("triggered %d pipeline runs, want 1", len(f.runInputs))
	}
	if got := f.runInputs[0]["branch"]; got != "agent/t1abc999" {
		t.Errorf("pipeline branch input = %q, want the agent's branch", got)
	}
	if got := f.runInputs[0]["ticket_id"]; got != "t1abc999" {
		t.Errorf("pipeline ticket_id input = %q", got)
	}

	// The push must have happened BEFORE the run was triggered.
	var pushedAt, triggeredAt = -1, -1
	for i, c := range f.calls {
		if strings.Contains(c, "/forge/executions") && pushedAt == -1 && i > 0 {
			pushedAt = i
		}
		if strings.Contains(c, "/workflows/pipelines/") {
			triggeredAt = i
		}
	}
	if triggeredAt == -1 || pushedAt == -1 || pushedAt > triggeredAt {
		t.Errorf("push at %d, trigger at %d: a pipeline verifies a branch, so the push must come first", pushedAt, triggeredAt)
	}
}

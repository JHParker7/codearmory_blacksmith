package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Verification runs through CodeArmory's pipelines, not through a command the
// agent invents.
//
// The agent pushes a branch and triggers the repository's own pipeline, exactly
// as a person's push would. That matters for three reasons:
//
//   - PARITY. Agent-written code is verified by the same definition, the same
//     steps and the same runner as everyone else's. A separate "agent test
//     command" is a second definition of "does this work", and two definitions
//     drift.
//   - The pipeline lives in the PLATFORM, so nobody has to keep an
//     AGENTS_REPO_TEST_COMMAND in sync with what CI actually does.
//   - The run is visible where every other run is — with its own history, logs
//     and approval gates — rather than buried in an agent's transcript.
//
// The agent still uses forge to read the repository and push the branch. That is
// workspace I/O, not verification, and it is the same distinction a developer
// makes between their laptop and CI.

const workflowsService = "/workflows"

// Workflow run statuses.
const (
	RunPending          = "pending"
	RunRunning          = "running"
	RunCompleted        = "completed"
	RunFailed           = "failed"
	RunCancelled        = "cancelled"
	RunAwaitingApproval = "awaiting_approval"
	RunWaitingResources = "waiting_for_resources"
)

// Run is a pipeline run.
type Run struct {
	RunID       string            `json:"run_id"`
	WorkflowID  string            `json:"workflow_id"`
	Status      string            `json:"status"`
	CurrentStep int               `json:"current_step"`
	Inputs      map[string]string `json:"inputs,omitempty"`
	Outputs     map[string]string `json:"outputs,omitempty"`
}

// Passed reports a run that finished successfully.
func (r Run) Passed() bool { return r.Status == RunCompleted }

// isTerminalRun reports whether a run's status will not change on its own.
//
// awaiting_approval counts as terminal HERE though the run is not finished: it
// is waiting on a human, and an agent that blocks on it holds a slot for as long
// as the person takes. The agent reports it and stops — deciding to approve is
// not the agent's call, which is the point of the gate.
func isTerminalRun(status string) bool {
	switch status {
	case RunCompleted, RunFailed, RunCancelled, RunAwaitingApproval:
		return true
	}
	return false
}

// TriggerRun starts a pipeline.
func (c *CodeArmory) TriggerRun(ctx context.Context, pipelineID string, inputs map[string]string) (Run, error) {
	if pipelineID == "" {
		return Run{}, fmt.Errorf("trigger run: no pipeline configured")
	}
	var out Run
	err := c.do(ctx, http.MethodPost,
		workflowsService+"/pipelines/"+url.PathEscape(pipelineID)+"/runs",
		map[string]any{"inputs": inputs}, &out)
	return out, err
}

// GetRun reads a run's current state.
func (c *CodeArmory) GetRun(ctx context.Context, runID string) (Run, error) {
	var out Run
	err := c.do(ctx, http.MethodGet, workflowsService+"/runs/"+url.PathEscape(runID), nil, &out)
	return out, err
}

// WaitForRun polls until the run reaches a terminal state.
//
// Deliberately does NOT cancel the run when the caller goes away, which is the
// opposite of the sandbox rule. A pipeline run belongs to the platform and is
// visible there: if the agent is shut down mid-verification, the run should
// finish and be reviewable, exactly as it would if a person closed their laptop.
func (c *CodeArmory) WaitForRun(ctx context.Context, runID string) (Run, error) {
	minWait, maxWait := c.pollBounds()
	for attempt := 0; ; attempt++ {
		select {
		case <-ctx.Done():
			return Run{}, fmt.Errorf("run %s: stopped waiting: %w", runID, ctx.Err())
		case <-time.After(execPollInterval(attempt, minWait, maxWait)):
		}

		run, err := c.GetRun(ctx, runID)
		if err != nil {
			if ctx.Err() != nil {
				return Run{}, fmt.Errorf("run %s: stopped waiting: %w", runID, ctx.Err())
			}
			// A blip must not abandon a run that is still going; do() has already
			// exhausted its own retries.
			if isTransient(err) {
				continue
			}
			return Run{}, fmt.Errorf("run %s: %w", runID, err)
		}
		if isTerminalRun(run.Status) {
			return run, nil
		}
	}
}

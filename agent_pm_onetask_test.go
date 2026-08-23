package main

import "testing"

// THE PIPELINE IS STRICTLY SERIAL, SO EVERY SPLIT IS A FIXED COST PAID IN FULL.
//
// Tasks are chained — every task waits for every task before it — and the model
// serves one request at a time whatever the slot count says, so splitting buys
// parallelism that neither the graph nor the hardware can deliver. What it does
// buy is a spec-merge attempt, a security pass, an integrator run and a fresh
// developer's startup, per task.
//
// Measured on r85 at the section level: collapsing several section authors into
// one per task took a run from 3.6 minutes per task to 2.3, on a bigger board.
// This is the same argument one level up.

func TestOneTaskIsOffByDefault(t *testing.T) {
	t.Setenv("AGENTS_PM_ONE_TASK", "")
	if onePlanTask() {
		t.Error("collapsing to one task is on with no setting; every existing board would change shape")
	}
}

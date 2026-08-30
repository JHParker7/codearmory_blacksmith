package main

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/code-armory-app/blacksmith/internal/agents"
)

// testModel builds a model whose session never touches a gateway: these tests
// exercise the SCREEN's state machine, not the pipeline behind it.
func testModel() tuiModel {
	sess := &session{
		stages: agents.PlanStages(),
		base:   "/tmp/x",
		events: make(chan uiEvent, 16),
		ready:  true,
	}
	return tuiModel{sess: sess, ctx: context.Background()}
}

func typeIn(m tuiModel, s string) tuiModel {
	for _, r := range s {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(tuiModel)
	}
	return m
}

// ONE REQUEST RUNS AT A TIME — the model host serves one at a time, and the
// UI's job is to make the queue visible rather than pretend otherwise.
func TestASecondRequestQueuesWhileTheFirstRuns(t *testing.T) {
	m := testModel()

	m = typeIn(m, "build a thing")
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(tuiModel)
	if m.cur == nil || cmd == nil {
		t.Fatal("the first request did not start a run")
	}
	if m.cur.task != "build a thing" {
		t.Fatalf("running task = %q", m.cur.task)
	}

	m = typeIn(m, "build another")
	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(tuiModel)
	if cmd != nil {
		t.Fatal("a second run started while the first held the GPU")
	}
	if len(m.queue) != 1 || m.queue[0] != "build another" {
		t.Fatalf("the second request did not queue: %v", m.queue)
	}
}

// Events land on the stage rows; completion moves the run to history and pulls
// the next request off the queue.
func TestEventsDriveTheStagesAndCompletionStartsTheNextRun(t *testing.T) {
	m := testModel()
	m = typeIn(m, "first")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(tuiModel)
	m = typeIn(m, "second")
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(tuiModel)

	next, _ = m.Update(uiEvent{kind: "stage-start", stage: agents.StagePlanArchitect})
	m = next.(tuiModel)
	if m.cur.stages[0].status != "running" {
		t.Fatalf("stage-start did not mark the row: %+v", m.cur.stages[0])
	}
	next, _ = m.Update(uiEvent{kind: "stage-pass", stage: agents.StagePlanArchitect})
	m = next.(tuiModel)
	if m.cur.stages[0].status != "passed" {
		t.Fatalf("stage-pass did not mark the row: %+v", m.cur.stages[0])
	}

	next, cmd := m.Update(runDoneMsg{took: 3 * time.Second})
	m = next.(tuiModel)
	if len(m.hist) != 1 || !m.hist[0].passed {
		t.Fatalf("the finished run did not reach history: %+v", m.hist)
	}
	if m.cur == nil || m.cur.task != "second" || cmd == nil {
		t.Fatal("completion did not start the queued request")
	}
}

// A fresh draw wipes the previous draw's stage results: the screen must not
// show draw one's greens against draw two's work.
func TestAFreshDrawResetsTheStageRows(t *testing.T) {
	m := testModel()
	m = typeIn(m, "task")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(tuiModel)

	next, _ = m.Update(uiEvent{kind: "stage-pass", stage: agents.StagePlanArchitect})
	m = next.(tuiModel)
	next, _ = m.Update(uiEvent{kind: "draw", n: 2})
	m = next.(tuiModel)

	if m.cur.draw != 2 {
		t.Fatalf("draw = %d", m.cur.draw)
	}
	if m.cur.stages[0].status != "pending" {
		t.Fatalf("draw two still shows draw one's result: %+v", m.cur.stages[0])
	}
}

// A failed run lands in history with its reason, not silently.
func TestAFailedRunCarriesItsReasonIntoHistory(t *testing.T) {
	m := testModel()
	m = typeIn(m, "task")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(tuiModel)

	next, _ = m.Update(runDoneMsg{err: contextError("after 3 attempts: stage plan-dev: boom"), took: time.Minute})
	m = next.(tuiModel)
	if len(m.hist) != 1 || m.hist[0].passed {
		t.Fatalf("the failure did not reach history: %+v", m.hist)
	}
	if !strings.Contains(m.hist[0].reason, "after 3 attempts") {
		t.Fatalf("the reason was lost: %q", m.hist[0].reason)
	}
}

// The tool tail is bounded; the screen is a window, the log file is the record.
func TestTheActivityTailIsBounded(t *testing.T) {
	m := testModel()
	m = typeIn(m, "task")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(tuiModel)

	for i := 0; i < 40; i++ {
		next, _ = m.Update(uiEvent{kind: "tool", line: "line"})
		m = next.(tuiModel)
	}
	if len(m.cur.tail) > 12 {
		t.Fatalf("the tail grew to %d lines", len(m.cur.tail))
	}
}

// Empty input submits nothing — a stray enter must not enqueue a blank run.
func TestABlankRequestIsNotSubmitted(t *testing.T) {
	m := testModel()
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(tuiModel)
	if m.cur != nil || len(m.queue) != 0 || cmd != nil {
		t.Fatal("a blank request was accepted")
	}
}

type contextError string

func (e contextError) Error() string { return string(e) }

// A request typed while the sandbox is still booting queues, and starts the
// moment readiness lands — the boot happens ON the screen now, not before it.
func TestARequestWaitsForTheSandboxAndStartsOnReady(t *testing.T) {
	m := testModel()
	m.sess.ready = false

	m = typeIn(m, "early bird")
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(tuiModel)
	if cmd != nil || m.cur != nil {
		t.Fatal("a run started before the sandbox existed")
	}
	if len(m.queue) != 1 {
		t.Fatalf("the request did not queue: %v", m.queue)
	}

	next, cmd = m.Update(uiEvent{kind: "sandbox-ready"})
	m = next.(tuiModel)
	if !m.sess.ready || m.cur == nil || cmd == nil {
		t.Fatal("readiness did not start the queued request")
	}
}

// A failed boot is shown, and nothing ever starts against it.
func TestAFailedBootHoldsTheQueueAndSaysWhy(t *testing.T) {
	m := testModel()
	m.sess.ready = false
	m = typeIn(m, "doomed")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(tuiModel)

	next, cmd := m.Update(uiEvent{kind: "sandbox-fail", line: "no route to host"})
	m = next.(tuiModel)
	if m.cur != nil {
		t.Fatal("a run started against a failed sandbox")
	}
	if m.sess.bootErr != "no route to host" {
		t.Fatalf("the failure is not held for the screen: %q", m.sess.bootErr)
	}
	_ = cmd
}

package main

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// The boot starts with the FIRST request, exactly once — not with the screen.
// An idle TUI must hold no lease: forge reaps them at 300 seconds, so a box
// booted for an operator who paused to think dies unused, and the first run
// re-boots one anyway. Eager acquisition bought a held lease and nothing else.
func TestTheSandboxBootsOnFirstSubmissionExactlyOnce(t *testing.T) {
	m := testModel()
	m.sess.ready = false
	boots := 0
	m.sess.boot = func() { boots++ }

	if m.sess.bootStarted {
		t.Fatal("the boot started with the screen")
	}
	m = typeIn(m, "first")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(tuiModel)
	m = typeIn(m, "second")
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(tuiModel)

	if !m.sess.bootStarted {
		t.Fatal("the first submission did not start the boot")
	}
	// ensureBoot runs the boot in a goroutine; give it a beat.
	deadline := time.Now().Add(time.Second)
	for boots == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if boots != 1 {
		t.Fatalf("the boot ran %d times, want exactly once", boots)
	}
}

package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func runeKey(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

// Switching to a section, running its fetch, and rendering the result.
func TestSectionNav_ReposAndAgents(t *testing.T) {
	m := tuiModel{api: fakePlatform(t)}

	// "3" -> repos section + a fetch command.
	nm, cmd := m.handleKey(runeKey("3"))
	m = nm.(tuiModel)
	if m.section != secRepos {
		t.Fatalf("section = %v, want secRepos", m.section)
	}
	if cmd == nil {
		t.Fatal("expected a fetch command for repos")
	}
	m2, _ := m.Update(cmd())
	m = m2.(tuiModel)
	if len(m.repos) != 1 || m.repos[0].Name != "task-tracker" {
		t.Fatalf("repos not loaded: %+v", m.repos)
	}
	if !strings.Contains(m.View(), "task-tracker") {
		t.Fatalf("repos view missing repo:\n%s", m.View())
	}

	// enter -> that repo's PRs.
	nm, cmd = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm.(tuiModel)
	if m.pullsFor == "" || cmd == nil {
		t.Fatal("enter should open the repo's PRs")
	}
	m2, _ = m.Update(cmd())
	m = m2.(tuiModel)
	if len(m.pulls) != 1 || m.pulls[0].Number != 1 {
		t.Fatalf("pulls not loaded: %+v", m.pulls)
	}
	if !strings.Contains(m.View(), "feat: backend") {
		t.Fatalf("pulls view missing PR:\n%s", m.View())
	}

	// esc backs out of the PR list to the repo list.
	nm, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	m = nm.(tuiModel)
	if m.pullsFor != "" {
		t.Fatal("esc should return to the repo list")
	}

	// "4" -> agents, then enter opens the role prompt.
	nm, cmd = m.handleKey(runeKey("4"))
	m = nm.(tuiModel)
	m2, _ = m.Update(cmd())
	m = m2.(tuiModel)
	if len(m.roles) != 1 || m.roles[0].Name != "developer" {
		t.Fatalf("roles not loaded: %+v", m.roles)
	}
	nm, cmd = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm.(tuiModel)
	m2, _ = m.Update(cmd())
	m = m2.(tuiModel)
	if m.roleDetail == nil || m.roleDetail.Prompt == "" {
		t.Fatal("enter should open the role's prompt")
	}
	if !strings.Contains(m.View(), "Go developer") {
		t.Fatalf("agent detail missing prompt:\n%s", m.View())
	}

	// "1" returns to the board.
	nm, _ = m.handleKey(runeKey("1"))
	m = nm.(tuiModel)
	if m.section != secBoard {
		t.Fatal("1 should return to the board")
	}
}

// An unconfigured platform shows a message, not a crash or empty silence.
func TestSectionNav_NoPlatform(t *testing.T) {
	m := tuiModel{api: newPlatformAPI("", nil)}
	nm, cmd := m.handleKey(runeKey("2"))
	m = nm.(tuiModel)
	if m.section != secRuns || cmd != nil {
		t.Fatal("unconfigured platform should switch section without a fetch")
	}
	if !strings.Contains(m.View(), "no platform configured") {
		t.Fatalf("expected the no-platform message:\n%s", m.View())
	}
}

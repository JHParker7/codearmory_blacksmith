package main

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// The settings screen: the host's own knobs, edited where they are used.
//
// It shows a deliberately SHORT list. Everything the config loader understands
// is not the same as everything worth retuning while the department runs, and
// the difference is what keeps credentials and endpoints off this screen — see
// editableSettings.

// settingsView renders the form.
func (m tuiModel) settingsView() string {
	var b strings.Builder
	b.WriteString(m.header() + "\n")
	b.WriteString(cRule.Render(strings.Repeat("─", m.rule())) + "\n")
	b.WriteString(cHead.Render("SETTINGS") + cDim.Render("  — this host; projects have their own") + "\n\n")

	for i, s := range editableSettings {
		cursor := "  "
		if i == m.setField {
			cursor = cLive.Render("▸ ")
		}
		v := m.setValues[s.Key]
		shown := v
		if strings.TrimSpace(v) == "" {
			// An unset value is not blank, it is the built-in default — and saying
			// which is the difference between "nothing here" and "this is what will
			// happen".
			shown = cDim.Render("default" + defaultHint(s.Key))
		}
		fmt.Fprintf(&b, "%s%-20s %s\n", cursor, s.Name+":", shown)
		if i == m.setField {
			fmt.Fprintf(&b, "    %s\n", cDim.Render(s.Help))
			if s.Kind == settingChoice {
				fmt.Fprintf(&b, "    %s\n", cDim.Render("one of: "+strings.Join(s.Choices, ", ")))
			}
		}
	}

	if m.note != "" {
		b.WriteString("\n" + cErr.Render("  "+m.note) + "\n")
	}
	b.WriteString("\n" + cDim.Render("  clearing a field restores the built-in default") + "\n\n")
	b.WriteString(m.keys("↑↓ field", "type to edit", "ctrl+s save", "esc cancel"))
	return b.String()
}

// defaultHint names the built-in for the settings where it is not obvious, so
// "default" is an answer rather than a shrug.
func defaultHint(key string) string {
	switch key {
	case "AGENTS_DEV_MAX_ITERATIONS":
		return fmt.Sprintf(" (%d)", defaultMaxIterations)
	case "AGENTS_DEV_CONCURRENCY":
		return fmt.Sprintf(" (%d)", defaultDevConcurrency)
	case "AGENTS_TEST_CONCURRENCY":
		return fmt.Sprintf(" (%d)", defaultTestConcurrency)
	case "AGENTS_POLL_SECONDS":
		return fmt.Sprintf(" (%d)", int(defaultPollInterval.Seconds()))
	case "AGENTS_REPO_INTEGRATION_BRANCH":
		return " (" + defaultIntegrationBranch + ")"
	}
	return ""
}

// onSettingsKey drives the settings form.
func (m tuiModel) onSettingsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	cur := editableSettings[m.setField].Key
	switch msg.Type {
	case tea.KeyEsc:
		m.mode, m.note = modeList, ""
		return m, nil
	case tea.KeyUp:
		if m.setField > 0 {
			m.setField--
		}
		return m, nil
	case tea.KeyDown, tea.KeyTab:
		if m.setField < len(editableSettings)-1 {
			m.setField++
		}
		return m, nil
	case tea.KeyCtrlS:
		if err := WriteSettings(m.setValues); err != nil {
			m.note = err.Error()
			return m, nil
		}
		m.mode, m.note = modeList, "settings saved — restart blacksmith to apply"
		return m, nil
	case tea.KeyBackspace:
		if v := m.setValues[cur]; v != "" {
			m.setValues[cur] = v[:len(v)-1]
		}
		return m, nil
	case tea.KeyRunes:
		m.setValues[cur] += string(msg.Runes)
		return m, nil
	}
	return m, nil
}

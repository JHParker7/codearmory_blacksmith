package main

import (
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func projectModel(t *testing.T, ps ...Project) tuiModel {
	t.Helper()
	t.Setenv("AGENTS_PROJECTS_FILE", filepath.Join(t.TempDir(), projectsFileName))
	return tuiModel{projects: ps, mode: modeProjects, acts: map[string]activity{}}
}

func key(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

// PAUSING KEEPS THE BOARD. It is the common case for a project you are not
// working on this week, and it is what makes deleting rare enough to be safe.
func TestPausingAProjectPersistsWithoutRemovingIt(t *testing.T) {
	m := projectModel(t, Project{Name: "api", BoardID: "b1"}, Project{Name: "web", BoardID: "b2"})
	next, _ := m.onProjectsKey(tea.KeyMsg{Type: tea.KeySpace})
	got := next.(tuiModel)

	if len(got.projects) != 2 {
		t.Fatalf("pausing removed a project: %+v", got.projects)
	}
	if got.projects[0].Active() {
		t.Error("the project was not paused")
	}
	// It must survive a reload, or the pause is only in the window.
	reloaded, err := LoadProjects(baseConfig())
	if err != nil {
		t.Fatal(err)
	}
	if reloaded[0].Active() {
		t.Error("the pause was not written to disk")
	}
	if !strings.Contains(got.note, "restart") {
		t.Errorf("the note does not say a restart is needed to apply it: %q", got.note)
	}
}

// DELETING A PROJECT MUST NOT DELETE ITS BOARD. Stopping work on something and
// destroying its tickets are different decisions, and conflating them turns an
// undo-able config change into data loss.
func TestDeletingAProjectLeavesItsBoardAlone(t *testing.T) {
	m := projectModel(t, Project{Name: "api", BoardID: "b1"}, Project{Name: "web", BoardID: "b2"})
	next, _ := m.onProjectsKey(key("d"))
	got := next.(tuiModel)

	if len(got.projects) != 1 || got.projects[0].Name != "web" {
		t.Fatalf("delete removed the wrong project: %+v", got.projects)
	}
	if !strings.Contains(got.note, "untouched") {
		t.Errorf("the note does not say the board survives: %q", got.note)
	}
}

// A refused save must leave the running list intact rather than half-applied.
func TestAnInvalidEditIsRefusedAndChangesNothing(t *testing.T) {
	m := projectModel(t, Project{Name: "api", BoardID: "b1"})
	m.mode, m.draft, m.draftField = modeProjectForm, draftFrom(m.projects[0], 0), fieldName
	m.draft.values[fieldName] = "   " // whitespace is not a name

	next, _ := m.saveDraft()
	got := next.(tuiModel)
	if got.note == "" {
		t.Error("an unnamed project was saved without complaint")
	}
	if got.projects[0].Name != "api" {
		t.Errorf("the refused edit was applied anyway: %+v", got.projects)
	}
}

// An edit must not drop settings the form has no field for — a rename would
// otherwise silently wipe a pipeline id or a runner class.
func TestEditingPreservesFieldsTheFormDoesNotShow(t *testing.T) {
	full := Project{
		Name: "api", BoardID: "b1",
		Repo: RepoConfig{
			URL: "git://host/api.git", Branch: "main",
			PipelineID: "pipe-1", RunnerClass: "special", TimeoutSecs: 1234,
		},
	}
	d := draftFrom(full, 0)
	d.values[fieldName] = "renamed"
	got := d.toProject(full)

	if got.Name != "renamed" {
		t.Errorf("the rename did not apply: %+v", got)
	}
	for name, ok := range map[string]bool{
		"pipeline":     got.Repo.PipelineID == "pipe-1",
		"runner class": got.Repo.RunnerClass == "special",
		"timeout":      got.Repo.TimeoutSecs == 1234,
	} {
		if !ok {
			t.Errorf("editing dropped the project's %s: %+v", name, got.Repo)
		}
	}
}

// The form must reach every field and wrap, or a field becomes unreachable.
func TestFormCyclesThroughEveryField(t *testing.T) {
	m := projectModel(t, Project{Name: "api", BoardID: "b1"})
	m.mode, m.draftField = modeProjectForm, fieldName
	seen := map[projectField]bool{m.draftField: true}
	for range int(projectFieldCount) {
		next, _ := m.onProjectFormKey(tea.KeyMsg{Type: tea.KeyTab})
		m = next.(tuiModel)
		seen[m.draftField] = true
	}
	for f := projectField(0); f < projectFieldCount; f++ {
		if !seen[f] {
			t.Errorf("field %q is unreachable by tabbing", f.label())
		}
	}
	if m.draftField != fieldName {
		t.Errorf("tabbing did not wrap back to the first field, landed on %q", m.draftField.label())
	}
}

// Every field must explain itself. A form that only names its fields assumes you
// already know the model, which is the assumption that makes hand-editing the
// file error-prone in the first place.
func TestEveryFormFieldExplainsItself(t *testing.T) {
	for f := projectField(0); f < projectFieldCount; f++ {
		if f.label() == "" {
			t.Errorf("field %d has no label", f)
		}
		if f.help() == "" {
			t.Errorf("field %q has no explanation", f.label())
		}
	}
}

package main

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// THE WINDOW IS HOW A PERSON RUNS THE DEPARTMENT, so managing projects belongs
// here rather than in a file you edit beside it.
//
// Editing projects.json by hand works and will keep working — it is the format,
// and a host provisioned by configuration management will never open a TUI. But
// the file is not the interface: adding a project by hand means knowing the
// schema, finding a board id, and restarting the service, and every one of those
// is a place to get it wrong silently. Here the fields are named, the board is
// created for you, and a bad edit is refused before it is written.

// projectField indexes the editable fields of a project, in the order shown.
type projectField int

const (
	fieldName projectField = iota
	fieldRepoURL
	fieldBranch
	fieldBoardID
	projectFieldCount
)

func (f projectField) label() string {
	switch f {
	case fieldName:
		return "Name"
	case fieldRepoURL:
		return "Repository"
	case fieldBranch:
		return "Base branch"
	case fieldBoardID:
		return "Board id"
	}
	return ""
}

// help is what the field is FOR, shown beside it. A form that only names its
// fields assumes you already know the model, which is the assumption that makes
// hand-editing the file error-prone in the first place.
func (f projectField) help() string {
	switch f {
	case fieldName:
		return "how you refer to it here; nothing keys on it"
	case fieldRepoURL:
		return "clone URL the agents work against"
	case fieldBranch:
		return "branch cut from and integrated into; blank inherits the host default"
	case fieldBoardID:
		return "leave blank to create a new board named after the project"
	}
	return ""
}

// projectDraft is a project being edited. Kept as strings because that is what a
// form holds: an in-progress board id is not yet a board id, and validating on
// every keystroke would fight the person typing.
type projectDraft struct {
	editing int // index into the list, or -1 for a new project
	values  [projectFieldCount]string
	enabled bool
}

func draftFrom(p Project, idx int) projectDraft {
	d := projectDraft{editing: idx, enabled: p.Active()}
	d.values[fieldName] = p.Name
	d.values[fieldRepoURL] = p.Repo.URL
	d.values[fieldBranch] = p.Repo.Branch
	d.values[fieldBoardID] = p.BoardID
	return d
}

// toProject turns a draft back into a project, preserving everything the form
// does not show.
//
// The preservation matters: a project may carry settings this form has no field
// for — a pipeline id, a runner class, a lease timeout — and an edit that
// silently dropped them would be a data-losing save disguised as a rename.
func (d projectDraft) toProject(existing Project) Project {
	p := existing
	p.Name = strings.TrimSpace(d.values[fieldName])
	p.BoardID = strings.TrimSpace(d.values[fieldBoardID])
	p.Repo.URL = strings.TrimSpace(d.values[fieldRepoURL])
	p.Repo.Branch = strings.TrimSpace(d.values[fieldBranch])
	on := d.enabled
	p.Enabled = &on
	return p
}

// projectsView lists the configured projects and what each one works.
func (m tuiModel) projectsView() string {
	var b strings.Builder
	b.WriteString(m.header() + "\n")
	b.WriteString(cRule.Render(strings.Repeat("─", m.rule())) + "\n")
	b.WriteString(cHead.Render("PROJECTS") + cDim.Render("  — a board and a repository; the department runs each independently") + "\n\n")

	if m.note != "" {
		b.WriteString(cErr.Render("  "+m.note) + "\n\n")
	}
	if len(m.projects) == 0 {
		b.WriteString(cDim.Render("  none configured\n"))
	}
	for i, p := range m.projects {
		marker, name := "  ", p.Name
		if i == m.projSelected {
			marker = cLive.Render("▸ ")
			name = cHead.Render(p.Name)
		}
		state := cDone.Render("running")
		if !p.Active() {
			state = cDim.Render("paused")
		}
		repo := p.Repo.URL
		if repo == "" {
			repo = cDim.Render("(host default)")
		}
		fmt.Fprintf(&b, "%s%-18s %-9s %s\n", marker, name, state, cDim.Render(repo))
		fmt.Fprintf(&b, "    %s\n", cDim.Render("board "+shortID(p.BoardID)+" · branch "+orDefault(p.Repo.Branch, "(host default)")))
	}

	b.WriteString("\n")
	b.WriteString(m.keys("↑↓ move", "a add", "e edit", "space pause", "d delete", "esc back"))
	return b.String()
}

// projectFormView is the add/edit form.
func (m tuiModel) projectFormView() string {
	var b strings.Builder
	b.WriteString(m.header() + "\n")
	b.WriteString(cRule.Render(strings.Repeat("─", m.rule())) + "\n")
	title := "NEW PROJECT"
	if m.draft.editing >= 0 {
		title = "EDIT PROJECT"
	}
	b.WriteString(cHead.Render(title) + "\n\n")

	for f := projectField(0); f < projectFieldCount; f++ {
		cursor := "  "
		if f == m.draftField {
			cursor = cLive.Render("▸ ")
		}
		value := m.draft.values[f]
		if value == "" {
			value = cDim.Render("—")
		}
		fmt.Fprintf(&b, "%s%-13s %s\n", cursor, f.label()+":", value)
		fmt.Fprintf(&b, "    %s\n", cDim.Render(f.help()))
	}
	state := "running"
	if !m.draft.enabled {
		state = "paused"
	}
	fmt.Fprintf(&b, "\n  %-13s %s\n", "State:", state)

	if m.note != "" {
		b.WriteString("\n" + cErr.Render("  "+m.note) + "\n")
	}
	b.WriteString("\n")
	b.WriteString(m.keys("tab next field", "ctrl+e pause/resume", "ctrl+s save", "esc cancel"))
	return b.String()
}

// onProjectsKey drives the list.
func (m tuiModel) onProjectsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.mode, m.note = modeList, ""
		return m, nil
	case "j", "down":
		if m.projSelected < len(m.projects)-1 {
			m.projSelected++
		}
	case "k", "up":
		if m.projSelected > 0 {
			m.projSelected--
		}
	case "a":
		m.mode, m.note = modeProjectForm, ""
		m.draft, m.draftField = projectDraft{editing: -1, enabled: true}, fieldName
	case "e":
		if len(m.projects) > 0 {
			m.mode, m.note = modeProjectForm, ""
			m.draft, m.draftField = draftFrom(m.projects[m.projSelected], m.projSelected), fieldName
		}
	case " ":
		// PAUSE RATHER THAN DELETE is the common case for a project you are not
		// working on this week, and it keeps the board and its history.
		if len(m.projects) > 0 {
			ps := append([]Project(nil), m.projects...)
			on := !ps[m.projSelected].Active()
			ps[m.projSelected].Enabled = &on
			return m.persist(ps, fmt.Sprintf("%s %s — restart blacksmith to apply",
				ps[m.projSelected].Name, map[bool]string{true: "resumed", false: "paused"}[on]))
		}
	case "d":
		if len(m.projects) > 0 {
			ps := append([]Project(nil), m.projects...)
			gone := ps[m.projSelected].Name
			ps = append(ps[:m.projSelected], ps[m.projSelected+1:]...)
			if m.projSelected >= len(ps) && m.projSelected > 0 {
				m.projSelected--
			}
			// The BOARD IS NOT DELETED with the project. Removing a project stops
			// the department working it; destroying the tickets is a separate and
			// much less reversible decision, and conflating them would make an
			// undo-able config change into a data loss.
			return m.persist(ps, "removed "+gone+" — its board and tickets are untouched")
		}
	}
	return m, nil
}

// onProjectFormKey drives the form.
func (m tuiModel) onProjectFormKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.mode, m.note = modeProjects, ""
		return m, nil
	case tea.KeyTab, tea.KeyDown:
		m.draftField = (m.draftField + 1) % projectFieldCount
		return m, nil
	case tea.KeyShiftTab, tea.KeyUp:
		m.draftField = (m.draftField + projectFieldCount - 1) % projectFieldCount
		return m, nil
	case tea.KeyCtrlE:
		m.draft.enabled = !m.draft.enabled
		return m, nil
	case tea.KeyCtrlS:
		return m.saveDraft()
	case tea.KeyBackspace:
		if v := m.draft.values[m.draftField]; v != "" {
			m.draft.values[m.draftField] = v[:len(v)-1]
		}
		return m, nil
	case tea.KeySpace:
		m.draft.values[m.draftField] += " "
		return m, nil
	case tea.KeyRunes:
		m.draft.values[m.draftField] += string(msg.Runes)
		return m, nil
	}
	return m, nil
}

// saveDraft validates the form and writes it.
func (m tuiModel) saveDraft() (tea.Model, tea.Cmd) {
	if strings.TrimSpace(m.draft.values[fieldName]) == "" {
		m.note = "a project needs a name"
		return m, nil
	}
	var existing Project
	if m.draft.editing >= 0 && m.draft.editing < len(m.projects) {
		existing = m.projects[m.draft.editing]
	}
	p := m.draft.toProject(existing)

	// A BLANK BOARD MEANS "MAKE ME ONE". Requiring a person to create a board
	// elsewhere and paste its id back is the step that makes adding a project
	// feel like configuration rather than use.
	if p.BoardID == "" {
		created, err := m.api.CreateBoard(context.Background(), p.Name)
		if err != nil {
			m.note = "could not create a board: " + err.Error()
			return m, nil
		}
		p.BoardID = created
	}

	ps := append([]Project(nil), m.projects...)
	if m.draft.editing >= 0 && m.draft.editing < len(ps) {
		ps[m.draft.editing] = p
	} else {
		ps = append(ps, p)
	}
	return m.persist(ps, "saved "+p.Name+" — restart blacksmith to apply")
}

// persist writes the list and keeps the model in step with it.
//
// The RUNNING SERVICE IS NOT RELOADED, and the note says so. Dispatchers are
// built at startup from this file; making them reconfigurable at runtime means
// draining in-flight work per project, which is a real piece of work and not one
// to smuggle in behind a form. Saying "restart to apply" is honest; silently
// doing nothing would not be.
func (m tuiModel) persist(ps []Project, note string) (tea.Model, tea.Cmd) {
	if err := SaveProjects(ps); err != nil {
		m.note = err.Error()
		return m, nil
	}
	m.projects, m.note, m.mode = ps, note, modeProjects
	if m.project >= len(ps) {
		m.project = 0
	}
	if m.projSelected >= len(ps) {
		m.projSelected = max(0, len(ps)-1)
	}
	return m, nil
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

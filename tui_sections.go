package main

// Portal-parity sections for the bare-command TUI. The board (tickets) is the
// home section; keys 1-4 switch to Runs, Repos and Agents, each a list you
// navigate with j/k and open with enter, backed by the platform read-client
// (tui_platform.go). This is READ parity — the terminal browses the same data
// the web portal shows; writes stay with the existing request line and board.

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// section is which portal-parity view is on screen. The zero value is the board,
// so a model that never touches sections behaves exactly as before.
type section int

const (
	secBoard  section = iota // tickets — the home view (existing board)
	secRuns                  // workflow runs + pipelines
	secRepos                 // git-factory repos, and a repo's PRs
	secAgents                // blacksmith roles
)

func (s section) label() string {
	switch s {
	case secRuns:
		return "runs"
	case secRepos:
		return "repos"
	case secAgents:
		return "agents"
	default:
		return "board"
	}
}

// ── messages the fetches deliver ─────────────────────────────────────────────

type runsMsg struct {
	runs  []Run
	pipes map[string]string
	err   string
}
type reposMsg struct {
	repos []Repo
	err   string
}
type rolesMsg struct {
	roles []Role
	err   string
}
type pullsMsg struct {
	repoID string
	pulls  []Pull
	err    string
}
type roleMsg struct {
	role Role
	err  string
}

// ── fetch commands (run off the UI goroutine) ────────────────────────────────

func (m tuiModel) fetchRuns() tea.Cmd {
	api := m.api
	return func() tea.Msg {
		ctx := context.Background()
		pipes, err := api.Pipelines(ctx)
		if err != nil {
			return runsMsg{err: err.Error()}
		}
		names := map[string]string{}
		for _, p := range pipes {
			names[p.ID] = p.Name
		}
		runs, err := api.Runs(ctx)
		if err != nil {
			return runsMsg{err: err.Error()}
		}
		return runsMsg{runs: runs, pipes: names}
	}
}

func (m tuiModel) fetchRepos() tea.Cmd {
	api := m.api
	return func() tea.Msg {
		repos, err := api.Repos(context.Background())
		if err != nil {
			return reposMsg{err: err.Error()}
		}
		return reposMsg{repos: repos}
	}
}

func (m tuiModel) fetchRoles() tea.Cmd {
	api := m.api
	return func() tea.Msg {
		roles, err := api.Roles(context.Background())
		if err != nil {
			return rolesMsg{err: err.Error()}
		}
		return rolesMsg{roles: roles}
	}
}

func (m tuiModel) fetchPulls(repoID string) tea.Cmd {
	api := m.api
	return func() tea.Msg {
		pulls, err := api.Pulls(context.Background(), repoID)
		if err != nil {
			return pullsMsg{repoID: repoID, err: err.Error()}
		}
		return pullsMsg{repoID: repoID, pulls: pulls}
	}
}

func (m tuiModel) fetchRole(name string) tea.Cmd {
	api := m.api
	return func() tea.Msg {
		role, err := api.Role(context.Background(), name)
		if err != nil {
			return roleMsg{err: err.Error()}
		}
		return roleMsg{role: role}
	}
}

// enterSection switches to sec and kicks off its fetch (unless the platform is
// not configured, in which case the view shows that plainly).
func (m tuiModel) enterSection(sec section) (tuiModel, tea.Cmd) {
	m.section = sec
	m.secSel = 0
	m.secErr = ""
	m.roleDetail = nil
	m.pullsFor = ""
	if sec == secBoard {
		return m, nil
	}
	if !m.api.configured() {
		m.secErr = "no platform configured (set CODEARMORY_URL / CODEARMORY_TOKEN)"
		return m, nil
	}
	m.secLoading = true
	switch sec {
	case secRuns:
		return m, m.fetchRuns()
	case secRepos:
		return m, m.fetchRepos()
	case secAgents:
		return m, m.fetchRoles()
	}
	return m, nil
}

// sectionRows is how many items the active section's list holds — for clamping
// the selection on j/k/G.
func (m tuiModel) sectionRows() int {
	switch m.section {
	case secRuns:
		return len(m.runs)
	case secRepos:
		if m.pullsFor != "" {
			return len(m.pulls)
		}
		return len(m.repos)
	case secAgents:
		return len(m.roles)
	}
	return 0
}

// sectionKey handles navigation inside a non-board section. Section-switch keys
// (1-4) are handled by the caller before this.
func (m tuiModel) sectionKey(msg tea.KeyMsg) (tuiModel, tea.Cmd) {
	n := m.sectionRows()
	switch msg.String() {
	case "q":
		return m, tea.Quit
	case "r":
		if m.section == secRepos && m.pullsFor != "" {
			return m, m.fetchPulls(m.pullsFor)
		}
		return m.enterSection(m.section)
	case "j", "down":
		if m.secSel < n-1 {
			m.secSel++
		}
	case "k", "up":
		if m.secSel > 0 {
			m.secSel--
		}
	case "g", "home":
		m.secSel = 0
	case "G", "end":
		m.secSel = max(0, n-1)
	case "enter", "l", "right":
		return m.openSectionItem()
	case "esc", "h", "left":
		// back out of a detail to the section's list, else to the board
		if m.roleDetail != nil || m.pullsFor != "" {
			m.roleDetail, m.pullsFor, m.secSel = nil, "", 0
			return m, nil
		}
		return m.enterSection(secBoard)
	}
	return m, nil
}

// openSectionItem drills into the selected row: a repo opens its PRs, an agent
// opens its full prompt/guard. Runs have no deeper view yet.
func (m tuiModel) openSectionItem() (tuiModel, tea.Cmd) {
	switch m.section {
	case secRepos:
		if m.pullsFor == "" && m.secSel >= 0 && m.secSel < len(m.repos) {
			r := m.repos[m.secSel]
			m.pullsFor = r.ID
			m.secSel = 0
			m.secLoading = true
			return m, m.fetchPulls(r.ID)
		}
	case secAgents:
		if m.secSel >= 0 && m.secSel < len(m.roles) {
			return m, m.fetchRole(m.roles[m.secSel].Name)
		}
	}
	return m, nil
}

// sectionView renders the active non-board section: a tab bar, then the list or
// the drilled-in detail.
func (m tuiModel) sectionView() string {
	var b strings.Builder
	b.WriteString(sectionTabs(m.section))
	b.WriteString("\n\n")
	if m.secErr != "" {
		b.WriteString(failStyle.Render(m.secErr) + "\n\n")
		b.WriteString(dimStyle.Render("1 board · r retry · q quit"))
		return b.String()
	}
	if m.secLoading && m.sectionRows() == 0 {
		b.WriteString(dimStyle.Render("loading…"))
		return b.String()
	}
	switch m.section {
	case secRuns:
		b.WriteString(m.runsView())
	case secRepos:
		b.WriteString(m.reposView())
	case secAgents:
		b.WriteString(m.agentsView())
	}
	b.WriteString("\n" + dimStyle.Render("↑↓ move · enter open · esc back · r refresh · 1-4 switch · q quit"))
	return b.String()
}

func sectionTabs(active section) string {
	parts := make([]string, 0, 4)
	for _, s := range []section{secBoard, secRuns, secRepos, secAgents} {
		label := fmt.Sprintf("%d %s", int(s)+1, s.label())
		if s == active {
			parts = append(parts, titleStyle.Render("["+label+"]"))
		} else {
			parts = append(parts, dimStyle.Render(" "+label+" "))
		}
	}
	return strings.Join(parts, " ")
}

func (m tuiModel) runsView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(fmt.Sprintf("runs (%d)", len(m.runs))) + "\n")
	if len(m.runs) == 0 {
		b.WriteString(dimStyle.Render("  no runs\n"))
		return b.String()
	}
	for i, r := range m.runs {
		name := m.pipes[r.WorkflowID]
		if name == "" {
			name = short(r.WorkflowID)
		}
		row := fmt.Sprintf("%s %-16s %-11s step %d  %s",
			runStatusGlyph(r.Status), truncate(name, 16), r.Status, r.CurrentStep, dimStyle.Render(short(r.ID)))
		b.WriteString(cursor(i == m.secSel) + row + "\n")
	}
	return b.String()
}

func (m tuiModel) reposView() string {
	var b strings.Builder
	if m.pullsFor != "" {
		b.WriteString(titleStyle.Render(fmt.Sprintf("pull requests (%d)", len(m.pulls))) + dimStyle.Render("   esc back") + "\n")
		if len(m.pulls) == 0 {
			b.WriteString(dimStyle.Render("  no open PRs\n"))
			return b.String()
		}
		for i, p := range m.pulls {
			row := fmt.Sprintf("#%-3d %-9s %s %s", p.Number, p.State,
				truncate(p.Title, 40), dimStyle.Render(p.SourceRef+"→"+p.TargetRef))
			b.WriteString(cursor(i == m.secSel) + row + "\n")
		}
		return b.String()
	}
	b.WriteString(titleStyle.Render(fmt.Sprintf("repos (%d)", len(m.repos))) + "\n")
	if len(m.repos) == 0 {
		b.WriteString(dimStyle.Render("  no repos\n"))
		return b.String()
	}
	for i, r := range m.repos {
		row := fmt.Sprintf("%-28s %-8s %s", truncate(r.Namespace+"/"+r.Name, 28), r.Visibility,
			dimStyle.Render(r.DefaultBranch))
		b.WriteString(cursor(i == m.secSel) + row + "\n")
	}
	return b.String()
}

func (m tuiModel) agentsView() string {
	var b strings.Builder
	if m.roleDetail != nil {
		r := m.roleDetail
		b.WriteString(titleStyle.Render(r.Name) + dimStyle.Render("   esc back") + "\n")
		b.WriteString(dimStyle.Render(fmt.Sprintf("class %s · check %s · max %d", r.Class, r.Check, r.MaxIterations)) + "\n\n")
		for _, line := range wrapLines(r.Prompt, max(40, m.width-2)) {
			b.WriteString(line + "\n")
		}
		return b.String()
	}
	b.WriteString(titleStyle.Render(fmt.Sprintf("agents (%d)", len(m.roles))) + "\n")
	if len(m.roles) == 0 {
		b.WriteString(dimStyle.Render("  no roles\n"))
		return b.String()
	}
	for i, r := range m.roles {
		row := fmt.Sprintf("%-20s %-8s %s", truncate(r.Name, 20), r.Class, dimStyle.Render(truncate(r.Check, 30)))
		b.WriteString(cursor(i == m.secSel) + row + "\n")
	}
	return b.String()
}

// ── small render helpers ─────────────────────────────────────────────────────

func cursor(sel bool) string {
	if sel {
		return runStyle.Render("▸ ")
	}
	return "  "
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func runStatusGlyph(status string) string {
	switch status {
	case "completed", "succeeded":
		return passStyle.Render("✓")
	case "failed", "error", "cancelled":
		return failStyle.Render("✗")
	case "running", "pending":
		return runStyle.Render("▶")
	default:
		return dimStyle.Render("·")
	}
}

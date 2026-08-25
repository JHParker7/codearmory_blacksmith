package window

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

// Refresh is how often the board reloads.
//
// TWO SECONDS, because this is watched while work is moving and a stage change
// the reader does not see for ten seconds reads as a stall. It costs one listing
// per tick against a store that is already serving the department.
const Refresh = 2 * time.Second

// Tickets is the board this window reads. READ-ONLY BY CONSTRUCTION: the window
// cannot claim, move or comment on anything, so it can be left open safely
// beside a running department.
type Tickets interface {
	List(ctx context.Context, opts ticket.ListOpts) ([]ticket.Ticket, error)
}

// Open runs the window until the reader quits or the context ends.
func Open(ctx context.Context, tickets Tickets, tb workflow.Table, boardID string) error {
	m := Model{tickets: tickets, table: tb, board: boardID, showFinished: false}
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	_, err := p.Run()
	return err
}

// Model is the window's state.
type Model struct {
	tickets Tickets
	table   workflow.Table
	board   string

	rows         Board
	err          error
	showFinished bool
	lastLoad     time.Time
}

type loadedMsg struct {
	tickets []ticket.Ticket
	err     error
}

type tickMsg time.Time

func tick() tea.Cmd {
	return tea.Tick(Refresh, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m Model) load() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		ts, err := m.tickets.List(ctx, ticket.ListOpts{BoardID: m.board})
		return loadedMsg{tickets: ts, err: err}
	}
}

func (m Model) Init() tea.Cmd { return tea.Batch(m.load(), tick()) }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		case "f":
			m.showFinished = !m.showFinished
			return m, nil
		case "r":
			return m, m.load()
		}
		return m, nil

	case tickMsg:
		return m, tea.Batch(m.load(), tick())

	case loadedMsg:
		// A FAILED RELOAD KEEPS THE LAST BOARD ON SCREEN. Blanking it would take
		// away the only information the reader has at the moment the store became
		// unreachable — which is exactly when they are looking.
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.lastLoad = time.Now()
		m.rows = Rows(msg.tickets, m.table, m.showFinished)
		return m, nil
	}
	return m, nil
}

var (
	headerStyle = lipgloss.NewStyle().Bold(true)
	dimStyle    = lipgloss.NewStyle().Faint(true)
	alertStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))
	liveStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	doneStyle   = lipgloss.NewStyle().Faint(true)
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
)

func (m Model) View() string {
	var b strings.Builder

	b.WriteString(headerStyle.Render("blacksmith") + dimStyle.Render("  ·  q quit   f finished   r refresh"))
	b.WriteString("\n")

	// THE ERROR SITS ABOVE THE BOARD RATHER THAN REPLACING IT. The rows below are
	// still the last thing that was true, and saying when they were read is what
	// stops them being mistaken for now.
	if m.err != nil {
		b.WriteString(errStyle.Render("could not read the board: "+clip(m.err.Error(), 100)) + "\n")
		if !m.lastLoad.IsZero() {
			b.WriteString(dimStyle.Render(fmt.Sprintf("showing what it looked like %s ago",
				RuntimeText(time.Since(m.lastLoad)))) + "\n")
		}
	}
	b.WriteString("\n")

	if len(m.rows.Rows) == 0 {
		if m.err == nil {
			b.WriteString(dimStyle.Render("nothing on this board.") + "\n")
		}
	}

	now := time.Now()
	for _, r := range m.rows.Rows {
		b.WriteString(m.line(r, now) + "\n")
	}

	// A BOARD THAT SILENTLY OMITS ROWS IS WORSE THAN A BUSY ONE.
	if m.rows.Hidden > 0 {
		b.WriteString("\n" + dimStyle.Render(fmt.Sprintf(
			"%s hidden — press f to show them", pluralRuns(m.rows.Hidden))) + "\n")
	}
	return b.String()
}

// line renders one row.
func (m Model) line(r Row, now time.Time) string {
	t := r.Ticket
	indent := strings.Repeat("  ", r.Depth)

	// THE STATE COMES FIRST, because it is what the reader is scanning for.
	state := Label(t.Status)
	style := lipgloss.NewStyle()
	switch {
	case NeedsAPerson(t.Status):
		style = alertStyle
	case Done(t.Status):
		style = doneStyle
	case HeldByAStage(m.table, t.Status):
		style = liveStyle
	}

	var parts []string
	parts = append(parts, indent+style.Render(clip(t.Title, 52)))
	parts = append(parts, style.Render(state))

	// A RUNTIME ONLY WHERE THERE IS ONE TO SHOW. A waiting ticket shows nothing
	// rather than a frozen figure, because a number that has stopped moving looks
	// exactly like a number nobody is updating.
	if d, _, show := Runtime(m.table, t, now); show {
		parts = append(parts, dimStyle.Render(RuntimeText(d)))
	}
	if r.Progress.Total > 0 {
		parts = append(parts, dimStyle.Render(fmt.Sprintf("%d/%d done",
			r.Progress.Done, r.Progress.Total)))
	}
	// WHAT IT IS WAITING FOR, because a queued ticket and an unclaimed one look
	// identical otherwise and need opposite responses.
	if unmet := UnmetDependencies(t); len(unmet) > 0 && !Stopped(t.Status) {
		parts = append(parts, dimStyle.Render(fmt.Sprintf("waiting on %d", len(unmet))))
	}
	if MergedAway(t) {
		into := MergedInto(t)
		if into == "" {
			into = "another ticket"
		}
		parts = append(parts, dimStyle.Render("merged into "+into))
	}
	return strings.Join(parts, "  ")
}

func pluralRuns(n int) string {
	if n == 1 {
		return "1 finished run"
	}
	return fmt.Sprintf("%d finished runs", n)
}

func clip(s string, max int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= max {
		return string(r)
	}
	return string(r[:max]) + "…"
}

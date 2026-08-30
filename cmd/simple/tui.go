package main

// The terminal UI: a human types a request, watches it run, and reads the
// result — submit and monitor, nothing else. It is a front end on EXACTLY the
// machinery the batch runs use (the same runWithReroll, the same stages, the
// same reroll arithmetic), because a UI that runs through different plumbing
// reports on a pipeline nobody is actually using.
//
// One request runs at a time — the model host serves one request at a time,
// so pretending otherwise in the UI would just hide the queue on the GPU.
// Further submissions queue visibly and run in order.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/code-armory-app/blacksmith/internal/agents"
	"github.com/code-armory-app/blacksmith/internal/config"
	"github.com/code-armory-app/blacksmith/internal/model"
)

// uiEvent is one thing the running pipeline tells the screen.
type uiEvent struct {
	kind  string // "draw", "stage-start", "stage-pass", "stage-fail", "tool"
	stage string
	n     int
	line  string
}

// runDoneMsg ends the current run on screen.
type runDoneMsg struct {
	err  error
	took time.Duration
}

type tickMsg time.Time

// stageView is one stage's row.
type stageView struct {
	name    string
	status  string // "pending", "running", "passed", "failed"
	started time.Time
	ended   time.Time
}

// runView is the run currently on the GPU.
type runView struct {
	task    string
	dir     string
	draw    int
	stages  []stageView
	tail    []string
	started time.Time
}

// histEntry is a finished run.
type histEntry struct {
	task   string
	dir    string
	took   time.Duration
	passed bool
	reason string
}

// session owns the wiring a TUI run needs: the creator, the stage list, the
// workspace root, and the channel the pipeline reports through.
type session struct {
	maker  agents.Creator
	stages []string
	base   string
	events chan uiEvent
	seq    int
}

// startRun launches one request in its own directory under the root.
func (s *session) startRun(ctx context.Context, task string) (string, tea.Cmd) {
	s.seq++
	dir := filepath.Join(s.base, fmt.Sprintf("%03d-%s", s.seq, slugOf(task)))
	cmd := func() tea.Msg {
		start := time.Now()
		var err error
		if err = os.MkdirAll(dir, 0o755); err == nil {
			err = runWithReroll(ctx, s.maker, s.stages, map[string]string{}, dir, task)
		}
		return runDoneMsg{err: err, took: time.Since(start)}
	}
	return dir, cmd
}

// slugOf names a run's directory after its request, filesystem-safely.
func slugOf(task string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(task) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_':
			b.WriteRune('-')
		}
		if b.Len() >= 24 {
			break
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "request"
	}
	return out
}

// tuiModel is the whole screen's state.
type tuiModel struct {
	sess    *session
	ctx     context.Context
	input   string
	queue   []string
	cur     *runView
	hist    []histEntry
	width   int
	height  int
	err     string
	started bool
}

func newRunView(task, dir string, stages []string) *runView {
	sv := make([]stageView, len(stages))
	for i, name := range stages {
		sv[i] = stageView{name: name, status: "pending"}
	}
	return &runView{task: task, dir: dir, draw: 1, stages: sv, started: time.Now()}
}

// waitForEvent re-arms the event pump: one channel read per Cmd, the standard
// bubbletea shape for a background feed.
func waitForEvent(ch chan uiEvent) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m tuiModel) Init() tea.Cmd {
	return tea.Batch(waitForEvent(m.sess.events), tick())
}

// submit takes the typed request into the queue, and onto the GPU if it is
// idle.
func (m tuiModel) submit() (tuiModel, tea.Cmd) {
	task := strings.TrimSpace(m.input)
	if task == "" {
		return m, nil
	}
	m.input = ""
	m.queue = append(m.queue, task)
	return m.maybeStart()
}

// maybeStart pulls the next queued request when nothing is running.
func (m tuiModel) maybeStart() (tuiModel, tea.Cmd) {
	if m.cur != nil || len(m.queue) == 0 {
		return m, nil
	}
	task := m.queue[0]
	m.queue = m.queue[1:]
	dir, cmd := m.sess.startRun(m.ctx, task)
	m.cur = newRunView(task, dir, m.sess.stages)
	return m, cmd
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC:
			return m, tea.Quit
		case tea.KeyEnter:
			return m.submit()
		case tea.KeyBackspace:
			if len(m.input) > 0 {
				m.input = m.input[:len(m.input)-len(string([]rune(m.input)[len([]rune(m.input))-1:]))]
			}
			return m, nil
		case tea.KeySpace:
			m.input += " "
			return m, nil
		case tea.KeyRunes:
			m.input += string(msg.Runes)
			return m, nil
		}
		return m, nil

	case tickMsg:
		return m, tick()

	case uiEvent:
		m = m.apply(msg)
		return m, waitForEvent(m.sess.events)

	case runDoneMsg:
		if m.cur != nil {
			e := histEntry{
				task: m.cur.task, dir: m.cur.dir,
				took: msg.took, passed: msg.err == nil,
			}
			if msg.err != nil {
				e.reason = msg.err.Error()
				if i := strings.IndexByte(e.reason, '\n'); i >= 0 {
					e.reason = e.reason[:i]
				}
			}
			m.hist = append(m.hist, e)
			m.cur = nil
		}
		return m.maybeStart()
	}
	return m, nil
}

// apply folds one pipeline event into the run view.
func (m tuiModel) apply(ev uiEvent) tuiModel {
	if m.cur == nil {
		return m
	}
	switch ev.kind {
	case "draw":
		m.cur.draw = ev.n
		if ev.n > 1 {
			// A fresh draw starts the stages over; the screen must not show the
			// previous draw's greens against the new draw's work.
			for i := range m.cur.stages {
				m.cur.stages[i] = stageView{name: m.cur.stages[i].name, status: "pending"}
			}
			m.cur.tail = append(m.cur.tail, fmt.Sprintf("— draw %d of %d: fresh start —", ev.n, MaxRunAttempts))
		}
	case "stage-start":
		for i := range m.cur.stages {
			if m.cur.stages[i].name == ev.stage {
				m.cur.stages[i].status = "running"
				if m.cur.stages[i].started.IsZero() {
					m.cur.stages[i].started = time.Now()
				}
			}
		}
	case "stage-pass", "stage-fail":
		for i := range m.cur.stages {
			if m.cur.stages[i].name == ev.stage {
				m.cur.stages[i].status = map[string]string{
					"stage-pass": "passed", "stage-fail": "failed",
				}[ev.kind]
				m.cur.stages[i].ended = time.Now()
			}
		}
	case "tool":
		m.cur.tail = append(m.cur.tail, ev.line)
		if len(m.cur.tail) > 12 {
			m.cur.tail = m.cur.tail[len(m.cur.tail)-12:]
		}
	}
	return m
}

var (
	titleStyle  = lipgloss.NewStyle().Bold(true)
	dimStyle    = lipgloss.NewStyle().Faint(true)
	passStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	failStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	runStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	promptStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
)

func glyph(status string) string {
	switch status {
	case "running":
		return runStyle.Render("◐")
	case "passed":
		return passStyle.Render("✓")
	case "failed":
		return failStyle.Render("✗")
	}
	return dimStyle.Render("○")
}

func elapsed(sv stageView) string {
	switch sv.status {
	case "pending":
		return ""
	case "running":
		return dimStyle.Render(time.Since(sv.started).Round(time.Second).String())
	}
	return dimStyle.Render(sv.ended.Sub(sv.started).Round(time.Second).String())
}

func (m tuiModel) View() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("blacksmith — the simple shape"))
	b.WriteString(dimStyle.Render("   workspace " + m.sess.base))
	b.WriteString("\n\n")

	if m.cur != nil {
		draw := ""
		if m.cur.draw > 1 {
			draw = runStyle.Render(fmt.Sprintf("  draw %d/%d", m.cur.draw, MaxRunAttempts))
		}
		b.WriteString(fmt.Sprintf("%s %s%s  %s\n",
			runStyle.Render("▶"), truncate(m.cur.task, 70), draw,
			dimStyle.Render(time.Since(m.cur.started).Round(time.Second).String())))
		for _, sv := range m.cur.stages {
			b.WriteString(fmt.Sprintf("   %s %-15s %s\n", glyph(sv.status), sv.name, elapsed(sv)))
		}
		for _, line := range m.cur.tail {
			b.WriteString("   " + dimStyle.Render(truncate(line, max(20, m.width-4))) + "\n")
		}
	} else {
		b.WriteString(dimStyle.Render("idle — type a request and press enter") + "\n")
	}

	if len(m.queue) > 0 {
		b.WriteString("\n" + titleStyle.Render(fmt.Sprintf("queued (%d)", len(m.queue))) + "\n")
		for _, q := range m.queue {
			b.WriteString("   · " + truncate(q, 70) + "\n")
		}
	}

	if len(m.hist) > 0 {
		b.WriteString("\n" + titleStyle.Render("finished") + "\n")
		for i := len(m.hist) - 1; i >= 0 && i >= len(m.hist)-5; i-- {
			e := m.hist[i]
			mark := passStyle.Render("✓")
			note := dimStyle.Render(e.dir)
			if !e.passed {
				mark = failStyle.Render("✗")
				note = failStyle.Render(truncate(e.reason, 60))
			}
			b.WriteString(fmt.Sprintf("   %s %s  %s\n      %s\n",
				mark, truncate(e.task, 60), dimStyle.Render(e.took.Round(time.Second).String()), note))
		}
	}

	b.WriteString("\n" + promptStyle.Render("request> ") + m.input + "▌\n")
	b.WriteString(dimStyle.Render("enter submits · ctrl+c quits") + "\n")
	if m.err != "" {
		b.WriteString(failStyle.Render(m.err) + "\n")
	}
	return b.String()
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// runTUI wires the pipeline to the screen and hands the terminal over.
func runTUI(base string) error {
	if base == "" {
		return fmt.Errorf("-repo is required: it is the workspace root the runs build under")
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return err
	}

	config.LoadOperatorEnv()
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	// THE LOG LEAVES THE TERMINAL. slog shares the screen with the UI and a
	// line of it through the alternate buffer corrupts the display; the file
	// keeps the full record the 12-line tail cannot.
	logf, err := os.OpenFile(filepath.Join(base, "tui.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()
	slog.SetDefault(slog.New(slog.NewTextHandler(logf, nil)))

	stages := agents.PlanStages()
	events := make(chan uiEvent, 256)

	gw := model.NewGateway(cfg.Host, cfg.Classes)
	maker := agents.Creator{
		Gateway: gw,
		Check:   cfg.Repo.TestCommand,
		Log: func(line string) {
			select {
			case events <- uiEvent{kind: "tool", line: line}:
			default: // a full screen buffer must never stall the pipeline
			}
		},
	}
	for _, name := range stages {
		a, err := stage(maker, name, nil)
		if err != nil {
			return err
		}
		if a.Class() != model.ClassNone && !gw.Serves(a.Class()) {
			return fmt.Errorf("this host does not serve class %q, which %s needs", a.Class(), a.Name())
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	box, release, err := acquireSandbox(ctx, cfg)
	if err != nil {
		return err
	}
	defer release()
	maker.Sandbox = box

	announce = func(kind, stage string, n int) {
		select {
		case events <- uiEvent{kind: kind, stage: stage, n: n}:
		default:
		}
	}
	defer func() { announce = func(string, string, int) {} }()

	sess := &session{maker: maker, stages: stages, base: base, events: events}
	m := tuiModel{sess: sess, ctx: ctx}

	_, err = tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}

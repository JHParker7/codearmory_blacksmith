package window

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/term"
	"golang.org/x/sys/unix"

	"github.com/code-armory-app/blacksmith/internal/agent/review"
	"github.com/code-armory-app/blacksmith/internal/record"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

// Refresh is how often the board reloads.
//
// TWO SECONDS, because this is watched while work is moving and a stage change
// the reader does not see for ten seconds reads as a stall. It costs one listing
// per tick against a store that is already serving the department.
const Refresh = 2 * time.Second

// MaxTickets bounds the per-row reads. A board larger than this is worth
// filtering rather than rendering.
const MaxTickets = 40

// Tickets is the board this window reads and files work on.
//
// LISTING IS NOT ENOUGH. A listing carries no comments, and every stage marker
// — the claim, the branch, the merge, the return ceiling — is a comment, so a
// view built from the listing alone shows every ticket as untriaged forever.
// Get is called per row for that reason.
type Tickets interface {
	List(ctx context.Context, opts ticket.ListOpts) ([]ticket.Ticket, error)
	Get(ctx context.Context, id string) (ticket.Ticket, error)
	Create(ctx context.Context, t ticket.Ticket) (ticket.Ticket, error)
}

// Options are what the window needs beyond the store.
type Options struct {
	Table         workflow.Table
	BoardID       string
	TranscriptDir string
	// RepoURL is used only to print a fetch command for a ticket that has become
	// a person's problem. Empty is fine; the branch is still named.
	RepoURL string
	// Where and Mode describe the store, for the header. A person running two
	// hosts needs to know which plane they are looking at before they read a row.
	Where, Mode string
}

// ErrNoTerminal means there is no terminal to draw on.
var ErrNoTerminal = errors.New("the window needs a terminal")

// Open runs the window until the reader quits or the context ends.
//
// IT REFUSES RATHER THAN EXITING QUIETLY when there is no terminal.
//
// A full-screen program whose input is not a terminal reaches EOF at once and
// quits CLEANLY — exit zero, having drawn a board into an alternate screen that
// is torn down on the way out, restoring whatever was there before. From the
// outside that is indistinguishable from a program that did nothing at all, and
// it is what a reader sees: three lines of startup log, no board, and a shell
// prompt back immediately.
//
// Reported exactly that way. Saying so costs one check and turns a silent
// nothing into a sentence naming the cause.
func Open(ctx context.Context, tickets Tickets, opts Options) error {
	if !term.IsTerminal(os.Stdin.Fd()) {
		return fmt.Errorf("%w: stdin is not one, so there is nothing to read keys "+
			"from and the board would close as soon as it opened. Run it from a "+
			"terminal, or use `blacksmith service` for the department itself",
			ErrNoTerminal)
	}
	if !term.IsTerminal(os.Stdout.Fd()) {
		return fmt.Errorf("%w: stdout is not one, so there is nowhere to draw it. "+
			"Redirecting the window's output cannot work — it is a screen, not a "+
			"stream", ErrNoTerminal)
	}

	// DISCARD WHATEVER IS ALREADY IN THE INPUT QUEUE.
	//
	// This breaks a loop that feeds itself. A window that ended badly can leave
	// bytes unread on the terminal — among them a ^C, which the line discipline
	// turns into SIGINT the moment the NEXT window starts. That cancels the
	// context before a single frame is drawn, the program quits cleanly, the
	// alternate screen is torn down restoring the screen beneath it, and the
	// reader sees three lines of startup log and their prompt back. Which leaves
	// the terminal in the same state, so it happens again.
	//
	// Reported as "it keeps happening", and the keeping is the tell: one bad exit
	// is a bug, a bug that reproduces itself on every subsequent launch is a
	// feedback loop. Nothing typed before the window opens was meant for it
	// anyway.
	_ = flushInput(os.Stdin)

	final, err := tea.NewProgram(newModel(tickets, opts),
		tea.WithAltScreen(), tea.WithContext(ctx)).Run()

	return closingError(final, err)
}

// ErrStartupInterrupted means the window was cancelled before its first frame.
var ErrStartupInterrupted = errors.New("interrupted during startup")

// closingError decides what the window's ending means.
//
// A CANCEL BEFORE THE FIRST FRAME IS NOT A QUIT, and must not be reported as
// one. The caller treats context.Canceled as "the reader pressed q or ^C", which
// is right once there is a board to leave — and wrong when the window never
// drew, where it exits ZERO having silently done nothing. That silence is the
// whole complaint: three lines of startup log and the prompt back, with no way
// to tell a broken window from one that was never given a chance.
//
// Separate from Open so the rule can be tested without a terminal.
func closingError(final tea.Model, err error) error {
	m, ok := final.(Model)
	if ok && m.loading && errors.Is(err, context.Canceled) {
		return fmt.Errorf("the window was interrupted before it drew anything — a "+
			"signal arrived during startup. If this repeats, the terminal is "+
			"delivering input left over from an earlier run; `reset` clears it: %w",
			ErrStartupInterrupted)
	}
	return err
}

// flushInput discards unread bytes on a terminal, and does nothing anywhere
// else.
//
// IT REPORTS WHAT IT DID even though Open ignores it. A function whose only
// effect is a syscall it swallows cannot be told from one that does nothing at
// all — not by a test, and not by anybody reading it later. The caller is still
// free to carry on: this is hygiene before the real work, and failing to tidy is
// not a reason to refuse to start.
func flushInput(f *os.File) error {
	if !term.IsTerminal(f.Fd()) {
		return nil // nothing queued anywhere but a terminal
	}
	return unix.IoctlSetInt(int(f.Fd()), unix.TCFLSH, unix.TCIFLUSH)
}

// newModel is the window's opening state.
//
// SEPARATE FROM Open SO IT CAN BE TESTED. The opening read counts as a read in
// flight and Init cannot say so — it has a value receiver and returns only
// commands — so it has to be set here, and a window built without it doubles up
// on its own first read.
func newModel(tickets Tickets, opts Options) Model {
	return Model{tickets: tickets, opts: opts, loading: true, inFlight: true}
}

// mode is which screen the window is showing.
//
// A MODE RATHER THAN A SET OF BOOLEANS. "detail && !composing" is a state you
// have to reason about; a mode is one you can read.
type mode int

const (
	modeList mode = iota
	modeDetail
	modeCompose
)

// Model is the window's state.
type Model struct {
	tickets Tickets
	opts    Options

	board Board
	// full is every ticket the last read returned. Kept beside the rows because
	// the board-wide alerts are asked of all of them, including the ones the
	// finished filter is hiding.
	full []ticket.Ticket
	acts map[string]Activity
	// thoughts is the reasoning per ticket, read with the board rather than at
	// render time. See load: View must be pure and cheap, because it runs on the
	// event loop and runs often.
	thoughts map[string][]Thought

	mode     mode
	selected int
	// scroll is the first visible row in the list, and detailScroll the first
	// visible line of a ticket. The reasoning panel made a ticket taller than a
	// terminal for the first time, so both have to be windowed rather than
	// assumed to fit.
	scroll       int
	detailScroll int

	// asking is the one-line request field on the board itself, for the sentence
	// you already have in your head. The compose screen is for a request you have
	// thought about and want two fields for.
	asking bool
	ask    string
	filing bool

	// The compose screen's two fields.
	field int
	title string
	body  string

	showFinished bool
	// inFlight is whether a read is already running. See Update's tick.
	inFlight bool
	err      error
	note     string
	loading  bool
	lastLoad time.Time
	width    int
	height   int
}

type loadedMsg struct {
	tickets []ticket.Ticket
	digest  Digest
	err     error
}

type tickMsg time.Time
type noteMsg string

func tick() tea.Cmd {
	return tea.Tick(Refresh, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m Model) load() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		listed, err := m.tickets.List(ctx, ticket.ListOpts{BoardID: m.opts.BoardID})
		if err != nil {
			return loadedMsg{err: err}
		}
		if len(listed) > MaxTickets {
			listed = listed[:MaxTickets]
		}
		full := make([]ticket.Ticket, 0, len(listed))
		for _, t := range listed {
			ft, getErr := m.tickets.Get(ctx, t.ID)
			if getErr != nil {
				// BETTER A STALE ROW THAN A MISSING ONE. The listing already said this
				// ticket exists; dropping it because its detail read failed would hide
				// work from the one view that exists to show it.
				full = append(full, t)
				continue
			}
			full = append(full, ft)
		}

		// THE TRANSCRIPTS ARE READ HERE, IN THE COMMAND'S OWN GOROUTINE, and never
		// on the event loop.
		//
		// This used to happen in Update and in View. Both run on bubbletea's single
		// thread, so every frame paid for a full parse of the corpus — measured at
		// 220ms for the activity and 212ms for the reasoning against 48MB, on a
		// board that refreshes every two seconds and re-renders on every keystroke.
		// The result is not a deadlock but a UI that stops responding, which is
		// indistinguishable from one that has hung and was reported as such.
		//
		// It grows with the corpus, so it gets worse rather than better: one busy
		// day wrote 45MB by itself.
		ids := make([]string, 0, len(full))
		for _, t := range full {
			ids = append(ids, t.ID)
		}
		return loadedMsg{tickets: full, digest: ReadDigest(m.opts.TranscriptDir, ids, time.Now())}
	}
}

// file creates a ticket from the ask line or the compose screen.
func (m Model) file(title, body string) tea.Cmd {
	board := m.opts.BoardID
	store := m.tickets
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		t := ticket.Ticket{
			BoardID:     &board,
			Title:       title,
			Description: body,
			Status:      workflow.ColInbox,
			Priority:    "medium",
		}
		created, err := store.Create(ctx, t)
		if err != nil {
			return noteMsg("could not file it: " + err.Error())
		}
		return noteMsg("filed " + ShortID(created.ID) + " — the product manager picks it up next")
	}
}

func (m Model) Init() tea.Cmd {
	// Init cannot mark the read in flight — it has a value receiver and returns
	// only commands — so the model is constructed with it already set. See Open.
	return tea.Batch(m.load(), tick())
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.KeyMsg:
		return m.onKey(msg)

	case tickMsg:
		// ONE READ AT A TIME. The tick fires every two seconds whatever the store
		// is doing, and a read is given twenty — so against a slow or unreachable
		// store the old behaviour stacked ten reads deep, each holding a listing
		// and a request per row, and kept doing it for as long as the window was
		// open. Skipping a tick costs two seconds of staleness; not skipping it
		// costs connections that are never coming back.
		if m.inFlight {
			return m, tick()
		}
		m.inFlight = true
		return m, tea.Batch(m.load(), tick())

	case noteMsg:
		m.note, m.filing = string(msg), false
		return m, m.load()

	case loadedMsg:
		m.loading, m.inFlight = false, false
		// A FAILED RELOAD KEEPS THE LAST BOARD ON SCREEN. Blanking it would take
		// away the only information the reader has at the moment the store became
		// unreachable — which is exactly when they are looking.
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		now := time.Now()
		m.err = nil
		m.lastLoad = now
		m.full = msg.tickets
		m.board = Rows(msg.tickets, m.opts.Table, m.showFinished)
		m.acts = RollUp(msg.tickets, msg.digest.Acts, now)
		m.thoughts = msg.digest.Thoughts
		m.clampSelection()
		return m, nil
	}
	return m, nil
}

// clampSelection keeps the cursor on a row that exists.
//
// THE BOARD RELOADS UNDER THE CURSOR every two seconds, and rows appear and
// vanish as work moves and as finished runs are hidden. Without this the
// selection indexes past the end and the detail view panics on the next frame.
func (m *Model) clampSelection() {
	if m.selected >= len(m.board.Rows) {
		m.selected = len(m.board.Rows) - 1
	}
	if m.selected < 0 {
		m.selected = 0
	}
	if len(m.board.Rows) == 0 && m.mode == modeDetail {
		m.mode = modeList
	}
}

func (m Model) onKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// The text fields take the keyboard first: while one is open every printable
	// key is content, not a command. Otherwise typing "quit" in the ask line
	// quits on the q.
	if m.asking {
		return m.onAskKey(msg)
	}
	if m.mode == modeCompose {
		return m.onComposeKey(msg)
	}
	if m.mode == modeDetail {
		return m.onDetailKey(msg)
	}

	switch msg.String() {
	// esc quits from the list because there is nothing here to back out of. In
	// every other mode it means cancel, and those are handled above.
	case "q", "esc", "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		if m.selected > 0 {
			m.selected--
		}
		m.followCursor()
	case "down", "j":
		if m.selected < len(m.board.Rows)-1 {
			m.selected++
		}
		m.followCursor()
	case "g":
		m.selected, m.scroll = 0, 0
	case "G":
		m.selected = len(m.board.Rows) - 1
		m.clampSelection()
		m.followCursor()
	case "enter":
		if len(m.board.Rows) > 0 {
			m.mode, m.detailScroll = modeDetail, 0
		}
	case "h":
		m.showFinished = !m.showFinished
		m.board = Rows(m.full, m.opts.Table, m.showFinished)
		m.clampSelection()
	case "r":
		if m.inFlight {
			return m, nil
		}
		m.inFlight = true
		return m, m.load()
	case "/":
		m.asking, m.ask, m.note = true, "", ""
	case "n":
		m.mode, m.field, m.title, m.body, m.note = modeCompose, 0, "", "", ""
	}
	return m, nil
}

// followCursor scrolls the list so the selected row stays on screen.
func (m *Model) followCursor() {
	rows := m.listRows()
	if m.selected < m.scroll {
		m.scroll = m.selected
	}
	if m.selected >= m.scroll+rows {
		m.scroll = m.selected - rows + 1
	}
	if m.scroll < 0 {
		m.scroll = 0
	}
}

func (m Model) onDetailKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "esc", "backspace", "left":
		m.mode = modeList
	case "up", "k":
		if m.detailScroll > 0 {
			m.detailScroll--
		}
	case "down", "j":
		m.detailScroll++
	case "pgup":
		m.detailScroll -= m.detailRows()
		if m.detailScroll < 0 {
			m.detailScroll = 0
		}
	case "pgdown", " ":
		m.detailScroll += m.detailRows()
	case "g":
		m.detailScroll = 0
	case "G":
		// Clamped when rendered, so a large number is the simplest way to say
		// "the end" without knowing how tall the ticket is from here.
		m.detailScroll = 1 << 20
	case "r":
		return m, m.load()
	}
	return m, nil
}

func (m Model) onAskKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.asking, m.ask = false, ""
		return m, nil
	case tea.KeyEnter:
		text := strings.TrimSpace(m.ask)
		if text == "" {
			m.asking = false
			return m, nil
		}
		m.asking, m.filing, m.ask = false, true, ""
		// THE WHOLE SENTENCE IS BOTH THE TITLE AND THE BODY. The product manager
		// triages it either way, and splitting one line into a title with an empty
		// description throws away the only context there was.
		return m, m.file(clip(text, 120), text)
	case tea.KeyBackspace:
		if r := []rune(m.ask); len(r) > 0 {
			m.ask = string(r[:len(r)-1])
		}
		return m, nil
	case tea.KeyCtrlC:
		return m, tea.Quit
	case tea.KeySpace:
		m.ask += " "
		return m, nil
	case tea.KeyRunes:
		m.ask += string(msg.Runes)
		return m, nil
	}
	return m, nil
}

func (m Model) onComposeKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.mode, m.note = modeList, ""
		return m, nil
	case tea.KeyTab:
		m.field = 1 - m.field
		return m, nil
	case tea.KeyCtrlS:
		if strings.TrimSpace(m.title) == "" {
			// NAMED, NOT SILENTLY REFUSED. A ctrl+s that appears to do nothing is
			// indistinguishable from a key that is not bound.
			m.note = "a title is needed — it is what appears on the board"
			return m, nil
		}
		title, body := strings.TrimSpace(m.title), strings.TrimSpace(m.body)
		m.mode, m.filing, m.title, m.body, m.note = modeList, true, "", "", ""
		return m, m.file(title, body)
	case tea.KeyCtrlC:
		return m, tea.Quit
	case tea.KeyEnter:
		if m.field == 1 {
			m.body += "\n"
		} else {
			m.field = 1 // enter on the title moves on, which is what a form does
		}
		return m, nil
	case tea.KeyBackspace:
		if m.field == 0 {
			if r := []rune(m.title); len(r) > 0 {
				m.title = string(r[:len(r)-1])
			}
		} else if r := []rune(m.body); len(r) > 0 {
			m.body = string(r[:len(r)-1])
		}
		return m, nil
	case tea.KeySpace:
		if m.field == 0 {
			m.title += " "
		} else {
			m.body += " "
		}
		return m, nil
	case tea.KeyRunes:
		if m.field == 0 {
			m.title += string(msg.Runes)
		} else {
			m.body += string(msg.Runes)
		}
		return m, nil
	}
	return m, nil
}

var (
	cHead   = lipgloss.NewStyle().Bold(true)
	cDim    = lipgloss.NewStyle().Faint(true)
	cRule   = lipgloss.NewStyle().Faint(true)
	cKey    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	cErr    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))
	cLive   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	cWait   = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	cDone   = lipgloss.NewStyle().Faint(true)
	cSel    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("4"))
	cNote   = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	cPrompt = lipgloss.NewStyle().Foreground(lipgloss.Color("5"))
)

func styleFor(t Tone) lipgloss.Style {
	switch t {
	case ToneLive:
		return cLive
	case ToneDone:
		return cDone
	case ToneAlert:
		return cErr
	case ToneQuiet:
		return cDim
	default:
		return cWait
	}
}

func (m Model) View() string {
	switch m.mode {
	case modeDetail:
		return m.detailView()
	case modeCompose:
		return m.composeView()
	default:
		return m.listView()
	}
}

func (m Model) header() string {
	mode := m.opts.Mode
	if mode == "" {
		mode = "board"
	}
	line := mode
	if m.opts.Where != "" {
		line += " · " + m.opts.Where
	}
	width := m.rule() - 13
	if width < 20 {
		width = 20
	}
	return cHead.Render("blacksmith") + cDim.Render("  "+clip(line, width))
}

func (m Model) rule() int  { return Rule(m.width) }
func (m Model) hr() string { return cRule.Render(strings.Repeat("─", m.rule())) }

// listRows is how many ticket rows fit, once the fixed furniture is taken out:
// the header, two rules, the summary, the headings, the ask line and the keys.
func (m Model) listRows() int {
	n := m.height - 9
	if n < 3 {
		n = 3
	}
	return n
}

func (m Model) progressLine() string {
	c := Tally(m.board.Rows, m.acts, time.Now())
	if c.Total() == 0 {
		return ""
	}
	parts := []string{cDone.Render(fmt.Sprintf("%d done", c.Done))}
	if c.Moving > 0 {
		parts = append(parts, cLive.Render(fmt.Sprintf("%d working", c.Moving)))
	}
	if c.Waiting > 0 {
		parts = append(parts, cWait.Render(fmt.Sprintf("%d waiting", c.Waiting)))
	}
	if c.Stuck > 0 {
		parts = append(parts, cErr.Render(fmt.Sprintf("%d need you", c.Stuck)))
	}
	return strings.Join(parts, cDim.Render(" · ")) +
		cDim.Render(fmt.Sprintf("  (%d total)", c.Total()))
}

func (m Model) ceilingAlert() string {
	hit := ReturnCeiling(m.full)
	if len(hit) == 0 {
		return ""
	}
	noun := "ticket"
	if len(hit) > 1 {
		noun = "tickets"
	}
	return cErr.Render(fmt.Sprintf("! %d %s hit the %d-return review ceiling and need a person: %s",
		len(hit), noun, review.MaxReturns, strings.Join(hit, " ")))
}

func (m Model) listView() string {
	var b strings.Builder
	b.WriteString(m.header() + "\n")
	b.WriteString(m.hr() + "\n")

	if p := m.progressLine(); p != "" {
		b.WriteString("  " + p + "\n")
	}
	if a := m.ceilingAlert(); a != "" {
		b.WriteString(a + "\n")
	}

	// THE COLUMN HEADINGS, on the same widths the rows use. Without them the
	// runtime column is a bare number beside a status and nothing says which is
	// which — and once the widths are shared, a heading that stops lining up is
	// the first sign a cell has started padding itself wrong again.
	if len(m.board.Rows) > 0 {
		// THE SAME WIDTHS THE ROWS USE, from the same call. A heading that computes
		// its own is a heading that stops lining up the moment a row concedes a
		// column, which is precisely when a person is trying to work out what the
		// remaining columns are.
		cols := m.cols()
		head := "    " + PadTo("id", ShortIDLen) + "  " + PadRunes("ticket", cols.Title) + "  "
		if cols.Priority > 0 {
			head += PadRunes(clip("priority", cols.Priority), cols.Priority) + " "
		}
		if cols.Runtime > 0 {
			head += PadLeft("took", cols.Runtime) + " "
		}
		b.WriteString(cDim.Render(head+"state") + "\n")
	}

	if m.err != nil {
		// THE ERROR SITS ABOVE THE BOARD RATHER THAN REPLACING IT. The rows below
		// are still the last thing that was true, and saying when they were read is
		// what stops them being mistaken for now.
		b.WriteString(cErr.Render("cannot read the ticket store: "+clip(m.err.Error(), 100)) + "\n")
		b.WriteString(cDim.Render("the department may still be working; this window just cannot see it") + "\n")
		if !m.lastLoad.IsZero() {
			b.WriteString(cDim.Render("showing what it looked like "+
				RuntimeText(time.Since(m.lastLoad))+" ago") + "\n")
		}
	}
	if len(m.board.Rows) == 0 && m.err == nil {
		b.WriteString(cDim.Render("\n  "+EmptyBoardReason(m.loading, m.board.Hidden)) + "\n")
	}

	rows := m.listRows()
	top := m.scroll
	if maxTop := len(m.board.Rows) - rows; top > maxTop {
		top = maxTop
	}
	if top < 0 {
		top = 0
	}
	end := min(top+rows, len(m.board.Rows))

	now := time.Now()
	for i := top; i < end; i++ {
		// ONE BLANK LINE BETWEEN RUNS. A request and its tasks read as one block
		// only if there is something between it and the next request; without it a
		// board carrying several runs is an undifferentiated list. Not before the
		// first visible row, where it would be a stray blank line at the top.
		if m.board.Rows[i].Depth == 0 && i > top {
			b.WriteString("\n")
		}
		b.WriteString(m.line(m.board.Rows[i], i == m.selected, now, m.cols()) + "\n")
	}
	if len(m.board.Rows) > rows {
		b.WriteString(cDim.Render(fmt.Sprintf("  %d-%d of %d rows",
			top+1, end, len(m.board.Rows))) + "\n")
	}

	b.WriteString(m.hr() + "\n")
	b.WriteString(m.askView() + "\n")
	if m.note != "" {
		b.WriteString(cNote.Render("  "+m.note) + "\n")
	}

	keys := []string{"↑↓ move", "enter detail", "/ ask", "n new ticket", "r refresh"}
	if m.showFinished {
		keys = append(keys, "h hide finished")
	} else if m.board.Hidden > 0 {
		keys = append(keys, fmt.Sprintf("h show %d finished", m.board.Hidden))
	}
	b.WriteString(m.keys(append(keys, "q quit")...))
	return b.String()
}

// cols is how every row and the heading spend the terminal's width.
//
// COMPUTED ONCE FOR THE WHOLE BOARD, because the title column is sized to the
// widest title on it — a per-row width would give each row a different column
// and the board would stop being a table.
func (m Model) cols() RowWidths {
	want := 0
	for _, r := range m.board.Rows {
		w := len([]rune(r.Ticket.Title)) + 2*r.Depth
		if w > want {
			want = w
		}
	}
	return LayOutRow(Rule(m.width), want)
}

// line renders one row.
func (m Model) line(r Row, selected bool, now time.Time, cols RowWidths) string {
	t := r.Ticket
	act := m.acts[t.ID]

	// A BAR AND AN ESTIMATE, not just a count. "4/19 done" says where a request
	// is; it does not say whether to wait for it.
	//
	// THE COUNT IS RESERVED FOR, NOT APPENDED. Appending it and clipping the
	// result meant the count was the first thing cut, and these titles are already
	// past the cap — so it never appeared on the rows that most needed it.
	// INDENTED UNDER WHAT IT CAME FROM, one level per generation. Rows already sit
	// directly beneath their parent; the indent is what makes the nesting visible
	// rather than merely true, so five tickets about task stores read as one
	// request broken down instead of five unrelated jobs.
	indent, width := "", cols.Title
	if r.Depth > 0 {
		indent = strings.Repeat("  ", r.Depth-1) + "└ "
		width = cols.Title - 2*r.Depth
		if width < 12 {
			width = 12
		}
	}

	// A BAR AND AN ESTIMATE, not just a count. "4/19 done" says where a request
	// is; it does not say whether to wait for it.
	//
	// THE CELL IS SIZED AGAINST THIS ROW'S REMAINING WIDTH, and gives up detail
	// rather than the title. Reserving for the full bar unconditionally left every
	// broken-down request on an eighty-column terminal rendering as "…".
	suffix := ProgressSuffix(r.Progress.Done, r.Progress.Total,
		now.Sub(t.CreatedAt), width)

	// clip ADDS its ellipsis to the length it was given, so a title clipped to n
	// comes back n+1 columns wide; without allowing for it the row overruns its
	// budget by one.
	room := width - len([]rune(suffix))
	if suffix != "" {
		room--
	}
	if room < 0 {
		room = 0
	}
	title := clip(t.Title, room) + suffix

	// A finished ticket shows its final time; one still running shows a "+" so the
	// two are not mistaken for each other at a glance.
	took := ""
	if d, final, show := Runtime(m.opts.Table, t, now); show {
		took = RuntimeText(d)
		if !final {
			took += "+"
		}
	}

	// FITTED, NOT APPENDED. Whatever runs past the terminal is cut by bubbletea
	// without a mark, and this is the cell that says whether a person is needed.
	var state strings.Builder
	for _, c := range FitCells(StateCells(t, act, now), cols.State) {
		state.WriteString(styleFor(c.Tone).Render(c.Text))
	}

	marker := "  "
	switch {
	case act.Live(now) && act.Role != "":
		marker = cLive.Render("▸ ")
	case act.Working > 0:
		marker = cDim.Render("· ")
	}

	// ONE COLUMN PER CELL, each padded to its own display width. Built by
	// concatenation rather than one format string because every cell may be
	// styled and fmt cannot pad around escapes — see PadTo.
	// EACH CELL CARRIES ITS OWN TRAILING SEPARATOR, so a dropped column takes its
	// separator with it — which is exactly how LayOutRow counted the width. A
	// column dropped without its space leaves the row one column wider than the
	// budget it was laid out to, and the state loses a character to it.
	left := PadTo(cDim.Render(indent)+title, width+lipgloss.Width(indent)) + "  "
	if cols.Priority > 0 {
		left += PadTo(clip(t.Priority, cols.Priority), cols.Priority) + " "
	}
	if cols.Runtime > 0 {
		left += PadLeft(took, cols.Runtime) + " "
	}

	if selected {
		return cSel.Render("▸ "+ShortID(t.ID)+"  "+left) + state.String()
	}
	return marker + cDim.Render(ShortID(t.ID)) + "  " + left + state.String()
}

func (m Model) askView() string {
	if m.filing {
		return cDim.Render("  filing it…")
	}
	if !m.asking {
		return cDim.Render("  press / to hand the department a job")
	}
	return cPrompt.Render("  ask  ") + m.ask + cPrompt.Render("█") +
		cDim.Render("   enter to file · esc to cancel")
}

// detailRows is how many lines of a ticket fit on screen, once the header, the
// two rules and the key line are taken out.
func (m Model) detailRows() int {
	n := m.height - 6
	if n < 5 {
		n = 5
	}
	return n
}

func (m Model) detailView() string {
	if len(m.board.Rows) == 0 {
		return m.listView()
	}
	t := m.board.Rows[m.selected].Ticket
	now := time.Now()

	var b strings.Builder
	b.WriteString(cHead.Render(t.Title) + "\n")
	meta := []string{ShortID(t.ID), t.Status}
	if t.Priority != "" {
		meta = append(meta, t.Priority)
	}
	meta = append(meta, Label(t.Status))
	b.WriteString(cDim.Render(strings.Join(meta, " · ")) + "\n")

	// BOTH CLOCKS, because they answer different questions: one is how long this
	// work has existed, the other how long it has been where it is now. A ticket
	// four hours old and two minutes into review is healthy; four hours old and
	// four hours into review is not.
	d, final, show := Runtime(m.opts.Table, t, now)
	switch {
	case !show && !Stopped(t.Status) && !t.CreatedAt.IsZero():
		// Said plainly rather than left as a blank where a duration was: the
		// question this answers is "why is nothing happening to this one".
		b.WriteString(cDim.Render(fmt.Sprintf(
			"waiting — queued %s ago, no stage has taken it yet",
			RuntimeText(now.Sub(t.CreatedAt)))) + "\n")
	case show && final:
		// "took" and "stopped after" are different claims and the difference is the
		// point: one is how long the work needed, the other how far it got before
		// it gave up.
		word := "took"
		if t.Status == workflow.ColBlocked {
			word = "stopped after"
		}
		b.WriteString(cDim.Render(word+" "+RuntimeText(d)) + "\n")
	case show:
		b.WriteString(cDim.Render(fmt.Sprintf("in progress %s · at this stage %s",
			RuntimeText(d), ShortDuration(now.Sub(StageSince(t))))) + "\n")
	}
	b.WriteString("\n")

	if desc := strings.TrimSpace(t.Description); desc != "" {
		for _, line := range WrapTo(desc, m.rule()-4) {
			b.WriteString("  " + line + "\n")
		}
		b.WriteString("\n")
	}

	// THE DEPENDENCY GRAPH IN FULL, not as a count. The useful question is not
	// "how many" but "which one, and has it landed" — the answer decides whether
	// you wait or go and look at the prerequisite.
	if len(t.DependsOn) > 0 {
		b.WriteString(cKey.Render("  waits on") + "\n")
		for _, dep := range t.DependsOn {
			mark, style := "•", cWait
			switch {
			case dep.Status == workflow.ColDone:
				mark, style = "✓", cDone
			case NeedsAPerson(dep.Status):
				// A prerequisite that is blocked will never complete on its own, and
				// everything behind it is stranded rather than queued.
				mark, style = "!", cErr
			}
			title := dep.Title
			if title == "" {
				title = "(untitled)"
			}
			b.WriteString("    " + style.Render(mark) + " " +
				cDim.Render(ShortID(dep.ID)) + "  " +
				style.Render(clip(title, 60)) + cDim.Render(" · "+dep.Status) + "\n")
		}
		b.WriteString("\n")
	}

	for _, c := range t.Comments {
		body := strings.TrimSpace(c.Body)
		if record.IsClaim(body) {
			// The claim is bookkeeping, not something a person reads — but WHO holds
			// it is, because a stale claim on a dead host is why nothing is moving.
			if cl, err := record.Parse(body); err == nil {
				b.WriteString(cDim.Render("  ── claimed by "+cl.Role+" on "+cl.Host) + "\n")
			}
			continue
		}
		b.WriteString(cKey.Render("  ┌ ") + "\n")
		for _, line := range WrapTo(body, m.rule()-4) {
			b.WriteString("  " + line + "\n")
		}
		b.WriteString("\n")
	}

	// WHAT THE MODEL IS ACTUALLY THINKING, below the record of what happened.
	//
	// The comments above say what the pipeline DID; this says why. Watching a
	// stuck ticket through counters gives "41 refusals" and leaves the cause to
	// guesswork — the reasoning beside it names the fault outright. Placed last
	// because it is the longest section and reads as a tail.
	if th := m.thoughts[t.ID]; len(th) > 0 {
		b.WriteString(cKey.Render("  reasoning") +
			cDim.Render(fmt.Sprintf("  (last %d turns)", len(th))) + "\n")
		for _, x := range th {
			head := x.At.Local().Format("15:04:05")
			if x.Role != "" {
				head += " " + x.Role
			}
			if x.Tool != "" {
				head += " → " + x.Tool
			}
			style := cDim
			if x.Failed {
				style = cErr
			}
			b.WriteString("    " + style.Render(head) + "\n")
			for _, line := range WrapTo(x.Prose, m.rule()-6) {
				b.WriteString("      " + line + "\n")
			}
		}
		b.WriteString("\n")
	}

	if why, branch, fetch := NextStep(t, m.opts.RepoURL); why != "" {
		b.WriteString("\n" + cErr.Render("  YOURS NOW") + cDim.Render(" — "+why) + "\n")
		b.WriteString(cDim.Render("  branch  ") + branch + "\n")
		if fetch != "" {
			b.WriteString(cDim.Render("  fetch   ") + fetch + "\n")
		}
	}

	// WINDOWED, because the reasoning panel made this view taller than a terminal
	// for the first time and everything past the fold was simply unreachable. The
	// header and the key line stay pinned so the position and the way out are
	// always visible.
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	rows := m.detailRows()
	top := m.detailScroll
	if maxTop := len(lines) - rows; top > maxTop {
		top = maxTop
	}
	if top < 0 {
		top = 0
	}
	end := min(top+rows, len(lines))

	var out strings.Builder
	out.WriteString(m.header() + "\n")
	out.WriteString(m.hr() + "\n")
	out.WriteString(strings.Join(lines[top:end], "\n") + "\n")
	out.WriteString(m.hr() + "\n")

	keys := []string{"esc back", "r refresh", "q quit"}
	if len(lines) > rows {
		keys = append([]string{"↑↓ scroll", "pgup/pgdn page", "g/G ends"}, keys...)
		out.WriteString(cDim.Render(fmt.Sprintf("  %d-%d of %d lines",
			top+1, end, len(lines))) + "\n")
	}
	out.WriteString(m.keys(keys...))
	return out.String()
}

func (m Model) composeView() string {
	var b strings.Builder
	b.WriteString(m.header() + "\n")
	b.WriteString(m.hr() + "\n")
	b.WriteString(cHead.Render("Hand the department some work") + "\n\n")

	tCur, bCur := " ", " "
	if m.field == 0 {
		tCur = cPrompt.Render("█")
	} else {
		bCur = cPrompt.Render("█")
	}
	b.WriteString(cPrompt.Render("  title  ") + m.title + tCur + "\n\n")
	b.WriteString(cPrompt.Render("  detail ") + "\n")
	for _, line := range strings.Split(m.body, "\n") {
		b.WriteString("    " + line + "\n")
	}
	b.WriteString("    " + bCur + "\n\n")
	if m.note != "" {
		b.WriteString(cErr.Render("  "+m.note) + "\n")
	}
	b.WriteString(cDim.Render("  The product manager triages it first, so a sentence of "+
		"context is worth more than a precise spec.") + "\n")
	b.WriteString(m.hr() + "\n")
	b.WriteString(m.keys("tab switch field", "enter newline in detail", "ctrl+s file it", "esc cancel"))
	return b.String()
}

// keys renders the hint line, dropping WHOLE hints that do not fit.
//
// A HALF A HINT IS WORSE THAN NONE. The line was joined at full length and left
// to the terminal, which cuts wherever it happens to run out — so a sixty-column
// window ended on "r " with the description gone, followed by the empty styling
// of every hint that had been cut away entirely. It read as a broken render
// rather than as a line that did not fit, and the hints that survived were the
// arbitrary ones rather than the useful ones.
func (m Model) keys(pairs ...string) string {
	const lead = "  "
	const sep = "  ·  "

	budget := Rule(m.width) - len(lead)
	out := make([]string, 0, len(pairs))
	used := 0
	for _, p := range pairs {
		word, rest, _ := strings.Cut(p, " ")
		cost := len([]rune(word)) + 1 + len([]rune(rest))
		if len(out) > 0 {
			cost += len(sep)
		}
		if used+cost > budget {
			break
		}
		used += cost
		out = append(out, cKey.Render(word)+cDim.Render(" "+rest))
	}
	return lead + strings.Join(out, cDim.Render(sep))
}

func pluralRuns(n int) string {
	if n == 1 {
		return "1 finished run"
	}
	return fmt.Sprintf("%d finished runs", n)
}

// clip bounds a string BY RUNE and trims it. Used for titles and single-line
// cells; see clipRunes for content whose leading whitespace matters.
func clip(s string, max int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= max {
		return string(r)
	}
	return string(r[:max]) + "…"
}

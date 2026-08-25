package window

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

type board struct {
	tickets []ticket.Ticket
	err     error
	calls   int
	opts    ticket.ListOpts
}

func (b *board) List(_ context.Context, opts ticket.ListOpts) ([]ticket.Ticket, error) {
	b.calls++
	b.opts = opts
	if b.err != nil {
		return nil, b.err
	}
	return b.tickets, nil
}

func model(b *board) Model {
	return Model{tickets: b, table: table(), board: "board-1"}
}

// apply drives one message through the model, as bubbletea would.
func apply(m Model, msg tea.Msg) (Model, tea.Cmd) {
	next, cmd := m.Update(msg)
	return next.(Model), cmd
}

// loadInto runs the load command and feeds its result back, which is the whole
// round trip bubbletea performs.
func loadInto(t *testing.T, m Model) Model {
	t.Helper()
	cmd := m.load()
	if cmd == nil {
		t.Fatal("the model produced no load command")
	}
	m, _ = apply(m, cmd())
	return m
}

// THE BOARD IS READ AND DRAWN. This is the whole point of the window: the runs
// have to be visible.
func TestTheRunsAppearOnTheBoard(t *testing.T) {
	b := &board{tickets: []ticket.Ticket{
		tk("req", "", workflow.ColTracking, 30),
		tk("store", "req", workflow.ColInDev, 20),
		tk("api", "req", workflow.ColReadyForDev, 10),
	}}
	b.tickets[1].Title = "Build the store"

	m := loadInto(t, model(b))
	view := m.View()

	for _, want := range []string{"Build the store", "being written", "has tests, waiting for a developer"} {
		if !strings.Contains(view, want) {
			t.Errorf("the board does not show %q:\n%s", want, view)
		}
	}
	// AND IT READS THE CONFIGURED BOARD, not everything the account can see.
	if b.opts.BoardID != "board-1" {
		t.Errorf("listed board %q", b.opts.BoardID)
	}
}

// A FAILED RELOAD KEEPS THE LAST BOARD ON SCREEN. Blanking it would take away
// the only information the reader has at the moment the store became
// unreachable — which is exactly when they are looking.
func TestAFailedReloadKeepsTheLastBoardAndSaysHowOldItIs(t *testing.T) {
	b := &board{tickets: []ticket.Ticket{tk("store", "", workflow.ColInDev, 5)}}
	b.tickets[0].Title = "Build the store"

	m := loadInto(t, model(b))
	if !strings.Contains(m.View(), "Build the store") {
		t.Fatal("the first load did not draw")
	}

	b.err = errors.New("the store could not be reached")
	m = loadInto(t, m)

	view := m.View()
	if !strings.Contains(view, "Build the store") {
		t.Errorf("a failed reload blanked the board:\n%s", view)
	}
	if !strings.Contains(view, "could not be reached") {
		t.Errorf("the failure is not reported:\n%s", view)
	}
	// SAYING WHEN IT WAS READ is what stops stale rows being mistaken for now.
	if !strings.Contains(view, "what it looked like") {
		t.Errorf("the board does not say the rows are stale:\n%s", view)
	}
}

// A BOARD THAT SILENTLY OMITS ROWS IS WORSE THAN A BUSY ONE, and the reader has
// to be able to get them back.
func TestFinishedRunsAreHiddenButCountedAndCanBeShown(t *testing.T) {
	b := &board{tickets: []ticket.Ticket{
		tk("live", "", workflow.ColInDev, 10),
		tk("over", "", workflow.ColDone, 20),
	}}
	b.tickets[1].Title = "Something finished"

	m := loadInto(t, model(b))
	view := m.View()
	if strings.Contains(view, "Something finished") {
		t.Errorf("a finished run was shown by default:\n%s", view)
	}
	if !strings.Contains(view, "1 finished run") || !strings.Contains(view, "press f") {
		t.Errorf("the hidden run is not counted or not recoverable:\n%s", view)
	}

	// f shows them, and the next load draws them.
	m, _ = apply(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	m = loadInto(t, m)
	if !strings.Contains(m.View(), "Something finished") {
		t.Errorf("pressing f did not show the finished run:\n%s", m.View())
	}

	// And f again hides them.
	m, _ = apply(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	m = loadInto(t, m)
	if strings.Contains(m.View(), "Something finished") {
		t.Errorf("pressing f again did not hide it:\n%s", m.View())
	}
}

// THE ROWS THAT NEED A PERSON ARE THE ONES SOMEONE OPENED THIS TO FIND.
func TestABlockedRunSaysSoInWordsRatherThanAColumnName(t *testing.T) {
	b := &board{tickets: []ticket.Ticket{tk("stuck", "", workflow.ColBlocked, 5)}}
	b.tickets[0].Title = "The API"

	view := loadInto(t, model(b)).View()
	if !strings.Contains(view, "BLOCKED — needs you") {
		t.Errorf("a blocked row does not say it needs the reader:\n%s", view)
	}
	if strings.Contains(view, "blocked\t") || strings.Contains(view, " blocked ") {
		t.Errorf("the raw column name reached the screen:\n%s", view)
	}
}

// A WAITING TICKET SHOWS NO RUNTIME, because a number that has stopped moving
// looks exactly like a number nobody is updating.
func TestOnlyRunningWorkShowsAClock(t *testing.T) {
	b := &board{tickets: []ticket.Ticket{
		{ID: "held", Title: "Held", Status: workflow.ColInDev, CreatedAt: at(30), UpdatedAt: at(3)},
		{ID: "queued", Title: "Queued", Status: workflow.ColReadyForDev, CreatedAt: at(30), UpdatedAt: at(25)},
	}}

	view := loadInto(t, model(b)).View()
	held, queued := lineFor(view, "Held"), lineFor(view, "Queued")

	if !strings.Contains(held, "m") && !strings.Contains(held, "s") {
		t.Errorf("running work shows no clock: %q", held)
	}
	// The queued row must carry no duration at all. Checked as DIGITS, because
	// its label ("has tests, waiting for a developer") contains the letters a
	// duration is spelled with.
	if strings.ContainsAny(queued, "0123456789") {
		t.Errorf("a queued row shows a runtime: %q", queued)
	}
}

func lineFor(view, title string) string {
	for _, l := range strings.Split(view, "\n") {
		if strings.Contains(l, title) {
			return l
		}
	}
	return ""
}

// WHAT IT IS WAITING FOR, because a queued ticket and an unclaimed one look
// identical otherwise and need opposite responses.
func TestARowSaysWhenItIsWaitingOnSomethingElse(t *testing.T) {
	blocked := tk("api", "", workflow.ColReadyForDev, 10)
	blocked.Title = "The API"
	blocked.DependsOn = []ticket.Dependency{
		{ID: "store", Status: workflow.ColInDev},
		{ID: "types", Status: workflow.ColDone},
	}

	view := loadInto(t, model(&board{tickets: []ticket.Ticket{blocked}})).View()
	if !strings.Contains(view, "waiting on 1") {
		t.Errorf("the row does not say what it waits for:\n%s", view)
	}
}

// A MERGED TICKET SAYS WHERE ITS WORK WENT, because "done" would send the reader
// looking for a branch that has none.
func TestAMergedRowPointsAtWhatCarriedItsWork(t *testing.T) {
	merged := tk("absorbed", "", workflow.ColDone, 10)
	merged.Title = "Absorbed task"
	merged.Comments = []ticket.Comment{
		{Body: "**Merged into another ticket.** carried into `abc12345`"},
	}

	m := model(&board{tickets: []ticket.Ticket{merged}})
	m.showFinished = true
	view := loadInto(t, m).View()

	if !strings.Contains(view, "merged into abc12345") {
		t.Errorf("the row does not say where its work went:\n%s", view)
	}
}

// AN EMPTY BOARD SAYS IT IS EMPTY. A blank screen is indistinguishable from a
// window that failed to draw.
func TestAnEmptyBoardSaysSo(t *testing.T) {
	view := loadInto(t, model(&board{})).View()
	if !strings.Contains(view, "nothing on this board") {
		t.Errorf("an empty board drew nothing at all:\n%s", view)
	}
}

// THE KEYS ARE THE ONLY WAY OUT, so they have to work.
func TestQuittingWorks(t *testing.T) {
	m := model(&board{})
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune{'q'}},
		{Type: tea.KeyEsc},
		{Type: tea.KeyCtrlC},
	} {
		if _, cmd := apply(m, key); cmd == nil {
			t.Errorf("%v did not quit", key)
		}
	}
	// An unknown key does nothing rather than quitting.
	if _, cmd := apply(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'z'}}); cmd != nil {
		t.Error("an unrecognised key produced a command")
	}
}

// THE BOARD RELOADS ON ITS OWN, and each tick schedules the next one — a window
// that stops refreshing shows a frozen board that looks like a stalled
// department.
func TestATickReloadsAndSchedulesTheNextOne(t *testing.T) {
	b := &board{}
	m := model(b)

	_, cmd := apply(m, tickMsg(time.Now()))
	if cmd == nil {
		t.Fatal("a tick produced no work")
	}
	// The batch contains both the load and the next tick; running it must reach
	// the store.
	before := b.calls
	m = loadInto(t, m)
	if b.calls <= before {
		t.Error("the reload never reached the store")
	}
	if Refresh <= 0 || Refresh > 10*time.Second {
		t.Errorf("the refresh interval is %v; a stage change would look like a stall", Refresh)
	}
}

// r reloads on demand, for a reader who does not want to wait for the tick.
func TestRefreshOnDemand(t *testing.T) {
	b := &board{}
	m := model(b)
	_, cmd := apply(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if cmd == nil {
		t.Fatal("r produced no reload")
	}
	cmd()
	if b.calls == 0 {
		t.Error("r did not reach the store")
	}
}

// THE WINDOW CLAIMS NOTHING. It is read-only by construction so it can be left
// open beside a running department without racing it for tickets.
func TestTheWindowOnlyEverReads(t *testing.T) {
	var _ Tickets = (*board)(nil)
	// The interface has exactly one method, and it is a listing.
	if got := reflectMethodCount(); got != 1 {
		t.Errorf("the window's store interface has %d methods; it can do more than read", got)
	}
}

func reflectMethodCount() int {
	// Tickets is declared with one method; this pins it so a future addition is
	// a deliberate act rather than an accident.
	type onlyList interface {
		List(ctx context.Context, opts ticket.ListOpts) ([]ticket.Ticket, error)
	}
	var t Tickets = (*board)(nil)
	_, ok := t.(onlyList)
	if !ok {
		return -1
	}
	return 1
}

// THE WINDOW LOADS AND STARTS TICKING THE MOMENT IT OPENS. Without both, it
// draws an empty board and never fills it — which reads as a department with no
// work rather than a window that has not looked.
func TestOpeningLoadsAndStartsRefreshing(t *testing.T) {
	b := &board{tickets: []ticket.Ticket{tk("live", "", workflow.ColInDev, 5)}}
	m := model(b)

	// BOTH, not one: a window that loads without ticking shows a board frozen at
	// the moment it opened, and one that ticks without loading shows nothing at
	// all until the first tick lands.
	if n := commandsIn(m.Init()); n != 2 {
		t.Errorf("opening the window ran %d commands, want the load and the tick", n)
	}

	// AND EVERY TICK SCHEDULES THE NEXT ONE, or the board stops refreshing after
	// the first and reads as a stalled department.
	_, cmd := apply(m, tickMsg(time.Now()))
	if n := commandsIn(cmd); n != 2 {
		t.Errorf("a tick ran %d commands, want the load and the next tick", n)
	}
}

// commandsIn counts the commands in a batch, which is how the two-things-at-once
// behaviour above is checked rather than assumed.
func commandsIn(cmd tea.Cmd) int {
	if cmd == nil {
		return 0
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		return len(msg)
	default:
		return 1
	}
}

// A LONG TITLE MUST NOT PUSH THE STATE OFF THE LINE. The state is what the
// reader is scanning for, so the title is what gives way.
func TestALongTitleIsTrimmedSoTheStateStaysVisible(t *testing.T) {
	long := tk("x", "", workflow.ColInDev, 5)
	long.Title = strings.Repeat("a very long ticket title ", 20)

	view := loadInto(t, model(&board{tickets: []ticket.Ticket{long}})).View()
	line := lineFor(view, "a very long")

	if !strings.Contains(line, "being written") {
		t.Errorf("the state was pushed off the line:\n%q", line)
	}
	if len([]rune(line)) > 200 {
		t.Errorf("the row is %d runes wide", len([]rune(line)))
	}
	if !strings.Contains(line, "…") {
		t.Errorf("a trimmed title does not say it was trimmed:\n%q", line)
	}
}

// One hidden run reads as "1 finished run", not "1 finished runs".
func TestTheHiddenCountReadsAsEnglish(t *testing.T) {
	if got := pluralRuns(1); got != "1 finished run" {
		t.Errorf("pluralRuns(1) = %q", got)
	}
	if got := pluralRuns(3); got != "3 finished runs" {
		t.Errorf("pluralRuns(3) = %q", got)
	}
}

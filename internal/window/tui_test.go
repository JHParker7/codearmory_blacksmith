package window

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/code-armory-app/blacksmith/internal/agent/review"
	"github.com/code-armory-app/blacksmith/internal/record"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/transcript"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

type board struct {
	tickets []ticket.Ticket
	err     error
	getErr  error
	calls   int
	gets    int
	opts    ticket.ListOpts

	created   []ticket.Ticket
	createErr error
}

func (b *board) List(_ context.Context, opts ticket.ListOpts) ([]ticket.Ticket, error) {
	b.calls++
	b.opts = opts
	if b.err != nil {
		return nil, b.err
	}
	return b.tickets, nil
}

// Get returns the ticket WITH its comments, as the real store does. The listing
// carries none, which is the whole reason the window reads each row.
func (b *board) Get(_ context.Context, id string) (ticket.Ticket, error) {
	b.gets++
	if b.getErr != nil {
		return ticket.Ticket{}, b.getErr
	}
	for _, t := range b.tickets {
		if t.ID == id {
			return t, nil
		}
	}
	return ticket.Ticket{}, errors.New("no such ticket")
}

func (b *board) Create(_ context.Context, t ticket.Ticket) (ticket.Ticket, error) {
	if b.createErr != nil {
		return ticket.Ticket{}, b.createErr
	}
	t.ID = fmt.Sprintf("new%04d", len(b.created))
	b.created = append(b.created, t)
	return t, nil
}

func model(b *board) Model {
	return Model{
		tickets: b,
		opts:    Options{Table: table(), BoardID: "board-1", TranscriptDir: "off"},
		width:   140,
		height:  40,
	}
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

func key(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "space":
		return tea.KeyMsg{Type: tea.KeySpace}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	case "ctrl+s":
		return tea.KeyMsg{Type: tea.KeyCtrlS}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// typeIn sends each character as its own key message, as a terminal does.
func typeIn(m Model, s string) Model {
	for _, r := range s {
		if r == ' ' {
			m, _ = apply(m, key("space"))
			continue
		}
		m, _ = apply(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return m
}

func lineFor(view, title string) string {
	for _, l := range strings.Split(view, "\n") {
		if strings.Contains(l, title) {
			return l
		}
	}
	return ""
}

// writeTurns lays down recorded turns for the reasoning panel to read.
func writeTurns(t *testing.T, dir string, recs ...transcript.Record) {
	t.Helper()
	writeTranscript(t, dir, "2026-08-25", recs...)
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

// A LISTING CARRIES NO COMMENTS, and every stage marker is a comment — so a view
// built from the listing alone shows every ticket as untriaged forever.
func TestEachRowIsReadInFull(t *testing.T) {
	b := &board{tickets: []ticket.Ticket{
		tk("a", "", workflow.ColInDev, 5),
		tk("b", "", workflow.ColInDev, 5),
	}}

	loadInto(t, model(b))
	if b.gets != 2 {
		t.Fatalf("read %d tickets in full, want 2 — the markers the rows depend on "+
			"live in comments the listing does not carry", b.gets)
	}
}

// BETTER A STALE ROW THAN A MISSING ONE. The listing already said the ticket
// exists; dropping it would hide work from the one view meant to show it.
func TestAFailedDetailReadKeepsTheRow(t *testing.T) {
	b := &board{tickets: []ticket.Ticket{tk("a", "", workflow.ColInDev, 5)}}
	b.tickets[0].Title = "Still here"
	b.getErr = errors.New("gateway timeout")

	view := loadInto(t, model(b)).View()
	if !strings.Contains(view, "Still here") {
		t.Fatalf("a failed per-row read dropped the row:\n%s", view)
	}
}

// A FAILED RELOAD KEEPS THE LAST BOARD ON SCREEN.
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

// A BOARD THAT SILENTLY OMITS ROWS IS WORSE THAN A BUSY ONE.
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
	if !strings.Contains(view, "h show 1 finished") {
		t.Errorf("the hidden run is not counted or not recoverable:\n%s", view)
	}

	m, _ = apply(m, key("h"))
	if !strings.Contains(m.View(), "Something finished") {
		t.Errorf("pressing h did not show the finished run:\n%s", m.View())
	}

	m, _ = apply(m, key("h"))
	if strings.Contains(m.View(), "Something finished") {
		t.Errorf("pressing h again did not hide it:\n%s", m.View())
	}
}

// h REDRAWS FROM WHAT IS ALREADY HELD rather than waiting for the next poll. A
// key that appears to do nothing for two seconds reads as a key that is not
// bound, and the reader presses it again.
func TestShowingFinishedRunsDoesNotWaitForAPoll(t *testing.T) {
	b := &board{tickets: []ticket.Ticket{tk("over", "", workflow.ColDone, 20)}}
	b.tickets[0].Title = "Something finished"

	m := loadInto(t, model(b))
	before := b.calls
	m, _ = apply(m, key("h"))

	if b.calls != before {
		t.Fatal("h went back to the store; the rows were already in hand")
	}
	if !strings.Contains(m.View(), "Something finished") {
		t.Fatalf("h did not redraw immediately:\n%s", m.View())
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

	// A HELD TICKET'S CLOCK IS CLIMBING, and the "+" is what says so.
	if !strings.Contains(held, "+") {
		t.Errorf("running work shows no climbing clock: %q", held)
	}
	if strings.Contains(queued, "+") {
		t.Errorf("a queued row shows a running clock: %q", queued)
	}
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
	if !strings.Contains(view, "waits on store") {
		t.Errorf("the row does not name what it waits for:\n%s", view)
	}
}

// A MERGED TICKET SAYS WHERE ITS WORK WENT.
func TestAMergedRowPointsAtWhatCarriedItsWork(t *testing.T) {
	merged := tk("absorbed", "", workflow.ColDone, 10)
	merged.Title = "Absorbed task"
	merged.Comments = []ticket.Comment{
		{Body: record.MergedIntoMarker + " carried into `abc12345`"},
	}

	m := model(&board{tickets: []ticket.Ticket{merged}})
	m.showFinished = true
	view := loadInto(t, m).View()

	if !strings.Contains(view, "merged into abc12345") {
		t.Errorf("the row does not say where its work went:\n%s", view)
	}
}

// AN EMPTY BOARD SAYS IT IS EMPTY AND HOW TO FILL IT.
func TestAnEmptyBoardSaysSo(t *testing.T) {
	view := loadInto(t, model(&board{})).View()
	if !strings.Contains(view, "no tickets on this board") {
		t.Errorf("an empty board drew nothing at all:\n%s", view)
	}
}

// THE SUMMARY IS THE GLANCE. "what has this thing done" should not need a scan
// of forty rows.
func TestTheSummaryCountsTheBoard(t *testing.T) {
	b := &board{tickets: []ticket.Ticket{
		tk("a", "", workflow.ColBlocked, 5),
		tk("b", "", workflow.ColReadyForDev, 5),
	}}
	view := loadInto(t, model(b)).View()

	if !strings.Contains(view, "1 waiting") || !strings.Contains(view, "1 need you") {
		t.Errorf("the summary does not count the board:\n%s", view)
	}
	if !strings.Contains(view, "(2 total)") {
		t.Errorf("the summary does not give a total:\n%s", view)
	}
}

// THE ONE THING THAT ASKS FOR A PERSON BY NAME. A ticket at the ceiling has had
// ten developer runs spent on it and no further agent round will settle it.
func TestTheReturnCeilingIsAlertedAtTheTopOfTheBoard(t *testing.T) {
	stuck := tk("aaaaaaaa1111", "", workflow.ColBlocked, 60)
	stuck.Comments = []ticket.Comment{{Body: review.ReturnCeilingMarker + " after 10 returns"}}

	view := loadInto(t, model(&board{tickets: []ticket.Ticket{stuck}})).View()
	if !strings.Contains(view, "review ceiling and need a person") {
		t.Errorf("the ceiling alert is missing:\n%s", view)
	}
	if !strings.Contains(view, "aaaaaaaa") {
		t.Errorf("the alert does not name the ticket:\n%s", view)
	}
}

// A FINISHED RUN THAT HIT THE CEILING STILL NEEDS A PERSON, so the alert is
// asked of every ticket rather than only the visible rows.
func TestTheCeilingAlertSurvivesTheFinishedFilter(t *testing.T) {
	stuck := tk("aaaaaaaa1111", "", workflow.ColDone, 60)
	stuck.Comments = []ticket.Comment{{Body: review.ReturnCeilingMarker}}

	m := loadInto(t, model(&board{tickets: []ticket.Ticket{stuck}}))
	if !strings.Contains(m.View(), "review ceiling") {
		t.Errorf("hiding the row hid the alert with it:\n%s", m.View())
	}
}

// ---- the detail view ----

func TestEnterOpensTheDetailView(t *testing.T) {
	tkt := tk("store", "", workflow.ColInDev, 20)
	tkt.Title = "Build the store"
	tkt.Description = "Keep the tasks in memory; there is no database."

	m := loadInto(t, model(&board{tickets: []ticket.Ticket{tkt}}))
	m, _ = apply(m, key("enter"))

	view := m.View()
	if !strings.Contains(view, "no database") {
		t.Errorf("the detail view does not show the description:\n%s", view)
	}
	if !strings.Contains(view, "esc") {
		t.Errorf("the detail view does not say how to get out:\n%s", view)
	}

	m, _ = apply(m, key("esc"))
	if m.mode != modeList {
		t.Error("esc did not return to the list")
	}
}

// THE DEPENDENCY GRAPH IN FULL, because the useful question is "which one, and
// has it landed" — the answer decides whether you wait or go and look.
func TestTheDetailViewNamesEveryPrerequisite(t *testing.T) {
	tkt := tk("api", "", workflow.ColReadyForDev, 10)
	tkt.DependsOn = []ticket.Dependency{
		{ID: "store111", Title: "The store", Status: workflow.ColDone},
		{ID: "types222", Title: "The types", Status: workflow.ColBlocked},
	}

	m := loadInto(t, model(&board{tickets: []ticket.Ticket{tkt}}))
	m, _ = apply(m, key("enter"))
	view := m.View()

	if !strings.Contains(view, "The store") || !strings.Contains(view, "The types") {
		t.Errorf("a prerequisite is missing:\n%s", view)
	}
	if !strings.Contains(view, "✓") {
		t.Errorf("a met prerequisite is not marked as met:\n%s", view)
	}
	// A BLOCKED PREREQUISITE WILL NEVER COMPLETE ON ITS OWN, and everything
	// behind it is stranded rather than queued.
	if !strings.Contains(view, "!") {
		t.Errorf("a blocked prerequisite is not marked:\n%s", view)
	}
}

// THE REASONING IS THE DIAGNOSIS. Counters say "41 refusals" and leave the cause
// to guesswork; the reasoning names the fault.
func TestTheDetailViewShowsTheModelsReasoning(t *testing.T) {
	dir := t.TempDir()
	writeTurns(t, dir, transcript.Record{
		Kind: transcript.KindTurn, TaskID: "store", Role: "dev-agent",
		Reasoning: "the router drops the leading slash", At: time.Now(),
	})

	tkt := tk("store", "", workflow.ColInDev, 20)
	m := model(&board{tickets: []ticket.Ticket{tkt}})
	m.opts.TranscriptDir = dir
	m = loadInto(t, m)
	m, _ = apply(m, key("enter"))

	view := m.View()
	if !strings.Contains(view, "reasoning") {
		t.Errorf("the reasoning panel is missing:\n%s", view)
	}
	if !strings.Contains(view, "leading slash") {
		t.Errorf("the reasoning itself is missing:\n%s", view)
	}
	if !strings.Contains(view, "dev-agent") {
		t.Errorf("the reasoning does not say which stage said it:\n%s", view)
	}
}

// A TICKET IS NOW TALLER THAN A TERMINAL, and everything past the fold was
// unreachable before the view was windowed.
func TestTheDetailViewScrolls(t *testing.T) {
	tkt := tk("store", "", workflow.ColInDev, 20)
	tkt.Description = strings.Repeat("a line of description that will wrap several times over. ", 60)

	m := loadInto(t, model(&board{tickets: []ticket.Ticket{tkt}}))
	m.height = 20
	m, _ = apply(m, key("enter"))

	first := m.View()
	if !strings.Contains(first, "lines") {
		t.Errorf("a long ticket does not report its position:\n%s", first)
	}

	m, _ = apply(m, key("down"))
	if m.View() == first {
		t.Error("scrolling down changed nothing")
	}

	// G goes to the end and is clamped there rather than scrolling into blank.
	m, _ = apply(m, key("G"))
	end := m.View()
	if strings.Contains(end, "  1-") {
		t.Errorf("G did not move off the first page:\n%s", end)
	}
	m, _ = apply(m, key("down"))
	if m.View() != end {
		t.Error("scrolling past the end moved the view into blank space")
	}

	m, _ = apply(m, key("g"))
	if m.View() != first {
		t.Error("g did not return to the top")
	}
}

// A TICKET THAT HAS BECOME A PERSON'S PROBLEM GIVES THE COMMAND.
func TestTheDetailViewHandsOverTheBranch(t *testing.T) {
	tkt := tk("api", "", workflow.ColBlocked, 60)
	tkt.Comments = []ticket.Comment{
		{Body: record.PublishBranch(record.BranchMarker, "bs/api-handlers")},
	}

	m := model(&board{tickets: []ticket.Ticket{tkt}})
	m.opts.RepoURL = "http://git.local/repo.git"
	m = loadInto(t, m)
	m, _ = apply(m, key("enter"))

	view := m.View()
	if !strings.Contains(view, "YOURS NOW") {
		t.Errorf("the handover is not announced:\n%s", view)
	}
	if !strings.Contains(view, "git fetch http://git.local/repo.git bs/api-handlers") {
		t.Errorf("the fetch command is missing:\n%s", view)
	}
}

// A STALE CLAIM ON A DEAD HOST IS WHY NOTHING IS MOVING, so who holds it is
// worth showing even though the claim itself is bookkeeping.
func TestTheDetailViewNamesWhoHoldsTheClaim(t *testing.T) {
	claim, err := record.Render(record.Claim{Role: "dev-agent", Host: "osiris"})
	if err != nil {
		t.Fatal(err)
	}
	tkt := tk("store", "", workflow.ColInDev, 20)
	tkt.Comments = []ticket.Comment{{Body: claim}}

	m := loadInto(t, model(&board{tickets: []ticket.Ticket{tkt}}))
	m, _ = apply(m, key("enter"))

	view := m.View()
	if !strings.Contains(view, "claimed by dev-agent on osiris") {
		t.Errorf("the claim holder is not named:\n%s", view)
	}
	if strings.Contains(view, record.ClaimMarker) {
		t.Errorf("the raw claim comment reached the screen:\n%s", view)
	}
}

// THE BOARD RELOADS UNDER THE CURSOR every two seconds. Without clamping, the
// selection indexes past the end and the next frame panics.
func TestTheCursorSurvivesTheBoardShrinking(t *testing.T) {
	b := &board{tickets: []ticket.Ticket{
		tk("a", "", workflow.ColInDev, 5),
		tk("b", "", workflow.ColInDev, 5),
		tk("c", "", workflow.ColInDev, 5),
	}}
	m := loadInto(t, model(b))
	m, _ = apply(m, key("G"))
	m, _ = apply(m, key("enter"))

	b.tickets = b.tickets[:1]
	m = loadInto(t, m)

	if m.selected >= len(m.board.Rows) {
		t.Fatalf("selected = %d with %d rows", m.selected, len(m.board.Rows))
	}
	m.View() // must not panic
}

func TestTheDetailViewFallsBackWhenTheBoardEmpties(t *testing.T) {
	b := &board{tickets: []ticket.Ticket{tk("a", "", workflow.ColInDev, 5)}}
	m := loadInto(t, model(b))
	m, _ = apply(m, key("enter"))

	b.tickets = nil
	m = loadInto(t, m)

	if m.mode != modeList {
		t.Error("the detail view stayed open on a ticket that no longer exists")
	}
	m.View() // must not panic
}

// ---- filing work ----

// HANDING THE DEPARTMENT WORK SHOULD NOT MEAN REMEMBERING WHICH STORE IS
// AUTHORITATIVE ON THIS HOST AND CURLING JSON AT IT.
func TestTheAskLineFilesATicket(t *testing.T) {
	b := &board{}
	m := loadInto(t, model(b))

	m, _ = apply(m, key("/"))
	if !strings.Contains(m.View(), "ask") {
		t.Fatalf("the ask line did not open:\n%s", m.View())
	}
	m = typeIn(m, "add rate limiting to the API")

	m, cmd := apply(m, key("enter"))
	if cmd == nil {
		t.Fatal("enter did not file anything")
	}
	m, _ = apply(m, cmd())

	if len(b.created) != 1 {
		t.Fatalf("created %d tickets, want 1", len(b.created))
	}
	got := b.created[0]
	if got.Title != "add rate limiting to the API" {
		t.Errorf("Title = %q", got.Title)
	}
	// THE WHOLE SENTENCE IS ALSO THE BODY. The product manager triages it either
	// way, and a title with an empty description throws away the only context.
	if got.Description != "add rate limiting to the API" {
		t.Errorf("Description = %q, want the sentence kept", got.Description)
	}
	if got.Status != workflow.ColInbox {
		t.Errorf("Status = %q, want it to enter at the inbox", got.Status)
	}
	if got.BoardID == nil || *got.BoardID != "board-1" {
		t.Errorf("BoardID = %v, want the configured board", got.BoardID)
	}
	if !strings.Contains(m.View(), "filed") {
		t.Errorf("the reader is not told it worked:\n%s", m.View())
	}
}

// WHILE A FIELD IS OPEN EVERY PRINTABLE KEY IS CONTENT, NOT A COMMAND.
// Otherwise typing "quit the job" quits on the q.
func TestKeysDoNotLeakOutOfTheAskLine(t *testing.T) {
	m := loadInto(t, model(&board{}))
	m, _ = apply(m, key("/"))

	for _, k := range []string{"q", "n", "h", "r", "g"} {
		var cmd tea.Cmd
		m, cmd = apply(m, key(k))
		if cmd != nil {
			t.Fatalf("%q escaped the ask line and ran a command", k)
		}
	}
	if m.ask != "qnhrg" {
		t.Fatalf("ask = %q, want the characters typed", m.ask)
	}
}

func TestTheAskLineCanBeCancelled(t *testing.T) {
	b := &board{}
	m := loadInto(t, model(b))
	m, _ = apply(m, key("/"))
	m = typeIn(m, "never mind")
	m, _ = apply(m, key("esc"))

	if m.asking || m.ask != "" {
		t.Error("esc did not close and clear the ask line")
	}
	if len(b.created) != 0 {
		t.Error("cancelling filed a ticket anyway")
	}
}

// An empty ask files nothing rather than an untitled ticket.
func TestAnEmptyAskFilesNothing(t *testing.T) {
	b := &board{}
	m := loadInto(t, model(b))
	m, _ = apply(m, key("/"))
	m, cmd := apply(m, key("enter"))

	if cmd != nil {
		cmd()
	}
	if len(b.created) != 0 {
		t.Error("an empty ask created a ticket")
	}
	if m.asking {
		t.Error("the ask line stayed open")
	}
}

func TestBackspaceInTheAskLine(t *testing.T) {
	m := loadInto(t, model(&board{}))
	m, _ = apply(m, key("/"))
	m = typeIn(m, "abc")
	m, _ = apply(m, key("backspace"))

	if m.ask != "ab" {
		t.Fatalf("ask = %q, want %q", m.ask, "ab")
	}
	for i := 0; i < 5; i++ {
		m, _ = apply(m, key("backspace"))
	}
	if m.ask != "" {
		t.Fatalf("ask = %q, want empty", m.ask)
	}
}

// A FAILURE TO FILE MUST BE SAID. Silence is indistinguishable from success.
func TestAFailureToFileIsReported(t *testing.T) {
	b := &board{createErr: errors.New("board is read-only")}
	m := loadInto(t, model(b))
	m, _ = apply(m, key("/"))
	m = typeIn(m, "something")
	m, cmd := apply(m, key("enter"))
	m, _ = apply(m, cmd())

	if !strings.Contains(m.View(), "could not file it") {
		t.Errorf("the failure is not reported:\n%s", m.View())
	}
	if !strings.Contains(m.View(), "read-only") {
		t.Errorf("the reason is not given:\n%s", m.View())
	}
}

// ---- the compose screen ----

func TestComposeFilesATicketWithTwoFields(t *testing.T) {
	b := &board{}
	m := loadInto(t, model(b))

	m, _ = apply(m, key("n"))
	if m.mode != modeCompose {
		t.Fatal("n did not open the compose screen")
	}
	m = typeIn(m, "Add rate limiting")
	m, _ = apply(m, key("tab"))
	m = typeIn(m, "Per IP, sliding window.")

	m, cmd := apply(m, key("ctrl+s"))
	if cmd == nil {
		t.Fatal("ctrl+s filed nothing")
	}
	m, _ = apply(m, cmd())

	if len(b.created) != 1 {
		t.Fatalf("created %d tickets", len(b.created))
	}
	if b.created[0].Title != "Add rate limiting" {
		t.Errorf("Title = %q", b.created[0].Title)
	}
	if b.created[0].Description != "Per IP, sliding window." {
		t.Errorf("Description = %q", b.created[0].Description)
	}
	if m.mode != modeList {
		t.Error("filing did not return to the board")
	}
}

// NAMED, NOT SILENTLY REFUSED. A ctrl+s that appears to do nothing is
// indistinguishable from a key that is not bound.
func TestComposeRefusesAnEmptyTitleAndSaysWhy(t *testing.T) {
	b := &board{}
	m := loadInto(t, model(b))

	m, _ = apply(m, key("n"))
	m, _ = apply(m, key("tab"))
	m = typeIn(m, "detail but no title")
	m, cmd := apply(m, key("ctrl+s"))

	if cmd != nil {
		t.Fatal("an untitled ticket was filed")
	}
	if m.mode != modeCompose {
		t.Error("the compose screen closed on a refusal")
	}
	if !strings.Contains(m.View(), "a title is needed") {
		t.Errorf("the refusal does not say what is wrong:\n%s", m.View())
	}
}

func TestComposeEnterBehavesLikeAForm(t *testing.T) {
	m := loadInto(t, model(&board{}))
	m, _ = apply(m, key("n"))

	// Enter on the title moves to the detail field rather than filing.
	m, _ = apply(m, key("enter"))
	if m.field != 1 {
		t.Fatal("enter on the title did not move to the detail field")
	}
	m = typeIn(m, "one")
	m, _ = apply(m, key("enter"))
	m = typeIn(m, "two")
	if m.body != "one\ntwo" {
		t.Fatalf("body = %q, want two lines", m.body)
	}
}

func TestComposeCanBeCancelled(t *testing.T) {
	b := &board{}
	m := loadInto(t, model(b))
	m, _ = apply(m, key("n"))
	m = typeIn(m, "half a thought")
	m, _ = apply(m, key("esc"))

	if m.mode != modeList {
		t.Error("esc did not leave the compose screen")
	}
	if len(b.created) != 0 {
		t.Error("cancelling filed a ticket anyway")
	}
}

func TestComposeBackspaceInBothFields(t *testing.T) {
	m := loadInto(t, model(&board{}))
	m, _ = apply(m, key("n"))
	m = typeIn(m, "abc")
	m, _ = apply(m, key("backspace"))
	if m.title != "ab" {
		t.Fatalf("title = %q", m.title)
	}
	m, _ = apply(m, key("tab"))
	m = typeIn(m, "xyz")
	m, _ = apply(m, key("backspace"))
	if m.body != "xy" {
		t.Fatalf("body = %q", m.body)
	}
	for i := 0; i < 5; i++ {
		m, _ = apply(m, key("backspace"))
	}
	if m.body != "" {
		t.Fatalf("body = %q, want empty", m.body)
	}
}

// ---- navigation and lifecycle ----

func TestMovingTheCursor(t *testing.T) {
	b := &board{tickets: []ticket.Ticket{
		tk("a", "", workflow.ColInDev, 5),
		tk("b", "", workflow.ColInDev, 5),
		tk("c", "", workflow.ColInDev, 5),
	}}
	m := loadInto(t, model(b))

	m, _ = apply(m, key("down"))
	if m.selected != 1 {
		t.Fatalf("selected = %d after down, want 1", m.selected)
	}
	m, _ = apply(m, key("up"))
	if m.selected != 0 {
		t.Fatalf("selected = %d after up, want 0", m.selected)
	}
	// UP AT THE TOP AND DOWN AT THE BOTTOM STOP rather than wrapping: a cursor
	// that jumps to the far end of a forty-row board loses the reader's place.
	m, _ = apply(m, key("up"))
	if m.selected != 0 {
		t.Fatalf("selected = %d, want the cursor held at the top", m.selected)
	}
	m, _ = apply(m, key("G"))
	if m.selected != 2 {
		t.Fatalf("selected = %d after G, want the last row", m.selected)
	}
	m, _ = apply(m, key("down"))
	if m.selected != 2 {
		t.Fatalf("selected = %d, want the cursor held at the bottom", m.selected)
	}
}

// A CURSOR THAT SCROLLS OFF THE SCREEN IS A CURSOR YOU CANNOT FIND.
func TestTheListScrollsToFollowTheCursor(t *testing.T) {
	var ts []ticket.Ticket
	for i := 0; i < 30; i++ {
		ts = append(ts, tk(fmt.Sprintf("t%02d", i), "", workflow.ColInDev, 5))
	}
	m := loadInto(t, model(&board{tickets: ts}))
	m.height = 16

	for i := 0; i < 25; i++ {
		m, _ = apply(m, key("down"))
	}
	if m.scroll == 0 {
		t.Fatal("the list never scrolled")
	}
	if m.selected < m.scroll || m.selected >= m.scroll+m.listRows() {
		t.Fatalf("selected %d is outside the visible window [%d,%d)",
			m.selected, m.scroll, m.scroll+m.listRows())
	}
}

// THE KEYS ARE THE ONLY WAY OUT, so they have to work.
func TestQuittingWorks(t *testing.T) {
	m := loadInto(t, model(&board{}))
	for _, k := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune{'q'}},
		{Type: tea.KeyEsc},
		{Type: tea.KeyCtrlC},
	} {
		if _, cmd := apply(m, k); cmd == nil {
			t.Errorf("%v did not quit", k)
		}
	}
	if _, cmd := apply(m, key("z")); cmd != nil {
		t.Error("an unrecognised key produced a command")
	}
}

// ctrl+c QUITS FROM EVERY SCREEN. A text field that swallows it leaves the only
// universal way out unavailable.
func TestCtrlCQuitsFromEveryScreen(t *testing.T) {
	base := loadInto(t, model(&board{tickets: []ticket.Ticket{tk("a", "", workflow.ColInDev, 5)}}))

	asking, _ := apply(base, key("/"))
	composing, _ := apply(base, key("n"))
	detail, _ := apply(base, key("enter"))

	for name, m := range map[string]Model{
		"list": base, "ask": asking, "compose": composing, "detail": detail,
	} {
		if _, cmd := apply(m, tea.KeyMsg{Type: tea.KeyCtrlC}); cmd == nil {
			t.Errorf("ctrl+c did not quit from the %s screen", name)
		}
	}
}

// THE BOARD RELOADS ON ITS OWN, and each tick schedules the next one.
func TestATickReloadsAndSchedulesTheNextOne(t *testing.T) {
	b := &board{}
	m := model(b)

	_, cmd := apply(m, tickMsg(time.Now()))
	if cmd == nil {
		t.Fatal("a tick produced no work")
	}
	before := b.calls
	m = loadInto(t, m)
	if b.calls <= before {
		t.Error("the reload never reached the store")
	}
	if Refresh <= 0 || Refresh > 10*time.Second {
		t.Errorf("the refresh interval is %v; a stage change would look like a stall", Refresh)
	}
}

func TestRefreshOnDemand(t *testing.T) {
	b := &board{}
	m := model(b)
	_, cmd := apply(m, key("r"))
	if cmd == nil {
		t.Fatal("r produced no reload")
	}
	cmd()
	if b.calls == 0 {
		t.Error("r did not reach the store")
	}
}

// THE WINDOW READS AND FILES, AND DOES NOTHING ELSE. It can be left open beside
// a running department because it cannot claim, move or comment on a ticket —
// the three things that would race the stages for work.
func TestTheWindowCannotRaceTheDepartment(t *testing.T) {
	var iface Tickets = (*board)(nil)

	type mutates interface {
		Move(ctx context.Context, id, status string) error
	}
	if _, ok := iface.(mutates); ok {
		t.Fatal("the window can move tickets; it would race the stages that claim them")
	}
	type comments interface {
		Comment(ctx context.Context, id, body string) error
	}
	if _, ok := iface.(comments); ok {
		t.Fatal("the window can comment; a claim is a comment and this would corrupt claiming")
	}
}

// THE WINDOW LOADS AND STARTS TICKING THE MOMENT IT OPENS.
func TestOpeningLoadsAndStartsRefreshing(t *testing.T) {
	b := &board{tickets: []ticket.Ticket{tk("live", "", workflow.ColInDev, 5)}}
	m := model(b)

	if n := commandsIn(m.Init()); n != 2 {
		t.Errorf("opening the window ran %d commands, want the load and the tick", n)
	}
	_, cmd := apply(m, tickMsg(time.Now()))
	if n := commandsIn(cmd); n != 2 {
		t.Errorf("a tick ran %d commands, want the load and the next tick", n)
	}
}

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

// THE FIRST FRAME MUST NOT CLAIM THE BOARD IS EMPTY. "no tickets" while the
// first read is still in flight is a lie, and it is the frame every launch opens
// on.
func TestTheOpeningFrameSaysItIsStillReading(t *testing.T) {
	m := Model{tickets: &board{}, opts: Options{Table: table(), TranscriptDir: "off"},
		loading: true, width: 140, height: 40}
	if !strings.Contains(m.View(), "reading the store") {
		t.Errorf("the opening frame does not say it is still reading:\n%s", m.View())
	}
}

// A LONG TITLE MUST NOT PUSH THE STATE OFF THE LINE.
func TestALongTitleIsTrimmedSoTheStateStaysVisible(t *testing.T) {
	long := tk("x", "", workflow.ColInDev, 5)
	long.Title = strings.Repeat("a very long ticket title ", 20)

	view := loadInto(t, model(&board{tickets: []ticket.Ticket{long}})).View()
	line := lineFor(view, "a very long")

	if !strings.Contains(line, "being written") {
		t.Errorf("the state was pushed off the line:\n%q", line)
	}
	if !strings.Contains(line, "…") {
		t.Errorf("a trimmed title does not say it was trimmed:\n%q", line)
	}
}

// THE PROGRESS COUNT IS RESERVED FOR, NOT APPENDED. Appending it and clipping
// the result made the count the first thing cut — so it never appeared on the
// rows that most needed it.
func TestAParentRowKeepsItsProgressBarWhateverTheTitle(t *testing.T) {
	parent := tk("req", "", workflow.ColTracking, 60)
	parent.Title = strings.Repeat("an extremely long request title ", 10)
	ts := []ticket.Ticket{parent}
	for i := 0; i < 4; i++ {
		ts = append(ts, tk(fmt.Sprintf("c%d", i), "req", workflow.ColDone, 30))
	}

	m := model(&board{tickets: ts})
	m.showFinished = true
	view := loadInto(t, m).View()
	line := lineFor(view, "an extremely long")

	if !strings.Contains(line, "4/4") {
		t.Errorf("the progress count was clipped off the row:\n%q", line)
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

// A NARROW TERMINAL MUST STILL DRAW. The first frame arrives before bubbletea
// has reported a size, so every width calculation has to survive zero.
func TestTheWindowDrawsBeforeItKnowsItsSize(t *testing.T) {
	b := &board{tickets: []ticket.Ticket{tk("a", "", workflow.ColInDev, 5)}}
	m := Model{tickets: b, opts: Options{Table: table(), TranscriptDir: "off"}}
	m = loadInto(t, m)

	m.View() // must not panic at width 0
	m, _ = apply(m, key("enter"))
	m.View()
}

// VIEW MUST NOT TOUCH THE FILESYSTEM, and neither must Update.
//
// Both run on bubbletea's single event loop. Reading the transcripts there cost
// a full parse of the corpus per frame — measured at 220ms for the activity and
// 212ms for the reasoning against 48MB, on a board that refreshes every two
// seconds and re-renders on every keystroke. That is not a deadlock; it is a UI
// that stops responding, which is indistinguishable from one that has hung.
//
// Pinned by pointing the window at a corpus that EXISTS and would be read, then
// rendering every screen and asserting nothing opened it. The reasoning still
// has to appear — it comes from the load, which is the whole point.
func TestTheEventLoopNeverReadsTheCorpus(t *testing.T) {
	dir := t.TempDir()
	writeTurns(t, dir, transcript.Record{
		Kind: transcript.KindTurn, TaskID: "store", Role: "dev-agent",
		Reasoning: "the router drops the leading slash", At: time.Now(),
	})

	m := model(&board{tickets: []ticket.Ticket{tk("store", "", workflow.ColInDev, 20)}})
	m.opts.TranscriptDir = dir
	m = loadInto(t, m) // the read belongs HERE

	// Make the corpus unreadable. Anything that still needs it will now fail.
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Skipf("cannot revoke access to the corpus: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	if _, err := os.ReadDir(dir); err == nil {
		t.Skip("access to the corpus could not be revoked (running as root?)")
	}

	// A RELOAD LANDING ON THE EVENT LOOP must not go back to the corpus either:
	// Update runs on the same single thread as View. The digest is handed to it
	// already built, so a live agent still shows as live with the files gone.
	m, _ = apply(m, loadedMsg{
		tickets: []ticket.Ticket{tk("store", "", workflow.ColInDev, 20)},
		digest: Digest{
			Acts:     map[string]Activity{"store": {Role: "dev-agent", What: "editing", At: time.Now()}},
			Thoughts: map[string][]Thought{"store": {{Role: "dev-agent", Prose: "the router drops the leading slash"}}},
		},
	})
	if !strings.Contains(m.View(), "dev-agent") {
		t.Fatalf("the activity was re-read on the event loop rather than taken "+
			"from the load:\n%s", m.View())
	}

	// Every screen, and a burst of keystrokes, with the corpus unreachable.
	if !strings.Contains(m.View(), "store") {
		t.Fatalf("the list did not draw:\n%s", m.View())
	}
	m, _ = apply(m, key("enter"))
	detail := m.View()
	if !strings.Contains(detail, "leading slash") {
		t.Fatalf("the reasoning is missing, so it was being read at render time "+
			"rather than carried by the load:\n%s", detail)
	}
	for i := 0; i < 20; i++ {
		m, _ = apply(m, key("down"))
		m.View()
	}
	m, _ = apply(m, key("esc"))
	m, _ = apply(m, key("n"))
	m.View()
}

// THE REASONING TRAVELS WITH THE BOARD. If the load stopped carrying it, the
// detail view would silently lose the panel rather than fail loudly.
func TestTheLoadCarriesTheReasoning(t *testing.T) {
	dir := t.TempDir()
	writeTurns(t, dir, transcript.Record{
		Kind: transcript.KindTurn, TaskID: "store", Role: "dev-agent",
		Reasoning: "the store is keyed by title", At: time.Now(),
	})

	m := model(&board{tickets: []ticket.Ticket{tk("store", "", workflow.ColInDev, 20)}})
	m.opts.TranscriptDir = dir
	m = loadInto(t, m)

	if len(m.thoughts["store"]) == 0 {
		t.Fatal("the load did not carry the reasoning for a ticket on the board")
	}
}

// ONE READ AT A TIME.
//
// The tick fires every two seconds whatever the store is doing, and a read is
// given twenty. Against a slow or unreachable store that stacked ten reads
// deep — each holding a listing and a request per row — and kept doing it for
// as long as the window was open. The goroutine dump from a wedged window was
// full of HTTP read loops for exactly this reason.
//
// Asserted on whether a read was STARTED rather than on the returned command:
// running a bare tick command actually waits the refresh interval, so counting
// commands here would sleep rather than test.
func TestATickDoesNotStackReadsOnASlowStore(t *testing.T) {
	b := &board{tickets: []ticket.Ticket{tk("a", "", workflow.ColInDev, 5)}}
	m := model(b)

	m, cmd := apply(m, tickMsg(time.Now()))
	if cmd == nil {
		t.Fatal("the first tick started nothing")
	}
	if !m.inFlight {
		t.Fatal("the first tick did not mark a read in flight")
	}

	// Every tick while it is outstanding still has to schedule the next one, or
	// the board never recovers once the store comes back.
	before := b.calls
	for i := 0; i < 10; i++ {
		var c tea.Cmd
		m, c = apply(m, tickMsg(time.Now()))
		if c == nil {
			t.Fatalf("tick %d stopped the refresh loop; the board would freeze", i)
		}
	}
	if b.calls != before {
		t.Fatalf("%d further reads were started while one was outstanding",
			b.calls-before)
	}
}

// AND THE NEXT TICK READS AGAIN once the outstanding one reports, or the board
// stops refreshing after a single slow read.
func TestReadsResumeOnceTheOutstandingOneReports(t *testing.T) {
	b := &board{tickets: []ticket.Ticket{tk("a", "", workflow.ColInDev, 5)}}
	m := model(b)

	m, _ = apply(m, tickMsg(time.Now()))
	m = loadInto(t, m) // the read reports
	if m.inFlight {
		t.Fatal("a completed read left the window believing one was outstanding")
	}

	m, cmd := apply(m, tickMsg(time.Now()))
	if !m.inFlight {
		t.Fatal("the tick after a completed read started no new one")
	}
	// A batch of two: the read and the next tick. Cheap to count, because
	// running a batch yields its children rather than executing them.
	if n := commandsIn(cmd); n != 2 {
		t.Fatalf("the tick ran %d commands, want the read and the next tick", n)
	}
}

// A FAILED READ ALSO CLEARS IT. Otherwise one unreachable moment stops the
// window refreshing for as long as it stays open — the exact failure the
// stale-board banner exists to ride out.
func TestAFailedReadDoesNotWedgeTheRefresh(t *testing.T) {
	b := &board{err: errors.New("the store could not be reached")}
	m := model(b)

	m, _ = apply(m, tickMsg(time.Now()))
	m = loadInto(t, m)
	if m.inFlight {
		t.Fatal("a failed read left the window believing one was outstanding")
	}

	m, _ = apply(m, tickMsg(time.Now()))
	if !m.inFlight {
		t.Fatal("the window stopped reading after one failure")
	}
}

// r IS NOT A WAY ROUND THE GUARD either: leaning on it during a slow read would
// stack exactly what the tick no longer does.
func TestRefreshOnDemandRespectsAnOutstandingRead(t *testing.T) {
	b := &board{tickets: []ticket.Ticket{tk("a", "", workflow.ColInDev, 5)}}
	m := model(b)

	m, _ = apply(m, tickMsg(time.Now()))
	before := b.calls
	for i := 0; i < 5; i++ {
		var cmd tea.Cmd
		m, cmd = apply(m, key("r"))
		// RUN what r returned. apply only collects commands, so asserting on the
		// store without this passes whether or not r started a read.
		if cmd != nil {
			cmd()
		}
	}
	if b.calls != before {
		t.Fatalf("r started %d reads while one was outstanding", b.calls-before)
	}
}

// THE FIRST FRAME COUNTS AS A READ. Init cannot mark it — it has a value
// receiver — so Open builds the model with it already set, or the first tick
// doubles up on the opening read.
func TestOpeningTheWindowMarksItsFirstReadInFlight(t *testing.T) {
	b := &board{}
	// Built the way Open builds it, so the two cannot drift.
	m := newModel(b, Options{Table: table(), TranscriptDir: "off"})

	if !m.inFlight {
		t.Fatal("the opening read is not marked in flight, so the first tick " +
			"doubles up on it")
	}
	before := b.calls
	m, cmd := apply(m, tickMsg(time.Now()))
	if cmd != nil {
		cmd()
	}
	if b.calls != before {
		t.Fatal("the first tick read again while the opening read was outstanding")
	}
	if !m.loading {
		t.Fatal("the opening frame does not know it is still reading")
	}
}

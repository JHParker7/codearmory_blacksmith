package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// The department's window.
//
// blacksmith is a systemd unit with no interface, and before this the only way to
// answer "what is it doing" was to tail a log, poll the ticket store and check
// kubectl for a sandbox pod — three tools, none of which answers the question on
// its own. This is one screen that does.
//
// IT READS THE DURABLE RECORD, and holds no connection to the running process.
// blacksmith deliberately has no inbound API — it is a pull-only client so the
// control plane never has to reach an intermittent workstation behind NAT — and
// an attach-to-the-process design would either break that or tie this window's
// life to the service's, so you could not open it on the department already
// running. Everything here comes from the two places the work is already written
// down: the ticket store, and the transcripts.
//
// What that costs is in-memory state: queue depth and busy serving slots live in
// the process and are not written anywhere, so they are absent here. That wants a
// local socket, which is a separate piece of work.

// tuiRefresh is how often the view reloads. Fast enough that a stage change feels
// immediate, slow enough that reading a ticket per row is not a poll loop against
// the store.
const tuiRefresh = 2 * time.Second

// stage is where a ticket has reached in the pipeline. It IS the board column,
// so the window shows what the department is actually routing on rather than a
// second opinion about it.
//
// This used to be derived from the comment markers each agent left. That was a
// parallel implementation of the routing rules, and the two could disagree — a
// window confidently reporting a stage the dispatchers did not agree with is
// worse than no window, because it is believed.
type stage string

func stageOf(t Ticket) stage { return stage(t.Status) }

// needsAPerson reports the columns that exist because an agent could not
// finish. These are the rows worth colouring: everything else is in motion.
func (s stage) needsAPerson() bool {
	return s == stage(ColBlocked) || s == stage(ColConflicted)
}

func (s stage) done() bool { return s == stage(ColDone) }

func (s stage) label() string {
	switch string(s) {
	case ColInbox:
		return "waiting to be designed"
	case ColDesigning:
		return "having its documentation written"
	case ColReadyForScoping:
		return "documented, waiting to be scoped"
	case ColScoping:
		return "being scoped"
	case ColTracking:
		return "broken down — tracking its children"
	case ColReadyForTests:
		return "waiting for its tests to be written"
	case ColWritingTests:
		return "having its tests written"
	case ColReadyForDev:
		return "has tests, waiting for a developer"
	case ColInDev:
		return "being written"
	case ColReadyForCoverage:
		return "waiting for its edge cases to be covered"
	case ColCovering:
		return "having its edge cases covered"
	case ColReadyForReview:
		return "waiting for review"
	case ColInReview:
		return "being reviewed"
	case ColReadyForIntegration:
		return "waiting to merge"
	case ColIntegrating:
		return "merging"
	case ColConflicted:
		return "MERGE CONFLICT"
	case ColBlocked:
		return "BLOCKED — needs you"
	case ColDone:
		return "merged to the integration branch"
	case "":
		return "no status"
	}
	// A column the department does not know about is someone else's, and saying
	// so is more useful than pretending it is a stage.
	return string(s) + " (not a department column)"
}

// nextRole names the agent that will pick a ticket up, READ OFF THE ROUTING
// TABLE rather than restated here. A stage added to that table shows up in the
// window the day it is written, and cannot disagree with it.
func (s stage) nextRole() string {
	for role, st := range stages {
		if st.Ready == string(s) {
			return role
		}
	}
	return ""
}

// activity is what a task's transcript says is happening right now.
type activity struct {
	role       string
	what       string
	at         time.Time
	turns      int
	finished   bool
	lastStatus string
	// working counts how many DESCENDANTS are live, and is zero on the ticket
	// actually doing the work.
	//
	// The first roll-up copied a live descendant's role and state onto every
	// ancestor, so a root, its task and its section all read "dev-agent ·
	// editing" — a board on which everything appeared to be working while one
	// thing was. A parent's honest state is how many of its children are busy,
	// not an impersonation of the busiest.
	working int
}

// live reports whether this looks like work in progress rather than a finished
// task. A transcript with no outcome record is the signal the format was designed
// around: it marks a task that never finished, which from here is the same shape
// as one still running.
func (a activity) live() bool {
	return !a.finished && time.Since(a.at) < 10*time.Minute
}

type tuiModel struct {
	cfg Config
	api *CodeArmory

	// projects is every project this host is configured for, and project is the
	// one being viewed — or all of them, when it is -1.
	//
	// The window OPENS ON ALL OF THEM. It exists to answer "is anything wrong",
	// and a view scoped to one project answers that for one project while hiding
	// every other board — so a stuck ticket somewhere else is invisible until you
	// think to look. The collision this used to worry about (two codebases both
	// with "add filtering") is handled by tagging each row with its project and
	// showing the ticket id, not by hiding the other projects. Press p to narrow.
	projects []Project
	project  int

	tickets []Ticket
	// depth is how far each ticket is nested under the request it came from, so a
	// broken-down piece's children sit a level in from it rather than flat beside
	// it. Built by orderByParent alongside the ordering it implies.
	depth map[string]int
	// progress is how many of each ticket's children are finished. See
	// orderByParent.
	progress map[string]childProgress
	acts     map[string]activity
	selected int
	detail   bool
	// detailScroll is how far the detail view is scrolled, in lines. The reasoning
	// panel made the view taller than a terminal for the first time, so its content
	// has to be windowed rather than assumed to fit.
	detailScroll int

	// mode is which screen is showing. It replaced a pair of booleans once there
	// were more than two screens: "detail && !composing" is a state you have to
	// reason about, where a mode is one you can read.
	mode tuiMode

	// Composing a new ticket. This is the reason the window exists as much as
	// watching is: handing the department work should not mean remembering which
	// store is authoritative on this host and curling JSON at it.
	composing bool

	// The ask line: one field on the board itself. asking is whether it has the
	// keyboard, filing is whether a request is in the air.
	//
	// SEPARATE FROM composing ON PURPOSE. The compose screen is for a request you
	// have thought about and want two fields for; this is for the sentence you
	// already have in your head, typed without leaving the view you are watching.
	asking bool
	ask    string
	filing bool

	// showFinished lists runs that are over. Off by default: the board answers
	// "what is happening now", and every run this host has ever done answers a
	// different question that nobody asked on opening it.
	showFinished bool
	// hidden is how many finished runs the last read left out.
	hidden int

	// gw is the model gateway, used only to refine a one-line ask into a request.
	// Nil is fine everywhere: refineRequest files the typed line unchanged.
	gw    *Gateway
	field int
	title string
	body  string

	// Project management state.
	projSelected int
	draft        projectDraft
	draftField   projectField

	// Settings state. Values are held as strings for the same reason the project
	// draft is: a half-typed number is not a number yet.
	setValues map[string]string
	setField  int

	err     error
	note    string
	width   int
	height  int
	loading bool
}

// tuiMode is which screen the window is showing.
type tuiMode int

const (
	modeList tuiMode = iota
	modeProjects
	modeProjectForm
	modeSettings
)

// allProjects is the sentinel for the combined view.
const allProjects = -1

// visibleProjects is the set the current selection covers.
func (m tuiModel) visibleProjects() []Project {
	if m.project == allProjects || m.project >= len(m.projects) {
		return m.projects
	}
	return m.projects[m.project : m.project+1]
}

// projectOf names the project a ticket came from, matched by board.
func (m tuiModel) projectOf(t Ticket) string {
	for _, p := range m.projects {
		if t.BoardID != nil && *t.BoardID == p.BoardID {
			return p.Name
		}
	}
	return ""
}

type ticketsMsg struct {
	tickets []Ticket
	// depth is how far each ticket is nested under the request it came from, so
	// a broken-down piece's children sit a level in from it rather than flat
	// beside it. Built by orderByParent alongside the ordering it implies.
	depth map[string]int
	// progress is how many of each ticket's children are finished, so a parent
	// row can say how much of it is left.
	progress map[string]childProgress
	acts     map[string]activity
	// hidden is how many finished runs were left out. A board that silently drops
	// rows is worse than a busy one, so the count is shown.
	hidden int
	err    error
}
type tickMsg time.Time
type noteMsg string

func tick() tea.Cmd {
	return tea.Tick(tuiRefresh, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// load reads the store and the transcripts. Tickets are read in FULL, one request
// each: a listing carries no comments, and every stage marker is a comment, so a
// view built from the listing would show every ticket as untriaged forever.
func (m tuiModel) load() tea.Cmd {
	showFinished := m.showFinished
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		// EVERY VISIBLE PROJECT'S BOARD, not one. The window is how a person works
		// several projects at once, so the read follows the selection rather than
		// the host's single configured board.
		//
		// A board that cannot be read does not blank the others: its error is kept
		// and shown, and the projects that answered are still rendered. One
		// unreachable project is a much smaller problem than a window that goes
		// dark because of it.
		var listed []Ticket
		var readErr error
		for _, proj := range m.visibleProjects() {
			ts, err := m.api.ListTickets(ctx, ListOpts{BoardID: proj.BoardID})
			if err != nil {
				readErr = err
				continue
			}
			listed = append(listed, ts...)
		}
		if listed == nil && readErr != nil {
			return ticketsMsg{err: readErr}
		}
		if len(listed) > tuiMaxTickets {
			listed = listed[:tuiMaxTickets]
		}
		full := make([]Ticket, 0, len(listed))
		for _, t := range listed {
			ft, err := m.api.GetTicket(ctx, t.TicketID)
			if err != nil {
				full = append(full, t) // better a stale row than a missing one
				continue
			}
			full = append(full, ft)
		}
		ordered, depth, progress, hidden := orderByParent(full, showFinished)
		// A PARENT NEVER WORKS, so without the roll-up a decomposed request reads
		// "turn 0" while every section beneath it is being edited and merged.
		// Reported as a hang twice before the cause was found.
		acts := rollUp(ordered, readActivity(m.cfg.TranscriptDir))
		return ticketsMsg{tickets: ordered, depth: depth, progress: progress,
			acts: acts, hidden: hidden, err: readErr}
	}
}

// tuiMaxTickets bounds the per-row reads. A board larger than this is worth
// filtering rather than rendering.
const tuiMaxTickets = 40

// sortRows is the order WITHIN one group: what needs a person first, then work
// in flight, then queued, then the finished record — and newest first inside
// each of those.
//
// Purely chronological order scatters finished tickets among unfinished ones, so
// "what has been done" is only answerable by reading every row. Anything needing
// a person sorts first because it is the only thing on the board that will not
// move on its own.
func sortRows(ts []Ticket) {
	sort.SliceStable(ts, func(i, j int) bool {
		ri, rj := tuiRank(stageOf(ts[i])), tuiRank(stageOf(ts[j]))
		if ri != rj {
			return ri < rj
		}
		return ts[i].CreatedAt.After(ts[j].CreatedAt)
	})
}

// orderByParent nests each request above the tickets it produced, and each
// broken-down piece above the tickets IT produced, returning the depth of every
// row so the view can indent it.
//
// The parent is the ONLY thing on the board that explains why twelve tickets
// about task stores and HTML handlers exist at all, and separating it from its
// children — which chronological order does, because the children are created
// after it and then move at different speeds — throws that away exactly when the
// board gets big enough to need it.
//
// A CONTAINER IS RANKED BY ITS BEST CHILD, which is what keeps the board about
// the work. A request and a broken-down piece are never "in progress"
// themselves; they sit in tracking, which ranked as held-by-a-stage and floated
// five inert rows above everything being built. Ranking a container by the most
// urgent thing beneath it puts it exactly where its work is, and sinks it when
// its work is done.
//
// A child whose parent is not on this board (a different project, or a parent
// deleted out from under it) is treated as a root. That is the honest rendering:
// nothing here can show a relationship to a ticket it cannot see.
// runFinished reports whether a whole run — a root request and everything under
// it — is over.
//
// THE ROOT ALONE IS NOT ENOUGH. A request reaches done when every task under it
// does, but a run can also end with a task blocked, and a root still in tracking
// above finished work reads as live when nothing is moving. So the question is
// asked of the descendants, and a run is over when none of them can still
// progress on their own.
func runFinished(root Ticket, children map[string][]Ticket) bool {
	over := true
	var walk func(t Ticket)
	walk = func(t Ticket) {
		if !over {
			return
		}
		st := stageOf(t)
		if !st.done() && !st.needsAPerson() && st != stage(ColTracking) {
			over = false
			return
		}
		for _, c := range children[t.TicketID] {
			walk(c)
		}
	}
	walk(root)
	return over
}

// showFinished is whether runs that are over are listed at all. See the hidden
// return value: a board that silently drops rows is worse than a busy one, so
// the count of what was hidden goes back to the caller to be shown.
func orderByParent(ts []Ticket, showFinished bool) ([]Ticket, map[string]int, map[string]childProgress, int) {
	present := make(map[string]bool, len(ts))
	for _, t := range ts {
		present[t.TicketID] = true
	}
	children := map[string][]Ticket{}
	roots := make([]Ticket, 0, len(ts))
	for _, t := range ts {
		if t.ParentID != nil && present[*t.ParentID] && *t.ParentID != t.TicketID {
			children[*t.ParentID] = append(children[*t.ParentID], t)
			continue
		}
		roots = append(roots, t)
	}

	// best is a row's own rank, or its most urgent descendant's, whichever is
	// more urgent. Memoised because a deep board would otherwise walk each
	// subtree once per comparison.
	best := map[string]int{}
	var rank func(t Ticket) int
	rank = func(t Ticket) int {
		if r, ok := best[t.TicketID]; ok {
			return r
		}
		best[t.TicketID] = tuiRank(stageOf(t)) // guards a cycle
		r := tuiRank(stageOf(t))
		for _, c := range children[t.TicketID] {
			if cr := rank(c); cr < r {
				r = cr
			}
		}
		best[t.TicketID] = r
		return r
	}
	sortByBest := func(rows []Ticket) {
		sort.SliceStable(rows, func(i, j int) bool {
			ri, rj := rank(rows[i]), rank(rows[j])
			if ri != rj {
				return ri < rj
			}
			return rows[i].CreatedAt.After(rows[j].CreatedAt)
		})
	}

	// A LIVE RUN OUTRANKS A FINISHED ONE WHATEVER IS INSIDE IT. rank takes the most
	// urgent descendant, which is right within a run and wrong between them: one
	// blocked section in a run that ended days ago ranks the entire dead run above
	// today's work, because a blocked ticket is the most urgent thing there is.
	//
	// That is what made the board muddy once there were several runs on it. The
	// question a person opens this to ask is "what is happening now", and the
	// answer must not be sorted underneath what happened last week.
	sortRoots := func(rows []Ticket) {
		sort.SliceStable(rows, func(i, j int) bool {
			fi, fj := runFinished(rows[i], children), runFinished(rows[j], children)
			if fi != fj {
				return !fi
			}
			if ri, rj := rank(rows[i]), rank(rows[j]); ri != rj {
				return ri < rj
			}
			return rows[i].CreatedAt.After(rows[j].CreatedAt)
		})
	}

	out := make([]Ticket, 0, len(ts))
	depth := make(map[string]int, len(ts))
	// FINISHED SLICES ARE COUNTED, NOT LISTED. A request broken into four tasks
	// and eleven specification sections is sixteen rows, and within a few minutes
	// most of them are done — so the board fills with work that needs no
	// attention and buries the two tickets that do. A finished section is
	// interesting as a number on its task; only an unfinished one is a row.
	//
	// Only the SECTION level collapses. A finished task stays visible: it is the
	// unit of work, and "which tasks are done" is the question the board exists
	// to answer.
	// HOW MUCH OF THIS IS LEFT is the question a parent row exists to answer. A
	// request holds four tasks and a task holds three sections; the row that
	// matters is the one that says two of three are finished, not the one that
	// lists all three.
	progress := map[string]childProgress{}
	for parent, kids := range children {
		p := childProgress{total: len(kids)}
		for _, k := range kids {
			if stageOf(k).done() {
				p.done++
			}
		}
		progress[parent] = p
	}

	var walk func(rows []Ticket, d int)
	walk = func(rows []Ticket, d int) {
		sortByBest(rows)
		for _, r := range rows {
			// A finished SECTION is counted on its task rather than listed: within
			// minutes most of them are done, and the board fills with work needing
			// no attention. A finished TASK stays visible — it is the unit of work,
			// and "which tasks are done" is what the board is for.
			if d >= 2 && stageOf(r).done() {
				continue
			}
			out = append(out, r)
			depth[r.TicketID] = d
			if kids := children[r.TicketID]; len(kids) > 0 {
				walk(kids, d+1)
			}
		}
	}
	// THE ROOTS ARE WALKED SEPARATELY from everything beneath them, because they
	// are the only rows where "is this run over" is a question worth asking. Inside
	// a run the existing ordering is right: the most urgent thing first.
	sortRoots(roots)
	hidden := 0
	for _, r := range roots {
		if !showFinished && runFinished(r, children) {
			hidden++
			continue
		}
		walk([]Ticket{r}, 0)
	}
	return out, depth, progress, hidden
}

// childProgress is how many of a ticket's children are finished, and how many
// there are. Both numbers are needed: "3 done" says nothing without the total.
type childProgress struct{ done, total int }

// runtimeText renders how long a ticket took, with SECONDS kept.
//
// shortDuration rounds to whole minutes, which is right for "how stale is this"
// and useless for "which of these was slow". Measured on r77: two sections took
// 5m44s and 5m00s and both render as "5m"; two more took 1m50s and 1m12s and both
// render as "1m". The whole reason to show a runtime is to compare runtimes, and
// half of r77's tickets collided with another one under the coarser clock.
func runtimeText(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// heldByAStage reports whether a stage currently HAS this ticket, as opposed to
// the ticket sitting in a queue waiting for one.
//
// Derived from the routing table rather than listed here, so a new stage cannot
// be added without its working column being counted.
func heldByAStage(status string) bool {
	for _, st := range stages {
		if st.Working == status {
			return true
		}
	}
	return false
}

// ticketStopped reports whether a ticket has stopped moving, so its clock should
// stop with it.
//
// DONE AND BLOCKED BOTH STOP. Done is obvious. Blocked is the one that matters
// more in practice: it means the department has given up and is waiting for a
// person, so nothing is being spent on it — and a counter that keeps climbing
// says the opposite, exactly when someone is trying to work out how long the
// board has been stuck rather than busy. On r79 five tasks blocked at once and
// every one of them went on reporting time as though it were still being worked.
func ticketStopped(status string) bool {
	return status == ColDone || status == ColBlocked
}

// ticketRuntime is how long a ticket has taken, and whether that number is final.
//
// A STOPPED TICKET MUST STOP COUNTING. time.Since(CreatedAt) is the right clock
// for work in flight and the wrong one for work that is over — it keeps climbing
// for as long as the board is open, so a task that took four minutes yesterday
// reads as eighteen hours today and the number answers nothing.
//
// Measured from creation, not from the first claim, so it matches the clock the
// detail view already shows and includes the time a ticket spent waiting for a
// free slot. That wait is part of how long the work took to arrive.
// A WAITING TICKET IS NOT RUNNING, AND ITS CLOCK MUST NOT SAY IT IS.
//
// This counted from CreatedAt for anything unfinished, so a ticket queued behind
// its dependencies climbed while nothing whatever was being spent on it. On a
// board where tasks are chained — every task waits for every task before it —
// that is most of the board most of the time, and the number it showed was the
// age of the run rather than the cost of the work.
//
// Reported from the window on r85: four tickets sat at fifteen minutes each while
// one dev agent worked, and the obvious reading was that the run had got slower.
// It had not; they had not started.
//
// So there are three states, and only one of them ticks:
//
//	held by a stage   climbing, from when it entered that stage
//	waiting in a queue  nothing — the row shows no runtime at all
//	finished            frozen at what it took, end to end
//
// Nothing is shown rather than a frozen figure for a waiting ticket, because a
// number that has stopped moving looks exactly like a number nobody is updating,
// and this window is read to find out whether anything is wrong.
func ticketRuntime(t Ticket) (time.Duration, bool) {
	if t.CreatedAt.IsZero() {
		return 0, false
	}
	if ticketStopped(t.Status) && !t.UpdatedAt.IsZero() && t.UpdatedAt.After(t.CreatedAt) {
		return t.UpdatedAt.Sub(t.CreatedAt), true
	}
	if heldByAStage(t.Status) {
		// From when the stage took it, not from creation: the queue time before it
		// is not this stage's doing and adding it hides how long the stage is
		// actually taking.
		since := t.UpdatedAt
		if since.IsZero() || since.Before(t.CreatedAt) {
			since = t.CreatedAt
		}
		return time.Since(since), false
	}
	return 0, false
}

// mergedAway reports whether this ticket was folded into another rather than
// worked. Its requirements live on, in the ticket named by mergedInto.
func mergedAway(t Ticket) bool {
	for _, c := range t.Comments {
		if strings.Contains(c.Body, mergedIntoMarker) {
			return true
		}
	}
	return false
}

// mergedInto is the id the merge comment names, or "another ticket" when the
// comment cannot be read — the row still says merged rather than done, which is
// the part that matters.
func mergedInto(t Ticket) string {
	for _, c := range t.Comments {
		i := strings.Index(c.Body, mergedIntoMarker)
		if i < 0 {
			continue
		}
		// The comment carries the id in backticks: "carried into `abc12345`".
		rest := c.Body[i:]
		if j := strings.Index(rest, "`"); j >= 0 {
			rest = rest[j+1:]
			if k := strings.Index(rest, "`"); k > 0 {
				return rest[:k]
			}
		}
	}
	return "another ticket"
}

// pluralRuns renders a run count the way a person would say it.
func pluralRuns(n int) string {
	if n == 1 {
		return "1 run"
	}
	return fmt.Sprintf("%d runs", n)
}

// padTo pads s to n DISPLAY columns, and padLeft right-aligns it in them.
//
// fmt's %-*s CANNOT DO THIS, and that is the whole reason these exist. It pads to
// a count of bytes, and every cell on this board may carry colour: a styled cell
// is its text wrapped in ANSI escapes that occupy bytes and no columns, so %-*s
// pads it by however many bytes the escapes took and the column ends short.
//
// It showed up as a table that lined up on some rows and not others, because
// only SOME cells are styled — a row whose title carried a project tag was padded
// about twenty columns short while an untagged row beside it was correct, and
// every column after the title shifted with it.
//
// lipgloss.Width measures what the terminal will actually show, which is the only
// measurement a column can be built from.
func padTo(s string, n int) string {
	if w := lipgloss.Width(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

func padLeft(s string, n int) string {
	if w := lipgloss.Width(s); w < n {
		return strings.Repeat(" ", n-w) + s
	}
	return s
}

// priorityWidth is the priority column, wide enough for "critical".
const priorityWidth = 9

// runtimeWidth is the runtime column, wide enough for "12m34s" and the "+" a
// running ticket carries.
const runtimeWidth = 7

func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// stageSince is when the ticket entered the state it is in now.
//
// Read off the LAST ACTIVITY rather than a column-change timestamp, because the
// store keeps no history of transitions — but every stage in this pipeline both
// moves the ticket and writes a comment when it claims it and again when it
// reports, so the newest of those is when the current state began. UpdatedAt is
// taken too, in case something moved the ticket without commenting.
//
// It is a lower bound, and honest as one: a ticket that has sat untouched shows
// the age of the last thing that happened to it, which is exactly the number a
// person is looking for when they ask why nothing is happening.
func stageSince(t Ticket) time.Time {
	at := t.UpdatedAt
	for _, c := range t.Comments {
		if c.CreatedAt.After(at) {
			at = c.CreatedAt
		}
	}
	if at.IsZero() {
		at = t.CreatedAt
	}
	return at
}

// unmetDeps is the work a ticket is still waiting for.
//
// Only ColDone counts as met, matching dependenciesMet exactly — the dispatcher
// and this window must not disagree about why something is not moving, because
// the whole value of showing it is that it answers that question.
func unmetDeps(t Ticket) []TicketDependency {
	var out []TicketDependency
	for _, d := range t.DependsOn {
		if d.Status != ColDone {
			out = append(out, d)
		}
	}
	return out
}

// tuiRank orders the board by what each row asks of the reader: something stuck
// first, then work in flight, then work waiting, then the finished record.
func tuiRank(s stage) int {
	switch {
	case s.needsAPerson():
		return 0
	case s.done():
		return 3
	case s == stage(ColInbox) || s == stage(ColReadyForScoping) || s == stage(ColReadyForTests) ||
		s == stage(ColReadyForDev) || s == stage(ColReadyForCoverage) || s == stage(ColReadyForReview) ||
		s == stage(ColReadyForIntegration):
		return 2 // queued, waiting for a stage to take it
	default:
		return 1 // held by a stage right now
	}
}

// readActivity folds the transcript into one entry per task: what it last did,
// how many model turns it has taken, and whether it finished.
func readActivity(dir string) map[string]activity {
	out := map[string]activity{}
	if dir == "" || dir == transcriptOff {
		return out
	}
	files, _ := filepath.Glob(filepath.Join(dir, "transcripts-*.jsonl"))
	sort.Strings(files)
	// Today and yesterday is enough: anything older is not "happening now", and
	// reading the whole corpus would grow this from a glance into a scan.
	if len(files) > 2 {
		files = files[len(files)-2:]
	}
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for sc.Scan() {
			var r Record
			if json.Unmarshal(sc.Bytes(), &r) != nil || r.TaskID == "" {
				continue
			}
			a := out[r.TaskID]
			if r.Role != "" {
				a.role = r.Role
			}
			a.at = r.At
			switch r.Kind {
			case KindTurn:
				a.turns++
				a.what = "thinking"
				if r.Model != "" {
					a.what = "thinking · " + r.Model
				}
			case KindAction:
				a.what = describeAction(r)
			case KindOutcome:
				a.finished, a.lastStatus, a.what = true, r.Status, "finished: "+r.Status
			case KindStart:
				a.turns, a.finished, a.lastStatus = 0, false, ""
				a.what = "starting"
			}
			out[r.TaskID] = a
		}
		fh.Close()
	}
	return out
}

// describeAction turns a recorded action into something readable at a glance. The
// sandbox scripts are hundreds of characters of shell; what a watcher wants is
// the verb.
func describeAction(r Record) string {
	d := r.Detail
	switch {
	case strings.Contains(d, "git push"):
		return "pushing a branch"
	case strings.Contains(d, "--- lint ---"), strings.Contains(d, "HARNESS_GATE"):
		return "running the checks"
	case strings.Contains(d, "git clone"), strings.Contains(d, "git diff"):
		return "reading the repository"
	case r.Tool == "verify":
		return "verifying: " + clip(d, 48)
	case r.Tool == "pipeline":
		return "pipeline: " + clip(d, 48)
	case r.Tool == "ticket-comment":
		return "commenting on the ticket"
	case r.Tool != "":
		return r.Tool + ": " + clip(d, 40)
	}
	return clip(d, 56)
}

func (m tuiModel) Init() tea.Cmd { return tea.Batch(m.load(), tick()) }

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tickMsg:
		if m.composing {
			// Reloading under a half-typed ticket would be rude, and the store is
			// not going anywhere.
			return m, tick()
		}
		return m, tea.Batch(m.load(), tick())

	case ticketsMsg:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err, m.tickets, m.acts = nil, msg.tickets, msg.acts
		m.depth, m.progress, m.hidden = msg.depth, msg.progress, msg.hidden
		if m.selected >= len(m.tickets) {
			m.selected = max(0, len(m.tickets)-1)
		}
		return m, nil

	case noteMsg:
		m.note = string(msg)
		m.filing = false
		return m, m.load()

	case tea.KeyMsg:
		return m.onKey(msg)
	}
	return m, nil
}

func (m tuiModel) onKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.mode {
	case modeProjects:
		return m.onProjectsKey(msg)
	case modeProjectForm:
		return m.onProjectFormKey(msg)
	case modeSettings:
		return m.onSettingsKey(msg)
	}
	if m.composing {
		return m.onComposeKey(msg)
	}
	if m.asking {
		return m.onAskKey(msg)
	}
	switch msg.String() {
	case "q", "ctrl+c", "esc":
		if m.detail {
			m.detail, m.detailScroll = false, 0
			return m, nil
		}
		return m, tea.Quit
	case "j", "down":
		// IN THE DETAIL VIEW THESE SCROLL, because there is nothing else they could
		// usefully mean there: moving the selection behind a full-screen pane changes
		// something the reader cannot see.
		if m.detail {
			m.detailScroll++
			return m, nil
		}
		if m.selected < len(m.tickets)-1 {
			m.selected++
		}
	case "k", "up":
		if m.detail {
			if m.detailScroll > 0 {
				m.detailScroll--
			}
			return m, nil
		}
		if m.selected > 0 {
			m.selected--
		}
	case "pgdown", " ":
		if m.detail {
			m.detailScroll += m.detailRows()
		}
		return m, nil
	case "pgup", "b":
		if m.detail {
			m.detailScroll -= m.detailRows()
			if m.detailScroll < 0 {
				m.detailScroll = 0
			}
		}
		return m, nil
	case "g", "home":
		if m.detail {
			m.detailScroll = 0
		}
		return m, nil
	case "G", "end":
		if m.detail {
			m.detailScroll = 1 << 30 // clamped when the view renders
		}
		return m, nil
	case "enter":
		if len(m.tickets) > 0 {
			m.detail = !m.detail
			m.detailScroll = 0
		}
	case "h":
		// FINISHED RUNS ARE HISTORY, not the board. Toggled rather than filtered
		// away for good, because "what did that run do" is a real question — it is
		// just not the one this view opens on.
		m.showFinished = !m.showFinished
		m.selected, m.loading, m.note = 0, true, ""
		return m, m.load()
	case "/":
		// THE BOARD IS THE HOME PAGE, so the fastest way to hand over work belongs
		// on it rather than behind a screen change. n still opens the fuller form
		// for a request that wants two fields.
		m.asking, m.ask, m.note = true, "", ""
		return m, nil
	case "n":
		m.composing, m.field, m.title, m.body, m.note = true, 0, "", "", ""
	case "r":
		m.loading = true
		return m, m.load()
	case "S":
		// The host's own settings, as opposed to a project's.
		vals, err := ReadSettings()
		if err != nil {
			m.note = err.Error()
			return m, nil
		}
		m.setValues, m.setField, m.mode, m.note = vals, 0, modeSettings, ""
		return m, nil
	case "P":
		// Capital P manages, lower-case p switches. Managing is the rarer and more
		// consequential action, so it takes the deliberate keystroke.
		m.mode, m.note, m.projSelected = modeProjects, "", 0
		return m, nil
	case "p":
		// Cycle: all … project 1 … project N … all. The window OPENS on all, so
		// this key narrows rather than widens — the increment from the allProjects
		// sentinel (-1) lands on the first project, which is what makes one key
		// walk the whole set and come back.
		if len(m.projects) > 1 {
			m.project++
			if m.project >= len(m.projects) {
				m.project = allProjects
			}
			m.selected, m.loading = 0, true
			return m, m.load()
		}
	}
	return m, nil
}

// onAskKey drives the one-line ask.
//
// ENTER SENDS. That is the whole point of it being one line — the request goes
// with the keystroke that finishes the sentence, not with a second one that has
// to be remembered.
func (m tuiModel) onAskKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.asking, m.ask = false, ""
		return m, nil
	case tea.KeyEnter:
		line := strings.TrimSpace(m.ask)
		if line == "" {
			// Nothing typed is a cancel, not an error. Refusing an empty line with a
			// message would be a complaint about a keystroke that meant "never mind".
			m.asking, m.ask = false, ""
			return m, nil
		}
		m.asking, m.ask, m.filing = false, "", true
		m.note = ""
		return m, m.fileAsk(line)
	case tea.KeyBackspace:
		if m.ask != "" {
			r := []rune(m.ask)
			m.ask = string(r[:len(r)-1])
		}
		return m, nil
	case tea.KeySpace:
		m.ask += " "
		return m, nil
	case tea.KeyRunes:
		m.ask += string(msg.Runes)
		return m, nil
	}
	return m, nil
}

func (m tuiModel) onComposeKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.composing = false
		return m, nil
	case tea.KeyTab, tea.KeyShiftTab:
		m.field = 1 - m.field
		return m, nil
	case tea.KeyCtrlS:
		if strings.TrimSpace(m.title) == "" {
			m.note = "a ticket needs a title"
			return m, nil
		}
		// FILE IT ON THE PROJECT YOU ARE LOOKING AT. Defaulting to the host's
		// configured board would put work on whichever project happened to be
		// first in the config, which is only right by accident.
		board := m.composeBoard()
		if board == "" {
			m.note = "pick a single project with p before filing a ticket"
			return m, nil
		}
		title, body := m.title, m.body
		m.composing, m.title, m.body = false, "", ""
		api := m.api
		return m, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			// THE COLUMN IS STATED, never left to default. The board's left-most
			// column is one of the tickets service's seeded defaults, which no
			// stage takes from — a request filed there is accepted, looks filed,
			// and is never picked up by anything.
			t := Ticket{Title: title, Description: body, Priority: "medium", Status: ColInbox}
			if board != "" {
				t.BoardID = &board
			}
			created, err := api.CreateTicket(ctx, t)
			if err != nil {
				return noteMsg("could not file it: " + err.Error())
			}
			return noteMsg("filed " + shortID(created.TicketID) + " in " + ColInbox + " — the department will scope it")
		}
	case tea.KeyBackspace:
		if m.field == 0 && m.title != "" {
			m.title = m.title[:len(m.title)-1]
		} else if m.field == 1 && m.body != "" {
			m.body = m.body[:len(m.body)-1]
		}
		return m, nil
	case tea.KeyEnter:
		if m.field == 0 {
			m.field = 1
		} else {
			m.body += "\n"
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

// ── rendering ────────────────────────────────────────────────────────────────

var (
	cHead   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	cDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	cLive   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42"))
	cWait   = lipgloss.NewStyle().Foreground(lipgloss.Color("178"))
	cDone   = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	cErr    = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	cSel    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("238"))
	cKey    = lipgloss.NewStyle().Foreground(lipgloss.Color("111"))
	cRule   = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	cNote   = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	cPrompt = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("111"))
)

func (m tuiModel) View() string {
	switch m.mode {
	case modeProjects:
		return m.projectsView()
	case modeProjectForm:
		return m.projectFormView()
	case modeSettings:
		return m.settingsView()
	}
	if m.composing {
		return m.composeView()
	}
	if m.detail && len(m.tickets) > 0 {
		return m.detailView()
	}
	return m.listView()
}

func (m tuiModel) header() string {
	where := m.cfg.PlatformURL
	mode := "platform"
	if m.cfg.Standalone() {
		where, mode = m.cfg.TicketsURL, "standalone"
	}
	verify := "sandbox: " + m.cfg.Repo.TestCommand
	if m.cfg.VerifiesWithPipeline() {
		verify = "pipeline " + m.cfg.Repo.PipelineID
	}
	// Clipped to the terminal, and the store URL is what goes first: on a narrow
	// window the mode and the sizing matter more than the host, which does not
	// change while you are watching.
	// WHICH PROJECT YOU ARE LOOKING AT comes first. With several configured, a
	// list of tickets with no project on it is ambiguous in exactly the way that
	// matters — "add filtering" exists on two codebases — and the answer must not
	// require counting rows.
	scope := "all projects"
	if len(m.projects) == 1 {
		scope = m.projects[0].Name
	} else if m.project != allProjects && m.project < len(m.projects) {
		scope = fmt.Sprintf("%s (%d/%d)", m.projects[m.project].Name, m.project+1, len(m.projects))
	}
	line := fmt.Sprintf("%s · %s · poll %s · dev %d×%dGB/%d cores · verify %s · %s",
		scope, mode, m.cfg.Poll, m.cfg.DevConcurrency,
		m.cfg.DevMemoryMB/1024, m.cfg.DevCPUMillicores/1000, verify, where)
	// The gap is rendered separately: clip trims, which is right for the model
	// output it was written for and wrong for anything holding a column.
	return cHead.Render("blacksmith") + cDim.Render("  "+clip(line, max(20, m.rule()-13)))
}

// returnCeilingAlert names the tickets the reviewer bounced until it ran out of
// rounds. Empty when there are none, so the line costs nothing on a healthy
// board.
//
// This is the one thing in the window that ASKS FOR A PERSON by name rather than
// just colouring a row. A ticket at the ceiling has had ten developer runs spent
// on it and is still not passing review, which means either the change is harder
// than the pipeline can handle or the reviewer is wrong — and both of those are
// judgements no further agent round will make. It is deliberately not derived
// from the blocked column: plenty of things land there, and this alert is
// specifically the loop that had to be stopped.
func (m tuiModel) returnCeilingAlert() string {
	var hit []string
	for _, t := range m.tickets {
		for _, c := range t.Comments {
			if strings.Contains(c.Body, returnCeilingMarker) {
				hit = append(hit, shortID(t.TicketID))
				break
			}
		}
	}
	if len(hit) == 0 {
		return ""
	}
	noun := "ticket"
	if len(hit) > 1 {
		noun = "tickets"
	}
	return cErr.Render(fmt.Sprintf("! %d %s hit the %d-return review ceiling and need a person: %s",
		len(hit), noun, maxReturns, strings.Join(hit, " ")))
}

// progressLine counts the board by what a person actually wants to know: how
// much is finished, how much is moving, and how much is stuck.
//
// Finished work was previously legible only by reading every row and noticing
// which ones said "merged" — the rows were there, but "what has this thing
// actually done?" took a scan rather than a glance, which on a board of forty is
// not an answer at all.
func (m tuiModel) progressLine() string {
	var done, moving, stuck, waiting int
	for _, t := range m.tickets {
		st := stageOf(t)
		switch {
		case st.done():
			done++
		case st.needsAPerson():
			stuck++
		case m.acts[t.TicketID].live():
			moving++
		default:
			waiting++
		}
	}
	total := done + moving + stuck + waiting
	if total == 0 {
		return ""
	}
	parts := []string{cDone.Render(fmt.Sprintf("%d done", done))}
	if moving > 0 {
		parts = append(parts, cLive.Render(fmt.Sprintf("%d working", moving)))
	}
	if waiting > 0 {
		parts = append(parts, cWait.Render(fmt.Sprintf("%d waiting", waiting)))
	}
	if stuck > 0 {
		parts = append(parts, cErr.Render(fmt.Sprintf("%d need you", stuck)))
	}
	return strings.Join(parts, cDim.Render(" · ")) + cDim.Render(fmt.Sprintf("  (%d total)", total))
}

func (m tuiModel) listView() string {
	var b strings.Builder
	b.WriteString(m.header() + "\n")
	b.WriteString(cRule.Render(strings.Repeat("─", m.rule())) + "\n")

	if p := m.progressLine(); p != "" {
		b.WriteString("  " + p + "\n")
	}
	if alert := m.returnCeilingAlert(); alert != "" {
		b.WriteString(alert + "\n")
	}

	// THE COLUMN HEADINGS, on the same widths the rows use. Without them the
	// runtime column is a bare number beside a status and nothing says which is
	// which — and once the widths are shared, a heading that stops lining up is
	// the first sign a cell has started padding itself wrong again.
	if len(m.tickets) > 0 {
		b.WriteString(cDim.Render(
			"  "+padTo("id", 8)+"  "+padTo("ticket", m.titleWidth())+
				"  "+padTo("priority", priorityWidth)+
				" "+padLeft("took", runtimeWidth)+
				" state") + "\n")
	}

	if m.err != nil {
		b.WriteString(cErr.Render("cannot read the ticket store: "+m.err.Error()) + "\n")
		b.WriteString(cDim.Render("the department may still be working; this window just cannot see it") + "\n")
	}
	if len(m.tickets) == 0 && m.err == nil {
		// "no tickets" while the first read is still in flight is a lie, and it is
		// the frame every launch opens on.
		switch {
		case m.loading:
			b.WriteString(cDim.Render("\n  reading the store…\n"))
		case m.hidden > 0:
			// AN EMPTY BOARD AND A FINISHED ONE LOOK THE SAME, and saying the wrong
			// one is worse than saying nothing. Finished runs are hidden by default,
			// so a board where everything is done shows no rows at all — and the
			// message for an empty board sent the reader to file a ticket when seven
			// had just been built.
			//
			// Reported from the window after r89 finished: every ticket done, the
			// board blank, and it read as stuck.
			b.WriteString(cDim.Render(fmt.Sprintf(
				"\n  nothing running — %s finished and hidden. Press h to see %s.\n",
				pluralRuns(m.hidden), map[bool]string{true: "it", false: "them"}[m.hidden == 1])))
		default:
			b.WriteString(cDim.Render("\n  no tickets on this board yet — press n to file one\n"))
		}
	}

	for i, t := range m.tickets {
		st := stageOf(t)
		act := m.acts[t.TicketID]
		marker, state := "  ", cWait.Render(st.label())
		switch {
		case act.live() && act.role != "":
			marker = cLive.Render("▸ ")
			state = cLive.Render(act.role+" · "+act.what) +
				cDim.Render(fmt.Sprintf(" · turn %d", act.turns))
		case act.working > 0:
			// A PARENT IS NOT AN AGENT. It shows what is happening beneath it, dimly,
			// so the eye goes to the row actually being worked rather than to every
			// ancestor of it.
			marker = cDim.Render("· ")
			state = cDim.Render(act.what) +
				cDim.Render(fmt.Sprintf(" · turn %d", act.turns))
		case mergedAway(t):
			// MERGED IS NOT BUILT. The ticket-merge stage folds several tickets into
			// one and closes the rest, so four of them reach done within seconds of
			// the product manager finishing — which on a board of "done" labels reads
			// as four pieces of work completed instantly.
			//
			// Reported from the window on r90. The distinction is in the ticket's
			// comment and was nowhere on the row, which is where it is read.
			state = cDim.Render("merged into " + mergedInto(t))
		case st.done():
			// THE VERSION, WHEN THERE IS ONE. "done" is true of every finished
			// ticket and says nothing about which of them this was; the tag is what
			// a person actually wants to read off a board — it is the thing they can
			// go and look at.
			if v, _ := releaseOf(t); v != "" {
				state = cDone.Render(v)
			} else {
				state = cDone.Render(st.label())
			}
		case st.needsAPerson():
			state = cErr.Render(st.label())
		}
		// HOW LONG IT HAS BEEN THERE, on the row. A board is a set of rows that
		// all look equally alive; the age is what separates "working on it" from
		// "wedged since an hour ago", and it is the first thing a person wants when
		// they glance at this. Total age lives in the detail view — on the row, the
		// question is whether THIS stage is progressing.
		if !stageSince(t).IsZero() {
			state += cDim.Render(" · " + shortDuration(time.Since(stageSince(t))))
		}
		// WHY IT IS NOT MOVING, on the row. A queued ticket and a blocked-by-a-
		// prerequisite ticket look identical otherwise — both simply sit there —
		// and the difference is the single most asked question about this board.
		// Watching four tickets wait on one foundation reads as a broken pipeline
		// until you can see that is exactly what they are doing.
		if u := unmetDeps(t); len(u) > 0 && !act.live() && !st.done() {
			if len(u) == 1 {
				state += cDim.Render(" · waits on " + shortID(u[0].TicketID))
			} else {
				state += cDim.Render(fmt.Sprintf(" · waits on %d", len(u)))
			}
		}
		// Once a branch exists it is the only thing on the row you can act on, so
		// it is shown rather than left to be dug out of a comment.
		if br := branchFromTicket(t); br != "" && !act.live() {
			state += cDim.Render(" · " + br)
		}
		// SAY HOW MUCH IS LEFT. Both numbers, because "3 done" says nothing
		// without the total — and finished sections are hidden, so without this
		// the reader cannot tell "no sections" from "all of them done".
		//
		// THE COUNT IS RESERVED FOR, NOT APPENDED. Appending it and clipping the
		// result meant the count was the first thing cut, and these titles are
		// already past the 60-character cap: "Implement concurrent in-memory task
		// store with basic operations" is 62. The count never appeared on the rows
		// that most needed it.
		suffix := ""
		if p := m.progress[t.TicketID]; p.total > 0 {
			// A BAR AND AN ESTIMATE, not just a count. "4/19 done" says where a
			// request is; it does not say whether to wait for it. The estimate is
			// derived from the pace THIS request has managed — elapsed over
			// completed — because sections on one board range from ninety seconds to
			// four failed attempts, and a constant would be fiction.
			suffix = "  " + progressBar(p.done, p.total, time.Since(t.CreatedAt), progressBarWidth)
		}
		// The -1 is clip's ellipsis, which it ADDS to the length it was given: a
		// title clipped to n comes back n+1 columns wide, and without allowing for
		// it the row overruns the column budget by one.
		room := m.titleWidth() - len([]rune(suffix))
		if suffix != "" {
			room--
		}
		title := clip(t.Title, room) + suffix
		// The project tag appears only when the view spans more than one. On a
		// single-project window it is the same word on every row, which is noise.
		//
		// IT PREPENDS RATHER THAN REBUILDS. This branch used to compose a fresh
		// title from t.Title, which silently discarded the progress count added
		// three lines earlier — and since the window OPENS on all projects, that
		// was every row on the default view. The count was correct and never once
		// visible.
		if len(m.visibleProjects()) > 1 {
			if name := m.projectOf(t); name != "" {
				tag := "[" + name + "] "
				room := m.titleWidth() - len([]rune(suffix)) - len([]rune(tag))
				if suffix != "" {
					room--
				}
				// A tag longer than the row leaves nothing for the title. Clamped
				// here as well as in clip, because a negative room is a bug in this
				// arithmetic and clip should not be the thing that notices.
				if room < 0 {
					room = 0
				}
				title = cDim.Render(tag) + clip(t.Title, room) + suffix
			}
		}
		// THE ID IS ON THE ROW because the rows refer to each other: "waits on
		// cb583cbd" is only useful if you can find cb583cbd without opening every
		// ticket. Short form, and the same eight characters the claim records, the
		// branch names and the dependency lines all use.
		// INDENTED UNDER THE REQUEST IT CAME FROM. orderByParent has already placed
		// it directly beneath its parent; the indent is what makes that visible
		// rather than merely true, so five tickets about task stores read as one
		// request broken down instead of five unrelated jobs.
		id := cDim.Render(shortID(t.TicketID))
		// INDENTED UNDER WHAT IT CAME FROM, one level per generation: a request
		// holds broken-down pieces, and a piece holds the tickets it was split
		// into. orderByParent has already placed each row directly beneath its
		// parent; the indent is what makes the nesting visible.
		indent, width := "", m.titleWidth()
		if d := m.depth[t.TicketID]; d > 0 {
			indent = cDim.Render(strings.Repeat("  ", d-1) + "└ ")
			width = m.titleWidth() - 2*d
			if width < 12 {
				width = 12
			}
		}
		// HOW LONG IT TOOK, on the row rather than only in the detail view. The
		// question "which ticket was slow" is asked of the whole board at once, and
		// answering it by opening each ticket in turn is not answering it.
		//
		// A finished ticket shows its final time; one still running shows a "+" so
		// the two are not mistaken for each other at a glance.
		took := ""
		if d, final := ticketRuntime(t); d > 0 {
			took = runtimeText(d)
			if !final {
				took += "+"
			}
		}
		// ONE BLANK LINE BETWEEN RUNS. A request and its tasks read as one block
		// only if there is something between it and the next request; without it a
		// board carrying several runs is an undifferentiated list, and which task
		// belongs to which request has to be worked out from the indent alone.
		//
		// Before the first row it would be a stray blank line at the top.
		if m.depth[t.TicketID] == 0 && i > 0 {
			b.WriteString("\n")
		}
		// ONE COLUMN PER CELL, each padded to its own display width. Built by
		// concatenation rather than one format string because every cell may be
		// styled and fmt cannot pad around escapes — see padTo.
		row := marker + id + "  " + padTo(indent+title, width+lipgloss.Width(indent)) +
			"  " + padTo(t.Priority, priorityWidth) +
			" " + padLeft(took, runtimeWidth) +
			" " + state
		if i == m.selected {
			row = cSel.Render("▸ "+shortID(t.TicketID)+"  "+
				padTo(indent+title, width+lipgloss.Width(indent))+
				"  "+padTo(t.Priority, priorityWidth)+
				" "+padLeft(took, runtimeWidth)) + " " + state
		}
		b.WriteString(row + "\n")
	}

	b.WriteString(cRule.Render(strings.Repeat("─", m.rule())) + "\n")
	// ABOVE THE NOTE, so the reply to the last request appears under the box that
	// sent it rather than scrolling away above it.
	b.WriteString(m.askView() + "\n")
	if m.note != "" {
		b.WriteString(cNote.Render(m.note) + "\n")
	}
	keys := []string{"↑↓ move", "enter detail", "/ ask", "n new ticket", "r refresh"}
	if m.showFinished {
		keys = append(keys, "h hide finished")
	} else if m.hidden > 0 {
		keys = append(keys, fmt.Sprintf("h show %d finished", m.hidden))
	}
	if len(m.projects) > 1 {
		keys = append(keys, "p project")
	}
	keys = append(keys, "P projects", "S settings")
	b.WriteString(m.keys(append(keys, "q quit")...))
	return b.String()
}

// detailRows is how many lines of a ticket's body fit on screen, once the header,
// the two rules and the key line are taken out.
func (m tuiModel) detailRows() int {
	n := m.height - 5
	if n < 5 {
		n = 5 // a tiny terminal still gets a usable window rather than nothing
	}
	return n
}

func (m tuiModel) detailView() string {
	t := m.tickets[m.selected]
	var b strings.Builder
	b.WriteString(cHead.Render(t.Title) + "\n")
	b.WriteString(cDim.Render(fmt.Sprintf("%s · %s · %s · %s",
		shortID(t.TicketID), t.Status, t.Priority, stageOf(t).label())) + "\n")
	// Both clocks, because they answer different questions: the first is how long
	// this piece of work has existed, the second is how long it has been stuck
	// where it is now. A ticket four hours old and two minutes into review is
	// healthy; four hours old and four hours in review is not.
	if d, final := ticketRuntime(t); d == 0 && !ticketStopped(t.Status) && !t.CreatedAt.IsZero() {
		// Say it plainly rather than leaving a blank where a duration was: the
		// question this answers is "why is nothing happening to this one".
		b.WriteString(cDim.Render(fmt.Sprintf("waiting — queued %s ago, no stage has taken it yet",
			runtimeText(time.Since(t.CreatedAt)))) + "\n")
	} else if d > 0 {
		if final {
			// Stopped: one number, and the stage clock is meaningless here — it
			// would report how long the ticket has been sitting still.
			//
			// "took" and "stopped after" are different claims and the difference is
			// the point: one is how long the work needed, the other is how far it
			// got before it gave up.
			word := "took"
			if t.Status == ColBlocked {
				word = "stopped after"
			}
			b.WriteString(cDim.Render(fmt.Sprintf("%s %s", word, runtimeText(d))) + "\n")
		} else {
			b.WriteString(cDim.Render(fmt.Sprintf("in progress %s · at this stage %s",
				runtimeText(d), shortDuration(time.Since(stageSince(t))))) + "\n")
		}
	}
	b.WriteString("\n")

	// WHAT THE AGENTS ACTUALLY CHANGED, above the ticket's own text. The
	// description is what was ASKED for; this is what was done, and on a finished
	// ticket that is the more useful of the two to read first.
	if v, notes := releaseOf(t); v != "" {
		b.WriteString(cHead.Render("Released "+v) + "\n")
		if notes != "" {
			b.WriteString(clip(notes, 900) + "\n")
		}
		b.WriteString("\n")
	}

	if d := strings.TrimSpace(t.Description); d != "" {
		b.WriteString(clip(d, 600) + "\n\n")
	}
	// The dependency graph, from this ticket's point of view. Shown in full rather
	// than as a count, because the useful question is not "how many" but "which
	// one, and has it landed" — the answer decides whether you wait or go and look
	// at the prerequisite.
	if len(t.DependsOn) > 0 {
		b.WriteString(cKey.Render("  waits on") + "\n")
		for _, d := range t.DependsOn {
			mark, style := "•", cWait
			if d.Status == ColDone {
				mark, style = "✓", cDone
			} else if stage(d.Status).needsAPerson() {
				// A prerequisite that is blocked will never complete on its own, and
				// everything behind it is stranded rather than queued.
				mark, style = "!", cErr
			}
			title := d.Title
			if title == "" {
				title = "(untitled)"
			}
			b.WriteString(fmt.Sprintf("    %s %s  %s\n",
				style.Render(mark), cDim.Render(shortID(d.TicketID)),
				style.Render(clip(title, 60)+cDim.Render(" · "+d.Status))))
		}
		b.WriteString("\n")
	}
	for _, c := range t.Comments {
		body := strings.TrimSpace(c.Body)
		if strings.HasPrefix(body, claimMarker) {
			// The claim is bookkeeping, not something a person reads.
			if tok, err := parseClaim(body); err == nil {
				b.WriteString(cDim.Render("  ── claimed by "+tok.Role+" on "+tok.Host) + "\n")
			}
			continue
		}
		b.WriteString(cKey.Render("  ┌ ") + "\n")
		for _, line := range strings.Split(clip(body, 900), "\n") {
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
	if th := readThoughts(m.cfg.TranscriptDir, t.TicketID); len(th) > 0 {
		b.WriteString(cKey.Render("  reasoning") + cDim.Render(fmt.Sprintf("  (last %d turns)", len(th))) + "\n")
		for _, x := range th {
			head := x.at.Local().Format("15:04:05")
			if x.role != "" {
				head += " " + x.role
			}
			if x.tool != "" {
				head += " → " + x.tool
			}
			b.WriteString("    " + cDim.Render(head) + "\n")
			for _, line := range wrapTo(clip(x.prose, 800), m.rule()-6) {
				b.WriteString("      " + line + "\n")
			}
		}
		b.WriteString("\n")
	}

	if next := m.nextStep(t); next != "" {
		b.WriteString(next)
	}

	// WINDOWED, because the reasoning panel made this view taller than a terminal
	// for the first time and everything past the fold was simply unreachable.
	// The header and the key line stay pinned so the position indicator and the
	// way out are always visible.
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	rows := m.detailRows()
	top := m.detailScroll
	if max := len(lines) - rows; top > max {
		top = max
	}
	if top < 0 {
		top = 0
	}
	end := top + rows
	if end > len(lines) {
		end = len(lines)
	}

	var out strings.Builder
	out.WriteString(m.header() + "\n")
	out.WriteString(cRule.Render(strings.Repeat("─", m.rule())) + "\n")
	out.WriteString(strings.Join(lines[top:end], "\n") + "\n")
	out.WriteString(cRule.Render(strings.Repeat("─", m.rule())) + "\n")

	keys := []string{"esc back", "q quit"}
	if len(lines) > rows {
		keys = append([]string{"↑↓ scroll", "pgup/pgdn page", "g/G ends"}, keys...)
		out.WriteString(cDim.Render(fmt.Sprintf("  %d-%d of %d lines", top+1, end, len(lines))) + "\n")
	}
	out.WriteString(m.keys(keys...))
	return out.String()
}

// wrapTo breaks text to a width, so a paragraph of reasoning does not run off
// the side of the pane.
func wrapTo(s string, width int) []string {
	if width < 20 {
		width = 20
	}
	var out []string
	for _, para := range strings.Split(s, "\n") {
		line := ""
		for _, w := range strings.Fields(para) {
			if line == "" {
				line = w
				continue
			}
			if len(line)+1+len(w) > width {
				out = append(out, line)
				line = w
				continue
			}
			line += " " + w
		}
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// nextStep says what the PERSON does now.
//
// "reviewed — yours to merge" is a true statement that leaves you nowhere: the
// work is on a branch, on a remote the window knows and you would otherwise have
// to dig out of a comment, and the department will not touch it again. So the
// state that hands the ticket back is the one state that spells out the command.
func (m tuiModel) nextStep(t Ticket) string {
	branch := branchFromTicket(t)
	if branch == "" {
		return ""
	}
	st := stageOf(t)
	if !st.needsAPerson() {
		return ""
	}
	repo := m.cfg.Repo.URL
	var b strings.Builder
	// The two reasons a person is needed read very differently, so they are named
	// separately: one is a disagreement to settle, the other is a bug to fix.
	why := " — the department is out of options on this one"
	if st == stage(ColConflicted) {
		why = " — this one cannot be merged mechanically"
	}
	b.WriteString("\n" + cErr.Render("  YOURS NOW") + cDim.Render(why) + "\n")
	b.WriteString(cDim.Render("  branch  ") + branch + "\n")
	if repo != "" {
		b.WriteString(cDim.Render("  fetch   ") + fmt.Sprintf("git fetch %s %s && git switch -c %s FETCH_HEAD", repo, branch, branch) + "\n")
	}
	return b.String()
}

func (m tuiModel) composeView() string {
	var b strings.Builder
	b.WriteString(m.header() + "\n")
	b.WriteString(cRule.Render(strings.Repeat("─", m.rule())) + "\n")
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
	b.WriteString(cDim.Render("  The product manager triages it first, so a sentence of context is worth more than a precise spec.") + "\n")
	b.WriteString(cRule.Render(strings.Repeat("─", m.rule())) + "\n")
	b.WriteString(m.keys("tab switch field", "ctrl+s file it", "esc cancel"))
	return b.String()
}

func (m tuiModel) keys(pairs ...string) string {
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		word, rest, _ := strings.Cut(p, " ")
		out = append(out, cKey.Render(word)+cDim.Render(" "+rest))
	}
	return "  " + strings.Join(out, cDim.Render("  ·  "))
}

func (m tuiModel) rule() int {
	if m.width > 20 {
		return m.width - 1
	}
	return 78
}

func (m tuiModel) titleWidth() int {
	// 42 for the marker, priority and state columns, plus 10 for the short id and
	// its separator, plus the runtime column and the space before it. A title is
	// the most compressible thing on the row; an id that wrapped would be unusable
	// for the cross-referencing it is there to serve, and a runtime that wrapped
	// would be worse than absent.
	w := m.rule() - 52 - runtimeWidth - 1
	if w < 20 {
		return 20
	}
	if w > 60 {
		return 60
	}
	return w
}

// runTUI opens the window. It builds the same client main() does, so it reads
// whatever store this host is actually configured against rather than a second
// opinion about where the work lives.
func runTUI(ctx context.Context, cfg Config) error {
	var api *CodeArmory
	var err error
	if cfg.Standalone() && cfg.PlatformURL == "" {
		api = NewStandaloneCodeArmory()
	} else {
		if api, err = NewCodeArmory(cfg.PlatformURL, cfg.PlatformToken); err != nil {
			return err
		}
	}
	if cfg.ForgeURL != "" {
		token := cfg.ForgeToken
		if cfg.ForgeEmail != "" && cfg.ForgePassword != "" {
			token = "renewing"
		}
		if err := api.UseLocalForge(cfg.ForgeURL, token); err != nil {
			return err
		}
		if cfg.ForgeEmail != "" && cfg.ForgePassword != "" {
			cred, err := NewCredential("", cfg.SandboxLoginURL(), cfg.ForgeEmail, cfg.ForgePassword, nil)
			if err != nil {
				return err
			}
			api.UseRenewingForgeCredential(cred)
		}
	}
	if err := api.UseLocalTickets(cfg.TicketsURL); err != nil {
		return err
	}

	// The gateway is for the ask line and nothing else. Built from the same config
	// the service uses, and simply absent when no class is configured — a window
	// with no model still files what was typed.
	var gw *Gateway
	if len(cfg.Classes) > 0 {
		gw = NewGateway(cfg)
	}

	// A BAD PROJECTS FILE MUST NOT CLOSE THE WINDOW. This is the tool you open to
	// find out why something is wrong, and refusing to start over the very file
	// you would be coming here to fix is the worst possible moment to be strict.
	// The service is strict about it; this reports and carries on with whatever
	// the environment describes.
	projects, perr := LoadProjects(cfg)
	if perr != nil {
		projects = []Project{fromEnv(cfg)}
	}
	// OPENS ON EVERY PROJECT. The window is how a person checks on a department
	// that runs several at once, and the first question is "is anything wrong
	// anywhere" rather than "how is this one project doing" — opening on a single
	// project answers the second and silently hides the rest, so work stuck on
	// another board is invisible until you think to go looking for it. Press p to
	// narrow to one.
	m := tuiModel{
		cfg: cfg, api: api, gw: gw, acts: map[string]activity{}, loading: true,
		projects: projects, project: allProjects,
	}
	if perr != nil {
		m.err = perr
	}
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	_, err = p.Run()
	return err
}

// composeBoard is the board a new ticket goes to: the selected project's.
//
// Empty in the combined view, deliberately. "All projects" is a reading mode and
// there is no sensible answer to which repository a ticket filed from it belongs
// to — guessing would put work on the wrong codebase, which is worse than asking
// the person to choose.
func (m tuiModel) composeBoard() string {
	switch {
	case len(m.projects) == 1:
		return m.projects[0].BoardID
	case m.project != allProjects && m.project < len(m.projects):
		return m.projects[m.project].BoardID
	default:
		return ""
	}
}

// thought is one turn's reasoning, as a person would read it.
type thought struct {
	at    time.Time
	role  string
	tool  string
	prose string
}

// maxThoughtsShown bounds the reasoning panel in the detail view.
//
// Raised once the view could scroll: the cap existed to stop the reasoning
// pushing the comments and dependencies off a fixed screen, and windowing removed
// that constraint. Twenty turns is enough to see a loop develop and break —
// the same diagnosis three times running is the signature that matters — while
// the whole history remains in the transcript file.
const maxThoughtsShown = 20

// readThoughts returns what the model SAID on a ticket, newest last.
//
// THE PROSE IS THE USEFUL SIGNAL AND IT WAS ONLY IN THE FILE. Watching a stuck
// ticket through counters says "41 refusals" and leaves the cause to guesswork;
// the reasoning beside it says "the routing uses HasPrefix on a path missing its
// leading slash", which is the actual fault. Every diagnosis worth having today
// came from reading these, and reading them meant leaving the tool and grepping
// a JSONL file by hand.
func readThoughts(dir, ticketID string) []thought {
	if dir == "" || dir == transcriptOff || ticketID == "" {
		return nil
	}
	files, _ := filepath.Glob(filepath.Join(dir, "transcripts-*.jsonl"))
	sort.Strings(files)
	if len(files) > 2 {
		files = files[len(files)-2:]
	}
	var out []thought
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for sc.Scan() {
			var r Record
			if json.Unmarshal(sc.Bytes(), &r) != nil {
				continue
			}
			if r.Kind != KindTurn || r.TaskID != ticketID || r.Completion == "" {
				continue
			}
			// proseOf strips the tool call and the fenced wrappers, which is exactly
			// the difference between what the model MEANT and what it emitted.
			p := proseOf(r.Completion)
			if strings.TrimSpace(p) == "" {
				p = "(no reasoning — tool call only)"
			}
			out = append(out, thought{at: r.At, role: r.Role, tool: toolOf(r.Completion), prose: p})
		}
		fh.Close()
	}
	if len(out) > maxThoughtsShown {
		out = out[len(out)-maxThoughtsShown:]
	}
	return out
}

// toolOf names the tool a completion called, for the line above its reasoning.
func toolOf(completion string) string {
	const key = `"tool":"`
	i := strings.Index(completion, key)
	if i < 0 {
		return ""
	}
	rest := completion[i+len(key):]
	if j := strings.Index(rest, `"`); j > 0 {
		return rest[:j]
	}
	return ""
}

// rollUp gives a ticket the activity of its descendants when it has none of its
// own.
//
// A PARENT NEVER WORKS, so it never has turns. The root of a decomposed request
// sits in "tracking" while thirteen sections beneath it are claimed, edited and
// merged, and its row read "turn 0" throughout — which looks exactly like a hung
// pipeline and was reported as one twice. The work is really happening; it is
// happening one or two levels down, filed under the ticket that is doing it.
//
// Own activity wins when there is any, because a ticket that IS working should
// show its own state rather than a summary of its children.
func rollUp(tickets []Ticket, acts map[string]activity) map[string]activity {
	kids := map[string][]string{}
	for _, t := range tickets {
		if t.ParentID != nil && *t.ParentID != "" {
			kids[*t.ParentID] = append(kids[*t.ParentID], t.TicketID)
		}
	}
	out := make(map[string]activity, len(acts))
	for k, v := range acts {
		out[k] = v
	}
	var gather func(id string) activity
	seen := map[string]bool{}
	gather = func(id string) activity {
		if seen[id] {
			return activity{} // a cycle cannot happen, but a wrong parent_id could
		}
		seen[id] = true
		self := out[id]
		own := self.live()
		for _, c := range kids[id] {
			ca := gather(c)
			// TURNS ROLL UP; THE STATE DOES NOT. A parent never works, so summing
			// its children's effort is the useful number — but claiming their role
			// made every ancestor look busy. It reports how many are busy instead.
			self.turns += ca.turns
			self.working += ca.working
			// ONLY A TICKET WITH AN AGENT ON IT COUNTS. A child that is itself only
			// reporting "n working below" has no agent of its own, and counting it
			// too inflated every level: two busy sections read as three at the root.
			if ca.live() && ca.role != "" {
				self.working++
			}
			// The newest descendant only sets the CLOCK, so an ancestor's age
			// reflects the work beneath it rather than when it was last touched.
			if ca.at.After(self.at) {
				self.at = ca.at
			}
		}
		// A ticket doing its own work keeps describing that; one whose children are
		// working says so plainly.
		if !own && self.working > 0 {
			self.role, self.finished = "", false
			self.what = fmt.Sprintf("%d working below", self.working)
			if self.working == 1 {
				self.what = "1 working below"
			}
		}
		out[id] = self
		return self
	}
	for _, t := range tickets {
		if t.ParentID == nil || *t.ParentID == "" {
			gather(t.TicketID)
		}
	}
	return out
}

// progressBar renders how far a request has got, and how long the rest is likely
// to take.
//
// THE ESTIMATE IS FROM THIS RUN, not from a constant. Sections on one board vary
// enormously — some merge in ninety seconds, some fail four times first — so the
// only honest predictor is the pace this particular request has managed so far:
// elapsed divided by completed, times what is left. It is shown only once enough
// has finished to mean anything, because an extrapolation from one sample is a
// guess wearing a number's clothes.
func progressBar(done, total int, elapsed time.Duration, width int) string {
	if total <= 0 {
		return ""
	}
	if width < 8 {
		width = 8
	}
	filled := done * width / total
	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
	out := fmt.Sprintf("%s %d/%d", bar, done, total)

	if done >= minSamplesForETA && done < total && elapsed > 0 {
		per := elapsed / time.Duration(done)
		out += " · ~" + shortDuration(per*time.Duration(total-done)) + " left"
	}
	return out
}

// minSamplesForETA is how many finished children an estimate needs.
//
// Two, because one tells you nothing about variance and this board's sections
// have ranged from ninety seconds to four failed attempts. A number that swings
// by an order of magnitude between refreshes is worse than no number.
const minSamplesForETA = 2

// progressBarWidth keeps the bar narrow enough to leave the title readable.
//
// Ten cells on a row that already carries a title, a stage, an age and a reason.
// The bar is there to be glanced at, not measured off.
const progressBarWidth = 10

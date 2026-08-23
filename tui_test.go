package main

import (
	"encoding/json"
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The stage is derived from the same markers the dispatchers select on, so the
// window shows what the department will actually do next rather than a second
// opinion about it.
// orderAll is orderByParent with finished runs listed. These tests are about
// ORDERING and nesting; whether a finished run is shown at all is a view choice
// made with the h key, and hiding them here would only make the fixtures harder
// to read.
func orderAll(ts []Ticket) ([]Ticket, map[string]int, map[string]childProgress, int) {
	return orderByParent(ts, true)
}

func TestStageMatchesTheDispatcherPredicates(t *testing.T) {
	// The stage IS the column. What this asserts is that the window and the
	// dispatchers cannot disagree — the labels come from the column, and the
	// "who is coming next" is read off the same routing table dispatch uses.
	for _, c := range []struct {
		name string
		tk   Ticket
		want stage
	}{
		{"fresh", Ticket{Status: ColInbox}, stage(ColInbox)},
		{"scoped", Ticket{Status: ColReadyForDev}, stage(ColReadyForDev)},
		{"being written", Ticket{Status: ColInDev}, stage(ColInDev)},
		{"reviewed", Ticket{Status: ColReadyForIntegration}, stage(ColReadyForIntegration)},
		{"merged", Ticket{Status: ColDone}, stage(ColDone)},
		{"conflicted", Ticket{Status: ColConflicted}, stage(ColConflicted)},
	} {
		if got := stageOf(c.tk); got != c.want {
			t.Errorf("%s: stage = %v, want %v", c.name, got, c.want)
		}
	}

	// Every column a stage takes from must name that stage, or a row says where a
	// ticket is without saying whether anything will happen next.
	for role, st := range stages {
		if got := stage(st.Ready).nextRole(); got != role {
			t.Errorf("column %q names %q as next, want %q", st.Ready, got, role)
		}
	}

	// Every column on the board must read as something. An unlabelled column is a
	// blank cell in the one view meant to answer "what is happening".
	for _, c := range workflowColumns {
		if stage(c.Value).label() == "" {
			t.Errorf("column %q has no label", c.Value)
		}
	}

	// The two columns that mean a person is needed must be distinguishable from
	// work in motion, since they are the only rows worth acting on.
	if !stage(ColBlocked).needsAPerson() || !stage(ColConflicted).needsAPerson() {
		t.Error("a column needing a person does not say so; it would render as work in progress")
	}
	if stage(ColInDev).needsAPerson() || stage(ColDone).needsAPerson() {
		t.Error("work in motion is flagged as needing a person")
	}

	// Done is the end of this department's involvement.
	if stage(ColDone).nextRole() != "" {
		t.Error("a merged ticket claims an agent is still coming for it")
	}
}

// Activity is read from the transcript, which is the only record of what an agent
// is doing mid-task. A run with no outcome is in flight; one with an outcome is
// not, and showing the second as live would make a finished department look busy.
func TestActivityReadsTheTranscript(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcripts-2026-08-11.jsonl")
	lines := []string{
		`{"transcript_id":"a","kind":"start","task_id":"t1","role":"dev-agent","at":"` + time.Now().Format(time.RFC3339) + `"}`,
		`{"transcript_id":"a","kind":"turn","task_id":"t1","role":"dev-agent","model":"qwen3-coder","at":"` + time.Now().Format(time.RFC3339) + `"}`,
		`{"transcript_id":"a","kind":"turn","task_id":"t1","role":"dev-agent","model":"qwen3-coder","at":"` + time.Now().Format(time.RFC3339) + `"}`,
		`{"transcript_id":"b","kind":"outcome","task_id":"t2","role":"pm-agent","status":"success","at":"` + time.Now().Format(time.RFC3339) + `"}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	acts := readActivity(dir)
	live := acts["t1"]
	if live.turns != 2 {
		t.Errorf("turns = %d, want 2; the iteration count is what shows a cap approaching", live.turns)
	}
	if !live.live() {
		t.Error("a task with no outcome is not shown as live")
	}
	if live.role != "dev-agent" {
		t.Errorf("role = %q, want dev-agent", live.role)
	}
	done := acts["t2"]
	if done.live() {
		t.Error("a finished task is shown as live; the department would always look busy")
	}
	// Capture off must not be an error, just an empty view.
	if got := readActivity(transcriptOff); len(got) != 0 {
		t.Errorf("readActivity(off) = %v, want empty", got)
	}
}

// Sandbox scripts are hundreds of characters of shell. A watcher wants the verb.
func TestActionsAreDescribedNotDumped(t *testing.T) {
	cases := map[string]string{
		"set -e\ngit clone --filter=blob:none":         "reading the repository",
		"git add -A\ngit push --force origin agent/t1": "pushing a branch",
		"echo '--- lint ---'\ngo vet ./...":            "running the checks",
	}
	for detail, want := range cases {
		if got := describeAction(Record{Kind: KindAction, Detail: detail}); got != want {
			t.Errorf("describeAction(%q) = %q, want %q", detail, got, want)
		}
	}
	long := describeAction(Record{Kind: KindAction, Tool: "verify", Detail: strings.Repeat("x", 300)})
	if len(long) > 80 {
		t.Errorf("a described action is %d chars; it would wrap the row", len(long))
	}
}

// The person is only pulled in when the machine could not finish the job. A
// reviewed ticket is the integrator's, not yours; a CONFLICT is yours, and that
// is the state that must spell out the command.
func TestOnlyAConflictAsksThePersonToAct(t *testing.T) {
	m := tuiModel{cfg: Config{Repo: RepoConfig{URL: "git://git-local:9418/demo.git"}}}
	base := []Comment{
		{Body: "**Triage** (automated)\n\nx"},
		{Body: branchMarker + "\n\n- **Branch:** `agent/t1`\n"},
		{Body: reviewMarker + " (automated)\n\nclean"},
	}

	// Waiting to merge: the integrator has it, so do not summon a person.
	if got := m.nextStep(Ticket{Status: ColReadyForIntegration, Comments: base}); got != "" {
		t.Errorf("a reviewed ticket asks the person to act: %q", got)
	}
	// Merged: finished, nothing to do here either.
	if got := m.nextStep(Ticket{Status: ColDone, Comments: base}); got != "" {
		t.Errorf("a merged ticket asks the person to act: %q", got)
	}
	// Conflicted: theirs, and it must name the branch and a usable command.
	next := m.nextStep(Ticket{Status: ColConflicted, Comments: base})
	if !strings.Contains(next, "cannot be merged mechanically") {
		t.Errorf("a conflict does not say why it is theirs:\n%s", next)
	}
	// And blocked reads differently: not a merge to settle, a bug to fix.
	blocked := m.nextStep(Ticket{Status: ColBlocked, Comments: base})
	if blocked == "" || strings.Contains(blocked, "cannot be merged mechanically") {
		t.Errorf("blocked work is described as a merge conflict:\n%s", blocked)
	}
	if !strings.Contains(next, "agent/t1") {
		t.Errorf("the conflict does not name the branch:\n%s", next)
	}
	if !strings.Contains(next, "git fetch git://git-local:9418/demo.git") {
		t.Errorf("the conflict gives no command to act on:\n%s", next)
	}
}

// A request filed from the window must land in the column the department
// actually reads. Left to default it takes the board's left-most column, which
// is a seeded default no stage takes from: the ticket is accepted, looks filed,
// and is picked up by nothing.
func TestComposedTicketLandsInTheInbox(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)

	m := tuiModel{
		cfg: Config{BoardID: "b1"}, api: api, composing: true, title: "add rate limiting",
		projects: []Project{{Name: "demo", BoardID: "b1"}},
	}
	_, cmd := m.onComposeKey(tea.KeyMsg{Type: tea.KeyCtrlS})
	if cmd == nil {
		t.Fatal("submitting composed nothing")
	}
	if msg := cmd(); msg == nil {
		t.Fatal("filing returned no result")
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.tickets) != 1 {
		t.Fatalf("%d tickets created, want 1", len(f.tickets))
	}
	for _, tk := range f.tickets {
		if tk.Status != ColInbox {
			t.Errorf("filed into %q, want %q — no stage takes from anywhere else", tk.Status, ColInbox)
		}
		if tk.Title != "add rate limiting" {
			t.Errorf("title = %q", tk.Title)
		}
	}
}

// The window has to ASK FOR A PERSON when the reviewer ran out of rounds.
// Colouring the row is not enough: a ticket at the ceiling has had ten developer
// runs spent on it and no further agent round will happen, so it is the one
// state that needs naming rather than watching.
func TestReturnCeilingRaisesAnAlert(t *testing.T) {
	quiet := tuiModel{tickets: []Ticket{
		{TicketID: "aaaaaaaa-1111", Status: ColReadyForDev},
		{TicketID: "bbbbbbbb-2222", Status: ColBlocked, Comments: []Comment{
			{Body: "**Developer agent stopped.**"},
		}},
	}}
	if got := quiet.returnCeilingAlert(); got != "" {
		t.Errorf("alert on a board with no ceiling hit: %q", got)
	}

	loud := tuiModel{tickets: []Ticket{
		{TicketID: "aaaaaaaa-1111", Status: ColReadyForDev},
		{TicketID: "cccccccc-3333", Status: ColBlocked, Comments: []Comment{
			{Body: returnCeilingMarker + "\n\nten rounds"},
		}},
	}}
	got := loud.returnCeilingAlert()
	if got == "" {
		t.Fatal("no alert for a ticket that hit the return ceiling; nothing tells a person to look")
	}
	if !strings.Contains(got, shortID("cccccccc-3333")) {
		t.Errorf("the alert does not name the ticket: %q", got)
	}
	// A blocked ticket that merely failed must not be swept in: this alert is
	// specifically the loop that had to be stopped.
	if strings.Contains(got, shortID("aaaaaaaa-1111")) {
		t.Errorf("the alert names a ticket that never hit the ceiling: %q", got)
	}
}

// A ticket must be filed on the project you are LOOKING AT. Defaulting to the
// host's configured board would put work on whichever project happened to be
// first in the config, which is only right by accident.
func TestComposeFilesAgainstTheSelectedProject(t *testing.T) {
	projects := []Project{{Name: "api", BoardID: "b-api"}, {Name: "web", BoardID: "b-web"}}

	m := tuiModel{cfg: Config{BoardID: "ignored"}, projects: projects, project: 1}
	if got := m.composeBoard(); got != "b-web" {
		t.Errorf("composeBoard = %q, want the selected project's board", got)
	}

	// A single project needs no selection.
	one := tuiModel{cfg: Config{BoardID: "ignored"}, projects: projects[:1]}
	if got := one.composeBoard(); got != "b-api" {
		t.Errorf("composeBoard = %q with one project, want its board", got)
	}

	// The combined view has NO answer, and guessing would file work against the
	// wrong codebase — worse than asking the person to choose.
	all := tuiModel{cfg: Config{BoardID: "ignored"}, projects: projects, project: allProjects}
	if got := all.composeBoard(); got != "" {
		t.Errorf("composeBoard = %q in the combined view, want it to refuse", got)
	}
}

// Cycling runs project 1 … N … all, so the default view is a single project and
// the merged list is the occasional question rather than the resting state.
func TestProjectCycleEndsOnTheCombinedView(t *testing.T) {
	m := tuiModel{projects: []Project{{Name: "a", BoardID: "b1"}, {Name: "b", BoardID: "b2"}}}
	seen := []int{m.project}
	for range 3 {
		next, _ := m.onKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
		m = next.(tuiModel)
		seen = append(seen, m.project)
	}
	want := []int{0, 1, allProjects, 0}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("cycle = %v, want %v", seen, want)
		}
	}

	// With one project there is nothing to cycle and the key must do nothing.
	solo := tuiModel{projects: []Project{{Name: "only", BoardID: "b1"}}}
	next, _ := solo.onKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	if next.(tuiModel).project != 0 {
		t.Error("cycling with a single project moved off it")
	}
}

// The combined view spans every project; a selected one spans only itself.
func TestVisibleProjectsFollowsTheSelection(t *testing.T) {
	ps := []Project{{Name: "a", BoardID: "b1"}, {Name: "b", BoardID: "b2"}}
	if got := (tuiModel{projects: ps, project: allProjects}).visibleProjects(); len(got) != 2 {
		t.Errorf("the combined view spans %d projects, want all", len(got))
	}
	got := (tuiModel{projects: ps, project: 1}).visibleProjects()
	if len(got) != 1 || got[0].Name != "b" {
		t.Errorf("a selected project spans %+v, want only itself", got)
	}
}

// "What has this thing actually done?" must be answerable at a glance. Finished
// tickets were rendered before this, but scattered among unfinished ones in
// creation order, so on a board of any size the answer took a scan.
func TestProgressLineCountsWhatMatters(t *testing.T) {
	m := tuiModel{
		tickets: []Ticket{
			{TicketID: "a", Status: ColDone},
			{TicketID: "b", Status: ColDone},
			{TicketID: "c", Status: ColInDev},
			{TicketID: "d", Status: ColReadyForDev},
			{TicketID: "e", Status: ColBlocked},
		},
		acts: map[string]activity{},
	}
	got := m.progressLine()
	for _, want := range []string{"2 done", "1 need you"} {
		if !strings.Contains(got, want) {
			t.Errorf("progress line %q is missing %q", got, want)
		}
	}
	if m2 := (tuiModel{}); m2.progressLine() != "" {
		t.Errorf("an empty board reports progress: %q", m2.progressLine())
	}
}

// Stuck first, finished last: the top of the list is what will not move without
// you, and the bottom is the record of what is already done.
func TestBoardOrdersStuckFirstAndDoneLast(t *testing.T) {
	ranks := map[string]int{
		ColBlocked:             tuiRank(stage(ColBlocked)),
		ColInDev:               tuiRank(stage(ColInDev)),
		ColReadyForDev:         tuiRank(stage(ColReadyForDev)),
		ColDone:                tuiRank(stage(ColDone)),
		ColConflicted:          tuiRank(stage(ColConflicted)),
		ColReadyForIntegration: tuiRank(stage(ColReadyForIntegration)),
	}
	if !(ranks[ColBlocked] < ranks[ColInDev]) {
		t.Error("blocked work does not sort above work in flight")
	}
	if !(ranks[ColInDev] < ranks[ColReadyForDev]) {
		t.Error("work being done does not sort above work merely queued")
	}
	if !(ranks[ColReadyForDev] < ranks[ColDone]) {
		t.Error("finished work does not sort last")
	}
	if ranks[ColConflicted] != ranks[ColBlocked] {
		t.Error("a conflict needs a person just as much as a block does")
	}
	if ranks[ColReadyForIntegration] != ranks[ColReadyForDev] {
		t.Error("every queue column should rank the same")
	}
}

// "Why is this not moving?" is the most asked question about this board, and a
// queued ticket looked identical to one stranded behind a prerequisite. The
// window must agree with dependenciesMet about what counts as met, or it answers
// the question wrongly — which is worse than not answering it.
func TestUnmetDependenciesMatchTheDispatchersRule(t *testing.T) {
	tk := Ticket{DependsOn: []TicketDependency{
		{TicketID: "aaaaaaaa", Status: ColDone},
		{TicketID: "bbbbbbbb", Status: ColInDev},
		{TicketID: "cccccccc", Status: ColBlocked},
		{TicketID: "dddddddd", Status: ColReadyForDev},
	}}
	got := unmetDeps(tk)
	if len(got) != 3 {
		t.Fatalf("unmet = %d, want 3 (only done counts as met): %+v", len(got), got)
	}
	// Exactly the dispatcher's rule: anything that is not done is still waited on.
	if dependenciesMet(tk) {
		t.Error("the dispatcher considers these met while the window does not; they must agree")
	}
	if u := unmetDeps(Ticket{DependsOn: []TicketDependency{{TicketID: "a", Status: ColDone}}}); len(u) != 0 {
		t.Errorf("a finished prerequisite is still reported as waited on: %+v", u)
	}
	if !dependenciesMet(Ticket{DependsOn: []TicketDependency{{TicketID: "a", Status: ColDone}}}) {
		t.Error("dependenciesMet disagrees that a done prerequisite is met")
	}
}

// The window opens on every project. Scoped to one, a ticket stuck on another
// board is invisible until you think to go looking for it — and "is anything
// wrong" is the question this window exists to answer.
func TestWindowOpensOnAllProjects(t *testing.T) {
	m := tuiModel{
		projects: []Project{{Name: "a", BoardID: "b1"}, {Name: "b", BoardID: "b2"}},
		project:  allProjects,
	}
	if len(m.visibleProjects()) != 2 {
		t.Fatalf("visible = %d projects, want both", len(m.visibleProjects()))
	}
	// p narrows from all to the first project, then walks the set and returns.
	m.project++
	if got := m.visibleProjects(); len(got) != 1 || got[0].Name != "a" {
		t.Errorf("after p the view is %+v, want just the first project", got)
	}
}

// A board is a set of rows that all look equally alive. The age is what
// separates "working on it" from "wedged an hour ago", and it is the first thing
// a person wants when they glance at the window.
func TestShortDurationReadsAtEveryScale(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{90 * time.Second, "1m"},
		{45 * time.Minute, "45m"},
		{3 * time.Hour, "3h"},
		{72 * time.Hour, "3d"},
	} {
		if got := shortDuration(c.d); got != c.want {
			t.Errorf("shortDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// The store keeps no transition history, so "how long at this stage" is read off
// the last thing that happened to the ticket — every stage both moves it and
// comments. It must be a LOWER BOUND that never reads older than the truth.
func TestStageSinceTracksTheMostRecentActivity(t *testing.T) {
	created := time.Now().Add(-4 * time.Hour)
	updated := time.Now().Add(-90 * time.Minute)
	newest := time.Now().Add(-5 * time.Minute)

	tk := Ticket{
		CreatedAt: created,
		UpdatedAt: updated,
		Comments: []Comment{
			{Body: "old", CreatedAt: time.Now().Add(-3 * time.Hour)},
			{Body: "newest", CreatedAt: newest},
		},
	}
	if got := stageSince(tk); !got.Equal(newest) {
		t.Errorf("stageSince = %v, want the newest comment %v", got, newest)
	}
	// No comments: fall back to the last update.
	if got := stageSince(Ticket{CreatedAt: created, UpdatedAt: updated}); !got.Equal(updated) {
		t.Errorf("stageSince with no comments = %v, want UpdatedAt %v", got, updated)
	}
	// Nothing at all: creation is the only honest answer, never the zero time.
	if got := stageSince(Ticket{CreatedAt: created}); !got.Equal(created) {
		t.Errorf("stageSince with nothing = %v, want CreatedAt", got)
	}
	// And the two clocks differ, which is the whole point of showing both.
	if time.Since(tk.CreatedAt) <= time.Since(stageSince(tk)) {
		t.Error("total age is not greater than stage age; the two would be redundant")
	}
}

// A request and the tickets it produced belong together. Chronological order
// separates them — the children are created after the parent and then move at
// different speeds — so by the time a board is big enough to need the context,
// it has lost it.
func TestChildrenAreGroupedUnderTheirParent(t *testing.T) {
	now := time.Now()
	parent := Ticket{TicketID: "parent00", Status: ColTracking, CreatedAt: now.Add(-time.Hour)}
	pid := parent.TicketID
	kids := []Ticket{
		{TicketID: "kid-done", Status: ColDone, ParentID: &pid, CreatedAt: now.Add(-30 * time.Minute)},
		{TicketID: "kid-block", Status: ColBlocked, ParentID: &pid, CreatedAt: now.Add(-20 * time.Minute)},
		{TicketID: "kid-dev", Status: ColInDev, ParentID: &pid, CreatedAt: now.Add(-10 * time.Minute)},
	}
	other := Ticket{TicketID: "unrelated", Status: ColInDev, CreatedAt: now}

	got, depth, _, _ := orderAll([]Ticket{other, kids[0], parent, kids[1], kids[2]})

	// Find the parent, then assert its children follow it immediately.
	var at int
	for i, x := range got {
		if x.TicketID == pid {
			at = i
		}
	}
	block := got[at+1 : at+4]
	for _, x := range block {
		if x.ParentID == nil || *x.ParentID != pid {
			t.Fatalf("a non-child appeared inside the parent's block: %+v", block)
		}
	}
	// Ranking still applies WITHIN the group: blocked first, done last.
	if block[0].TicketID != "kid-block" {
		t.Errorf("children not ranked: first is %q, want the blocked one", block[0].TicketID)
	}
	if block[2].TicketID != "kid-done" {
		t.Errorf("children not ranked: last is %q, want the done one", block[2].TicketID)
	}
	// An orphan is a root rather than being dropped.
	if len(got) != 5 {
		t.Errorf("orderByParent returned %d rows, want every ticket kept", len(got))
	}
	// Depth is what the view indents by: a root is 0, its children 1.
	if depth[pid] != 0 {
		t.Errorf("depth[parent] = %d, want 0", depth[pid])
	}
	for _, k := range kids {
		if depth[k.TicketID] != 1 {
			t.Errorf("depth[%s] = %d, want 1", k.TicketID, depth[k.TicketID])
		}
	}

	// A CONTAINER IS RANKED BY ITS BEST CHILD. The parent sits in tracking, which
	// is not a stage working on anything — ranking it as one floated the request
	// and every broken-down piece above the tickets actually being built.
	if got[0].TicketID != pid {
		t.Errorf("first row is %q; the container holding the BLOCKED ticket should sort first", got[0].TicketID)
	}
}

// A BROKEN-DOWN PIECE HOLDS ITS OWN TICKETS, so the nesting has to go deeper
// than one level: request, then the piece it was split into, then that piece's
// tickets.
func TestNestingGoesDeeperThanOneLevel(t *testing.T) {
	now := time.Now()
	req := Ticket{TicketID: "request0", Status: ColTracking, CreatedAt: now.Add(-time.Hour)}
	rid := req.TicketID
	grp := Ticket{TicketID: "group000", Status: ColTracking, ParentID: &rid, CreatedAt: now.Add(-40 * time.Minute)}
	gid := grp.TicketID
	leaf := Ticket{TicketID: "leaf0000", Status: ColInDev, ParentID: &gid, CreatedAt: now.Add(-10 * time.Minute)}

	got, depth, _, _ := orderAll([]Ticket{leaf, grp, req})
	if len(got) != 3 {
		t.Fatalf("orderByParent returned %d rows, want 3", len(got))
	}
	if got[0].TicketID != rid || got[1].TicketID != gid || got[2].TicketID != leaf.TicketID {
		t.Errorf("order = %s/%s/%s, want request, group, leaf",
			got[0].TicketID, got[1].TicketID, got[2].TicketID)
	}
	for id, want := range map[string]int{rid: 0, gid: 1, leaf.TicketID: 2} {
		if depth[id] != want {
			t.Errorf("depth[%s] = %d, want %d", id, depth[id], want)
		}
	}
}

// A child whose parent is not on this board has no relationship to show, so it
// must not be indented under nothing.
func TestOnlyAChildOfAVisibleParentIsIndented(t *testing.T) {
	pid := "parent00"
	absent := "not-on-this-board"
	_, depth, _, _ := orderAll([]Ticket{
		{TicketID: pid},
		{TicketID: "kid", ParentID: &pid},
		{TicketID: "orphan", ParentID: &absent},
		{TicketID: "root"},
	})
	if depth["kid"] != 1 {
		t.Errorf("depth[kid] = %d, want 1 — a child of a visible parent is indented", depth["kid"])
	}
	if depth["orphan"] != 0 {
		t.Errorf("depth[orphan] = %d, want 0 — its parent is not on this board", depth["orphan"])
	}
	if depth["root"] != 0 {
		t.Errorf("depth[root] = %d, want 0", depth["root"])
	}
}

// FINISHED SLICES ARE COUNTED, NOT LISTED. A request broken into four tasks and
// eleven specification sections is sixteen rows, and within minutes most are
// done — so the board fills with work needing no attention and buries the two
// tickets that do.
func TestFinishedSectionsCollapseUnderTheirTask(t *testing.T) {
	now := time.Now()
	req := Ticket{TicketID: "request0", Status: ColTracking, CreatedAt: now.Add(-time.Hour)}
	rid := req.TicketID
	task := Ticket{TicketID: "task0000", Status: ColInDev, ParentID: &rid, CreatedAt: now.Add(-40 * time.Minute)}
	tid := task.TicketID
	rows := []Ticket{req, task}
	for i, st := range []string{ColDone, ColDone, ColWritingSpec} {
		rows = append(rows, Ticket{
			TicketID: fmt.Sprintf("sec0000%d", i), Status: st, ParentID: &tid,
			CreatedAt: now.Add(-time.Duration(30-i) * time.Minute),
		})
	}

	got, depth, progress, _ := orderAll(rows)

	var ids []string
	for _, r := range got {
		ids = append(ids, r.TicketID)
	}
	// The two finished sections are gone; the one still being written stays.
	if len(got) != 3 {
		t.Fatalf("rows = %v, want the request, its task and the unfinished section", ids)
	}
	// BOTH NUMBERS: "2 done" says nothing without the total, and the finished two
	// are hidden, so this is the only thing telling the reader they exist.
	if p := progress[tid]; p.done != 2 || p.total != 3 {
		t.Errorf("progress[task] = %d/%d, want 2/3", p.done, p.total)
	}
	// The request sees its own children counted too, not its grandchildren.
	if p := progress[rid]; p.total != 1 {
		t.Errorf("progress[request] = %d/%d, want one child", p.done, p.total)
	}
	if got[2].TicketID != "sec00002" || depth[got[2].TicketID] != 2 {
		t.Errorf("the unfinished section is missing or unnested: %v", ids)
	}

	// A FINISHED TASK STAYS VISIBLE. It is the unit of work, and "which tasks are
	// done" is the question the board exists to answer.
	doneTask := Ticket{TicketID: "task0001", Status: ColDone, ParentID: &rid, CreatedAt: now}
	got, _, _, _ = orderAll([]Ticket{req, doneTask})
	if len(got) != 2 {
		t.Error("a finished task was collapsed; only sections should be")
	}
}

// THE COUNT IS RESERVED FOR, NOT APPENDED. Appending it and clipping the result
// made the count the first thing cut — and these titles are already past the
// 60-character cap: "Implement concurrent in-memory task store with basic
// operations" is 62 characters, so the count never appeared on the rows that
// most needed it.
func TestALongTitleKeepsItsProgressCount(t *testing.T) {
	m := tuiModel{
		width:    120,
		progress: map[string]childProgress{"task0000": {done: 2, total: 3}},
	}
	long := "Implement concurrent in-memory task store with basic operations"
	if len([]rune(long)) <= m.titleWidth() {
		t.Fatalf("fixture: the title is %d columns, not longer than the %d cap", len([]rune(long)), m.titleWidth())
	}

	suffix := "  (2/3 done)"
	rendered := clip(long, m.titleWidth()-len([]rune(suffix))-1) + suffix
	if !strings.HasSuffix(rendered, "(2/3 done)") {
		t.Errorf("the count was clipped off a long title: %q", rendered)
	}
	// Measured in RUNES, not bytes: clip's ellipsis is one column and three bytes,
	// and the column budget is what the layout cares about.
	if got := len([]rune(rendered)); got > m.titleWidth() {
		t.Errorf("the row is %d columns wide, over the %d budget: %q", got, m.titleWidth(), rendered)
	}
	// A short title is untouched apart from the count.
	short := clip("store", m.titleWidth()-len([]rune(suffix))-1) + suffix
	if short != "store"+suffix {
		t.Errorf("a short title was mangled: %q", short)
	}
}

// THE PROJECT TAG PREPENDS RATHER THAN REBUILDS. It used to compose a fresh
// title from t.Title, silently discarding the progress count added three lines
// earlier — and since the window OPENS on all projects, that was every row on
// the default view. The count was correct and never once visible.
func TestTheProjectTagKeepsTheProgressCount(t *testing.T) {
	suffix := "  (2/3 done)"
	tag := "[tracker] "
	width := 60

	room := width - len([]rune(suffix)) - len([]rune(tag))
	if suffix != "" {
		room--
	}
	title := tag + clip("Implement concurrent in-memory task store with basic operations", room) + suffix

	if !strings.HasSuffix(title, "(2/3 done)") {
		t.Errorf("the project tag dropped the count: %q", title)
	}
	if !strings.HasPrefix(title, tag) {
		t.Errorf("the project tag is missing: %q", title)
	}
	if got := len([]rune(title)); got > width {
		t.Errorf("the row is %d columns, over the %d budget: %q", got, width, title)
	}
}

// THE PROSE IS THE USEFUL SIGNAL AND IT WAS ONLY IN THE FILE. Watching a stuck
// ticket through counters says "41 refusals" and leaves the cause to guesswork;
// the reasoning says "the routing uses HasPrefix on a path missing its leading
// slash", which is the actual fault. Every diagnosis worth having today came
// from reading these by hand out of a JSONL file.
func TestReadThoughtsReturnsTheModelsProse(t *testing.T) {
	dir := t.TempDir()
	recs := []Record{
		{Kind: KindTurn, TaskID: "t1", Role: "dev-agent", At: time.Now(),
			Completion: `{"tool":"write_files","arguments":{}}` + "\nThe routing is wrong: HasPrefix is missing a slash."},
		{Kind: KindTurn, TaskID: "other", Role: "dev-agent", At: time.Now(), Completion: "not this ticket"},
		{Kind: KindAction, TaskID: "t1", Role: "dev-agent", At: time.Now()},
	}
	var buf strings.Builder
	for _, r := range recs {
		b, _ := json.Marshal(r)
		buf.Write(b)
		buf.WriteString("\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "transcripts-2026-08-18.jsonl"), []byte(buf.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	got := readThoughts(dir, "t1")
	if len(got) != 1 {
		t.Fatalf("got %d thoughts, want 1 (turns for this ticket only)", len(got))
	}
	if !strings.Contains(got[0].prose, "missing a slash") {
		t.Errorf("the reasoning was lost: %q", got[0].prose)
	}
	if strings.Contains(got[0].prose, `"tool"`) {
		t.Errorf("the tool call leaked into the prose: %q", got[0].prose)
	}
	if got[0].tool != "write_files" {
		t.Errorf("tool = %q, want write_files", got[0].tool)
	}
}

// Transcript capture off, or a ticket with no turns, must not break the view.
func TestReadThoughtsIsQuietWhenThereIsNothing(t *testing.T) {
	if got := readThoughts("", "t1"); got != nil {
		t.Errorf("returned %v with no directory", got)
	}
	if got := readThoughts(transcriptOff, "t1"); got != nil {
		t.Errorf("returned %v with capture disabled", got)
	}
	if got := readThoughts(t.TempDir(), "t1"); got != nil {
		t.Errorf("returned %v for a ticket with no turns", got)
	}
}

// A long paragraph must not run off the pane.
func TestWrapTo(t *testing.T) {
	lines := wrapTo(strings.Repeat("word ", 40), 30)
	if len(lines) < 2 {
		t.Fatalf("wrapTo did not wrap: %v", lines)
	}
	for _, l := range lines {
		if len(l) > 30 {
			t.Errorf("line exceeds width: %q", l)
		}
	}
}

// THE REASONING PANEL MADE THIS VIEW TALLER THAN A TERMINAL for the first time,
// and everything past the fold was unreachable — there was no scrolling at all,
// and j/k moved the selection behind the pane instead.
func TestDetailViewWindowsLongContent(t *testing.T) {
	// Comments are what make this view tall in practice — a ticket accumulates a
	// claim and a hand-back per round trip, and each renders over several lines.
	var comments []Comment
	for i := range 12 {
		comments = append(comments, Comment{
			CommentID: fmt.Sprintf("c%d", i),
			Body:      fmt.Sprintf("comment %d\nsecond line\nthird line", i),
		})
	}
	long := Ticket{TicketID: "t1", Title: "long one", Status: ColInDev,
		Description: "short", Comments: comments}
	m := tuiModel{tickets: []Ticket{long}, detail: true, height: 20, width: 100,
		depth: map[string]int{}, progress: map[string]childProgress{}, acts: map[string]activity{}}

	first := m.detailView()
	rows := strings.Count(first, "\n")
	if rows > m.height+2 {
		t.Errorf("the view renders %d rows into a %d-row terminal", rows, m.height)
	}
	if !strings.Contains(first, "of") || !strings.Contains(first, "lines") {
		t.Error("no position indicator, so the reader cannot tell there is more")
	}
	if !strings.Contains(first, "scroll") {
		t.Error("the scroll keys are not advertised")
	}

	// Scrolling must actually change what is shown.
	m.detailScroll = 6
	if second := m.detailView(); second == first {
		t.Error("scrolling did not change the visible window")
	}

	// And it must clamp rather than scrolling into empty space.
	m.detailScroll = 1 << 30
	end := m.detailView()
	if strings.TrimSpace(end) == "" {
		t.Error("scrolling to the end emptied the view")
	}
}

// A short ticket must not advertise scrolling it does not need.
func TestDetailViewIsQuietWhenEverythingFits(t *testing.T) {
	m := tuiModel{tickets: []Ticket{{TicketID: "t1", Title: "short", Status: ColInDev}},
		detail: true, height: 40, width: 100,
		depth: map[string]int{}, progress: map[string]childProgress{}, acts: map[string]activity{}}
	if v := m.detailView(); strings.Contains(v, "scroll") {
		t.Error("scroll keys advertised on a view that fits")
	}
}

// A PARENT NEVER WORKS, so it never has turns of its own. The root of a
// decomposed request sits in "tracking" while thirteen sections beneath it are
// claimed, edited and merged, and its row read "turn 0" throughout — which looks
// exactly like a hung pipeline and was reported as one twice.
func TestActivityRollsUpToTheParent(t *testing.T) {
	root := "root"
	task := "task"
	pid := func(s string) *string { return &s }
	tickets := []Ticket{
		{TicketID: root},
		{TicketID: task, ParentID: pid(root)},
		{TicketID: "sectionA", ParentID: pid(task)},
		{TicketID: "sectionB", ParentID: pid(task)},
	}
	now := time.Now()
	acts := map[string]activity{
		"sectionA": {turns: 7, role: "dev-agent", what: "editing", at: now.Add(-time.Minute)},
		"sectionB": {turns: 3, role: "spec-agent", what: "writing tests", at: now},
	}

	got := rollUp(tickets, acts)
	if got[root].turns != 10 {
		t.Errorf("root turns = %d, want 10 (7+3 from two levels down)", got[root].turns)
	}
	if got[task].turns != 10 {
		t.Errorf("task turns = %d, want 10", got[task].turns)
	}
	// A PARENT IS NOT AN AGENT. The first version copied a live descendant's role
	// onto every ancestor, so a root, its task and its section all read
	// "dev-agent · editing" — a board on which everything appeared to be working
	// while one thing was.
	if got[root].role != "" {
		t.Errorf("root claims to be role %q; a parent does no work", got[root].role)
	}
	if got[root].working != 2 {
		t.Errorf("root reports %d working below, want 2", got[root].working)
	}
	if !strings.Contains(got[root].what, "working below") {
		t.Errorf("root does not say what is happening beneath it: %q", got[root].what)
	}
	// A leaf keeps exactly its own.
	if got["sectionA"].turns != 7 {
		t.Errorf("sectionA turns = %d, want its own 7", got["sectionA"].turns)
	}
}

// Own activity wins: a ticket that IS working shows its own state rather than a
// summary of its children.
func TestOwnActivitySurvivesTheRollUp(t *testing.T) {
	pid := func(s string) *string { return &s }
	tickets := []Ticket{{TicketID: "p"}, {TicketID: "c", ParentID: pid("p")}}
	acts := map[string]activity{
		"p": {turns: 5, role: "pm-agent", what: "scoping", at: time.Now()},
		"c": {turns: 2, role: "dev-agent", what: "editing", at: time.Now().Add(-time.Hour)},
	}
	got := rollUp(tickets, acts)
	if got["p"].turns != 7 {
		t.Errorf("parent turns = %d, want 7 (its own 5 plus the child's 2)", got["p"].turns)
	}
	// A ticket doing its OWN work keeps describing that, rather than being
	// replaced by a summary of its children.
	if got["p"].role != "pm-agent" {
		t.Errorf("a child overwrote the parent's own state: %q", got["p"].role)
	}
}

// "4/19 done" says where a request is; it does not say whether to wait for it.
func TestProgressBarEstimatesFromThisRunsPace(t *testing.T) {
	// Four done in twenty minutes → five minutes each → fifteen left for three.
	got := progressBar(4, 7, 20*time.Minute, 10)
	if !strings.Contains(got, "4/7") {
		t.Errorf("no count: %q", got)
	}
	if !strings.Contains(got, "15m left") {
		t.Errorf("estimate wrong or missing: %q", got)
	}

	// AN EXTRAPOLATION FROM ONE SAMPLE IS A GUESS WEARING A NUMBER'S CLOTHES, and
	// this board's sections have ranged from ninety seconds to four failed attempts.
	if one := progressBar(1, 7, 5*time.Minute, 10); strings.Contains(one, "left") {
		t.Errorf("estimated from a single sample: %q", one)
	}
	// Finished work needs no estimate.
	if all := progressBar(7, 7, time.Hour, 10); strings.Contains(all, "left") {
		t.Errorf("estimated time for completed work: %q", all)
	}
	if empty := progressBar(0, 0, time.Hour, 10); empty != "" {
		t.Errorf("rendered a bar for nothing: %q", empty)
	}
}

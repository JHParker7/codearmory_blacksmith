package ticketmerge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/code-armory-app/blacksmith/internal/record"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

// board is an in-memory store that records what the stage did to it, so the
// assertions are about the board's resulting SHAPE rather than about which
// method was called.
type board struct {
	mu sync.Mutex

	tickets map[string]*ticket.Ticket
	order   []string
	seq     int
	now     time.Time

	deps     [][2]string // {ticket, depends-on}
	comments map[string][]string

	failCreate bool
	failUpdate bool
}

func newBoard() *board {
	return &board{
		tickets:  map[string]*ticket.Ticket{},
		comments: map[string][]string{},
		now:      time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
	}
}

func (b *board) add(t ticket.Ticket) ticket.Ticket {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	if t.ID == "" {
		t.ID = fmt.Sprintf("t-%02d", b.seq)
	}
	if t.CreatedAt.IsZero() {
		b.now = b.now.Add(time.Minute)
		t.CreatedAt = b.now
	}
	b.tickets[t.ID] = &t
	b.order = append(b.order, t.ID)
	return t
}

func (b *board) get(id string) ticket.Ticket {
	b.mu.Lock()
	defer b.mu.Unlock()
	if t, ok := b.tickets[id]; ok {
		return *t
	}
	return ticket.Ticket{}
}

// List returns the board in REVERSE insertion order, because a real store makes
// no ordering promise and this stage must not inherit one.
func (b *board) List(context.Context, ticket.ListOpts) ([]ticket.Ticket, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]ticket.Ticket, 0, len(b.order))
	for i := len(b.order) - 1; i >= 0; i-- {
		out = append(out, *b.tickets[b.order[i]])
	}
	return out, nil
}

func (b *board) Update(_ context.Context, id string, up ticket.Update) (ticket.Ticket, error) {
	if b.failUpdate {
		return ticket.Ticket{}, errors.New("the store could not be reached")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.tickets[id]
	if !ok {
		return ticket.Ticket{}, fmt.Errorf("no such ticket %s", id)
	}
	if up.Description != "" {
		t.Description = up.Description
	}
	if up.Status != "" {
		t.Status = up.Status
	}
	return *t, nil
}

func (b *board) Create(_ context.Context, t ticket.Ticket) (ticket.Ticket, error) {
	if b.failCreate {
		return ticket.Ticket{}, errors.New("the store could not be reached")
	}
	return b.add(t), nil
}

func (b *board) AddComment(_ context.Context, id, body string) (ticket.Comment, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.comments[id] = append(b.comments[id], body)
	return ticket.Comment{ID: "c", TicketID: id, Body: body}, nil
}

func (b *board) AddDependency(_ context.Context, id, dependsOn string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deps = append(b.deps, [2]string{id, dependsOn})
	return nil
}

func (b *board) MoveTo(_ context.Context, id, column string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if t, ok := b.tickets[id]; ok {
		t.Status = column
	}
	return nil
}

func (b *board) sections() []ticket.Ticket {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []ticket.Ticket
	for _, id := range b.order {
		if t := b.tickets[id]; t.Status == workflow.ColReadyForSpec {
			out = append(out, *t)
		}
	}
	return out
}

func (b *board) dependsOn(id string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, d := range b.deps {
		if d[0] == id {
			out = append(out, d[1])
		}
	}
	return out
}

func (b *board) commentsOn(id string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Join(b.comments[id], "\n")
}

// plan builds a request with n tasks waiting to be merged.
func plan(b *board, n int) (parent ticket.Ticket, tasks []ticket.Ticket) {
	parent = b.add(ticket.Ticket{Title: "Build a task manager", Status: workflow.ColTracking})
	for i := 1; i <= n; i++ {
		tasks = append(tasks, b.add(ticket.Ticket{
			Title: fmt.Sprintf("Task %d", i),
			Description: fmt.Sprintf("Do the %d%s thing.\n\n%sunit%d.go`.",
				i, "th", TaskFileIntro, i),
			Status:   workflow.ColReadyForTicketMerge,
			ParentID: &parent.ID,
		}))
	}
	return parent, tasks
}

// A SUMMARY IS WHERE A REQUIREMENT GOES MISSING, and there is no reader
// downstream who could notice: the developer satisfies every test it is given
// and reports success while the requirement is simply gone.
func TestEveryTasksRequirementsSurviveTheMerge(t *testing.T) {
	b := newBoard()
	_, tasks := plan(b, 4)

	a := New(b)
	status, detail, err := a.Handle(context.Background(), tasks[0])
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if status != workflow.OutcomeSuccess {
		t.Fatalf("status = %q (%s)", status, detail)
	}

	brief := b.get(tasks[0].ID).Description
	for _, task := range tasks {
		if !strings.Contains(brief, strings.TrimSpace(task.Description)) {
			t.Errorf("the merged brief lost %q entirely", task.Title)
		}
		if !strings.Contains(brief, task.Title) {
			t.Errorf("the merged brief does not name %q", task.Title)
		}
	}
}

// THE FIRST TICKET ABSORBS THE REST rather than a new one being created, so the
// merged ticket keeps its history: its claim record, its place among the
// request's children, and the id anything already referring to it used.
func TestTheFirstTicketAbsorbsTheOthersRatherThanANewOne(t *testing.T) {
	b := newBoard()
	parent, tasks := plan(b, 3)

	if _, _, err := New(b).Handle(context.Background(), tasks[0]); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	kept := b.get(tasks[0].ID)
	if kept.ID != tasks[0].ID {
		t.Error("the merged ticket is not the original")
	}
	if kept.Parent() != parent.ID {
		t.Errorf("the merged ticket lost its place among the request's children: parent %q", kept.Parent())
	}
	// The others stay on the board, each saying where its work went.
	for _, s := range tasks[1:] {
		got := b.get(s.ID)
		if got.Status != workflow.ColDone {
			t.Errorf("an absorbed task is in %q, want done", got.Status)
		}
		if !strings.Contains(b.commentsOn(s.ID), record.MergedIntoMarker) {
			t.Errorf("an absorbed task does not say where its work went; it reads as abandoned")
		}
		if !strings.Contains(b.commentsOn(s.ID), shortID(tasks[0].ID)) {
			t.Errorf("an absorbed task does not name the ticket that took it")
		}
	}
}

// THE ORDER IS SORTED, NOT ASSUMED. It decides how the brief reads and — since
// each section waits for the one before it — the order the specification is
// written in. Leaving it to the store made both vary run to run.
func TestTheOrderIsTheOrderTheWorkWasPlanned(t *testing.T) {
	b := newBoard()
	_, tasks := plan(b, 4)

	// The listing returns them backwards, as a store with no ordering promise may.
	if _, _, err := New(b).Handle(context.Background(), tasks[0]); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	brief := b.get(tasks[0].ID).Description
	at := make([]int, len(tasks))
	for i, task := range tasks {
		at[i] = strings.Index(brief, task.Title)
		if at[i] < 0 {
			t.Fatalf("the brief does not mention %q", task.Title)
		}
	}
	for i := 1; i < len(at); i++ {
		if at[i] < at[i-1] {
			t.Errorf("the brief reads %q before %q; the plan's order was lost",
				tasks[i].Title, tasks[i-1].Title)
		}
	}
}

// ONE SECTION PER UNIT OF WORK. An author is finished when its gate passes, and
// that happens as soon as its first file compiles and fails correctly — give it
// five units and it stops after one.
func TestOneSectionIsOpenedPerUnitOfWork(t *testing.T) {
	b := newBoard()
	_, tasks := plan(b, 3)

	if _, _, err := New(b).Handle(context.Background(), tasks[0]); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	secs := b.sections()
	if len(secs) != 3 {
		t.Fatalf("opened %d sections for 3 units of work", len(secs))
	}
	for i, sec := range secs {
		if sec.Parent() != tasks[0].ID {
			t.Errorf("section %d hangs off %q, not the merged ticket", i, sec.Parent())
		}
		// Each carries only its OWN requirements, so its gate passes exactly when
		// its job is done.
		if !strings.Contains(sec.Description, fmt.Sprintf("slice %d of 3", i+1)) {
			t.Errorf("section %d does not say which slice it is", i)
		}
	}
}

// THE TASK WAITS FOR EVERY SECTION, so the developer starts once the whole
// specification exists rather than partway through it.
func TestTheMergedTaskWaitsForEverySection(t *testing.T) {
	b := newBoard()
	_, tasks := plan(b, 3)

	if _, _, err := New(b).Handle(context.Background(), tasks[0]); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	waits := b.dependsOn(tasks[0].ID)
	secs := b.sections()
	if len(waits) != len(secs) {
		t.Fatalf("the task waits for %d of %d sections", len(waits), len(secs))
	}
	have := map[string]bool{}
	for _, id := range waits {
		have[id] = true
	}
	for _, sec := range secs {
		if !have[sec.ID] {
			t.Errorf("the task does not wait for section %q; it may develop early", sec.ID)
		}
	}
}

// AND EACH SECTION WAITS FOR THE ONE BEFORE IT. They share a branch, and a
// sandbox begins by resetting hard to it — so two authors writing at once wipe
// each other's work and NEITHER CAN TELL. Measured: five sections claimed at
// once, four looping until the run was stopped.
func TestTheSectionsAreChainedSoTheyCannotRaceOnTheBranch(t *testing.T) {
	b := newBoard()
	_, tasks := plan(b, 4)

	if _, _, err := New(b).Handle(context.Background(), tasks[0]); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	secs := b.sections()
	if len(secs) < 2 {
		t.Fatalf("only %d sections", len(secs))
	}
	// The first waits for nothing; every later one waits for its predecessor.
	if got := b.dependsOn(secs[0].ID); len(got) != 0 {
		t.Errorf("the first section waits for %v; nothing precedes it", got)
	}
	for i := 1; i < len(secs); i++ {
		got := b.dependsOn(secs[i].ID)
		if len(got) != 1 || got[0] != secs[i-1].ID {
			t.Errorf("section %d waits for %v, want only %q", i, got, secs[i-1].ID)
		}
	}
}

// EACH SECTION IS TOLD WHICH FILE IS ITS OWN. An author appending to a file it
// did not write is editing rather than creating, and it spent three turns
// failing to quote a function back exactly before one landed.
func TestEachSectionIsGivenItsOwnFileToCreate(t *testing.T) {
	b := newBoard()
	_, tasks := plan(b, 3)

	if _, _, err := New(b).Handle(context.Background(), tasks[0]); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	seen := map[string]bool{}
	for i, sec := range b.sections() {
		want := fmt.Sprintf("unit%d_test.go", i+1)
		if !strings.Contains(sec.Description, want) {
			t.Errorf("section %d was not given %q: %s", i, want, sec.Description)
		}
		if seen[want] {
			t.Errorf("two sections were given the same file %q; one would edit the other's work", want)
		}
		seen[want] = true
		if !strings.Contains(sec.Description, "touch no other test file") {
			t.Errorf("section %d is not told to leave the others alone", i)
		}
	}
}

// A plan that named no file still gets a usable default rather than leaving the
// author to choose.
func TestASectionWithNoNamedFileGetsADefault(t *testing.T) {
	b := newBoard()
	parent := b.add(ticket.Ticket{Title: "A request", Status: workflow.ColTracking})
	first := b.add(ticket.Ticket{
		Title: "One", Description: "no file named here",
		Status: workflow.ColReadyForTicketMerge, ParentID: &parent.ID,
	})
	b.add(ticket.Ticket{
		Title: "Two", Description: "nor here",
		Status: workflow.ColReadyForTicketMerge, ParentID: &parent.ID,
	})

	if _, _, err := New(b).Handle(context.Background(), first); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	for _, sec := range b.sections() {
		if !strings.Contains(sec.Description, "_test.go") {
			t.Errorf("a section names no test file at all: %s", sec.Description)
		}
	}
}

// ANYTHING UNDER AN ABSORBED TICKET GOES TOO. A section left queued against one
// is written anyway, by an author briefed on a fifth of the work, onto a branch
// nobody develops.
func TestSectionsUnderAnAbsorbedTicketAreClosed(t *testing.T) {
	b := newBoard()
	_, tasks := plan(b, 2)
	orphan := b.add(ticket.Ticket{
		Title: "Specification for Task 2", Status: workflow.ColReadyForSpec, ParentID: &tasks[1].ID,
	})

	if _, _, err := New(b).Handle(context.Background(), tasks[0]); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	got := b.get(orphan.ID)
	if got.Status != workflow.ColDone {
		t.Errorf("a section under an absorbed ticket is in %q; it would be written onto a branch nobody develops",
			got.Status)
	}
	if !strings.Contains(b.commentsOn(orphan.ID), record.MergedIntoMarker) {
		t.Error("the closed section does not say why")
	}
}

// ONLY WHAT IS STILL WAITING is absorbed. A sibling already past this stage is
// not this ticket's to take.
func TestATaskPastThisStageIsNotAbsorbed(t *testing.T) {
	b := newBoard()
	parent := b.add(ticket.Ticket{Title: "A request", Status: workflow.ColTracking})
	first := b.add(ticket.Ticket{
		Title: "One", Description: "a", Status: workflow.ColReadyForTicketMerge, ParentID: &parent.ID,
	})
	waiting := b.add(ticket.Ticket{
		Title: "Two", Description: "b", Status: workflow.ColReadyForTicketMerge, ParentID: &parent.ID,
	})
	moved := b.add(ticket.Ticket{
		Title: "Three", Description: "c", Status: workflow.ColInDev, ParentID: &parent.ID,
	})

	if _, _, err := New(b).Handle(context.Background(), first); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if got := b.get(moved.ID); got.Status != workflow.ColInDev {
		t.Errorf("a task already in development was absorbed; it is in %q", got.Status)
	}
	if got := b.get(waiting.ID); got.Status != workflow.ColDone {
		t.Errorf("a task still waiting was not absorbed; it is in %q", got.Status)
	}
	if brief := b.get(first.ID).Description; strings.Contains(brief, "Three") {
		t.Error("the brief carries a task that was not absorbed")
	}
}

// A ticket from another request must never be taken, however it is ordered.
func TestATaskFromAnotherRequestIsNeverAbsorbed(t *testing.T) {
	b := newBoard()
	_, mine := plan(b, 2)
	_, theirs := plan(b, 2)

	if _, _, err := New(b).Handle(context.Background(), mine[0]); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	for _, other := range theirs {
		if got := b.get(other.ID); got.Status != workflow.ColReadyForTicketMerge {
			t.Errorf("another request's task was moved to %q", got.Status)
		}
	}
}

// A REQUEST SCOPED AS ONE UNIT OF WORK is already what this stage would produce,
// so there is nothing to do and nothing to open.
func TestATicketWithNoParentIsAlreadyOneTask(t *testing.T) {
	b := newBoard()
	lone := b.add(ticket.Ticket{Title: "One thing", Status: workflow.ColReadyForTicketMerge})

	status, detail, err := New(b).Handle(context.Background(), lone)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if status != workflow.OutcomeSuccess {
		t.Errorf("status = %q", status)
	}
	if !strings.Contains(detail, "already one task") {
		t.Errorf("detail = %q", detail)
	}
	if len(b.sections()) != 0 {
		t.Error("sections were opened for a ticket with nothing to merge")
	}
}

func TestATicketWithNoSiblingsLeftDoesNothing(t *testing.T) {
	b := newBoard()
	parent := b.add(ticket.Ticket{Title: "A request", Status: workflow.ColTracking})
	only := b.add(ticket.Ticket{
		Title: "One", Description: "a", Status: workflow.ColReadyForTicketMerge, ParentID: &parent.ID,
	})

	status, detail, err := New(b).Handle(context.Background(), only)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if status != workflow.OutcomeSuccess || !strings.Contains(detail, "nothing left to merge") {
		t.Errorf("status = %q, detail = %q", status, detail)
	}
	if len(b.sections()) != 0 {
		t.Error("sections were opened when there was nothing to merge")
	}
}

// A FAILURE TO WRITE THE MERGED BRIEF MUST NOT LEAVE THE BOARD HALF-MERGED with
// a success reported: the brief is the merge.
func TestAFailureToWriteTheBriefIsReportedAsOne(t *testing.T) {
	b := newBoard()
	_, tasks := plan(b, 3)
	b.failUpdate = true

	status, _, err := New(b).Handle(context.Background(), tasks[0])
	if err == nil {
		t.Fatal("a failed brief was reported as a merge")
	}
	if status != workflow.OutcomeFailed {
		t.Errorf("status = %q", status)
	}
	for _, s := range tasks[1:] {
		if got := b.get(s.ID); got.Status == workflow.ColDone {
			t.Error("a task was absorbed even though the brief was never written")
		}
	}
}

func TestAFailureToOpenASectionIsReportedAsOne(t *testing.T) {
	b := newBoard()
	_, tasks := plan(b, 3)
	b.failCreate = true

	status, _, err := New(b).Handle(context.Background(), tasks[0])
	if err == nil {
		t.Fatal("a failed section was reported as a merge")
	}
	if status != workflow.OutcomeFailed {
		t.Errorf("status = %q", status)
	}
}

func TestTheStageIdentifiesItself(t *testing.T) {
	a := New(newBoard())
	if a.Role() != workflow.RoleTicketMerge {
		t.Errorf("Role() = %q", a.Role())
	}
	if !a.Wants(ticket.Ticket{}) {
		t.Error("the stage refused a ticket in its own queue")
	}
	// It calls no model, but must still name a class a one-model host serves.
	if a.Class() == "" {
		t.Error("the stage names no class; a host could not wire it")
	}
}

func TestSpecFileNaming(t *testing.T) {
	cases := map[string]string{
		"store.go":     "store_test.go",
		"a/b/store.go": "a/b/store_test.go",
		"store":        "store_test.go",
		"":             "_test.go",
	}
	for in, want := range cases {
		if got := SpecFileFor(in); got != want {
			t.Errorf("SpecFileFor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTheSourceFileIsReadBackFromTheBrief(t *testing.T) {
	desc := "Do the thing.\n\n" + TaskFileIntro + "store.go`, and nowhere else."
	if got := SourceFileFromBrief(desc); got != "store.go" {
		t.Errorf("SourceFileFromBrief() = %q", got)
	}
	// A brief that names no file yields nothing, which is why the caller keeps a
	// default rather than trusting this.
	for _, desc := range []string{"", "no file here", TaskFileIntro + "unterminated"} {
		if got := SourceFileFromBrief(desc); got != "" {
			t.Errorf("SourceFileFromBrief(%q) = %q, want empty", desc, got)
		}
	}
}

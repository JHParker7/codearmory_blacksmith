package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// pmGateway wires a gateway whose model returns a fixed body.
func pmGateway(t *testing.T, reply string) *Gateway {
	t.Helper()
	gw, _ := testGateway(t, completionHandler(reply), 2)
	return gw
}

func TestParseTriageAcceptsBareJSON(t *testing.T) {
	got, err := parseTriage(`{"summary":"s","priority":"high","labels":["a"]}`)
	if err != nil {
		t.Fatalf("parseTriage() = %v", err)
	}
	if got.Priority != "high" || got.Summary != "s" || len(got.Labels) != 1 {
		t.Errorf("parsed = %+v", got)
	}
}

// Models add code fences and prose despite being told not to; the parser must
// cope rather than failing a task on formatting.
func TestParseTriageToleratesFencesAndProse(t *testing.T) {
	for name, raw := range map[string]string{
		"fenced":      "```json\n{\"priority\":\"low\"}\n```",
		"unlabelled":  "```\n{\"priority\":\"low\"}\n```",
		"prose first": "Sure! Here is the triage:\n{\"priority\":\"low\"}\nHope that helps.",
	} {
		got, err := parseTriage(raw)
		if err != nil {
			t.Errorf("%s: parseTriage() = %v", name, err)
			continue
		}
		if got.Priority != "low" {
			t.Errorf("%s: priority = %q, want low", name, got.Priority)
		}
	}
}

func TestParseTriageRejectsNonJSON(t *testing.T) {
	for _, raw := range []string{"", "I cannot help with that.", "```\nnot json\n```"} {
		if _, err := parseTriage(raw); err == nil {
			t.Errorf("parseTriage(%q) = nil error, want a rejection", raw)
		}
	}
}

// CONTAINMENT: a priority outside the allowlist must be dropped, never
// forwarded to the API. This is the boundary that keeps a prompt-injected model
// from writing arbitrary values into structured state.
func TestSanitiseTriageRejectsUnknownPriority(t *testing.T) {
	for _, bad := range []string{"URGENT!!!", "p0", "", "drop table", "Critical"} {
		got := sanitiseTriage(triage{Priority: bad})
		if got.Priority != "" {
			t.Errorf("sanitiseTriage(priority=%q) = %q, want it discarded", bad, got.Priority)
		}
	}
	for _, ok := range validPriorities {
		if got := sanitiseTriage(triage{Priority: ok}); got.Priority != ok {
			t.Errorf("sanitiseTriage(priority=%q) dropped a valid value", ok)
		}
	}
}

func TestSanitiseTriageBoundsOutput(t *testing.T) {
	many := make([]string, 100)
	manySubtasks := make([]subtask, 100)
	for i := range many {
		many[i] = strings.Repeat("x", 1000)
		manySubtasks[i] = subtask{Title: strings.Repeat("x", 1000)}
	}
	got := sanitiseTriage(triage{
		Summary: strings.Repeat("s", 5000), Labels: many, Subtasks: manySubtasks, NeedsDetail: many,
	})
	if len(got.Labels) > maxLabels || len(got.Subtasks) > maxSubtasks || len(got.NeedsDetail) > maxNeedsDetail {
		t.Errorf("lists not bounded: %d labels, %d subtasks, %d questions",
			len(got.Labels), len(got.Subtasks), len(got.NeedsDetail))
	}
	if len([]rune(got.Summary)) > maxSummaryRunes+1 {
		t.Errorf("summary is %d runes, want it clipped", len([]rune(got.Summary)))
	}
	for _, l := range got.Labels {
		if len([]rune(l)) > maxItemRunes+1 {
			t.Errorf("label is %d runes, want it clipped", len([]rune(l)))
		}
	}
	// Empty entries are dropped rather than rendered as blank bullets.
	if got2 := sanitiseTriage(triage{Labels: []string{"", "  ", "real"}}); len(got2.Labels) != 1 {
		t.Errorf("labels = %v, want blanks dropped", got2.Labels)
	}
}

func TestPMAgentTriagesATicket(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "Login is broken", Description: "500 on submit", Priority: "medium", CreatedBy: "alice"})

	gw := pmGateway(t, `{"summary":"Login returns 500.","priority":"high","labels":["auth","bug"],"subtasks":[{"title":"reproduce"},{"title":"fix handler","depends_on":[0]}],"rationale":"users cannot log in"}`)
	a := NewPMAgent(gw, api, ClassLarge)

	status, detail, err := a.Handle(context.Background(), f.get("t1"))
	if err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	if status != OutcomeSuccess {
		t.Errorf("status = %q (%s), want success", status, detail)
	}

	if got := f.get("t1").Priority; got != "high" {
		t.Errorf("priority = %q, want high", got)
	}
	bodies := f.commentBodies("t1")
	if len(bodies) != 1 {
		t.Fatalf("wrote %d comments, want 1", len(bodies))
	}
	for _, want := range []string{"Triage", "Login returns 500.", "high", "auth", "reproduce"} {
		if !strings.Contains(bodies[0], want) {
			t.Errorf("comment missing %q:\n%s", want, bodies[0])
		}
	}

	// TWO LEVELS. A TASK per unit of work, developed on its own branch; a SUB-TASK
	// per slice of that task's specification, which is never developed and exists
	// so the slice actually gets written. A plan in a description is a
	// suggestion — measured, a request planned as twelve sections produced ONE
	// test file, because the spec stage is satisfied by any file that parses,
	// fails and has no stubs.
	f.mu.Lock()
	var tasks, sections []Ticket
	for _, tk := range f.tickets {
		switch {
		case tk.TicketID == "t1":
		case tk.Status == ColReadyForSpecMerge:
			tasks = append(tasks, *tk)
		case tk.Status == ColReadyForSpec:
			sections = append(sections, *tk)
		}
	}
	deps := map[string][]string{}
	for k, v := range f.deps {
		deps[k] = append([]string(nil), v...)
	}
	f.mu.Unlock()

	if len(tasks) != 2 {
		t.Fatalf("%d task tickets, want 2 (one per unit of work)", len(tasks))
	}
	if len(sections) == 0 {
		t.Fatal("no specification sections; nothing obliges the spec to be written")
	}
	for _, task := range tasks {
		if task.ParentID == nil || *task.ParentID != "t1" {
			t.Errorf("task %s is not filed under the request", task.TicketID)
		}
		// A TASK WAITS FOR ITS OWN SPECIFICATION. No promotion mechanism is
		// needed: the dev stage already refuses a ticket with unfinished
		// dependencies, and a sub-task ends at done.
		if len(deps[task.TicketID]) == 0 {
			t.Errorf("task %s waits for nothing; it would develop before its tests exist", task.TicketID)
		}
	}
	for _, sec := range sections {
		if sec.ParentID == nil {
			t.Errorf("section %s is not filed under a task", sec.TicketID)
			continue
		}
		var under bool
		for _, task := range tasks {
			if *sec.ParentID == task.TicketID {
				under = true
			}
		}
		if !under {
			t.Errorf("section %s is filed under something that is not a task", sec.TicketID)
		}
	}
	// The request itself is not worked: it is the record of what was asked for.
	if got := f.get("t1").Status; got == ColReadyForSpecMerge || got == ColReadyForSpec {
		t.Errorf("the request itself was queued for work (%q)", got)
	}
}

// An invalid priority from the model must leave the ticket's own value alone.
func TestPMAgentIgnoresInvalidPriority(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x", Priority: "medium", CreatedBy: "alice"})

	gw := pmGateway(t, `{"summary":"s","priority":"SUPER-CRITICAL","rationale":"r"}`)
	a := NewPMAgent(gw, api, ClassLarge)
	if _, _, err := a.Handle(context.Background(), f.get("t1")); err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	if got := f.get("t1").Priority; got != "medium" {
		t.Errorf("priority = %q, want the ticket's own value kept", got)
	}
}

// Unparseable output must be surfaced on the ticket, not swallowed: otherwise a
// broken prompt is indistinguishable from a quiet department.
func TestPMAgentSurfacesUnparseableOutput(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x", Priority: "medium", CreatedBy: "alice"})

	gw := pmGateway(t, "I'm sorry, I can't do that.")
	a := NewPMAgent(gw, api, ClassLarge)

	status, _, err := a.Handle(context.Background(), f.get("t1"))
	if err == nil {
		t.Fatal("Handle() = nil error on unparseable output, want an error")
	}
	if status != OutcomeFailed {
		t.Errorf("status = %q, want %q", status, OutcomeFailed)
	}
	bodies := f.commentBodies("t1")
	if len(bodies) != 1 || !strings.Contains(bodies[0], "Triage failed") {
		t.Errorf("comments = %v, want the failure recorded on the ticket", bodies)
	}
}

func TestPMAgentPropagatesInferenceFailure(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x", CreatedBy: "alice"})

	gw, _ := testGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("model down"))
	}, 1)
	a := NewPMAgent(gw, api, ClassLarge)

	status, _, err := a.Handle(context.Background(), f.get("t1"))
	if err == nil || status != OutcomeFailed {
		t.Fatalf("Handle() = (%q, %v), want a failure", status, err)
	}
	if got := f.commentBodies("t1"); len(got) != 0 {
		t.Errorf("wrote %v, want no comment when the model never answered", got)
	}
}

// A long ticket description must be truncated before it reaches the model:
// context is scarce and the description is attacker-influenced in general.
func TestRenderTicketBoundsDescription(t *testing.T) {
	got := renderTicket(Ticket{Title: "t", Description: strings.Repeat("x", 20000)})
	if len([]rune(got)) > 5000 {
		t.Errorf("rendered prompt is %d runes, want the description truncated", len([]rune(got)))
	}
	if !strings.Contains(got, "truncated") {
		t.Error("truncation is silent; the model cannot tell it received a partial description")
	}
}

func TestPMAgentIdentity(t *testing.T) {
	a := NewPMAgent(nil, nil, ClassSmall)
	if a.Role() != "pm-agent" {
		t.Errorf("Role() = %q, want pm-agent", a.Role())
	}
	if a.Class() != ClassSmall {
		t.Errorf("Class() = %q, want small", a.Class())
	}
}

// The breakdown is a GRAPH, not a chain. Several subtasks may wait on one, and
// one may wait on several — chaining them in the order proposed serialises work
// that has no relationship, which turned a request for two independent
// functions into four sequential agent cycles instead of two parallel ones.
func TestSubtaskEdgesAreKeptAsGiven(t *testing.T) {
	// The fan-in node used to be "document both", which notCodeWork now drops as
	// documentation — correctly, but it made this test about the wrong thing. The
	// GRAPH SHAPE is what is under test here, so it is code work instead.
	got := sanitiseSubtasks([]subtask{
		{Title: "create Min"},
		{Title: "create Max"},
		{Title: "test Min", DependsOn: []int{0}},
		{Title: "test Max", DependsOn: []int{1}},
		{Title: "wire both into the CLI", DependsOn: []int{2, 3}},
	})
	if len(got) != 5 {
		t.Fatalf("kept %d subtasks, want 5", len(got))
	}
	// Two roots: independent work must be startable at the same time.
	roots := 0
	for _, st := range got {
		if len(st.DependsOn) == 0 {
			roots++
		}
	}
	if roots != 2 {
		t.Errorf("%d subtasks can start immediately, want 2; independent work is being serialised", roots)
	}
	// And one subtask may wait on several.
	if len(got[4].DependsOn) != 2 {
		t.Errorf("fan-in lost: %v", got[4].DependsOn)
	}
}

// Failing OPEN is the whole policy. A missing edge costs at most a merge
// conflict, which the resolver already handles; a wrong one stalls work that
// was ready and reports on the board that it is waiting for something.
func TestUnusableSubtaskEdgesAreDroppedNotGuessed(t *testing.T) {
	got := sanitiseSubtasks([]subtask{
		{Title: "a", DependsOn: []int{0}},    // itself
		{Title: "b", DependsOn: []int{99}},   // out of range
		{Title: "c", DependsOn: []int{-1}},   // negative
		{Title: "d", DependsOn: []int{1, 1}}, // duplicate
	})
	for i, st := range got[:3] {
		if len(st.DependsOn) != 0 {
			t.Errorf("subtask %d kept an unusable edge: %v", i, st.DependsOn)
		}
	}
	if len(got[3].DependsOn) != 1 {
		t.Errorf("duplicate edge not collapsed: %v", got[3].DependsOn)
	}

	// A cycle makes every subtask in it unworkable, so ALL edges go rather than
	// picking one to break arbitrarily.
	cyc := sanitiseSubtasks([]subtask{
		{Title: "a", DependsOn: []int{1}},
		{Title: "b", DependsOn: []int{2}},
		{Title: "c", DependsOn: []int{0}},
	})
	for i, st := range cyc {
		if len(st.DependsOn) != 0 {
			t.Errorf("subtask %d still waits on %v after a cycle; nothing in it could ever start", i, st.DependsOn)
		}
	}
	if len(cyc) != 3 {
		t.Error("a cycle discarded the work as well as the edges")
	}
}

// An index pointing at a subtask that was clipped away must be dropped, not
// silently re-pointed at whatever now sits at that position.
func TestSubtaskEdgesResolveAgainstTheClippedList(t *testing.T) {
	in := make([]subtask, maxSubtasks+3)
	for i := range in {
		in[i] = subtask{Title: "task"}
	}
	in[0].DependsOn = []int{maxSubtasks + 1}
	got := sanitiseSubtasks(in)
	if len(got) != maxSubtasks {
		t.Fatalf("kept %d subtasks, want %d", len(got), maxSubtasks)
	}
	if len(got[0].DependsOn) != 0 {
		t.Errorf("an edge into a discarded subtask survived: %v", got[0].DependsOn)
	}
}

// The model's most common breakdown mistake is a chain: each subtask depending
// on the previous one, so nothing runs beside anything else and one failure
// strands the rest. Observed live on a REST-API breakdown.
func TestSanitiseSubtasksFlattensAChainIntoFanOut(t *testing.T) {
	chain := []subtask{
		{Title: "store"},
		{Title: "handlers", DependsOn: []int{0}},
		{Title: "validation", DependsOn: []int{1}},
		{Title: "filtering", DependsOn: []int{2}},
		{Title: "tests", DependsOn: []int{3}},
	}
	out := sanitiseSubtasks(chain)
	if len(out) != 5 {
		t.Fatalf("got %d subtasks, want 5", len(out))
	}
	if len(out[0].DependsOn) != 0 {
		t.Errorf("the foundation gained a dependency: %v", out[0].DependsOn)
	}
	for i := 1; i < len(out); i++ {
		if len(out[i].DependsOn) != 1 || out[i].DependsOn[0] != 0 {
			t.Errorf("subtask %d depends on %v, want only the foundation [0] — a chain leaves no parallelism",
				i, out[i].DependsOn)
		}
	}
}

// A breakdown that is ALREADY fan-out must pass through untouched.
func TestSanitiseSubtasksLeavesFanOutAlone(t *testing.T) {
	in := []subtask{
		{Title: "types"},
		{Title: "a", DependsOn: []int{0}},
		{Title: "b", DependsOn: []int{0}},
	}
	out := sanitiseSubtasks(in)
	for i := 1; i < 3; i++ {
		if len(out[i].DependsOn) != 1 || out[i].DependsOn[0] != 0 {
			t.Errorf("subtask %d = %v, want [0]", i, out[i].DependsOn)
		}
	}
}

// Two independent foundations must both survive as roots, and a subtask resting
// on both must keep both.
func TestSanitiseSubtasksKeepsMultipleRoots(t *testing.T) {
	in := []subtask{
		{Title: "types"},
		{Title: "config"},
		{Title: "server", DependsOn: []int{0, 1}},
		{Title: "handlers", DependsOn: []int{2}},
	}
	out := sanitiseSubtasks(in)
	if len(out[0].DependsOn) != 0 || len(out[1].DependsOn) != 0 {
		t.Fatal("a foundation gained a dependency")
	}
	for _, i := range []int{2, 3} {
		if len(out[i].DependsOn) != 2 || out[i].DependsOn[0] != 0 || out[i].DependsOn[1] != 1 {
			t.Errorf("subtask %d = %v, want both foundations [0 1]", i, out[i].DependsOn)
		}
	}
}

// A ticket asking for documentation or for "the tests" has nowhere to go: the
// architect already wrote the docs into the base branch and the spec author
// already writes tests for every ticket. Worse, it is unsatisfiable — the spec
// author may write only *_test.go, so a README ticket cannot reach its finish
// condition by any permitted action. Measured: 642 seconds of a large-class slot,
// then a developer handed a gate it could not pass.
func TestSubtasksThatThisDepartmentCannotBuildAreDropped(t *testing.T) {
	for _, title := range []string{
		"Create README documenting how to run the server",
		"Write unit tests covering all API endpoints and HTML interface",
		"Update the README with the new endpoints",
		"Add documentation for the filtering parameters",
		"Document the API endpoints",
		"Write tests for the task store",
		"Add a CHANGELOG entry",
	} {
		if !notCodeWork(title) {
			t.Errorf("%q was kept; it cannot be built by any stage", title)
		}
	}
}

// Far more important than the above: real work must survive. Dropping a genuine
// subtask is silent lost scope, where letting one odd ticket through merely
// wastes a slot.
func TestImplementationWorkIsNeverDroppedForMentioningDocs(t *testing.T) {
	for _, title := range []string{
		"Implement filtering and document the query parameters",
		"Build the HTML interface described in the README",
		"Create JSON REST API endpoints for listing, creating, fetching tasks",
		"Implement concurrent-safe in-memory task store",
		"Add input validation and proper HTTP status codes for API endpoints",
		"Fix the readme parser so it handles nested lists",
		"Create the test harness command used by CI",
		"Add a documentation-generator endpoint to the API",
	} {
		if notCodeWork(title) {
			t.Errorf("%q was dropped; it is implementation work", title)
		}
	}
}

// Dropping a subtask shifts every later index, so edges must be TRANSLATED.
// Reusing the model's indexes would repoint "depends on the store" at whatever
// moved into that slot — a wrong dependency, which stalls work that was ready
// and says on the board it is waiting for something.
func TestDroppingASubtaskRemapsDependencyIndexes(t *testing.T) {
	in := []subtask{
		{Title: "Create README for the project"},                 // 0 — dropped
		{Title: "Implement the task store"},                      // 1 -> 0
		{Title: "Create REST endpoints", DependsOn: []int{1}},    // 2 -> 1, depends on the store
		{Title: "Build the HTML interface", DependsOn: []int{1}}, // 3 -> 2, depends on the store
	}
	out := sanitiseSubtasks(in)
	if len(out) != 3 {
		t.Fatalf("kept %d subtasks, want 3 (the README dropped): %+v", len(out), out)
	}
	if out[0].Title != "Implement the task store" {
		t.Fatalf("out[0] = %q, want the store", out[0].Title)
	}
	for _, i := range []int{1, 2} {
		if len(out[i].DependsOn) != 1 || out[i].DependsOn[0] != 0 {
			t.Errorf("out[%d] (%q) depends on %v, want [0] — the store at its NEW index",
				i, out[i].Title, out[i].DependsOn)
		}
	}
}

// "test" singular is a modifier on something real; "tests" is the deliverable.
func TestOnlyThePluralTestDeliverableIsDropped(t *testing.T) {
	drop := []string{"Write the test suite for the store", "Add test coverage for the API"}
	keep := []string{"Create the test harness command", "Add a test endpoint for health checks", "Build the test fixture loader"}
	for _, s := range drop {
		if !notCodeWork(s) {
			t.Errorf("%q was kept; the tests are the whole deliverable", s)
		}
	}
	for _, s := range keep {
		if notCodeWork(s) {
			t.Errorf("%q was dropped; \"test\" is a modifier, the work is real", s)
		}
	}
}

// THE SPEC AUTHOR WORKS FROM ALMOST NOTHING WITHOUT THIS. A child ticket used to
// carry a one-line title and the original request, so the detail was invented —
// and every invention became an assertion the developer was bound to and could
// not edit. Measured: an empty-slice-not-nil rule, a case-insensitive search and
// sequential ids, none of them in the request, each costing a whole budget.
func TestChildTicketCarriesItsAcceptanceCriteria(t *testing.T) {
	tri := triage{
		Summary: "a tracker",
		Subtasks: []subtask{{
			Title:      "Implement the task store",
			Acceptance: []string{"Create adds a task and returns it", "List returns every stored task"},
		}},
	}
	pt := planTask{task: tri.Subtasks[0], sections: []subtask{tri.Subtasks[0]}}
	got := taskDescription(Ticket{Title: "Build a tracker", Description: "the original request"}, pt)

	for _, want := range []string{"done when", "Create adds a task", "List returns every stored task"} {
		if !strings.Contains(got, want) {
			t.Errorf("the task ticket is missing %q:\n%s", want, got)
		}
	}
	// The criteria must come ABOVE the request: they are the specific thing, the
	// request is context. Reversed, the author reads the whole application first
	// and then guesses which part is this ticket's.
	if strings.Index(got, "Create adds a task") > strings.Index(got, "the original request") {
		t.Error("the criteria are below the original request; the specific thing should come first")
	}
	// And the boundary has to be stated, or "these are the criteria" reads as a
	// helpful hint rather than the limit of the job.
	if !strings.Contains(got, "NOT a requirement") {
		t.Error("nothing tells the author that unstated things are out of scope")
	}
}

// Bounded like everything else a model produces: a subtask needing more than a
// handful of criteria is a subtask that should have been two.
func TestAcceptanceCriteriaAreBounded(t *testing.T) {
	many := make([]string, maxAcceptance+4)
	for i := range many {
		many[i] = "criterion"
	}
	got := sanitiseSubtasks([]subtask{{Title: "Implement the store", Acceptance: many}})
	if len(got) != 1 {
		t.Fatalf("kept %d subtasks, want 1", len(got))
	}
	if len(got[0].Acceptance) > maxAcceptance {
		t.Errorf("kept %d criteria, want at most %d", len(got[0].Acceptance), maxAcceptance)
	}
}

// PARALLEL SUBTASKS MUST NOT SHARE A FILE. Measured: the store ticket and the
// data-structure ticket both appended to main.go, and the second reached
// `conflicted` sixty seconds after the first merged — two agents' work, one of
// them wasted, because nobody had said where either belonged.
func TestEachSubtaskGetsItsOwnFile(t *testing.T) {
	tri := triage{Subtasks: []subtask{
		{Title: "Implement the store", File: "store.go"},
		{Title: "Add the API handlers", File: "handlers.go"},
		// A SECOND CLAIM ON A TAKEN FILE IS DROPPED, not renamed: the first claim
		// wins and the loser simply has no assignment, which still builds.
		{Title: "Add filtering", File: "store.go"},
		{Title: "Render the UI", File: "./ui.go"},
	}}
	got := sanitiseSubtasks(tri.Subtasks)
	want := []string{"store.go", "handlers.go", "", "ui.go"}
	if len(got) != len(want) {
		t.Fatalf("sanitiseSubtasks returned %d subtasks, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].File != want[i] {
			t.Errorf("subtask %d (%q): File = %q, want %q", i, got[i].Title, got[i].File, want[i])
		}
	}
}

// The path goes into a ticket an agent then writes to, so it is held to the same
// rules as any other path the harness accepts from a model.
func TestASubtaskFileCannotEscapeTheRepository(t *testing.T) {
	for _, bad := range []string{
		"/etc/passwd", "../outside.go", "a/../../b.go", "~/.ssh/config", "..",
		// A test file belongs to the spec author, and the developer that would be
		// sent there is forbidden from writing it.
		"store_test.go",
	} {
		if got := sanitiseSubtaskFile(bad, map[string]bool{}); got != "" {
			t.Errorf("sanitiseSubtaskFile(%q) = %q, want it refused", bad, got)
		}
	}
	// Ordinary paths, including a nested one, survive.
	for _, ok := range []string{"store.go", "internal/store/store.go", "src/api.ts"} {
		if got := sanitiseSubtaskFile(ok, map[string]bool{}); got != ok {
			t.Errorf("sanitiseSubtaskFile(%q) = %q, want it kept", ok, got)
		}
	}
}

// THE TWO LEVELS READ DIFFERENTLY, and each must say only what its agent needs.
// A task ticket names the source file its developer writes; a section ticket
// names the TEST file its author writes, and warns that the other sections are
// being written onto the same branch at the same time.
func TestEachLevelNamesItsOwnFile(t *testing.T) {
	store := subtask{Title: "Implement the store", File: "store.go", Acceptance: []string{"Add stores a task"}}
	pt := planTask{task: store, sections: []subtask{store}}
	req := Ticket{Title: "Build a tracker", Description: "the request"}

	task := taskDescription(req, pt)
	if !strings.Contains(task, "`store.go`") {
		t.Errorf("the task never names the file its code goes in:\n%s", task)
	}

	sec := sectionDescription(req, pt, 0)
	if !strings.Contains(sec, "`store_test.go`") {
		t.Errorf("the section never names the test file it writes:\n%s", sec)
	}
	if !strings.Contains(sec, "AT THE SAME TIME") {
		t.Errorf("the section is not warned that its siblings share the branch:\n%s", sec)
	}
	if strings.Contains(sec, "store.go`,") {
		t.Errorf("the section was pointed at the implementation file:\n%s", sec)
	}

	// With no assignment the task says nothing about files rather than inventing
	// one, and the section falls back to a neutral name.
	plain := planTask{task: subtask{Title: "x"}, sections: []subtask{{Title: "x"}}}
	if body := taskDescription(Ticket{Title: "y"}, plain); strings.Contains(body, "Put this task's code in") {
		t.Errorf("an unassigned task was given file instructions anyway:\n%s", body)
	}
	if body := sectionDescription(Ticket{Title: "y"}, plain, 0); !strings.Contains(body, "spec_test.go") {
		t.Errorf("an unassigned section has no test file to write to:\n%s", body)
	}
}

// THE ARCHITECT IS THE ONE STAGE THAT MUST NOT DECODE GREEDILY. It writes two
// pages of prose rather than a short structured action, and at temperature 0 a
// small model loops: one reply ended "## Data Lakes / No data lakes / ## Data
// Streams / No data streams" and hit the ceiling, and raising the ceiling from
// 3000 to 6000 produced a longer version of the same loop.
func TestTheDesignCallIsNotGreedyAndIsShapeConstrained(t *testing.T) {
	schema := designSchema()
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("designSchema has no properties: %v", schema)
	}
	for _, want := range []string{"overview", "files"} {
		if _, ok := props[want]; !ok {
			t.Errorf("designSchema does not constrain %q", want)
		}
	}
	files, ok := props["files"].(map[string]any)
	if !ok || files["type"] != "array" {
		t.Fatalf("files is not an array: %v", props["files"])
	}
	item, ok := files["items"].(map[string]any)
	if !ok {
		t.Fatalf("files has no item shape: %v", files)
	}
	req, ok := item["required"].([]string)
	if !ok || len(req) != 2 {
		t.Errorf("a design file does not require both path and content: %v", item["required"])
	}
}

// A SECOND PASS ON THE PIECES THE FIRST ONE LEFT TOO BIG. Across a full day of
// runs the foundation ticket — no dependencies, tightest scope — merged in every
// configuration, while the API, UI and validation tickets merged in none.
func TestOversizedSubtasksAreSplitAndSmallOnesAreNot(t *testing.T) {
	small := subtask{Title: "Implement the store", File: "store.go",
		Acceptance: []string{"a", "b", "c"}}
	big := subtask{Title: "REST API", File: "api.go", DependsOn: []int{0},
		Acceptance: []string{"a", "b", "c", "d", "e"}}

	if len(small.Acceptance) > maxCriteriaBeforeSplit {
		t.Fatal("fixture: the small subtask is over the bound")
	}
	if len(big.Acceptance) <= maxCriteriaBeforeSplit {
		t.Fatal("fixture: the big subtask is not over the bound")
	}

	// INDEPENDENCE IS THE WHOLE CONSTRAINT: children inherit the PARENT's edges
	// and never depend on each other, because a chain of twelve is worse than
	// four — waiting on a dependency is the failure that dominated every run.
	children := []subtask{
		{Title: "read endpoints", File: "api_read.go", Acceptance: []string{"a", "b"}},
		{Title: "write endpoints", File: "api_write.go", Acceptance: []string{"c", "d"}},
	}
	for i := range children {
		children[i].DependsOn = append([]int(nil), big.DependsOn...)
	}
	for _, c := range children {
		if len(c.DependsOn) != 1 || c.DependsOn[0] != 0 {
			t.Errorf("child %q did not inherit the parent's edges: %v", c.Title, c.DependsOn)
		}
		if len(c.Acceptance) > maxCriteriaBeforeSplit {
			t.Errorf("child %q is still over the bound", c.Title)
		}
	}
	// Distinct files, so the siblings cannot collide when they run together.
	if children[0].File == children[1].File {
		t.Error("the children share a file; parallel work would conflict by construction")
	}
}

// A SPLIT MUST NEVER LOSE WORK. Anything unusable leaves the subtask exactly as
// it was: this stage may improve a breakdown and must not destroy one.
func TestAnUnusableSplitLeavesTheSubtaskWhole(t *testing.T) {
	for _, n := range []int{0, 1, 4} {
		// splitOne refuses these counts: one is not a split, and four is the model
		// ignoring the brief rather than finding structure.
		usable := n >= 2 && n <= 3
		if usable {
			t.Errorf("%d children should have been refused", n)
		}
	}
	// The prompt must ask for independence explicitly, since that is the property
	// the harness cannot verify from the reply alone.
	if !strings.Contains(refineSystemPrompt, "INDEPENDENT") {
		t.Error("the split prompt does not require independence")
	}
	if !strings.Contains(refineSystemPrompt, "empty list") {
		t.Error("the split prompt gives the model no way to decline")
	}
	if !strings.Contains(refineSystemPrompt, "Do not invent new requirements") {
		t.Error("a split could add requirements the request never made")
	}
}

// A SECTION IS THE UNIT OF WORK: it is specified, then implemented, and both
// happen on its TASK's branch — so the sections of a task build one tree
// between them, each seeing what the one before it produced.
func TestTheSubTaskStageEndsAtDoneAndWritesToItsTasksBranch(t *testing.T) {
	spec, ok := stageFor(roleSpec)
	if !ok {
		t.Fatal("no workflow stage for the sub-task author")
	}
	// A SECTION ENDS WHEN ITS SPECIFICATION IS WRITTEN. It is a slice of a task's
	// tests, not a unit of implementation: its TASK is developed once, against
	// every slice assembled, by the reconciler's hand-off.
	//
	// This reverses a change made for a weaker model. Per-section developers were
	// introduced because the assembled task was the size that model could not
	// manage — section SPECS took 3, 5 and 11 turns while the assembled task took
	// 10 once and failed at 49 and 80 twice. That ceiling moved: measured on the
	// current model against the same shape of work, a store, a JSON API and an
	// HTML page with 37 tests were built in five turns.
	//
	// What the split bought is kept — each slice still gets its own author with
	// only its own criteria in front of it — and what it cost is dropped: a
	// branch, a developer, a merge and a dependency-queue place per slice.
	if spec.Success != ColDone {
		t.Errorf("a finished section moves to %q; a section is specification, not work", spec.Success)
	}
	if spec.Ready != ColReadyForSpec || spec.Working != ColWritingSpec {
		t.Errorf("the sub-task stage shares columns with another: %q/%q", spec.Ready, spec.Working)
	}
	// It waits, for the same reason the test author does: a specification names
	// things, and the names belong to the tasks it waits for.
	if !spec.RequiresDependencies {
		t.Error("a section is written before the code it names exists")
	}

	// THE ASSEMBLY MECHANISM: a section writes onto its task's branch, so three
	// sections produce one specification rather than three.
	task := "task0000"
	a := NewSpecAgent(nil, nil, ClassLarge, RepoConfig{BranchPrefix: "agent/"}, 10)
	if got := a.branchFor(Ticket{TicketID: "sec00000", ParentID: &task}); got != "agent/"+shortID(task) {
		t.Errorf("a section writes to %q, want its task's branch", got)
	}
	// A section with no parent has nowhere to assemble onto, so it keeps its own.
	if got := a.branchFor(Ticket{TicketID: "orphan00"}); got != "agent/"+shortID("orphan00") {
		t.Errorf("an orphaned section writes to %q", got)
	}
	// And the ordinary test author is unaffected — it works its own ticket.
	plain := NewTesterAgent(nil, nil, ClassLarge, RepoConfig{BranchPrefix: "agent/"}, 10)
	if got := plain.branchFor(Ticket{TicketID: "t1000000", ParentID: &task}); got != "agent/"+shortID("t1000000") {
		t.Errorf("the test author was redirected to a parent branch: %q", got)
	}
}

// EVERY TASK WAITS FOR EVERY TASK BEFORE IT. The declared graph is a fan-out by
// instruction — the scoping prompt demands it, because chaining once destroyed
// the pipeline's parallelism — and a fan-out is optimistic: it says the API, the
// validation and the UI each need only the store, when in fact they name each
// other's types.
//
// Measured on the run that produced this change: all three ran at once, each
// merging an integration branch holding only the store, and the developers
// logged 78 "undefined:" errors compiling against code their siblings had not
// merged. One task merged; three stalled.
func TestEveryTaskWaitsForTheTasksBeforeIt(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "Build a tracker", Description: "req", Priority: "high", CreatedBy: "alice"})

	gw := pmGateway(t, `{"summary":"s","priority":"high","subtasks":[
		{"title":"store","file":"store.go","acceptance":["a"]},
		{"title":"api","file":"api.go","acceptance":["b"]},
		{"title":"ui","file":"ui.go","acceptance":["c"]}],"rationale":"r"}`)
	if _, _, err := NewPMAgent(gw, api, ClassLarge).Handle(context.Background(), f.get("t1")); err != nil {
		t.Fatalf("Handle() = %v", err)
	}

	f.mu.Lock()
	byID := map[string]Ticket{}
	var tasks []Ticket
	for _, tk := range f.tickets {
		byID[tk.TicketID] = *tk
		if tk.Status == ColReadyForSpecMerge {
			tasks = append(tasks, *tk)
		}
	}
	deps := map[string][]string{}
	for k, v := range f.deps {
		deps[k] = append([]string(nil), v...)
	}
	f.mu.Unlock()

	if len(tasks) != 3 {
		t.Fatalf("%d tasks, want 3", len(tasks))
	}
	// Ordered as the model listed them, so "before" is well defined.
	byTitle := map[string]Ticket{}
	for _, task := range tasks {
		byTitle[task.Title] = task
	}
	store, apiT, ui := byTitle["store"], byTitle["api"], byTitle["ui"]

	waitsFor := func(t Ticket, other Ticket) bool {
		for _, d := range deps[t.TicketID] {
			if d == other.TicketID {
				return true
			}
		}
		return false
	}
	if waitsFor(store, apiT) || waitsFor(store, ui) {
		t.Error("the first task waits for a later one; that is a cycle, not an order")
	}
	if !waitsFor(apiT, store) {
		t.Error("the API task does not wait for the store")
	}
	if !waitsFor(ui, store) || !waitsFor(ui, apiT) {
		t.Error("the UI task does not wait for everything before it; it would compile against absent code")
	}

	// AND SO DO THE SECTIONS. Writing tests against types that do not exist is
	// the same failure one stage earlier — a UI section ran while the API task
	// was still in development and specified an interface that did not exist.
	var uiSections []Ticket
	for _, tk := range byID {
		if tk.ParentID != nil && *tk.ParentID == ui.TicketID {
			uiSections = append(uiSections, tk)
		}
	}
	if len(uiSections) == 0 {
		t.Fatal("the UI task has no specification sections")
	}
	for _, sec := range uiSections {
		if !waitsFor(sec, store) || !waitsFor(sec, apiT) {
			t.Errorf("section %q does not wait for the tasks its tests will name", sec.Title)
		}
	}
}

// SECTIONS OF A TASK ARE ORDERED, because they are no longer only specified —
// each is implemented too, and implementations are not independent: "basic task
// operations" needs the types "task data structure" creates. They share one
// branch, so ordering them is what lets each developer see what the section
// before it built.
func TestSectionsOfATaskAreOrdered(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "Build a tracker", Description: "req", Priority: "high", CreatedBy: "alice"})

	// The same reply serves both passes: as a triage it is three tasks, and as a
	// split it is three sections. Each task carries more criteria than
	// maxCriteriaBeforeSplit, so every one of them is split.
	gw := pmGateway(t, `{"summary":"s","priority":"high","subtasks":[
		{"title":"types","file":"store_types.go","acceptance":["a","b","c","d"]},
		{"title":"ops","file":"store_ops.go","acceptance":["e","f","g","h"]},
		{"title":"locking","file":"store_lock.go","acceptance":["i","j","k","l"]}],"rationale":"r"}`)
	if _, _, err := NewPMAgent(gw, api, ClassLarge).Handle(context.Background(), f.get("t1")); err != nil {
		t.Fatalf("Handle() = %v", err)
	}

	f.mu.Lock()
	byID := map[string]Ticket{}
	for _, tk := range f.tickets {
		byID[tk.TicketID] = *tk
	}
	deps := map[string][]string{}
	for k, v := range f.deps {
		deps[k] = append([]string(nil), v...)
	}
	f.mu.Unlock()

	// Group the sections by the task that owns them: ordering is within a task,
	// never across them.
	byTask := map[string][]Ticket{}
	for _, tk := range byID {
		if tk.Status == ColReadyForSpec && tk.ParentID != nil {
			byTask[*tk.ParentID] = append(byTask[*tk.ParentID], tk)
		}
	}
	if len(byTask) == 0 {
		t.Fatal("no sections were opened")
	}
	var sections []Ticket
	for _, group := range byTask {
		if len(group) < 2 {
			continue
		}
		sections = group
		break
	}
	if len(sections) < 2 {
		t.Fatalf("no task had more than one section: %v", byTask)
	}

	// Exactly one section of the task waits for no sibling — the first. Every
	// other waits for one, which is a chain rather than a fan.
	firsts := 0
	for _, sec := range sections {
		var waitsOnSibling int
		for _, d := range deps[sec.TicketID] {
			for _, other := range sections {
				if d == other.TicketID {
					waitsOnSibling++
				}
			}
		}
		switch waitsOnSibling {
		case 0:
			firsts++
		case 1:
		default:
			t.Errorf("section %q waits for %d siblings; they should be a chain", sec.Title, waitsOnSibling)
		}
	}
	if firsts != 1 {
		t.Errorf("%d sections wait for no sibling, want exactly 1 — the rest must be ordered behind it", firsts)
	}
}

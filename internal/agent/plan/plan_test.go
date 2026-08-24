package plan

import (
	"strings"
	"testing"
)

func titles(in []Subtask) []string {
	out := make([]string, len(in))
	for i, st := range in {
		out[i] = st.Title
	}
	return out
}

func has(in []Subtask, title string) bool {
	for _, st := range in {
		if st.Title == title {
			return true
		}
	}
	return false
}

// SEQUENCING IS THE NATURAL WAY TO DESCRIBE WORK, so it is what gets emitted —
// and a six-deep chain means nothing runs beside anything else, with a single
// failure stranding everything behind it. Measured: five minutes of work,
// thirteen minutes of deadlock.
func TestAChainIsFlattenedIntoFanOut(t *testing.T) {
	in := []Subtask{
		{Title: "store", File: "store.go"},
		{Title: "handlers", File: "handlers.go", DependsOn: []int{0}},
		{Title: "validation", File: "validation.go", DependsOn: []int{1}},
		{Title: "filtering", File: "filtering.go", DependsOn: []int{2}},
	}

	got := SanitiseSubtasks(in)
	if len(got) != 4 {
		t.Fatalf("kept %d subtasks: %v", len(got), titles(got))
	}
	// The foundation depends on nothing; everything else depends only on it.
	if len(got[0].DependsOn) != 0 {
		t.Errorf("the foundation waits for %v", got[0].DependsOn)
	}
	for i := 1; i < len(got); i++ {
		if len(got[i].DependsOn) != 1 || got[i].DependsOn[0] != 0 {
			t.Errorf("%q waits for %v, want only the foundation", got[i].Title, got[i].DependsOn)
		}
	}
}

// A subtask resting on two independent foundations keeps both.
func TestASubtaskKeepsEveryRootItRestsOn(t *testing.T) {
	in := []Subtask{
		{Title: "store", File: "a.go"},
		{Title: "config", File: "b.go"},
		{Title: "handlers", File: "c.go", DependsOn: []int{0, 1}},
		{Title: "api", File: "d.go", DependsOn: []int{2}},
	}
	got := SanitiseSubtasks(in)

	api := got[3]
	if len(api.DependsOn) != 2 || api.DependsOn[0] != 0 || api.DependsOn[1] != 1 {
		t.Errorf("the leaf waits for %v, want both foundations", api.DependsOn)
	}
}

// DROPPING SHIFTS EVERY LATER POSITION, so an edge into a dropped subtask must
// be dropped rather than silently pointing at whatever moved into that slot.
func TestAnEdgeIntoADroppedSubtaskIsDroppedNotReaimed(t *testing.T) {
	in := []Subtask{
		{Title: "Write the README", File: "README.md"}, // dropped: not code work
		{Title: "store", File: "store.go"},
		{Title: "handlers", File: "handlers.go", DependsOn: []int{0}}, // pointed at the dropped one
	}

	got := SanitiseSubtasks(in)
	if len(got) != 2 {
		t.Fatalf("kept %v", titles(got))
	}
	handlers := got[1]
	if len(handlers.DependsOn) != 0 {
		t.Errorf("%q waits for %v; the edge pointed at a subtask that no longer exists",
			handlers.Title, handlers.DependsOn)
	}
}

// An edge that survives must still point at the RIGHT subtask after the shift.
func TestASurvivingEdgeIsTranslatedToItsNewIndex(t *testing.T) {
	in := []Subtask{
		{Title: "Document the API", File: "docs.md"}, // dropped
		{Title: "store", File: "store.go"},
		{Title: "handlers", File: "handlers.go", DependsOn: []int{1}}, // the store
	}

	got := SanitiseSubtasks(in)
	if len(got) != 2 {
		t.Fatalf("kept %v", titles(got))
	}
	if len(got[1].DependsOn) != 1 || got[1].DependsOn[0] != 0 {
		t.Errorf("%q waits for %v, want the store at its new index 0", got[1].Title, got[1].DependsOn)
	}
}

// IT FAILS OPEN: a cycle leaves every subtask waiting on another and none of
// them workable, so every edge goes rather than a guess being made about which
// one to cut.
func TestACycleDropsEveryEdge(t *testing.T) {
	in := []Subtask{
		{Title: "a", File: "a.go", DependsOn: []int{1}},
		{Title: "b", File: "b.go", DependsOn: []int{2}},
		{Title: "c", File: "c.go", DependsOn: []int{0}},
	}
	if !HasCycle(in) {
		t.Fatal("HasCycle did not see the cycle")
	}

	got := SanitiseSubtasks(in)
	for _, st := range got {
		if len(st.DependsOn) != 0 {
			t.Errorf("%q still waits for %v after a cycle was found", st.Title, st.DependsOn)
		}
	}
}

func TestSelfAndDuplicateEdgesAreDropped(t *testing.T) {
	in := []Subtask{
		{Title: "store", File: "store.go"},
		{Title: "handlers", File: "handlers.go", DependsOn: []int{0, 0, 1, 99, -1}},
	}
	got := SanitiseSubtasks(in)

	deps := got[1].DependsOn
	if len(deps) != 1 || deps[0] != 0 {
		t.Errorf("edges = %v, want only the one real dependency", deps)
	}
}

// THE PROMPT ALREADY FORBIDS BOTH, IN CAPITALS, and a model emitted them anyway
// — twice on the same request. A ticket asking for either is UNSATISFIABLE: the
// author may write only test files, so a README ticket has no permitted action
// that reaches its finish condition.
func TestWorkThisDepartmentCannotBuildIsDropped(t *testing.T) {
	for _, title := range []string{
		"Write the README",
		"Create README.md",
		"Update the documentation",
		"Document the API endpoints",
		"Add a CHANGELOG",
		"Write the tests for the store",
		"Add tests covering the handlers",
		"Create a test suite",
		"Add test coverage for filtering",
	} {
		if !NotCodeWork(title) {
			t.Errorf("NotCodeWork(%q) = false; this ticket cannot be built by any stage", title)
		}
	}
}

// DELIBERATELY NARROW: dropping real work is far worse than letting one odd
// ticket through, so the match anchors on the LEADING VERB rather than searching
// the whole title.
func TestRealWorkThatMerelyMentionsDocsOrTestsSurvives(t *testing.T) {
	for _, title := range []string{
		"Implement filtering and document the query parameters",
		"Build the store",
		"Fix the README parser",              // the subject is a parser
		"Add a test harness for the runner",  // singular: a modifier on something real
		"Add a test endpoint for smoke runs", // ditto
		"Add a test command to the CLI",
		"Update the store to handle nil maps",
		"Create the task store",
		"Write the HTTP handlers",
		"Refactor the documentation generator",
	} {
		if NotCodeWork(title) {
			t.Errorf("NotCodeWork(%q) = true; real work was dropped", title)
		}
	}
}

// A FILE CLAIMED TWICE defeats the whole point of the assignment: two subtasks
// editing one file is the single most common way parallel work is lost.
func TestAFileIsClaimedOnceAndTheFirstClaimWins(t *testing.T) {
	in := []Subtask{
		{Title: "store", File: "main.go"},
		{Title: "handlers", File: "main.go"},
		{Title: "filtering", File: "filter.go"},
	}
	got := SanitiseSubtasks(in)

	if got[0].File != "main.go" {
		t.Errorf("the first claim was not kept: %q", got[0].File)
	}
	// The later claim is dropped rather than RENAMED: a guess would send an agent
	// to a file nobody meant.
	if got[1].File != "" {
		t.Errorf("the second claim on main.go became %q", got[1].File)
	}
	if got[2].File != "filter.go" {
		t.Errorf("an unrelated file was disturbed: %q", got[2].File)
	}
}

// THE MODEL SUPPLIES THIS STRING and it becomes a path an agent is pointed at.
func TestAFileThatEscapesTheTreeOrNamesATestIsDropped(t *testing.T) {
	for _, p := range []string{
		"/etc/passwd",
		"~/secrets.go",
		"../../outside.go",
		"..",
		"a/../../b.go",
		"store_test.go", // the author's to write, not this stage's to assign
		"a/b/handlers_test.go",
		strings.Repeat("x", MaxItemRunes+1) + ".go",
	} {
		if got := SanitiseFile(p, map[string]bool{}); got != "" {
			t.Errorf("SanitiseFile(%q) = %q, want it dropped", p, got)
		}
	}

	for _, p := range []string{"store.go", "a/b/handlers.go", "./filter.go"} {
		if got := SanitiseFile(p, map[string]bool{}); got == "" {
			t.Errorf("SanitiseFile(%q) dropped a legitimate file", p)
		}
	}
	if got := SanitiseFile("./store.go", map[string]bool{}); got != "store.go" {
		t.Errorf("SanitiseFile(./store.go) = %q, want it cleaned", got)
	}
}

// AN UNKNOWN PRIORITY LEAVES THE TICKET'S OWN VALUE ALONE rather than
// overwriting what a person chose with a guess.
func TestOnlyAKnownPriorityIsForwarded(t *testing.T) {
	for _, p := range Priorities {
		if got := Sanitise(Triage{Priority: p}); got.Priority != p {
			t.Errorf("a valid priority %q became %q", p, got.Priority)
		}
	}
	for _, p := range []string{"", "urgent", "P1", "CRITICAL", "as soon as possible"} {
		if got := Sanitise(Triage{Priority: p}); got.Priority != "" {
			t.Errorf("priority %q was forwarded as %q", p, got.Priority)
		}
	}
}

// A model that emits a very long response must not turn into a very long ticket
// comment.
func TestABreakdownIsBoundedInEveryDirection(t *testing.T) {
	var many []Subtask
	for i := 0; i < 50; i++ {
		many = append(many, Subtask{
			Title:      strings.Repeat("t", 400),
			Acceptance: []string{"a", "b", "c", "d", "e", "f", "g", "h"},
		})
	}
	var labels, questions []string
	for i := 0; i < 50; i++ {
		labels = append(labels, "label")
		questions = append(questions, "a question")
	}

	got := Sanitise(Triage{
		Summary: strings.Repeat("s", 5000), Rationale: strings.Repeat("r", 5000),
		Labels: labels, NeedsDetail: questions, Subtasks: many,
	})

	if len(got.Subtasks) != MaxSubtasks {
		t.Errorf("kept %d subtasks, want at most %d", len(got.Subtasks), MaxSubtasks)
	}
	if len(got.Labels) != MaxLabels {
		t.Errorf("kept %d labels", len(got.Labels))
	}
	if len(got.NeedsDetail) != MaxNeedsDetail {
		t.Errorf("kept %d questions", len(got.NeedsDetail))
	}
	if len([]rune(got.Summary)) > MaxSummaryRunes+1 {
		t.Errorf("the summary is %d runes", len([]rune(got.Summary)))
	}
	for _, st := range got.Subtasks {
		if len(st.Acceptance) > MaxAcceptance {
			t.Errorf("%d criteria on one subtask", len(st.Acceptance))
		}
		if len([]rune(st.Title)) > MaxItemRunes+1 {
			t.Errorf("a title is %d runes", len([]rune(st.Title)))
		}
	}
}

func TestASubtaskWithNoTitleIsNotAUnitOfWork(t *testing.T) {
	got := SanitiseSubtasks([]Subtask{
		{Title: "   ", File: "a.go"},
		{Title: "store", File: "b.go"},
	})
	if len(got) != 1 || got[0].Title != "store" {
		t.Errorf("kept %v", titles(got))
	}
}

func TestBlankLabelsAndQuestionsAreDropped(t *testing.T) {
	got := Sanitise(Triage{
		Labels:      []string{"api", "  ", "", "store"},
		NeedsDetail: []string{"", "which format?", "   "},
	})
	if len(got.Labels) != 2 {
		t.Errorf("labels = %v", got.Labels)
	}
	if len(got.NeedsDetail) != 1 || got.NeedsDetail[0] != "which format?" {
		t.Errorf("questions = %v", got.NeedsDetail)
	}
}

// THE ONLY TICKET THAT EVER MERGES IS THE SMALLEST ONE, so a unit asking for
// more than a few things is split rather than built.
func TestAUnitAskingForTooMuchIsMarkedForSplitting(t *testing.T) {
	small := Subtask{Title: "store", Acceptance: []string{"a", "b", "c"}}
	if NeedsSplit(small) {
		t.Error("a three-criterion unit was marked for splitting")
	}
	large := Subtask{Title: "the API", Acceptance: []string{"a", "b", "c", "d"}}
	if !NeedsSplit(large) {
		t.Error("a four-criterion unit was left whole")
	}
}

// A SPLIT INTO ONE IS NOT A SPLIT, and anything past three is the model ignoring
// the brief rather than finding structure.
func TestOnlyATwoOrThreeWaySplitIsTaken(t *testing.T) {
	cases := map[int]bool{0: false, 1: false, 2: true, 3: true, 4: false, 9: false}
	for n, want := range cases {
		children := make([]Subtask, n)
		if got := AcceptSplit(children); got != want {
			t.Errorf("AcceptSplit(%d children) = %v, want %v", n, got, want)
		}
	}
}

// A ticket that is already one unit of work returns no subtasks, and that has to
// survive sanitising rather than becoming something else.
func TestAnEmptyBreakdownStaysEmpty(t *testing.T) {
	got := Sanitise(Triage{Summary: "one thing", Priority: "medium"})
	if len(got.Subtasks) != 0 {
		t.Errorf("subtasks = %v on a request that needs no breakdown", titles(got.Subtasks))
	}
	if got.Summary != "one thing" || got.Priority != "medium" {
		t.Errorf("the rest of the triage was disturbed: %+v", got)
	}
}

// The whole breakdown, end to end: a model's plausible-but-wrong output becoming
// something the pipeline can actually run.
func TestARealisticBreakdownIsNormalisedIntoSomethingRunnable(t *testing.T) {
	got := Sanitise(Triage{
		Priority: "high",
		Summary:  "build a task manager",
		Subtasks: []Subtask{
			{Title: "Create the task store", File: "store.go", Acceptance: []string{"Add stores a task"}},
			{Title: "Write the HTTP handlers", File: "handlers.go", DependsOn: []int{0}},
			{Title: "Add validation", File: "handlers.go", DependsOn: []int{1}}, // file already claimed
			{Title: "Write the tests", File: "store_test.go", DependsOn: []int{2}},
			{Title: "Update the README", File: "README.md", DependsOn: []int{3}},
			{Title: "Add filtering", File: "filter.go", DependsOn: []int{2}},
		},
	})

	// The two unbuildable tickets are gone.
	if has(got.Subtasks, "Write the tests") || has(got.Subtasks, "Update the README") {
		t.Errorf("an unbuildable ticket survived: %v", titles(got.Subtasks))
	}
	if len(got.Subtasks) != 4 {
		t.Fatalf("kept %v", titles(got.Subtasks))
	}
	// The chain is fan-out: everything rests on the store.
	for i, st := range got.Subtasks {
		if i == 0 {
			if len(st.DependsOn) != 0 {
				t.Errorf("the foundation waits for %v", st.DependsOn)
			}
			continue
		}
		if len(st.DependsOn) != 1 || st.DependsOn[0] != 0 {
			t.Errorf("%q waits for %v, want only the foundation", st.Title, st.DependsOn)
		}
	}
	// The duplicate file claim was dropped, not renamed.
	for _, st := range got.Subtasks {
		if st.Title == "Add validation" && st.File != "" {
			t.Errorf("a second claim on handlers.go became %q", st.File)
		}
	}
}

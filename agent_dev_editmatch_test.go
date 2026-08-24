package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const matchSrc = "package main\n\nfunc add(a, b int) int {\n\treturn a + b\n}\n\nfunc sub(a, b int) int {\n\treturn a - b\n}\n"

// The escalated no-op message must still be counted as a no-op edit; matching one
// tense only silently moved it into "other".
func TestRefusalKindOfCoversBothNoopTenses(t *testing.T) {
	for _, notice := range []string{
		"Rejected: your edits produced no change — the file already contained exactly what you wrote.",
		"Rejected: that is the 2nd edit in a row that changed nothing — the file already contains exactly what you wrote.",
		"Rejected: you have not changed anything yet, so there is nothing to test.",
	} {
		if got := refusalKindOf(notice); got != RefusalNoopEdit {
			t.Errorf("refusalKindOf(%.50q) = %q, want %q", notice, got, RefusalNoopEdit)
		}
	}
}

// THE TRAP THIS GUARDS AGAINST. A no-op edit is refused, and a verification is
// refused whenever nothing has changed since the last one. If the no-op advice
// says "run the tests" while that is true, the agent has no legal move left and
// spins until it is killed — measured on r59.
func TestNoopEditNoticeNeverSendsAVerifiedTreeBackToTheTests(t *testing.T) {
	for _, tc := range []struct {
		name       string
		noopEdits  int
		testsPass  bool
		wantAction string
	}{
		{"verified and passing → finish", 1, true, "finish"},
		{"verified and passing, repeated → finish", 5, true, "finish"},
		{"verified and failing → a different change", 1, false, "DIFFERENT"},
		{"verified and failing, repeated → a different change", 5, false, "DIFFERENT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := noopEditNotice(tc.noopEdits, true, tc.testsPass, "FAIL\ttracker\t0.01s")
			if !strings.Contains(got, tc.wantAction) {
				t.Errorf("notice does not point at %q:\n%s", tc.wantAction, got)
			}
			if strings.Contains(got, "RUN THE TESTS") {
				t.Errorf("told a verified tree to run the tests, which the verify guard refuses:\n%s", got)
			}
		})
	}
}

// NO ADVICE MAY NAME A TOOL THAT NO LONGER EXISTS. This used to assert the
// opposite — that an unverified tree was told to run the tests — which was right
// until run_tests left the vocabulary. An instruction the agent cannot follow is
// worse than none: it spent 28 turns being told to call something absent.
func TestNoopEditNoticeNeverNamesARemovedTool(t *testing.T) {
	for _, n := range []string{
		noopEditNotice(1, false, false, ""),
		noopEditNotice(2, false, false, ""),
		noopEditNotice(1, true, true, ""),
		noopEditNotice(1, true, false, "FAIL"),
	} {
		for _, gone := range []string{"run_tests", "RUN THE TESTS", "give_up", "call finish"} {
			if strings.Contains(n, gone) {
				t.Errorf("advice names %q, which is not an action:\n%s", gone, n)
			}
		}
	}
	// An unverified tree must still be pointed at the only thing it can do: write
	// something that is actually different.
	got := noopEditNotice(2, false, false, "")
	if !strings.Contains(got, "different") {
		t.Errorf("an unverified no-op is not told to make a real change:\n%s", got)
	}
}

// Every branch must classify as a no-op edit, or the metric undercounts.
func TestEveryNoopNoticeIsClassifiedAsNoopEdit(t *testing.T) {
	for _, n := range []string{
		noopEditNotice(1, false, false, ""),
		noopEditNotice(2, false, false, ""),
		noopEditNotice(1, true, true, ""),
		noopEditNotice(1, true, false, "FAIL"),
	} {
		if got := refusalKindOf(n); got != RefusalNoopEdit {
			t.Errorf("refusalKindOf(%.60q) = %q, want %q", n, got, RefusalNoopEdit)
		}
	}
}

// A retry opens with nothing staged but a branch full of earlier commits. Asking
// where it stands must be allowed, or the agent has to write something before it
// can find out — and writing what the branch already holds is refused as a no-op.
// Measured on r62: 62 refusals on one section, every one of them this message.
func TestNothingToTestOnlyAppliesWhenThereIsTrulyNothing(t *testing.T) {
	cases := []struct {
		name     string
		lastTest string
		tree     []string
		refuse   bool
	}{
		{"retry inheriting a branch, no verdict yet", "", []string{"main.go", "store_test.go"}, false},
		{"already has a verdict this attempt", "ok tracker", []string{"main.go"}, true},
		{"nothing staged and nothing on the branch", "", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &devState{lastTest: tc.lastTest, tree: tc.tree}
			if refused := s.nothingToTest(); refused != tc.refuse {
				t.Errorf("refused = %v, want %v", refused, tc.refuse)
			}
		})
	}
}

// A no-op write must not look like a change, or every guard that asks "has the
// tree moved since the last test?" gets a false yes. r63 spent 80 refusals on one
// section alternating "that changed nothing" with "nothing has changed since you
// last ran the tests".
func TestChangedFromBaseline(t *testing.T) {
	cases := []struct {
		name     string
		baseline map[string]string
		staged   map[string]string
		want     bool
	}{
		{"rewrote identical content", map[string]string{"main.go": "a\n"}, map[string]string{"main.go": "a\n"}, false},
		{"real change", map[string]string{"main.go": "a\n"}, map[string]string{"main.go": "b\n"}, true},
		{"new file the branch does not have", map[string]string{}, map[string]string{"store.go": "a\n"}, true},
		{"nothing staged", map[string]string{"main.go": "a\n"}, map[string]string{}, false},
		{"one of two files changed", map[string]string{"a.go": "1\n", "b.go": "2\n"},
			map[string]string{"a.go": "1\n", "b.go": "CHANGED\n"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &devState{baseline: tc.baseline, staged: tc.staged}
			if got := s.changedFromBaseline(); got != tc.want {
				t.Errorf("changedFromBaseline() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A line range names a position in the file AS THE MODEL SAW IT. Applying an edit
// near the top shifts everything below it, so edits must be applied bottom-up or a
// later range lands somewhere the model never looked.
func TestMultipleEditsToOneFileApplyBottomUp(t *testing.T) {
	before := "package main\n\nvar a = 1\nvar b = 2\nvar c = 3\n"
	s := &devState{
		tree: []string{"main.go"}, staged: map[string]string{}, baseline: map[string]string{},
		read: map[string]string{"main.go": before}, missing: map[string]bool{},
	}
	// Given in TOP-DOWN order, as a model naturally would. The first edit removes a
	// line, so if they applied in this order the second would hit the wrong one.
	err := applyEdits(s, []devEdit{
		{Path: "main.go", StartLine: 3, EndLine: 3, Replace: "var a = 10"},
		{Path: "main.go", StartLine: 5, EndLine: 5, Replace: "var c = 30"},
	}, modeDevelop)
	if err != nil {
		t.Fatalf("applyEdits() = %v", err)
	}
	want := "package main\n\nvar a = 10\nvar b = 2\nvar c = 30\n"
	if s.staged["main.go"] != want {
		t.Errorf("staged =\n%q\nwant\n%q", s.staged["main.go"], want)
	}
}

// A replacement spanning several lines shifts the ones after it; the same rule
// covers it, and this is the case where getting the order wrong corrupts silently.
func TestAMultiLineReplacementDoesNotDisturbLaterEdits(t *testing.T) {
	before := "package main\n\nvar a = 1\nvar b = 2\nvar c = 3\n"
	s := &devState{
		tree: []string{"main.go"}, staged: map[string]string{}, baseline: map[string]string{},
		read: map[string]string{"main.go": before}, missing: map[string]bool{},
	}
	err := applyEdits(s, []devEdit{
		{Path: "main.go", StartLine: 3, EndLine: 3, Replace: "var a = 1\nvar a2 = 11\nvar a3 = 12"},
		{Path: "main.go", StartLine: 5, EndLine: 5, Replace: "var c = 30"},
	}, modeDevelop)
	if err != nil {
		t.Fatalf("applyEdits() = %v", err)
	}
	want := "package main\n\nvar a = 1\nvar a2 = 11\nvar a3 = 12\nvar b = 2\nvar c = 30\n"
	if s.staged["main.go"] != want {
		t.Errorf("staged =\n%q\nwant\n%q", s.staged["main.go"], want)
	}
}

// Mixing the two forms for one file describes two different files; picking a
// winner would silently discard one of them.
func TestAWholeFileAndALineEditForTheSameFileIsRefused(t *testing.T) {
	s := &devState{
		tree: []string{"main_test.go"}, staged: map[string]string{}, baseline: map[string]string{},
		read: map[string]string{"main_test.go": "package main\n\nvar x = 1\n"}, missing: map[string]bool{},
	}
	err := applyEdits(s, []devEdit{
		{Path: "main_test.go", Replace: "package main\n"},
		{Path: "main_test.go", StartLine: 3, EndLine: 3, Replace: "var x = 2"},
	}, modeTest)
	if err == nil {
		t.Fatal("a whole-file replacement and a line edit for the same file were both accepted")
	}
	if !strings.Contains(err.Error(), "two different files") {
		t.Errorf("the refusal does not explain: %v", err)
	}
}

// Deleting lines is legitimate: an empty replacement removes the range.
func TestAnEmptyReplacementDeletesTheRange(t *testing.T) {
	s := &devState{
		tree: []string{"main.go"}, staged: map[string]string{}, baseline: map[string]string{},
		read: map[string]string{"main.go": "package main\n\nvar a = 1\nvar b = 2\n"}, missing: map[string]bool{},
	}
	if err := applyEdits(s, []devEdit{{Path: "main.go", StartLine: 3, EndLine: 3}}, modeDevelop); err != nil {
		t.Fatalf("applyEdits() = %v", err)
	}
	if s.staged["main.go"] != "package main\n\nvar b = 2\n" {
		t.Errorf("staged = %q", s.staged["main.go"])
	}
}

// Removing run_tests made verification a consequence of a successful write, which
// left an agent that cannot land one with no verdict at all — and the broken-spec
// hand-back waits on exactly that verdict. A refused test-file edit is the one
// refusal that means "I think the tests are the problem", so it earns the check.
func TestARefusedTestFileEditEarnsAVerification(t *testing.T) {
	notice := "Rejected: store_test.go is a test file, and tests are written by a separate agent"
	if got := refusalKindOf(notice); got != RefusalTestFile {
		t.Fatalf("refusalKindOf = %q, want %q — the trigger keys off this", got, RefusalTestFile)
	}
	// With no verdict yet and a branch that has content, the check is warranted.
	s := &devState{lastTest: "", tree: []string{"store_test.go"}}
	if s.nothingToTest() {
		t.Error("a branch with files was treated as having nothing to test")
	}
	// Once a verdict exists it must NOT fire again: that would be a push, a clone
	// and a full suite for every subsequent refusal.
	s.lastTest = "--- FAIL: TestX"
	if !s.nothingToTest() {
		t.Error("a branch already verified this attempt would be verified again")
	}
}

// THE COUNTERS DESYNCED NINE TIMES. A no-op counted as a write; a push with
// nothing to commit returned early; a gate rejection skipped the update. Each was
// a different code path forgetting to keep two numbers in step, and each cost a
// run. A fingerprint is derived from the tree instead of maintained alongside it,
// so there is nothing left to forget.
func TestTreeVerifiedFollowsTheTreeNotACounter(t *testing.T) {
	s := &devState{staged: map[string]string{"main.go": "package main\n"}}

	// Nothing has run yet.
	if s.treeVerified() {
		t.Error("an unverified tree reported itself verified")
	}

	// A verification runs against this exact tree.
	s.lastTest, s.verifiedTree = "ok", s.treeHash()
	if !s.treeVerified() {
		t.Error("the tree that was just verified does not report itself verified")
	}

	// Any change to the tree invalidates it, with no counter to update.
	s.staged["main.go"] = "package main // changed\n"
	if s.treeVerified() {
		t.Error("a changed tree still reported itself verified")
	}

	// Changing it BACK makes the old verdict valid again, which is correct: it is
	// the same tree that was tested, whatever route it took to get here.
	s.staged["main.go"] = "package main\n"
	if !s.treeVerified() {
		t.Error("a tree restored to the verified content was not recognised")
	}

	// A new file counts as a change even though nothing existing was touched.
	s.staged["store.go"] = "package main\n"
	if s.treeVerified() {
		t.Error("adding a file left the tree looking verified")
	}
}

// The fingerprint must not depend on map iteration order.
func TestTreeHashIsStableAcrossInsertionOrder(t *testing.T) {
	a := &devState{staged: map[string]string{"a.go": "1", "b.go": "2", "c.go": "3"}}
	b := &devState{staged: map[string]string{}}
	for _, p := range []string{"c.go", "a.go", "b.go"} {
		b.staged[p] = map[string]string{"a.go": "1", "b.go": "2", "c.go": "3"}[p]
	}
	if a.treeHash() != b.treeHash() {
		t.Error("the same tree hashed differently depending on insertion order")
	}
}

// TEN SEPARATE SITES NAMED A TOOL THAT NO LONGER EXISTED, and each one cost a
// fixture pass to find: the no-op advice, the refusal escalation, the push-failure
// notice, the stale-read advice, the repeat-verification guard, finishAdvice, a
// schema description, and three places in the prompts. Removing an action from
// devActions is not a one-line change — every string that mentions it becomes an
// instruction the agent cannot follow, and it has no way to know.
//
// So the sweep is a test rather than a discipline.
func TestNothingOfferedToTheModelNamesARemovedAction(t *testing.T) {
	// FIELDS AS WELL AS ACTIONS. The first version of this test checked only the
	// three removed tools, and missed that the whole-file guard was still telling
	// developers to `Give the exact text you want to change in "search"` — a field
	// deleted with the edit format. That cost a full pipeline run: 80 of r64's 81
	// refusals were that one message.
	removed := []string{"run_tests", "give_up", "call finish", `"search"`, "empty search"}

	surfaces := map[string]string{
		"developer system prompt": devSystemPrompt(),
		"tester system prompt":    testerSystemPrompt(),
	}
	for _, n := range []string{
		noopEditNotice(1, false, false, ""),
		noopEditNotice(2, false, false, ""),
		noopEditNotice(1, true, true, ""),
		noopEditNotice(1, true, false, "FAIL"),
		(&devState{refusals: 2}).directive(),
		(&devState{refusals: 9, writes: 2}).directive(),
	} {
		surfaces["runtime advice: "+n[:min(40, len(n))]] = n
	}
	for _, tool := range devTools(modeDevelop) {
		surfaces["tool "+tool.Name] = tool.Description
	}
	for _, tool := range devTools(modeTest) {
		surfaces["test-mode tool "+tool.Name] = tool.Description
	}

	for where, text := range surfaces {
		for _, gone := range removed {
			if strings.Contains(text, gone) {
				t.Errorf("%s names %q, which the agent cannot call", where, gone)
			}
		}
	}
}

// THE PROMPT MUST TEACH THE FORMAT THE HARNESS ACTUALLY ACCEPTS. r64 failed with
// 80 of 81 refusals on one message because the developer's worked example still
// showed search/replace after the edit format had changed to line ranges: the
// model wrote what it was taught, and the harness refused what it wrote.
//
// A sweep for removed names cannot catch this — the prompt has to POSITIVELY
// teach the current fields.
func TestPromptsTeachTheCurrentEditFormat(t *testing.T) {
	for name, p := range map[string]string{
		"developer": devSystemPrompt(),
		"tester":    testerSystemPrompt(),
	} {
		for _, want := range []string{"start_line", "end_line", "replace"} {
			if !strings.Contains(p, want) {
				t.Errorf("%s prompt never mentions %q, so the model cannot use the edit format", name, want)
			}
		}
		// And it must not still be teaching the retired one.
		for _, gone := range []string{`search:`, `"search"`, "search/replace"} {
			if strings.Contains(p, gone) {
				t.Errorf("%s prompt still teaches %q, which the harness no longer accepts", name, gone)
			}
		}
	}
	// The tool description the model actually receives must agree with the prompt,
	// and must lead with TEXT addressing — that ordering is the change, not a
	// wording preference. Line arithmetic is what r69 could not do: 77 refusals on
	// one ticket, 41 of them a range cutting across a function, while the model's
	// diagnosis of the real bug never wavered.
	var wrote, undo bool
	for _, tool := range devTools(modeDevelop) {
		switch tool.Name {
		case actionWriteFiles:
			wrote = true
			for _, want := range []string{"old_str", "decl"} {
				if !strings.Contains(tool.Description, want) {
					t.Errorf("write_files does not offer %q: %s", want, tool.Description)
				}
			}
			// Text first, lines demoted — if lines are described before old_str the
			// model reaches for them, which is the behaviour being retired.
			if i, j := strings.Index(tool.Description, "old_str"), strings.Index(tool.Description, "Line numbers"); i > j {
				t.Error("write_files describes line numbers before old_str, so lines still read as the primary route")
			}
		case actionUndoEdit:
			undo = true
			if !strings.Contains(tool.Description, "back") {
				t.Errorf("undo_edit does not say what it does: %s", tool.Description)
			}
		}
	}
	if !wrote {
		t.Error("write_files is not offered at all")
	}
	if !undo {
		t.Error("undo_edit is not offered, so a broken file has no recovery")
	}
}

// r65 spent 78 of its 87 refusals on a developer asking to replace main.go
// wholesale. It was right to want that — main.go arrives as a six-line stub and
// writing an implementation into it is not a two-line edit — and a line range
// already expressed it. Nothing told it so, and the refusal only said no.
func TestReplacingAWholeExistingFileByRangeIsAllowed(t *testing.T) {
	stub := "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"tracker\")\n}\n"
	s := &devState{
		tree: []string{"main.go"}, staged: map[string]string{}, baseline: map[string]string{},
		read: map[string]string{"main.go": stub}, missing: map[string]bool{},
	}
	impl := "package main\n\ntype Store struct{}\n\nfunc NewStore() *Store { return &Store{} }\n"
	if err := applyEdits(s, []devEdit{{Path: "main.go", StartLine: 1, EndLine: 7, Replace: impl}}, modeDevelop); err != nil {
		t.Fatalf("replacing a whole file by explicit range was refused: %v", err)
	}
	if s.staged["main.go"] != impl {
		t.Errorf("staged =\n%q\nwant\n%q", s.staged["main.go"], impl)
	}
}

// ...and the refusal for the blind form must NAME that route, or the agent has
// no way to discover it.
func TestTheWholeFileRefusalNamesTheRangeRoute(t *testing.T) {
	stub := "package main\n\nfunc main() {}\n"
	s := &devState{
		tree: []string{"main.go"}, staged: map[string]string{}, baseline: map[string]string{},
		read: map[string]string{"main.go": stub}, missing: map[string]bool{},
	}
	err := applyEdits(s, []devEdit{{Path: "main.go", Replace: "package main\n"}}, modeDevelop)
	if err == nil {
		t.Fatal("a blind whole-file write to a shared file was accepted")
	}
	for _, want := range []string{"start_line 1", "end_line 3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not tell the agent to use %q:\n%s", want, err)
		}
	}
}

// THE LINE RANGE IS REQUIRED, and that is enforcement rather than instruction.
// Left optional, the model omitted it on every single write — forty in a row on
// the implementation fixture — because {path, replace} is the shape it reaches
// for. Naming the range route in the refusal changed nothing. Replies are
// grammar-constrained, so a required field simply cannot be left out.
func TestTheEditScheduleRequiresItsLineRange(t *testing.T) {
	for _, mode := range []agentMode{modeDevelop, modeTest, modeCoverage} {
		for _, tool := range devTools(mode) {
			if tool.Name != actionWriteFiles {
				continue
			}
			edits, ok := tool.Parameters["properties"].(map[string]any)["edits"].(map[string]any)
			if !ok {
				t.Fatal("write_files has no edits property")
			}
			// ONE SHAPE PER ADDRESS. The flat item this replaced carried old_str,
			// decl and a line range together, and a constrained sampler walking
			// adjacent long fields copied the quote into the replacement in 39 of 47
			// turns. Each branch must still demand everything it needs.
			item := edits["items"].(map[string]any)
			branches, ok := item["oneOf"].([]any)
			if !ok {
				t.Fatalf("mode %v: the edit item is not branched by address", mode)
			}
			addresses := map[string]bool{}
			for _, b := range branches {
				m := b.(map[string]any)
				req, _ := m["required"].([]string)
				has := map[string]bool{}
				for _, r := range req {
					has[r] = true
				}
				// Every edit needs somewhere to go and something to put there.
				for _, always := range []string{"path", "replace"} {
					if !has[always] {
						t.Errorf("mode %v: a branch does not require %q: %v", mode, always, req)
					}
				}
				switch {
				case has["decl"]:
					addresses["decl"] = true
					if has["old_str"] || has["start_line"] {
						t.Errorf("mode %v: the decl branch also carries another address: %v", mode, req)
					}
				case has["old_str"]:
					addresses["old_str"] = true
					if has["decl"] || has["start_line"] {
						t.Errorf("mode %v: the anchor branch also carries another address: %v", mode, req)
					}
				case has["start_line"]:
					addresses["lines"] = true
					if !has["end_line"] {
						t.Errorf("mode %v: the line branch does not require end_line: %v", mode, req)
					}
				default:
					t.Errorf("mode %v: a branch names no address at all: %v", mode, req)
				}
				if m["additionalProperties"] != false {
					t.Errorf("mode %v: a branch admits extra properties", mode)
				}
			}
			for _, want := range []string{"decl", "old_str", "lines"} {
				if !addresses[want] {
					t.Errorf("mode %v: no branch addresses by %s", mode, want)
				}
			}
		}
	}
}

// A model with no conversation memory, sampled at temperature 0, is a
// deterministic function of its prompt — so a refused turn regenerates the same
// reply from near-identical state. r66 did exactly that 75 times. The escape has
// to come from sampling, not from wording.
func TestTemperatureWarmsOnlyWhenStuck(t *testing.T) {
	if got := devTemperature(0); got != 0 {
		t.Errorf("devTemperature(0) = %v, want 0: determinism is worth keeping while it works", got)
	}
	prev := 0.0
	for r := 1; r <= 3; r++ {
		got := devTemperature(r)
		if got <= prev {
			t.Errorf("devTemperature(%d) = %v, did not rise above %v", r, got, prev)
		}
		prev = got
	}
	// CAPPED LOW. At 0.8 the ramp corrupted the structured half of the reply:
	// syntax breaks went from 0-6 per attempt to 17-27, and ranges appeared whose
	// end line preceded their start. Breaking a fixpoint needs one different
	// token, not a different personality.
	for _, r := range []int{5, 20, 100} {
		if got := devTemperature(r); got > 0.3 {
			t.Errorf("devTemperature(%d) = %v, hot enough to corrupt the line range", r, got)
		}
	}
}

// The trail is the ONLY record the agent has of what it already tried — each turn
// is a fresh two-message prompt, so there is no conversation to remember. Two
// writes of different content to one file must look different in it, and two
// writes of the SAME content must look the same.
func TestTheTrailDistinguishesWriteContent(t *testing.T) {
	mk := func(body string) devAction {
		return devAction{Action: actionWriteFiles, Edits: []devEdit{
			{Path: "main.go", StartLine: 1, EndLine: 9, Replace: body},
		}}
	}
	a := stepDetail(mk("package main\n"))
	b := stepDetail(mk("package main // different\n"))
	again := stepDetail(mk("package main\n"))

	if a == b {
		t.Errorf("two different payloads produced the same trail entry, so a repeat is invisible:\n%s", a)
	}
	if a != again {
		t.Errorf("the same payload produced different trail entries, so a repeat is unrecognisable:\n%s\n%s", a, again)
	}
	if !strings.Contains(a, "main.go") || !strings.Contains(a, "lines 1-9") {
		t.Errorf("the trail entry lost the path or the range: %s", a)
	}
}

// A compile error names a line in the POST-edit file — which the agent has never
// seen. Telling it "line 181" is the same mistake as naming a tool it does not
// have. The implementation fixture spent 19 refusals across two break points with
// no view of the damage.
func TestAParseFailureShowsTheDamage(t *testing.T) {
	broken := "package main\n\nfunc A() {\n\tx := 1\n}\n}\n"
	got := windowAround(broken, "x_test.go:6:1: expected declaration, found '}'")

	if !strings.Contains(got, ">> 6") {
		t.Errorf("the failing line is not marked:\n%s", got)
	}
	if !strings.Contains(got, "func A() {") {
		t.Errorf("no surrounding context, so the brace imbalance is invisible:\n%s", got)
	}
	// Line numbers must match the post-edit file, or they send it to the wrong place.
	if !strings.Contains(got, " 3\tfunc A() {") {
		t.Errorf("context is not numbered against the edited file:\n%s", got)
	}
}

// A window near the start of a file must not run off the top.
func TestTheWindowClampsAtFileEdges(t *testing.T) {
	src := "package main\n\nfunc A() {}\n"
	for _, e := range []string{"x.go:1:1: bad", "x.go:3:1: bad", "x.go:99:1: bad"} {
		got := windowAround(src, e)
		if got == "" {
			t.Errorf("empty window for %q", e)
		}
		if strings.Contains(got, " 0\t") || strings.Contains(got, " 4\t") {
			t.Errorf("window ran past the file for %q:\n%s", e, got)
		}
	}
}

// A stale re-read is REFUNDED rather than refused, so it never touched the
// refusal count — and the first temperature ramp keyed on refusals alone. The
// fixpoint simply moved: 66 of 68 turns were identical reads of main.go with
// refusals sitting at 1. Anything that says "not progressing" must feed the ramp.
func TestTemperatureWarmsOnAReadLoopToo(t *testing.T) {
	stuckOnReads := &devState{refusals: 0, staleReads: 4}
	if got := devTemperature(max(stuckOnReads.refusals, stuckOnReads.staleReads)); got == 0 {
		t.Error("a read loop leaves the sampler deterministic, so it cannot produce a different read")
	}
	stuckOnRefusals := &devState{refusals: 4, staleReads: 0}
	if got := devTemperature(max(stuckOnRefusals.refusals, stuckOnRefusals.staleReads)); got == 0 {
		t.Error("a refusal streak no longer warms the sampler")
	}
	moving := &devState{refusals: 0, staleReads: 0}
	if got := devTemperature(max(moving.refusals, moving.staleReads)); got != 0 {
		t.Errorf("an agent making progress lost its determinism: %v", got)
	}
}

// "Replace to line 185" in a 180-line file can only mean "to the end". Refusing
// it taught the agent nothing: 7 refusals on the implementation fixture, every one
// start_line 1 with an end overshooting by a handful of lines.
func TestAnOverlongEndLineClampsToTheFile(t *testing.T) {
	src := "package main\n\nvar a = 1\n"
	s := &devState{
		tree: []string{"main.go"}, staged: map[string]string{}, baseline: map[string]string{},
		read: map[string]string{"main.go": src}, missing: map[string]bool{},
	}
	impl := "package main\n\nvar b = 2\n"
	if err := applyEdits(s, []devEdit{{Path: "main.go", StartLine: 1, EndLine: 99, Replace: impl}}, modeDevelop); err != nil {
		t.Fatalf("an over-long end line was refused instead of clamped: %v", err)
	}
	if s.staged["main.go"] != impl {
		t.Errorf("staged = %q, want the whole file replaced", s.staged["main.go"])
	}
}

// A START past the end has no sensible reading and must still be refused.
func TestAStartPastTheEndIsStillRefused(t *testing.T) {
	s := &devState{
		tree: []string{"main.go"}, staged: map[string]string{}, baseline: map[string]string{},
		read: map[string]string{"main.go": "package main\n"}, missing: map[string]bool{},
	}
	err := applyEdits(s, []devEdit{{Path: "main.go", StartLine: 40, EndLine: 42, Replace: "x"}}, modeDevelop)
	if err == nil {
		t.Fatal("a range beginning after the file ends was accepted")
	}
	if !strings.Contains(err.Error(), "past its end") {
		t.Errorf("the refusal does not explain: %v", err)
	}
}

// THE AGENT HAD AMNESIA. Every turn was a fresh two-message prompt, so it could
// not remember trying something: read from one attempt's transcript, turns 195 and
// 197 carry byte-identical prose, as do 204 and 205, and the branch shows four
// consecutive commits removing the same duplicate methods before it deleted too
// much and lost Update and Delete entirely. Its reasoning was right every time.
func TestHistoryRemembersActionsAndOutcomes(t *testing.T) {
	s := &devState{}
	s.remember(`{"tool":"write_files","arguments":{...}}`, "RESULT: Rejected: that is a test file")
	s.remember(`{"tool":"read_files","arguments":{...}}`, "RESULT: accepted.")

	h := s.recentHistory()
	if len(h) != 1 {
		t.Fatalf("history is %d messages, want one recap", len(h))
	}
	if !strings.Contains(h[0].Content, "test file") {
		t.Errorf("the outcome was lost: %q", h[0].Content)
	}
	if !strings.Contains(h[0].Content, "read_files") {
		t.Errorf("the later action was lost: %q", h[0].Content)
	}
}

// AND IT MUST NEVER OCCUPY THE ASSISTANT ROLE. Whatever sits there is a worked
// example of what to produce: replaying raw completions taught the model to
// imitate the wire format, and replaying descriptions instead taught it to emit
// `write_files: store_concurrent_test.go lines 169-169, 1B [169]` as a literal
// reply — which, being off-schema, was also unconstrained, and nine turns of one
// attempt ran to 9,000-17,000 characters of `}(i)}(i)}(i)` before being cut off.
func TestHistoryNeverUsesTheAssistantRole(t *testing.T) {
	s := &devState{}
	for i := range 4 {
		s.remember(fmt.Sprintf("write_files: main.go lines %d-%d, 20B [a1b2c3]", i, i+1),
			"RESULT: Rejected: your edits changed nothing")
	}
	for _, m := range s.recentHistory() {
		if m.Role != "user" {
			t.Errorf("history occupies the %q role, which the model reads as a format to copy", m.Role)
		}
	}
}

// It must stay bounded, or the slot fills with old actions and crowds out the
// state the agent actually needs.
func TestHistoryIsBounded(t *testing.T) {
	s := &devState{}
	for i := range maxHistoryTurns * 3 {
		s.remember(fmt.Sprintf("action %d", i), "RESULT: accepted.")
	}
	if got := len(s.history); got != maxHistoryTurns {
		t.Errorf("history holds %d entries, want the last %d", got, maxHistoryTurns)
	}
	// And it must keep the RECENT end, not the stale one.
	recap := s.recentHistory()[0].Content
	if !strings.Contains(recap, fmt.Sprintf("action %d\n", maxHistoryTurns*3-1)) {
		t.Errorf("history dropped the newest entry: %q", recap)
	}
	if strings.Contains(recap, "action 0\n") {
		t.Errorf("history kept the stalest entry: %q", recap)
	}
}

// An empty action must not create a phantom exchange.
func TestHistoryIgnoresAnEmptyAction(t *testing.T) {
	s := &devState{}
	s.remember("", "RESULT: nothing")
	if len(s.recentHistory()) != 0 {
		t.Errorf("an empty action was recorded: %v", s.recentHistory())
	}
}

// THE HISTORY MUST NOT TEACH THE MODEL A WIRE FORMAT. Replaying raw completions
// as assistant turns fed it bare JSON, and it started imitating the shape instead
// of choosing a tool — fabricating {"error":{"type":"llm_call_failed"}} and
// {"tool_call_id":...} envelopes within minutes.
func TestHistoryCarriesDescriptionsNotRawReplies(t *testing.T) {
	s := &devState{}
	s.remember("write_files: main.go lines 31-41, 220B [a1b2c3]", "RESULT: accepted.")
	for _, m := range s.recentHistory() {
		for _, shape := range []string{`{"tool"`, `"tool_call_id"`, `{"error"`, `"arguments"`} {
			if strings.Contains(m.Content, shape) {
				t.Errorf("history replays a wire format the model will imitate: %q", m.Content)
			}
		}
	}
}

// The syntax is what the model imitates; the prose is what it reasoned. Keeping
// the second without the first is the whole point of carrying a history.
func TestProseOfKeepsReasoningAndDropsSyntax(t *testing.T) {
	cases := []struct{ name, reply, wantIn, wantOut string }{
		{
			"bare action then prose",
			`{"tool":"write_files","arguments":{"edits":[{"path":"main.go"}]}}` + "\nThere are two duplicate NewStore declarations.",
			"duplicate NewStore", `{"tool"`,
		},
		{
			"fenced json",
			"Looking at the error, the map is nil.\n```json\n{\"tool\":\"read_files\"}\n```",
			"map is nil", `"tool"`,
		},
		{
			"tagged wrapper",
			"<tool_call>{\"name\":\"read_files\"}</tool_call> I need to inspect the store.",
			"inspect the store", "tool_call",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := proseOf(c.reply)
			if !strings.Contains(got, c.wantIn) {
				t.Errorf("reasoning lost: %q", got)
			}
			if strings.Contains(got, c.wantOut) {
				t.Errorf("syntax survived, which is what it imitates: %q", got)
			}
		})
	}
	if got := proseOf(""); got != "" {
		t.Errorf("empty reply produced %q", got)
	}
	// A reply that is ONLY an action leaves nothing to carry.
	if got := proseOf(`{"tool":"read_files","arguments":{}}`); got != "" {
		t.Errorf("a pure action produced prose: %q", got)
	}
}

// THE ARITHMETIC IS THE PROBLEM, NOT THE CODE. r69 stalled for twenty minutes on
// one ticket: 41 refusals where a line range cut across a function so the
// replacement landed at file scope, 9 where end_line preceded start_line, and 27
// no-ops re-sending the same text. Through all of it the developer's diagnosis
// was correct and unchanging — Go's mux prefix-matching "/api/tasks/" so it
// swallows "/api/tasks", a handler returning 200 where 404 was wanted. It was
// told "main.go is not valid Go after this edit", read that as its code being
// wrong, and re-derived the same right answer every turn.
func TestEnclosingDeclNamesTheFunctionARangeCuts(t *testing.T) {
	src := `package main

import "fmt"

func alpha() {
	fmt.Println("a")
}

func beta() {
	fmt.Println("b")
}
`
	// A range starting inside alpha and ending inside beta is the exact edit that
	// strands statements between two declarations.
	name, from, to, ok := enclosingDecl(src, 6, 10)
	if !ok {
		t.Fatal("no declaration identified for a range that plainly crosses one")
	}
	if name != "func alpha" {
		t.Errorf("named %q, want the first declaration the range enters", name)
	}
	if from != 5 || to != 7 {
		t.Errorf("span %d-%d, want 5-7 covering the whole of alpha", from, to)
	}
}

// A range wholly inside one function still resolves to that function, so the
// advice is "give the whole of it" rather than nothing.
func TestEnclosingDeclHandlesAnInteriorRange(t *testing.T) {
	src := "package main\n\nfunc solo() {\n\tx := 1\n\t_ = x\n}\n"
	name, from, to, ok := enclosingDecl(src, 4, 4)
	if !ok || name != "func solo" || from != 3 || to != 6 {
		t.Errorf("got %q %d-%d ok=%v, want func solo 3-6", name, from, to, ok)
	}
}

// Source that is ALREADY broken is not this check's business — reporting a
// declaration parsed from unparseable input would be a guess.
func TestEnclosingDeclDeclinesBrokenSource(t *testing.T) {
	if _, _, _, ok := enclosingDecl("package main\n\nfunc broken( {\n", 1, 3); ok {
		t.Error("claimed a declaration from source that does not parse")
	}
}

// AN END ONE BEFORE THE START IS A TRANSPOSITION and is read as that single
// line. All 9 of r69's inverted ranges were this shape and each cost a turn.
//
// A WIDER INVERSION IS STILL REFUSED, because it has no single reading —
// swapping "start_line 3, end_line 1" would replace the top three lines of the
// file, package clause included, on a guess. See TestEditRefusesAnInvertedRange.
func TestAnAdjacentTranspositionIsReadAsOneLine(t *testing.T) {
	s := &devState{
		tree: []string{"main.go"},
		read: map[string]string{"main.go": "package main\n\nvar a = 1\nvar b = 2\n"},
		staged: map[string]string{}, missing: map[string]bool{}, baseline: map[string]string{},
	}
	if err := applyEdits(s, []devEdit{{Path: "main.go", StartLine: 4, EndLine: 3, Replace: "var b = 20"}}, modeDevelop); err != nil {
		t.Fatalf("an adjacent transposition was refused: %v", err)
	}
	if want := "package main\n\nvar a = 1\nvar b = 20\n"; s.staged["main.go"] != want {
		t.Errorf("staged =\n%q\nwant\n%q", s.staged["main.go"], want)
	}

	// The wide inversion must survive as a refusal.
	w := &devState{
		tree: []string{"main.go"},
		read: map[string]string{"main.go": "package main\n\nvar a = 1\nvar b = 2\n"},
		staged: map[string]string{}, missing: map[string]bool{}, baseline: map[string]string{},
	}
	if err := applyEdits(w, []devEdit{{Path: "main.go", StartLine: 4, EndLine: 1, Replace: "x"}}, modeDevelop); err == nil {
		t.Error("a wide inversion was accepted; it has no single reading")
	}
}

// TWO FAULTS, OPPOSITE ADVICE. A post-edit syntax error means either the range
// was cut in the wrong place or the replacement text is itself broken, and
// telling them apart by assumption is how the first version of this misfired: it
// blamed the range for `rune literal not terminated` and told the agent "your
// replacement text is probably fine — do not re-analyse the code", then advised
// re-issuing lines 230-241 when 230-241 was exactly what had been sent.
func TestReplacementIsMalformedSeparatesTextFromPlacement(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replace string
		want    bool
	}{
		{"whole declaration", "func a() {\n\treturn\n}", false},
		{"bare statements belong inside a function", "x := 1\n_ = x", false},
		{"unterminated rune literal", "x := 'a\n_ = x", true},
		{"unterminated string", "s := \"oops\n_ = s", true},
		{"unbalanced brace", "func a() {\n\tif true {\n}", true},
		{"a type declaration", "type T struct{ A int }", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := replacementIsMalformed(tc.replace); got != tc.want {
				t.Errorf("replacementIsMalformed() = %v, want %v", got, tc.want)
			}
		})
	}
}

// When the range already covers the declaration exactly, repeating it as advice
// is a loop — that case must not claim the range is at fault.
func TestAnExactDeclarationRangeIsNotBlamed(t *testing.T) {
	src := "package main\n\nfunc solo() {\n\tx := 1\n\t_ = x\n}\n"
	name, from, to, ok := enclosingDecl(src, 3, 6)
	if !ok || name != "func solo" {
		t.Fatalf("setup: got %q ok=%v", name, ok)
	}
	if from != 3 || to != 6 {
		t.Fatalf("setup: span %d-%d, want 3-6", from, to)
	}
	// The guard in applyEdits is (from != k.from || to != k.to); assert the shape
	// it depends on, so a change to enclosingDecl cannot silently revive the loop.
	if from == 3 && to == 6 {
		return // identical to the requested range: advice must not be emitted
	}
	t.Error("an exact-span range would still be reported as cutting the declaration")
}

const addrSrc = "package main\n\nimport \"fmt\"\n\nfunc alpha() {\n\tfmt.Println(\"a\")\n}\n\nfunc beta() {\n\tfmt.Println(\"b\")\n}\n"

func addrState() *devState {
	return &devState{
		tree: []string{"main.go"}, staged: map[string]string{}, baseline: map[string]string{},
		read: map[string]string{"main.go": addrSrc}, missing: map[string]bool{},
	}
}

// ADDRESSING BY TEXT REMOVES THE ARITHMETIC, which is the thing this model cannot
// do: r69 spent 77 refusals on one ticket, 41 of them a range that cut across a
// function, while its diagnosis of the real bug never changed. It is also where
// the field evidence points — reproducing Qwen3.6-27B on SWE-bench Pro, a
// bash-only agent scores ~28% pass@1 and the same agent given SWE-agent's
// str_replace_editor (exact text, no line numbers) scores ~50.7%.
func TestOldStrAddressesAnEditByItsText(t *testing.T) {
	s := addrState()
	err := applyEdits(s, []devEdit{{
		Path: "main.go", OldStr: "\tfmt.Println(\"a\")", Replace: "\tfmt.Println(\"CHANGED\")",
	}}, modeDevelop)
	if err != nil {
		t.Fatalf("a unique quote was not accepted: %v", err)
	}
	if !strings.Contains(s.staged["main.go"], "CHANGED") || strings.Contains(s.staged["main.go"], "\"a\"") {
		t.Errorf("the edit did not land where it was aimed:\n%s", s.staged["main.go"])
	}
	if !strings.Contains(s.staged["main.go"], "\"b\"") {
		t.Error("the edit disturbed the other function")
	}
}

// AMBIGUITY IS TEXT ADDRESSING'S ARITHMETIC, so it must be reported as precisely.
// "not found" alone is what made the previous search format unrecoverable.
func TestOldStrReportsAmbiguityAndAbsencePrecisely(t *testing.T) {
	dup := &devState{
		tree: []string{"main.go"}, staged: map[string]string{}, baseline: map[string]string{},
		read: map[string]string{"main.go": "package main\n\nvar a = 1\nvar b = 2\nvar a = 1\n"}, missing: map[string]bool{},
	}
	err := applyEdits(dup, []devEdit{{Path: "main.go", OldStr: "var a = 1", Replace: "var a = 9"}}, modeDevelop)
	if err == nil {
		t.Fatal("an ambiguous quote was accepted; it could edit either place")
	}
	if !strings.Contains(err.Error(), "2 times") {
		t.Errorf("the refusal does not say how many matches: %v", err)
	}
	if !strings.Contains(err.Error(), "3, 5") {
		t.Errorf("the refusal does not name where they are: %v", err)
	}

	miss := addrState()
	err = applyEdits(miss, []devEdit{{Path: "main.go", OldStr: "fmt.Println(\"nope\")", Replace: "x"}}, modeDevelop)
	if err == nil || !strings.Contains(err.Error(), "does not appear") {
		t.Errorf("a missing quote was not explained: %v", err)
	}
}

// A DECLARATION ADDRESS CANNOT DRIFT, and it matches how the model reasons — r69's
// developer named apiTasksHandler, apiTaskHandler and main in its prose on every
// turn while failing to name their lines.
func TestDeclReplacesAWholeDeclaration(t *testing.T) {
	s := addrState()
	err := applyEdits(s, []devEdit{{
		Path: "main.go", Decl: "beta", Replace: "func beta() {\n\tfmt.Println(\"NEW\")\n}",
	}}, modeDevelop)
	if err != nil {
		t.Fatalf("a declaration address was refused: %v", err)
	}
	if !strings.Contains(s.staged["main.go"], "NEW") || strings.Contains(s.staged["main.go"], "\"b\"") {
		t.Errorf("beta was not replaced:\n%s", s.staged["main.go"])
	}
	if !strings.Contains(s.staged["main.go"], "func alpha()") {
		t.Error("replacing beta removed alpha")
	}

	// An unknown name must list what IS there, or the agent guesses again.
	bad := addrState()
	err = applyEdits(bad, []devEdit{{Path: "main.go", Decl: "gamma", Replace: "x"}}, modeDevelop)
	if err == nil || !strings.Contains(err.Error(), "alpha") {
		t.Errorf("an unknown declaration did not list the available ones: %v", err)
	}
}

// THE ESCAPE HATCH. Once a file diverges from the model's mental image, every
// addressing scheme fails together — recorded in this repo before it had a name:
// a developer broke every struct tag in store.go and then spent 34 consecutive
// actions failing to search and replace its way out of its own damage.
func TestUndoEditRestoresThePreviousFile(t *testing.T) {
	s := addrState()
	if err := applyEdits(s, []devEdit{{Path: "main.go", Decl: "alpha", Replace: "func alpha() { BROKEN"}}, modeDevelop); err == nil {
		// The syntax guard may reject it outright; force a landed-but-unwanted edit.
		t.Log("syntax guard accepted it; continuing")
	}
	if err := applyEdits(s, []devEdit{{Path: "main.go", Decl: "alpha", Replace: "func alpha() {\n\tfmt.Println(\"X\")\n}"}}, modeDevelop); err != nil {
		t.Fatalf("setup edit failed: %v", err)
	}
	if !strings.Contains(s.staged["main.go"], "X") {
		t.Fatalf("setup did not land:\n%s", s.staged["main.go"])
	}

	what, ok := s.undoLastEdit()
	if !ok {
		t.Fatal("undo reported nothing to undo after a write")
	}
	if !strings.Contains(what, "main.go") {
		t.Errorf("undo did not name what it restored: %q", what)
	}
	if strings.Contains(s.staged["main.go"], "X") {
		t.Errorf("the file was not restored:\n%s", s.staged["main.go"])
	}
}

// Undo with nothing written must say so rather than silently succeeding.
func TestUndoEditWithNothingToUndo(t *testing.T) {
	if _, ok := addrState().undoLastEdit(); ok {
		t.Error("undo claimed to restore something on a fresh attempt")
	}
}

// A file the undone edit CREATED must go, or undo leaves a phantom behind.
func TestUndoRemovesAFileTheEditCreated(t *testing.T) {
	s := addrState()
	if err := applyEdits(s, []devEdit{{Path: "store.go", Replace: "package main\n\nvar X = 1\n"}}, modeDevelop); err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if _, ok := s.staged["store.go"]; !ok {
		t.Fatal("the file was not created")
	}
	if _, ok := s.undoLastEdit(); !ok {
		t.Fatal("nothing to undo after a create")
	}
	if _, still := s.staged["store.go"]; still {
		t.Error("undo left the created file behind")
	}
}

// THE SCHEMA REQUIRES EVERY FIELD, so the model emits every field — and refusing
// the pair left it with no legal move. Measured on the first run under the new
// format: 5 of its first 14 attempts were rejected for obeying the schema.
func TestBothAddressesGivenPrefersOldStrRatherThanRefusing(t *testing.T) {
	s := addrState()
	err := applyEdits(s, []devEdit{{
		Path: "main.go", OldStr: "\tfmt.Println(\"a\")", Decl: "beta",
		Replace: "\tfmt.Println(\"PICKED\")",
	}}, modeDevelop)
	if err != nil {
		t.Fatalf("supplying both addresses was refused: %v", err)
	}
	// old_str wins, so alpha's body changed and beta is untouched.
	if !strings.Contains(s.staged["main.go"], "PICKED") {
		t.Errorf("old_str did not take precedence:\n%s", s.staged["main.go"])
	}
	if !strings.Contains(s.staged["main.go"], "fmt.Println(\"b\")") {
		t.Errorf("decl was applied as well, so both addresses took effect:\n%s", s.staged["main.go"])
	}
}

// INDENTATION IS A TRANSCRIPTION SLIP, NOT A DIFFERENT INTENTION — but tolerance
// must never buy ambiguity, so it applies only when the relaxed match is unique.
func TestOldStrToleratesIndentationOnlyWhenUnique(t *testing.T) {
	s := addrState()
	// Same text, wrong leading whitespace.
	err := applyEdits(s, []devEdit{{
		Path: "main.go", OldStr: "fmt.Println(\"a\")", Replace: "\tfmt.Println(\"OK\")",
	}}, modeDevelop)
	if err != nil {
		t.Fatalf("a uniquely-identifiable quote was refused over indentation: %v", err)
	}
	if !strings.Contains(s.staged["main.go"], "OK") {
		t.Errorf("the edit did not land:\n%s", s.staged["main.go"])
	}

	// Two lines identical apart from indentation must stay a refusal.
	amb := &devState{
		tree: []string{"main.go"}, staged: map[string]string{}, baseline: map[string]string{},
		read: map[string]string{"main.go": "package main\n\nfunc f() {\n\tx := 1\n}\n\nfunc g() {\n\t\tx := 1\n}\n"},
		missing: map[string]bool{},
	}
	if err := applyEdits(amb, []devEdit{{Path: "main.go", OldStr: "x := 1", Replace: "x := 2"}}, modeDevelop); err == nil {
		t.Error("an ambiguous relaxed match was accepted; it could edit either place")
	}
}

// A LONG QUOTE FAILS ON ONE CHARACTER, and "does not appear" leaves the agent to
// guess which. Measured under the new format: it quoted twenty-line spans — a
// whole handler plus the var above it — reconstructed from memory rather than
// copied, and every miss was a dead end.
func TestOldStrMissNamesWhereItDiverged(t *testing.T) {
	src := "package main\n\nfunc f() {\n\tx := 1\n\ty := 2\n\treturn\n}\n"
	s := &devState{
		tree: []string{"main.go"}, staged: map[string]string{}, baseline: map[string]string{},
		read: map[string]string{"main.go": src}, missing: map[string]bool{},
	}
	// First two lines are right, the third is not.
	err := applyEdits(s, []devEdit{{
		Path: "main.go", OldStr: "func f() {\n\tx := 1\n\ty := 99", Replace: "whatever",
	}}, modeDevelop)
	if err == nil {
		t.Fatal("a quote that does not match was accepted")
	}
	msg := err.Error()
	for _, want := range []string{"first 2 line(s) DO match", "line 3 of your quote differs", "y := 99", "y := 2"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not contain %q:\n%s", want, msg)
		}
	}
	// And it must point at the cheaper routes.
	if !strings.Contains(msg, "decl") {
		t.Errorf("the refusal does not offer decl for a whole declaration:\n%s", msg)
	}
}

// A quote with no anchor at all is a different failure and must read differently
// — there is no divergence point to report.
func TestOldStrMissWithNoAnchorSaysSo(t *testing.T) {
	s := addrState()
	err := applyEdits(s, []devEdit{{Path: "main.go", OldStr: "nothing like this exists", Replace: "x"}}, modeDevelop)
	if err == nil || !strings.Contains(err.Error(), "not even its first line") {
		t.Errorf("an unanchored quote was not distinguished: %v", err)
	}
}

// THE SCHEMA MAKES COPYING THE EASY PATH. Properties emit alphabetically, so a
// long old_str is followed a field later by replace, and the model reproduces it
// rather than composing a new one — 9 of 15 edits in the first window under this
// format sent the two identical, a no-op by construction. "Your edits changed
// nothing" never named that.
func TestIdenticalOldStrAndReplaceIsNamed(t *testing.T) {
	s := addrState()
	same := "\tfmt.Println(\"a\")"
	err := applyEdits(s, []devEdit{{Path: "main.go", OldStr: same, Replace: same}}, modeDevelop)
	if err == nil {
		t.Fatal("an edit that cannot change anything was accepted")
	}
	for _, want := range []string{"IDENTICAL", "decl"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// WHOLE-FILE REWRITE IS ALREADY AVAILABLE, by range, and must stay that way
// rather than becoming implicit.
//
// Letting a blank address mean "replace everything" was tried and reverted: three
// existing guards assert that a developer may not blindly overwrite a file the
// sections of a task SHARE on one branch, and they are right — the risk is
// discarding a sibling section's work, which no error would report. The cheap
// route is start_line 1 with the file's last line number, and the refusal names
// it. See TestReplacingAWholeExistingFileByRangeIsAllowed.
func TestWholeFileRewriteStaysExplicit(t *testing.T) {
	s := addrState()
	n := len(strings.Split(strings.TrimSuffix(addrSrc, "\n"), "\n"))
	whole := "package main\n\nimport \"fmt\"\n\nfunc alpha() {\n\tfmt.Println(\"REWRITTEN\")\n}\n"
	if err := applyEdits(s, []devEdit{{Path: "main.go", StartLine: 1, EndLine: n, Replace: whole}}, modeDevelop); err != nil {
		t.Fatalf("an explicit whole-file range was refused: %v", err)
	}
	if !strings.Contains(s.staged["main.go"], "REWRITTEN") {
		t.Errorf("the rewrite did not take:\n%s", s.staged["main.go"])
	}

	// The same thing with no address at all must still be refused.
	blind := addrState()
	if err := applyEdits(blind, []devEdit{{Path: "main.go", Replace: whole}}, modeDevelop); err == nil {
		t.Error("a blind whole-file write was accepted on a shared file")
	}
}

// A MESSAGE CANNOT OUTVOTE THE SHAPE OF THE OUTPUT. Properties emit
// alphabetically, so a long old_str is followed a field later by replace, and
// when both are long the cheapest continuation is to repeat what was just
// written. Naming the mistake explicitly did not stop it — 11 of 26 refusals in
// the following window were still identical pairs. Capping the anchor makes the
// copy unrepresentable rather than discouraged.
func TestOldStrIsCappedToAShortAnchor(t *testing.T) {
	s := addrState()
	long := strings.Repeat("\tfmt.Println(\"a\")\n", maxOldStrLines+2)
	err := applyEdits(s, []devEdit{{Path: "main.go", OldStr: long, Replace: "x"}}, modeDevelop)
	if err == nil {
		t.Fatal("a whole-function-sized anchor was accepted, which is what invites the copy")
	}
	for _, want := range []string{"ANCHOR", "decl"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}

	// A short anchor with a much longer replacement is the shape we want, and must
	// keep working.
	ok := addrState()
	if err := applyEdits(ok, []devEdit{{
		Path:    "main.go",
		OldStr:  "\tfmt.Println(\"a\")",
		Replace: "\tfmt.Println(\"1\")\n\tfmt.Println(\"2\")\n\tfmt.Println(\"3\")\n\tfmt.Println(\"4\")\n\tfmt.Println(\"5\")\n\tfmt.Println(\"6\")",
	}}, modeDevelop); err != nil {
		t.Fatalf("a short anchor with a long replacement was refused: %v", err)
	}
	if !strings.Contains(ok.staged["main.go"], "\"6\"") {
		t.Errorf("the replacement did not land:\n%s", ok.staged["main.go"])
	}
}

// USE THE ADDRESS IT GOT RIGHT. An over-long quote sent alongside a declaration
// name is a correct decl address with a redundant quote attached, not an
// ambiguous edit — and refusing it discards the very thing the cap exists to push
// the agent towards. Measured immediately after the cap shipped: 16 consecutive
// refusals, every one carrying a decl that would have resolved.
func TestAnOverLongQuoteFallsBackToDecl(t *testing.T) {
	s := addrState()
	long := strings.Repeat("\tfmt.Println(\"a\")\n", maxOldStrLines+3)
	err := applyEdits(s, []devEdit{{
		Path: "main.go", Decl: "beta", OldStr: long,
		Replace: "func beta() {\n\tfmt.Println(\"VIA DECL\")\n}",
	}}, modeDevelop)
	if err != nil {
		t.Fatalf("an edit carrying a valid decl was refused for its quote: %v", err)
	}
	if !strings.Contains(s.staged["main.go"], "VIA DECL") {
		t.Errorf("the decl address was not used:\n%s", s.staged["main.go"])
	}

	// With no decl to fall back on it must still be refused, or the cap is gone.
	none := addrState()
	if err := applyEdits(none, []devEdit{{Path: "main.go", OldStr: long, Replace: "x"}}, modeDevelop); err == nil {
		t.Error("an over-long anchor with no decl was accepted; the cap no longer bites")
	}
}

// THE ADDRESS IS ALREADY IN WHAT IT SENT. An over-long anchor is almost always a
// whole function pasted in, and its first line names the declaration it means.
// Telling the agent to use decl instead is correct and does not work — measured
// twice: 6 refusals in three minutes, every one a function quoted in full with
// decl left empty, after the refusal had explicitly named decl as the route.
func TestDeclNameFromSource(t *testing.T) {
	for _, tc := range []struct{ name, src, want string }{
		{"plain function", "func apiTasksHandler(w http.ResponseWriter, r *http.Request) {\n\tx := 1\n}", "apiTasksHandler"},
		{"no-arg function", "func main() {\n}", "main"},
		{"pointer method", "func (s *Store) Add(t *Task) error {\n}", "Store.Add"},
		{"value method", "func (s Store) Len() int {\n}", "Store.Len"},
		{"type declaration", "type Task struct {\n\tID string\n}", "Task"},
		{"mid-body quote names nothing", "\tif err != nil {\n\t\treturn err\n\t}", ""},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := declNameFromSource(tc.src); got != tc.want {
				t.Errorf("declNameFromSource() = %q, want %q", got, tc.want)
			}
		})
	}
}

// End to end: a whole function quoted in full, decl left empty, must still land.
func TestAnOverLongQuoteOfAWholeFunctionResolvesItself(t *testing.T) {
	s := addrState()
	whole := "func beta() {\n\tfmt.Println(\"b\")\n\t// padding\n\t// padding\n\t// padding\n\t// padding\n}"
	if err := applyEdits(s, []devEdit{{
		Path: "main.go", OldStr: whole, Replace: "func beta() {\n\tfmt.Println(\"DERIVED\")\n}",
	}}, modeDevelop); err != nil {
		t.Fatalf("a fully-quoted function with no decl was refused: %v", err)
	}
	if !strings.Contains(s.staged["main.go"], "DERIVED") {
		t.Errorf("the derived decl address was not used:\n%s", s.staged["main.go"])
	}
	if !strings.Contains(s.staged["main.go"], "func alpha()") {
		t.Error("deriving the address damaged the rest of the file")
	}
}

// A SPAN COVERING SEVERAL DECLARATIONS IS NOT A DECL ADDRESS. Naming it after its
// first line replaces only that one while inserting all of them — measured on
// r69, where main.go ended up with main, store and apiTasksHandler each declared
// twice and the developer then spent its turns cleaning up harness damage.
func TestDeclIsOnlyDerivedFromASingleDeclarationSpan(t *testing.T) {
	one := "func beta() {\n\tfmt.Println(\"b\")\n}"
	many := one + "\n\nfunc gamma() {\n}\n\nvar z = 1"
	if got := declCount(one); got != 1 {
		t.Errorf("declCount(one) = %d, want 1", got)
	}
	if got := declCount(many); got != 3 {
		t.Errorf("declCount(many) = %d, want 3", got)
	}

	// An over-long multi-declaration quote must be refused, not converted.
	s := addrState()
	padded := many + strings.Repeat("\n// pad", maxOldStrLines+3)
	if err := applyEdits(s, []devEdit{{Path: "main.go", OldStr: padded, Replace: "x"}}, modeDevelop); err == nil {
		t.Error("a multi-declaration span was converted to a single decl address")
	}
}

// However the duplication arises, the edit must be stopped on the turn it
// happens rather than surfacing later as "redeclared in this block".
func TestAnEditThatDuplicatesADeclarationIsRefused(t *testing.T) {
	s := addrState()
	// Replace alpha with a copy of BETA's declaration: beta then exists twice.
	err := applyEdits(s, []devEdit{{
		Path: "main.go", Decl: "alpha", Replace: "func beta() {\n\tfmt.Println(\"dupe\")\n}",
	}}, modeDevelop)
	if err == nil {
		t.Fatal("an edit that declares beta twice was accepted")
	}
	if !strings.Contains(err.Error(), "beta") {
		t.Errorf("the refusal does not name the duplicated symbol: %v", err)
	}
}

// And a healthy edit must not be caught by it.
func TestDuplicateDeclsIgnoresAHealthyFile(t *testing.T) {
	if got := duplicateDecls(addrSrc); len(got) != 0 {
		t.Errorf("a clean file reported duplicates: %v", got)
	}
	if got := duplicateDecls("package main\n\nfunc a() {\n"); len(got) != 0 {
		t.Errorf("unparseable source reported duplicates: %v", got)
	}
}

// LENGTH IS ONLY A PROBLEM WHEN THE QUOTE FAILS. Capping the anchor up front was
// aimed at the developer copying long spans into replace, and it blocked the
// SPECIFICATION AUTHOR instead: a test function is twenty-odd lines by nature,
// and five consecutive refusals told it a quote that would have matched perfectly
// was too long. Try the quote, then talk about its size.
func TestALongQuoteThatMatchesIsAccepted(t *testing.T) {
	body := "func testish() {\n" + strings.Repeat("\t// line\n", 20) + "}"
	src := "package main\n\n" + body + "\n"
	s := &devState{
		tree: []string{"x_test.go"}, staged: map[string]string{}, baseline: map[string]string{},
		read: map[string]string{"x_test.go": src}, missing: map[string]bool{},
	}
	if err := applyEdits(s, []devEdit{{
		Path: "x_test.go", OldStr: body, Replace: "func testish() {\n\t// rewritten\n}",
	}}, modeTest); err != nil {
		t.Fatalf("a 22-line quote that matches exactly was refused: %v", err)
	}
	if !strings.Contains(s.staged["x_test.go"], "rewritten") {
		t.Errorf("the edit did not land:\n%s", s.staged["x_test.go"])
	}
}

// And when a long quote does NOT match, the size is worth mentioning alongside
// the miss — but only then.
func TestALongQuoteThatMissesMentionsItsSize(t *testing.T) {
	s := addrState()
	// A mid-body span: it names no declaration, so there is nothing to fall back
	// to and its length is the thing worth saying.
	miss := strings.Repeat("\tsomething that is not there\n", 20)
	err := applyEdits(s, []devEdit{{Path: "main.go", OldStr: miss, Replace: "x"}}, modeDevelop)
	if err == nil {
		t.Fatal("a quote matching nothing was accepted")
	}
	if !strings.Contains(err.Error(), "ANCHOR") {
		t.Errorf("a failed long quote does not mention its size: %v", err)
	}

	// A quote naming a declaration that does not exist gets the BETTER error —
	// what the file actually declares — rather than a complaint about length.
	named := "func nosuch() {\n" + strings.Repeat("\t// line\n", 20) + "}"
	err = applyEdits(addrState(), []devEdit{{Path: "main.go", OldStr: named, Replace: "x"}}, modeDevelop)
	if err == nil || !strings.Contains(err.Error(), "alpha") {
		t.Errorf("a missing declaration did not list what the file has: %v", err)
	}
}

// THE ARGUMENTS WERE NEVER CONSTRAINED. The dev turn offered Tools and kept
// Schema as a fallback, but the gateway drops response_format whenever tools are
// sent — so the JSON carrying whole Go files through a string field was generated
// free-form. Read from a live completion:
//
//	...Encode(task)\n\t}\n}'}], ","start_line": 89
//
// The model closed "replace" with an apostrophe and kept writing JSON; the
// recovered value took `'}], ` into the source and came back as "rune literal not
// terminated" — 14 of 23 refusals in one window.
func TestDevTurnsAreGrammarConstrained(t *testing.T) {
	sc := devActionSchema()
	if sc == nil {
		t.Fatal("no schema on the dev turn, so nothing constrains the reply")
	}
	// The schema must describe the SAME edit shape the tools did, or switching
	// paths would quietly drop the addressing fields.
	blob, err := json.Marshal(sc.Schema)
	if err != nil {
		t.Fatalf("schema does not marshal: %v", err)
	}
	for _, want := range []string{"old_str", "decl", "start_line", "end_line", "replace"} {
		if !strings.Contains(string(blob), want) {
			t.Errorf("the constrained schema omits %q, which the tool form offered", want)
		}
	}
	// And the action vocabulary must still be closed.
	for _, want := range devActions {
		if !strings.Contains(string(blob), want) {
			t.Errorf("the schema omits the %q action", want)
		}
	}
}

// A tool call is still ACCEPTED if a backend answers that way — tools are no
// longer offered, not no longer understood.
func TestAToolCallIsStillParsed(t *testing.T) {
	act, err := parseDevReply(ChatResult{
		Calls: []ToolCall{{Name: actionReadFiles, Arguments: `{"paths":["main.go"]}`}},
	}, modeDevelop)
	if err != nil {
		t.Fatalf("a tool-call reply was rejected: %v", err)
	}
	if act.Action != actionReadFiles || len(act.Paths) != 1 {
		t.Errorf("tool call decoded wrongly: %+v", act)
	}
}

// A SINGLE OBJECT WITH EVERYTHING OPTIONAL IS NOT A CONSTRAINT. The schema
// required only "action", which was harmless while it was a fallback and became
// the whole contract when the dev turn stopped offering tools — the grammar then
// admitted {"action":"write_files"} with nothing to apply. 25 refusals of
// "write_files with no edits" in the first window, every one schema-valid.
func TestEachActionCarriesItsOwnRequirements(t *testing.T) {
	blob, err := json.Marshal(devActionSchema().Schema)
	if err != nil {
		t.Fatalf("schema does not marshal: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(blob, &doc); err != nil {
		t.Fatal(err)
	}
	branches, ok := doc["oneOf"].([]any)
	if !ok || len(branches) != len(devActions) {
		t.Fatalf("expected one branch per action, got %v", doc["oneOf"])
	}

	seen := map[string][]string{}
	for _, b := range branches {
		m := b.(map[string]any)
		props := m["properties"].(map[string]any)
		enum := props["action"].(map[string]any)["enum"].([]any)
		name := enum[0].(string)
		var req []string
		for _, r := range m["required"].([]any) {
			req = append(req, r.(string))
		}
		seen[name] = req
		if m["additionalProperties"] != false {
			t.Errorf("%s admits extra properties", name)
		}
	}

	// The three the tool form required. An edit-less write is not a smaller edit,
	// it is a wasted turn.
	for _, want := range []string{"action", "edits", "summary", "type"} {
		if !containsStr(seen[actionWriteFiles], want) {
			t.Errorf("write_files does not require %q: %v", want, seen[actionWriteFiles])
		}
	}
	if !containsStr(seen[actionReadFiles], "paths") {
		t.Errorf("read_files does not require paths: %v", seen[actionReadFiles])
	}
	if _, ok := seen[actionUndoEdit]; !ok {
		t.Error("undo_edit has no branch, so it cannot be expressed")
	}
}

// An empty edits array is a turn spent saying nothing, and the grammar is the
// only place that can make it unrepresentable.
func TestEditsArrayRequiresAtLeastOne(t *testing.T) {
	blob, _ := json.Marshal(editsSchema())
	if !strings.Contains(string(blob), `"minItems":1`) {
		t.Errorf("edits may be empty: %s", blob)
	}
}

// BRANCHING REMOVES THE OPPORTUNITY TO COPY, where a message and a length cap
// both failed to. With old_str and replace both required and adjacent in one
// object, a constrained sampler's likeliest continuation after a long quote is
// that quote again: 39 of 47 turns sent them identical, and the no-progress
// ceiling then killed the ticket twice and took the whole board with it.
func TestNoEditShapeCarriesTwoAddresses(t *testing.T) {
	blob, err := json.Marshal(editsSchema())
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(blob, &doc); err != nil {
		t.Fatal(err)
	}
	branches := doc["items"].(map[string]any)["oneOf"].([]any)
	if len(branches) != 3 {
		t.Fatalf("expected decl, anchor and line shapes, got %d", len(branches))
	}
	for _, b := range branches {
		props := b.(map[string]any)["properties"].(map[string]any)
		n := 0
		for _, addr := range []string{"decl", "old_str", "start_line"} {
			if _, ok := props[addr]; ok {
				n++
			}
		}
		if n != 1 {
			t.Errorf("a shape offers %d addresses; each must offer exactly one: %v", n, props)
		}
	}

	// The anchor must stay SHORT, or the copy becomes available again inside its
	// own branch. Bounded by maxLength so the grammar enforces it, not a check.
	for _, b := range branches {
		props := b.(map[string]any)["properties"].(map[string]any)
		if old, ok := props["old_str"].(map[string]any); ok {
			if old["maxLength"] == nil {
				t.Error("old_str has no maxLength, so a whole function can be quoted again")
			}
		}
	}
}

// A DECLARATION THAT IS NOT THERE YET IS ONE TO ADD. The decl address could only
// REPLACE, which leaves test-first work — whose whole job is writing functions
// that do not exist — with no natural move. Measured: 55 refusals of "no
// top-level declaration named GETApiTasksHandler" against a file that was
// supposed to gain exactly that.
func TestDeclAddsADeclarationTheFileDoesNotHave(t *testing.T) {
	s := addrState()
	if err := applyEdits(s, []devEdit{{
		Path: "main.go", Decl: "gamma", Replace: "func gamma() {\n\tfmt.Println(\"new\")\n}",
	}}, modeDevelop); err != nil {
		t.Fatalf("adding a new declaration was refused: %v", err)
	}
	got := s.staged["main.go"]
	if !strings.Contains(got, "func gamma()") {
		t.Errorf("the declaration was not added:\n%s", got)
	}
	// And nothing that was there may be lost.
	for _, keep := range []string{"func alpha()", "func beta()", "package main"} {
		if !strings.Contains(got, keep) {
			t.Errorf("appending removed %q:\n%s", keep, got)
		}
	}
	// The result must still be valid Go.
	if declCount(strings.TrimPrefix(got, "package main\n")) < 3 {
		t.Errorf("the appended file does not parse as three declarations:\n%s", got)
	}
}

// But an append must not smuggle in a duplicate: the gate still applies.
func TestAddingADeclarationThatAlreadyExistsIsRefused(t *testing.T) {
	s := addrState()
	// "gamma" does not exist, but the body also redeclares beta.
	err := applyEdits(s, []devEdit{{
		Path: "main.go", Decl: "gamma",
		Replace: "func gamma() {}\n\nfunc beta() {\n\tfmt.Println(\"dupe\")\n}",
	}}, modeDevelop)
	if err == nil {
		t.Fatal("an append that redeclares beta was accepted")
	}
	if !strings.Contains(err.Error(), "beta") {
		t.Errorf("the refusal does not name the clash: %v", err)
	}
}

// A fragment that is not a declaration must not be appended — that would break
// the file rather than extend it.
func TestDeclDoesNotAppendANonDeclaration(t *testing.T) {
	s := addrState()
	if err := applyEdits(s, []devEdit{{
		Path: "main.go", Decl: "gamma", Replace: "\tx := 1\n\t_ = x",
	}}, modeDevelop); err == nil {
		t.Error("a bare statement was appended as though it were a declaration")
	}
}

// "REPLACE THIS FILE" ARRIVES AS A DECL NAMED AFTER THE FILE. The spec author
// rewrites whole test files by nature, and the branched schema left it no obvious
// whole-file route, so it put the filename in decl and the entire file in
// replace: 18 refusals of `no top-level declaration named
// "handlers_read_test.go"` in five minutes.
func TestADeclNamedAfterTheFileMeansTheWholeFile(t *testing.T) {
	src := "package main\n\nimport \"testing\"\n\nfunc TestOld(t *testing.T) {}\n"
	s := &devState{
		tree: []string{"x_test.go"}, staged: map[string]string{}, baseline: map[string]string{},
		read: map[string]string{"x_test.go": src}, missing: map[string]bool{},
	}
	whole := "package main\n\nimport \"testing\"\n\nfunc TestNew(t *testing.T) {}\n"
	// modeTest: a specification author rewriting its OWN test file, which is
	// allowed — the conversion must not change who may do what.
	if err := applyEdits(s, []devEdit{{Path: "x_test.go", Decl: "x_test.go", Replace: whole}}, modeTest); err != nil {
		t.Fatalf("a whole-file rewrite expressed as a decl was refused: %v", err)
	}
	got := s.staged["x_test.go"]
	if !strings.Contains(got, "TestNew") || strings.Contains(got, "TestOld") {
		t.Errorf("the file was not replaced:\n%s", got)
	}
	if strings.Count(got, "package main") != 1 {
		t.Errorf("the package clause was duplicated:\n%s", got)
	}
}

// A replacement that begins with a package clause is a whole file however it was
// addressed — the model does not always echo the filename.
func TestAPackageClauseMeansTheWholeFile(t *testing.T) {
	src := "package main\n\nfunc a() {}\n"
	s := &devState{
		tree: []string{"y_test.go"}, staged: map[string]string{}, baseline: map[string]string{},
		read: map[string]string{"y_test.go": src}, missing: map[string]bool{},
	}
	whole := "package main\n\nfunc b() {}\n"
	if err := applyEdits(s, []devEdit{{Path: "y_test.go", Decl: "anything", Replace: whole}}, modeTest); err != nil {
		t.Fatalf("a package-clause replacement was refused: %v", err)
	}
	if strings.Contains(s.staged["y_test.go"], "func a()") {
		t.Errorf("the old contents survived:\n%s", s.staged["y_test.go"])
	}
}

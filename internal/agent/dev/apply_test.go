package dev

import (
	"strconv"
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/edit"
)

const storeGo = `package main

type Store struct {
	tasks []Task
}

func (s *Store) Add(t Task) {
	s.tasks = append(s.tasks, t)
}

func List() []Task {
	return nil
}
`

func tree(files map[string]string) *State {
	s := &State{
		Read:    map[string]string{},
		Staged:  map[string]string{},
		Missing: map[string]bool{},
	}
	for p, c := range files {
		s.Tree = append(s.Tree, p)
		s.Read[p] = c
	}
	return s
}

func mustApply(t *testing.T, s *State, mode Mode, edits ...edit.Edit) {
	t.Helper()
	if err := Apply(s, edits, mode); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

func refusal(t *testing.T, s *State, mode Mode, edits ...edit.Edit) string {
	t.Helper()
	err := Apply(s, edits, mode)
	if err == nil {
		t.Fatal("the edit was accepted")
	}
	return err.Error()
}

// A BATCH THAT HALF-APPLIES leaves the tree in a state neither the model nor the
// prompt describes, and the next turn reasons about a file that is partly edited
// and partly not.
func TestAFailedBatchChangesNothingAtAll(t *testing.T) {
	s := tree(map[string]string{"store.go": storeGo})
	refusal(t, s, ModeDevelop,
		edit.Edit{Path: "store.go", OldStr: "func List() []Task {", Replace: "func List() []Task { // ok"},
		edit.Edit{Path: "store.go", OldStr: "nothing like this is in the file", Replace: "x"},
	)
	if len(s.Staged) != 0 {
		t.Errorf("a refused batch left %d staged files", len(s.Staged))
	}
	if s.Read["store.go"] != storeGo {
		t.Error("a refused batch changed what the agent sees")
	}
}

// BOTTOM-UP, because a line range names a position in the file as the model saw
// it. An edit at line 10 shifts everything below it, so a later edit at line 90
// would land somewhere the model never looked.
func TestSeveralRangesInOneBatchAllLandWhereTheModelLooked(t *testing.T) {
	s := tree(map[string]string{"config.yaml": "L1\nL2\nL3\nL4\nL5\n"})
	// Replacing line 3 with two lines would shift line 6 down if applied first.
	mustApply(t, s, ModeDevelop,
		edit.Edit{Path: "config.yaml", StartLine: 1, EndLine: 1, Replace: "L1a\nL1b"},
		edit.Edit{Path: "config.yaml", StartLine: 4, EndLine: 4, Replace: "L4-changed"},
	)
	got := s.Staged["config.yaml"]
	want := "L1a\nL1b\nL2\nL3\nL4-changed\nL5\n"
	if got != want {
		// Applied top-down, the first edit adds a line and the second lands on L3
		// instead of L4 — and the file still CONTAINS "L4-changed", which is why
		// this asserts the whole result rather than a substring.
		t.Errorf("the edits interfered:\ngot  %q\nwant %q", got, want)
	}
}

// AN IDENTICAL PAIR CHANGES NOTHING, whatever its length. The schema emits
// properties alphabetically, so a long old_str is followed a field later by
// replace, and repeating what was just written is the cheapest continuation — 11
// of 26 refusals in one window were exactly that.
func TestAQuoteCopiedIntoItsOwnReplacementIsNamed(t *testing.T) {
	s := tree(map[string]string{"store.go": storeGo})
	got := refusal(t, s, ModeDevelop, edit.Edit{
		Path: "store.go", OldStr: "func List() []Task {", Replace: "func List() []Task {",
	})
	if !strings.Contains(got, "IDENTICAL") {
		t.Errorf("the refusal does not name the copy:\n%s", got)
	}
	// AND IT SAYS WHAT TO DO INSTEAD. "Your edits changed nothing" does not.
	if !strings.Contains(got, `"decl"`) {
		t.Errorf("the refusal does not name the way to rewrite a whole function:\n%s", got)
	}
}

// LENGTH IS ONLY A PROBLEM WHEN THE QUOTE FAILS. Capping it up front was aimed at
// the developer copying long spans and blocked the SPECIFICATION AUTHOR instead —
// a test function is twenty-odd lines by nature, and five consecutive refusals
// told it its perfectly matchable quote was too long.
func TestALongQuoteThatMatchesIsAccepted(t *testing.T) {
	long := "func (s *Store) Add(t Task) {\n\ts.tasks = append(s.tasks, t)\n}"
	if len(strings.Split(long, "\n")) <= edit.MaxOldStrLines {
		// Make sure the fixture really is over the anchor cap.
		long = storeGo[strings.Index(storeGo, "type Store"):]
	}
	s := tree(map[string]string{"store.go": storeGo})
	mustApply(t, s, ModeDevelop, edit.Edit{Path: "store.go", OldStr: long, Replace: "// gone"})
	if !strings.Contains(s.Staged["store.go"], "// gone") {
		t.Errorf("a long but matchable quote was not applied:\n%s", s.Staged["store.go"])
	}

	// And when it does NOT match, the size is finally mentioned.
	got := refusal(t, tree(map[string]string{"store.go": storeGo}), ModeDevelop, edit.Edit{
		Path:    "store.go",
		OldStr:  "line1\nline2\nline3\nline4\nline5\nline6\nline7",
		Replace: "x",
	})
	if !strings.Contains(got, "ANCHOR") {
		t.Errorf("a long quote that missed was not told to shorten:\n%s", got)
	}
}

// A DECLARATION THAT IS NOT THERE YET IS ONE TO ADD. The decl shape could only
// ever REPLACE, which leaves test-first work — whose entire job is writing
// functions that do not exist — with no natural move. Measured: 55 refusals of
// "no top-level declaration named GETApiTasksHandler" against a file that was
// supposed to gain exactly that.
func TestNamingADeclarationThatDoesNotExistYetAppendsIt(t *testing.T) {
	s := tree(map[string]string{"store.go": storeGo})
	mustApply(t, s, ModeDevelop, edit.Edit{
		Path: "store.go", Decl: "Delete",
		Replace: "func Delete(id string) error {\n\treturn nil\n}",
	})
	got := s.Staged["store.go"]
	if !strings.Contains(got, "func Delete(id string) error") {
		t.Errorf("the new declaration was not added:\n%s", got)
	}
	// The rest of the file survives.
	if !strings.Contains(got, "func List() []Task") {
		t.Errorf("appending replaced the file:\n%s", got)
	}

	// ONLY WHEN THE REPLACEMENT REALLY IS A DECLARATION: an append of a stray
	// fragment would break the file.
	fresh := tree(map[string]string{"store.go": storeGo})
	if err := Apply(fresh, []edit.Edit{{Path: "store.go", Decl: "Nope", Replace: "\ts.x = 1"}},
		ModeDevelop); err == nil {
		t.Error("a stray fragment was appended as a declaration")
	}
}

// "REPLACE THIS FILE" ARRIVES AS A DECL NAMED AFTER THE FILE. The spec author
// rewrites whole test files by nature and the branched schema left it no obvious
// route, so it put the filename in decl. Measured: 18 refusals of `no top-level
// declaration named "handlers_read_test.go"` in five minutes.
func TestADeclNamedAfterTheFileIsReadAsAWholeFileRewrite(t *testing.T) {
	body := "package main\n\nimport \"testing\"\n\nfunc TestNew(t *testing.T) {}\n"
	for _, decl := range []string{"store_test.go", "anything at all"} {
		s := tree(map[string]string{"store_test.go": "package main\n\nfunc TestOld(t *testing.T) {}\n"})
		mustApply(t, s, ModeTest, edit.Edit{Path: "store_test.go", Decl: decl, Replace: body})
		if strings.Contains(s.Staged["store_test.go"], "TestOld") {
			t.Errorf("decl %q did not rewrite the file:\n%s", decl, s.Staged["store_test.go"])
		}
	}
}

// A WHOLE-FILE REPLACEMENT AND A LINE EDIT DESCRIBE TWO DIFFERENT FILES, and
// picking a winner would silently discard one of them.
func TestMixingAWholeFileWriteWithARangeIsRefused(t *testing.T) {
	s := tree(map[string]string{"store.go": storeGo})
	got := refusal(t, s, ModeDevelop,
		edit.Edit{Path: "store.go", StartLine: 3, EndLine: 3, Replace: "x"},
		edit.Edit{Path: "store.go", Replace: "package main\n"},
	)
	if !strings.Contains(got, "two different files") {
		t.Errorf("the refusal does not say why:\n%s", got)
	}
}

// EACH STAGE MAY WRITE WHAT ITS JOB IS, and the refusal says what the job IS
// rather than restating the rule.
func TestWhatEachStageMayNotWriteAndWhy(t *testing.T) {
	cases := []struct {
		name string
		mode Mode
		path string
		want string
	}{
		{"the developer may not edit tests", ModeDevelop, "store_test.go",
			"tests are written by a separate agent"},
		{"the developer may not edit documentation", ModeDevelop, "README.md",
			"the specification the OTHER tickets are being built against"},
		{"the author may only edit tests", ModeTest, "store.go",
			"defeats the point of writing the test"},
		{"the coverage stage may only add tests", ModeCoverage, "store.go",
			"changing it to make a test pass is not covering it"},
	}
	for _, c := range cases {
		s := tree(map[string]string{c.path: "package main\n"})
		got := refusal(t, s, c.mode, edit.Edit{Path: c.path, StartLine: 1, EndLine: 1, Replace: "x"})
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: the refusal does not explain itself:\n%s", c.name, got)
		}
	}
}

// THE SPECIFICATION IS NOT THE COVERAGE STAGE'S TO EDIT. The cheapest way to
// raise a coverage number is to weaken a test — which hits the target while
// destroying the thing the target is a proxy for.
func TestTheCoverageStageMayNotEditTestsThatCameBeforeIt(t *testing.T) {
	s := tree(map[string]string{"store_test.go": "package main\n"})
	got := refusal(t, s, ModeCoverage,
		edit.Edit{Path: "store_test.go", StartLine: 1, EndLine: 1, Replace: "x"})
	if !strings.Contains(got, "must not be edited") || !strings.Contains(got, "coverage_test.go") {
		t.Errorf("the refusal does not say what to do instead:\n%s", got)
	}

	// A NEW file is exactly what it is for.
	fresh := tree(map[string]string{"store_test.go": "package main\n"})
	mustApply(t, fresh, ModeCoverage,
		edit.Edit{Path: "coverage_test.go", Replace: "package main\n\nfunc TestMore(t *testing.T) {}\n"})
}

// SAY HOW TO DO WHAT IT IS TRYING TO DO. Replacing a file wholesale is a
// legitimate thing to want — main.go arrives as a six-line stub — and a line
// range expresses it exactly. Refusing without naming that cost r65: 78
// refusals, all this message, the developer asking for the one thing it was
// never told it already had.
func TestABlindWholeFileOverwriteIsRefusedWithTheRangeThatWouldWork(t *testing.T) {
	s := tree(map[string]string{"store.go": storeGo})
	got := refusal(t, s, ModeDevelop, edit.Edit{Path: "store.go", Replace: "package main\n"})

	if !strings.Contains(got, "another section of this task has written") {
		t.Errorf("the refusal does not say what is at risk:\n%s", got)
	}
	n := lineCount(storeGo)
	if !strings.Contains(got, "end_line "+strconv.Itoa(n)) {
		t.Errorf("the refusal does not hand back the range that works (want end_line %d):\n%s", n, got)
	}

	// And that range IS accepted.
	mustApply(t, tree(map[string]string{"store.go": storeGo}), ModeDevelop,
		edit.Edit{Path: "store.go", StartLine: 1, EndLine: n, Replace: "package main\n"})
}

// THE EXCEPTION IS NARROW: a spec author rewriting a test file it has read. That
// file is its own output, so replacing it loses nothing that belongs to anyone
// else — and a section author that retried onto a branch holding its own file
// spent 21 turns with no legal way to rewrite it.
func TestAnAuthorMayRewriteItsOwnTestFileWholesale(t *testing.T) {
	s := tree(map[string]string{"store_test.go": "package main\n\nfunc TestOld(t *testing.T) {}\n"})
	mustApply(t, s, ModeTest, edit.Edit{
		Path: "store_test.go", Replace: "package main\n\nfunc TestNew(t *testing.T) {}\n"})
	if strings.Contains(s.Staged["store_test.go"], "TestOld") {
		t.Error("the author could not rewrite its own test file")
	}

	// But a developer replacing an implementation file may not, because sections
	// of one task share a branch and a wholesale write can drop a sibling's work.
	dev := tree(map[string]string{"store.go": storeGo})
	refusal(t, dev, ModeDevelop, edit.Edit{Path: "store.go", Replace: "package main\n"})
}

func TestWritingToAFileTheAgentHasNotReadIsRefused(t *testing.T) {
	s := tree(map[string]string{"store.go": storeGo})
	delete(s.Read, "store.go") // listed in the tree, never read

	got := refusal(t, s, ModeDevelop, edit.Edit{Path: "store.go", Replace: "package main\n"})
	if !strings.Contains(got, "read_files it first") {
		t.Errorf("the refusal does not name the action that fixes it:\n%s", got)
	}

	// A range edit to an unread file says the same thing in its own terms.
	got = refusal(t, s, ModeDevelop, edit.Edit{Path: "store.go", StartLine: 1, EndLine: 2, Replace: "x"})
	if !strings.Contains(got, "read_files it first") {
		t.Errorf("the refusal does not name the action that fixes it:\n%s", got)
	}

	// A file that is genuinely not there is a CREATE, and is allowed.
	fresh := tree(nil)
	mustApply(t, fresh, ModeDevelop, edit.Edit{Path: "new.go", Replace: "package main\n"})
}

// A TREE ENTRY THAT COULD NOT BE READ must not block a create: the agent asked,
// the file was not there, and refusing would trap it.
func TestAPathTheTreeListsButAReadCouldNotFindMayBeCreated(t *testing.T) {
	s := tree(map[string]string{"store.go": storeGo})
	delete(s.Read, "store.go")
	s.Missing["store.go"] = true

	mustApply(t, s, ModeDevelop, edit.Edit{Path: "store.go", Replace: "package main\n"})
}

// ALL 9 OF r69'S INVERTED RANGES WERE A TRANSPOSITION — "start_line 88, end_line
// 87" — and each cost a turn to a message that only restated the rule.
func TestATransposedRangeIsReadRatherThanRefused(t *testing.T) {
	s := tree(map[string]string{"config.yaml": "L1\nL2\nL3\nL4\n"})
	mustApply(t, s, ModeDevelop, edit.Edit{Path: "config.yaml", StartLine: 3, EndLine: 2, Replace: "L3-changed"})
	got := s.Staged["config.yaml"]
	if !strings.Contains(got, "L3-changed") || strings.Contains(got, "\nL3\n") {
		t.Errorf("the transposition was not read as line 3:\n%s", got)
	}

	// NARROW ON PURPOSE. A wider inversion has no single reading — "start_line 3,
	// end_line 1" would replace the package clause on a guess — so it stays
	// refused.
	wide := refusal(t, tree(map[string]string{"config.yaml": "L1\nL2\nL3\nL4\n"}), ModeDevelop,
		edit.Edit{Path: "config.yaml", StartLine: 4, EndLine: 1, Replace: "x"})
	if !strings.Contains(wide, "is not a range") {
		t.Errorf("a wide inversion was not refused clearly:\n%s", wide)
	}
}

// AN END PAST THE LAST LINE CAN ONLY MEAN "TO THE END". Measured: 7 refusals,
// every one start_line 1 with an end a few lines past a 180-line file — the
// agent asking to replace the whole file and missing by a rounding error.
func TestAnEndPastTheFileIsClampedButAStartPastItIsNot(t *testing.T) {
	s := tree(map[string]string{"config.yaml": "L1\nL2\nL3\n"})
	mustApply(t, s, ModeDevelop, edit.Edit{Path: "config.yaml", StartLine: 1, EndLine: 99, Replace: "only\n"})
	if strings.Contains(s.Staged["config.yaml"], "L2") {
		t.Errorf("the clamped range did not replace the file:\n%s", s.Staged["config.yaml"])
	}

	got := refusal(t, tree(map[string]string{"config.yaml": "L1\nL2\nL3\n"}), ModeDevelop,
		edit.Edit{Path: "config.yaml", StartLine: 99, EndLine: 120, Replace: "x"})
	if !strings.Contains(got, "past its end") || !strings.Contains(got, "numbered") {
		t.Errorf("a start past the end was not explained:\n%s", got)
	}
}

// THE HARNESS MAKES UP FOR THE MODEL WHERE IT CAN. A developer wrote store.go
// with every struct tag missing its closing backtick, then spent 34 consecutive
// actions failing to search and replace its way out — search text never matches
// a file the model has already broken.
func TestTheMissingBacktickIsRepairedRatherThanRefused(t *testing.T) {
	broken := "package main\n\ntype Task struct {\n\tID   string `json:\"id\"\n\tName string `json:\"name\"\n}\n"
	s := tree(nil)
	mustApply(t, s, ModeDevelop, edit.Edit{Path: "task.go", Replace: broken})

	got := s.Staged["task.go"]
	if !strings.Contains(got, "`json:\"id\"`") || !strings.Contains(got, "`json:\"name\"`") {
		t.Errorf("the tags were not repaired:\n%s", got)
	}
	if len(s.Repairs) != 1 || s.Repairs[0] != "task.go" {
		t.Errorf("the repair was not recorded: %v", s.Repairs)
	}
}

// A WRONG GUESS IS DISCARDED and the ORIGINAL error reported: an error quoting a
// line the model never wrote is a worse clue than one quoting its own.
func TestAnUnrepairableFileIsRefusedWithWhatTheEditProduced(t *testing.T) {
	s := tree(nil)
	got := refusal(t, s, ModeDevelop, edit.Edit{
		Path: "a.go", Replace: "package main\n\nfunc main() {\n\tif true {\n",
	})
	if !strings.Contains(got, "AS YOUR EDIT") {
		t.Errorf("the refusal does not show the damage:\n%s", got)
	}
	// It shows numbered lines the agent can look at, since the parser's line
	// number refers to a file it has never seen.
	if !strings.Contains(got, ">>") {
		t.Errorf("the refusal does not point at the break:\n%s", got)
	}
}

// THE TWO FAULTS NEED OPPOSITE ADVICE. The first version assumed any post-edit
// syntax error meant the RANGE had cut a declaration and told the agent its
// replacement was probably fine — while the real error was a broken literal in
// the text. Nine refusals of confidently wrong advice, including "re-issue with
// start_line 230 and end_line 241" when that was exactly what it had sent.
func TestABrokenReplacementIsNotBlamedOnTheRange(t *testing.T) {
	s := tree(map[string]string{"store.go": storeGo})
	got := refusal(t, s, ModeDevelop, edit.Edit{
		Path: "store.go", StartLine: 11, EndLine: 13,
		Replace: "func List() []Task {\n\treturn []Task{{Name: 'x}}\n}",
	})
	if !strings.Contains(got, "LINE RANGE is fine") {
		t.Errorf("a broken literal was blamed on the range:\n%s", got)
	}
	if !strings.Contains(got, "re-sending the same text with a different range cannot help") {
		t.Errorf("the refusal does not head off the wrong fix:\n%s", got)
	}
}

// AND THE RANGE IS BLAMED WHEN IT IS THE FAULT, with the numbers that fix it.
// r69: 41 refusals were a range cutting across a function, and the message said
// only "main.go is not valid Go", so the agent re-derived a correct analysis
// turn after turn.
func TestARangeThatCutADeclarationIsNamedWithTheRangeThatWorks(t *testing.T) {
	s := tree(map[string]string{"store.go": storeGo})
	// Lines 7-8 are the middle of Add, so replacing them with a whole new
	// declaration strands its body at file scope.
	// Line 11 opens func List; replacing it with a statement strands the body.
	got := refusal(t, s, ModeDevelop, edit.Edit{
		Path: "store.go", StartLine: 11, EndLine: 11, Replace: "\ts.tasks = nil",
	})
	if !strings.Contains(got, "LINE RANGE is what broke it") {
		t.Errorf("the range was not blamed:\n%s", got)
	}
	for _, want := range []string{"cut across", "Re-issue the same change with start_line",
		"Do not re-analyse the code"} {
		if !strings.Contains(got, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, got)
		}
	}
}

// CAUGHT ON THE TURN IT HAPPENS. A declaration inserted while its original is
// still present compiles to "redeclared in this block" — reported later, from a
// line number in a file the agent has not seen.
func TestADuplicateDeclarationIsNamedBeforeItCompiles(t *testing.T) {
	s := tree(map[string]string{"store.go": storeGo})
	got := refusal(t, s, ModeDevelop, edit.Edit{
		Path: "store.go", StartLine: 13, EndLine: 13,
		Replace: "}\n\nfunc List() []Task {\n\treturn nil\n}",
	})
	if !strings.Contains(got, "List") || !strings.Contains(got, "twice") {
		t.Errorf("the duplicate symbol was not named:\n%s", got)
	}
	if !strings.Contains(got, `"decl"`) {
		t.Errorf("the refusal does not name the way to replace in place:\n%s", got)
	}
}

// ONCE A FILE DIVERGES FROM THE MODEL'S MENTAL IMAGE, every addressing scheme
// fails together. Undo is the escape hatch.
func TestUndoPutsTheFileBackAndTheAgentSeesIt(t *testing.T) {
	s := tree(map[string]string{"store.go": storeGo})
	mustApply(t, s, ModeDevelop, edit.Edit{Path: "store.go", StartLine: 11, EndLine: 13,
		Replace: "func List() []Task {\n\treturn []Task{}\n}"})
	if !strings.Contains(s.Staged["store.go"], "[]Task{}") {
		t.Fatal("the setup edit did not land")
	}

	what, ok := s.Undo()
	if !ok {
		t.Fatal("undo refused with a write to undo")
	}
	if _, still := s.Staged["store.go"]; still {
		t.Errorf("the file is still staged after undo: %q", s.Staged["store.go"])
	}
	// THE READ COPY GOES WITH IT. Apply overwrites Read so the agent sees its own
	// edit; leaving that behind would have the prompt describe a file that no
	// longer exists in that shape — the exact confusion undo is here to end.
	if _, still := s.Read["store.go"]; still {
		t.Errorf("the agent still sees the undone edit:\n%s", s.Read["store.go"])
	}
	// AND IT SAYS WHAT IT DID, so the agent knows which file to read again.
	if !strings.Contains(what, "store.go") {
		t.Errorf("undo does not name what it restored: %q", what)
	}

	// Nothing to undo is a refusal, not a panic.
	if _, ok := (&State{}).Undo(); ok {
		t.Error("undo claimed to restore something with no writes behind it")
	}
}

// AND A FILE THAT WAS ALREADY STAGED IS RESTORED, not removed — the previous
// write is put back and the agent is shown that text, or its next edit addresses
// a version of the file that no longer exists.
func TestUndoingTheSecondWriteRestoresTheFirst(t *testing.T) {
	s := tree(map[string]string{"store.go": storeGo})
	mustApply(t, s, ModeDevelop, edit.Edit{
		Path: "store.go", OldStr: "return nil", Replace: "return []Task{}"})
	first := s.Staged["store.go"]

	mustApply(t, s, ModeDevelop, edit.Edit{
		Path: "store.go", OldStr: "return []Task{}", Replace: "return make([]Task, 0)"})
	if s.Staged["store.go"] == first {
		t.Fatal("the second write did not land")
	}

	if _, ok := s.Undo(); !ok {
		t.Fatal("undo refused")
	}
	if s.Staged["store.go"] != first {
		t.Errorf("the first write was not restored:\n%s", s.Staged["store.go"])
	}
	// THE AGENT MUST SEE IT. Apply overwrites Read so the agent sees its own
	// edit; undo that leaves Read behind has the prompt describing a file the
	// tree no longer holds.
	if s.Read["store.go"] != first {
		t.Errorf("the agent still sees the undone write:\n%s", s.Read["store.go"])
	}
}

// THE USEFUL UNDO IS THE LAST ONE OR TWO. An agent that needs to walk back ten
// edits has lost the thread, and the iteration budget is what should stop it.
func TestTheUndoStackIsBounded(t *testing.T) {
	s := tree(map[string]string{"config.yaml": "L1\n"})
	for i := range 6 {
		mustApply(t, s, ModeDevelop, edit.Edit{
			Path: "config.yaml", StartLine: 1, EndLine: 1, Replace: "L" + strconv.Itoa(i)})
	}
	if len(s.UndoStack) != MaxUndoDepth {
		t.Errorf("the undo stack holds %d snapshots, want %d", len(s.UndoStack), MaxUndoDepth)
	}
}

func TestTheBatchBoundsAreEnforcedWithTheNumbers(t *testing.T) {
	s := tree(map[string]string{"a.go": "package main\n"})

	if got := refusal(t, s, ModeDevelop); !strings.Contains(got, "no edits") {
		t.Errorf("an empty batch: %s", got)
	}

	var many []edit.Edit
	for i := range MaxWriteFiles + 1 {
		many = append(many, edit.Edit{Path: "f" + strconv.Itoa(i) + ".go", Replace: "package main\n"})
	}
	got := refusal(t, s, ModeDevelop, many...)
	if !strings.Contains(got, "limit is") {
		t.Errorf("an oversized batch does not name the limit: %s", got)
	}

	// A path outside the tree is refused before anything is applied.
	if err := Apply(s, []edit.Edit{{Path: "../escape.go", Replace: "x"}}, ModeDevelop); err == nil {
		t.Error("a path outside the repository was accepted")
	}
}

func TestAFileTooLargeIsRefusedWithItsSize(t *testing.T) {
	s := tree(map[string]string{"config.yaml": "L1\n"})
	got := refusal(t, s, ModeDevelop, edit.Edit{
		Path: "config.yaml", StartLine: 1, EndLine: 1, Replace: strings.Repeat("x", MaxFileBytes+1)})
	if !strings.Contains(got, "limit is") {
		t.Errorf("the refusal does not name the limit:\n%s", got)
	}
}

// An empty create would leave a file with nothing in it, which is never what was
// meant — a deliberate deletion is a range edit, not a whole-file write.
func TestAnEmptyWholeFileWriteIsRefused(t *testing.T) {
	got := refusal(t, tree(nil), ModeDevelop, edit.Edit{Path: "new.go", Replace: "   \n"})
	if !strings.Contains(got, "empty file") {
		t.Errorf("the refusal does not name the cause:\n%s", got)
	}
}

// A FILE THAT ARRIVED BROKEN FROM THE REPOSITORY IS NOT THIS EDIT'S FAULT, and
// refusing a write because of it would trap the agent on someone else's mistake.
func TestAnAlreadyBrokenFileElsewhereDoesNotBlockAWrite(t *testing.T) {
	s := tree(map[string]string{
		"broken.go": "package main\n\nfunc main() {\n",
		"store.go":  storeGo,
	})
	mustApply(t, s, ModeDevelop, edit.Edit{
		Path: "store.go", OldStr: "return nil", Replace: "return []Task{}"})
}

// The doc-file rule covers what the architect actually writes.
func TestWhatCountsAsDocumentation(t *testing.T) {
	for _, p := range []string{"README.md", "docs/GUIDE.md", "NOTES.rst", "a.adoc", "b.txt"} {
		if !IsDocFile(p) {
			t.Errorf("%q is not treated as documentation", p)
		}
	}
	for _, p := range []string{"store.go", "Makefile", "go.mod", "a.markdown"} {
		if IsDocFile(p) {
			t.Errorf("%q is treated as documentation", p)
		}
	}
}

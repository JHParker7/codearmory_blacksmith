package dev

import (
	"context"
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/edit"
	"github.com/code-armory-app/blacksmith/internal/forge"
	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/record"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

// redWith is a verification result carrying the given output. (red() with no
// argument already exists for the ordinary failing-test case.)
func redWith(out string) forge.Result {
	return forge.Result{Status: forge.StatusCompleted, ExitCode: 1, Stdout: out}
}

// THE CASE THAT PROMPTED ALL OF THIS. A specification whose helpers call
// t.Cleanup with no *testing.T in scope cannot be satisfied by any
// implementation, and the developer may not edit it. Before this it cost 23
// turns of trying to conjure a `t` in production code.
func TestAnUnsatisfiableSpecificationGoesBackToItsAuthor(t *testing.T) {
	b := &box{
		tree:     []string{"main.go", "handlers_test.go"},
		files:    map[string]string{"main.go": storeGo},
		verdicts: []forge.Result{redWith(liveVetFailure)},
	}
	// Three real edits, so the streak has something to advance on.
	g := &gw{replies: []model.ChatResult{
		readCall("main.go"),
		writeCall(edit.Edit{Path: "main.go", OldStr: "return nil", Replace: "return []Task{}"}),
		writeCall(edit.Edit{Path: "main.go", OldStr: "return []Task{}", Replace: "return []Task{{}}"}),
		writeCall(edit.Edit{Path: "main.go", OldStr: "return []Task{{}}", Replace: "return nil"}),
		writeCall(edit.Edit{Path: "main.go", OldStr: "return nil", Replace: "return []Task{}"}),
	}}
	brd := &board{}

	status, detail, err := devAgent(g, b, brd, Options{}).Handle(context.Background(), devTicket())
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if status != workflow.OutcomeReturned {
		t.Fatalf("status = %q (%s), want it returned to the author — no "+
			"implementation can define a testing handle", status, detail)
	}
	if !brd.saidAny(record.SpecRepairMarker) {
		t.Errorf("the hand-back was not recorded: %v", brd.comments)
	}
	// THE AUTHOR CANNOT REPRODUCE THIS. It does not run the implementation, so
	// without the evidence it is told only that someone was unhappy.
	said := strings.Join(brd.comments, "\n")
	if !strings.Contains(said, "handlers_test.go") {
		t.Errorf("the note does not name the file at fault:\n%s", said)
	}
	if !strings.Contains(said, "undefined: t") {
		t.Errorf("the note does not quote what went wrong:\n%s", said)
	}
}

// A HEALTHY SPECIFICATION IS NOT BLAMED. "undefined: NewStore" is what every
// test-first ticket produces before its implementation exists, and handing that
// back abandons tickets that were nearly done.
func TestTheExpectedRedIsNotHandedBack(t *testing.T) {
	b := &box{
		tree:     []string{"main.go", "store_test.go"},
		files:    map[string]string{"main.go": storeGo},
		verdicts: []forge.Result{redWith("./store_test.go:9:12: undefined: NewStore\n")},
	}
	g := &gw{replies: []model.ChatResult{
		readCall("main.go"),
		writeCall(edit.Edit{Path: "main.go", OldStr: "return nil", Replace: "return []Task{}"}),
		writeCall(edit.Edit{Path: "main.go", OldStr: "return []Task{}", Replace: "return []Task{{}}"}),
		writeCall(edit.Edit{Path: "main.go", OldStr: "return []Task{{}}", Replace: "return nil"}),
		writeCall(edit.Edit{Path: "main.go", OldStr: "return nil", Replace: "return []Task{}"}),
	}}
	brd := &board{}

	status, _, err := devAgent(g, b, brd, Options{MaxTurns: 6}).Handle(context.Background(), devTicket())
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if status == workflow.OutcomeReturned {
		t.Fatal("a specification producing only the expected red was handed back")
	}
	if brd.saidAny(record.SpecRepairMarker) {
		t.Errorf("a healthy specification was blamed: %v", brd.comments)
	}
}

// A FAULT LOCAL TO THE TEST FILE IS CONCLUSIVE ON SIGHT — no implementation
// could change it, so there is nothing for a streak to establish and waiting
// spends the whole attempt on a verdict visible in the first run.
func TestALocalTestFaultIsHandedBackImmediately(t *testing.T) {
	b := &box{
		tree:     []string{"main.go", "store_test.go"},
		files:    map[string]string{"main.go": storeGo},
		verdicts: []forge.Result{redWith("./store_test.go:12:2: declared and not used: tasks\n")},
	}
	g := &gw{replies: []model.ChatResult{
		readCall("main.go"),
		writeCall(edit.Edit{Path: "main.go", OldStr: "return nil", Replace: "return []Task{}"}),
	}}
	brd := &board{}

	status, _, err := devAgent(g, b, brd, Options{}).Handle(context.Background(), devTicket())
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if status != workflow.OutcomeReturned {
		t.Fatalf("status = %q, want it returned on the FIRST verification", status)
	}
	if len(g.reqs) > 2 {
		t.Errorf("it took %d turns to reach a conclusion visible in the first "+
			"verification", len(g.reqs))
	}
}

// ONCE THE REPAIRS ARE SPENT a person is the right answer, and the ticket says
// so rather than going round again.
func TestAnExhaustedSpecificationStopsForAPerson(t *testing.T) {
	tk := devTicket()
	for i := 0; i < MaxSpecRepairs; i++ {
		tk.Comments = append(tk.Comments, ticket.Comment{Body: record.SpecRepairMarker})
	}

	b := &box{
		tree:     []string{"main.go", "store_test.go"},
		files:    map[string]string{"main.go": storeGo},
		verdicts: []forge.Result{redWith("./store_test.go:12:2: declared and not used: tasks\n")},
	}
	g := &gw{replies: []model.ChatResult{
		readCall("main.go"),
		writeCall(edit.Edit{Path: "main.go", OldStr: "return nil", Replace: "return []Task{}"}),
	}}
	brd := &board{}

	status, _, err := devAgent(g, b, brd, Options{}).Handle(context.Background(), tk)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if status != workflow.OutcomeBlocked {
		t.Fatalf("status = %q, want it blocked after %d repairs", status, MaxSpecRepairs)
	}
	if !brd.saidAny(record.BrokenSpecMarker) {
		t.Errorf("the terminal note was not written: %v", brd.comments)
	}
	if brd.saidAny(record.SpecRepairMarker) {
		t.Error("it was handed back again after its repairs were spent")
	}
}

// ONLY THE DEVELOPER JUDGES. Every other mode MAY edit test files, so a
// test-file fault is theirs to fix and handing it to themselves would loop.
func TestOnlyTheDeveloperHandsASpecificationBack(t *testing.T) {
	for _, mode := range []Mode{ModeTest, ModeCoverage, ModeSpecMerge} {
		s := &State{}
		s.Writes = 99
		s.JudgeSpec("./store_test.go:12:2: declared and not used: tasks\n", mode)
		if s.SpecBroken != "" {
			t.Errorf("mode %d judged the specification broken; it may edit the "+
				"tests itself", mode)
		}
	}
}

// ---- the streak ----

// THE STREAK NEEDS REAL ATTEMPTS. Repeat verifications with no edit between them
// must not run it up, or a developer that merely re-checks reaches the verdict
// without having tried anything.
func TestTheStreakAdvancesOnlyAfterAnEdit(t *testing.T) {
	out := "./store_test.go:9:12: cannot use x (untyped string) as int value\n"
	// A verification only ever follows a write, so the first judgement already
	// has one behind it.
	s := &State{Writes: 1}

	for i := 0; i < 5; i++ {
		s.JudgeSpec(out, ModeDevelop) // no further writes between
	}
	if s.SpecBrokenTries != 1 {
		t.Fatalf("SpecBrokenTries = %d after five verifications with no edit, want 1",
			s.SpecBrokenTries)
	}

	s.Writes++
	s.JudgeSpec(out, ModeDevelop)
	if s.SpecBrokenTries != 2 {
		t.Fatalf("SpecBrokenTries = %d after an edit, want 2", s.SpecBrokenTries)
	}
}

// A DIFFERENT FAILURE CLEARS THE EVIDENCE: the developer is making progress on
// something else, and stale evidence is what lets an unrelated later failure
// inherit a verdict.
func TestProgressElsewhereClearsTheVerdict(t *testing.T) {
	s := &State{Writes: 10}
	broken := "./store_test.go:12:2: declared and not used: tasks\n"
	s.JudgeSpec(broken, ModeDevelop)
	if s.SpecBroken == "" {
		t.Fatal("the fixture did not reach a verdict")
	}

	s.JudgeSpec("--- FAIL: TestThing\n    store_test.go:96: want 3, got 2\n", ModeDevelop)
	if s.SpecBroken != "" || s.SpecBrokenTries != 0 || s.RedIsExpected {
		t.Fatalf("a different failure left the verdict standing: %+v", s)
	}
}

// AN IMPLEMENTATION ERROR MEANS THE TESTS ARE NOT YET TO BLAME.
func TestTheDevelopersOwnBreakageIsNotTheSpecificationsFault(t *testing.T) {
	s := &State{Writes: 10}
	s.JudgeSpec("./main.go:12:2: declared and not used: x\n"+
		"./store_test.go:9:12: undefined: NewStore\n", ModeDevelop)
	if s.SpecBroken != "" {
		t.Fatalf("SpecBroken = %q while the developer's own file does not compile",
			s.SpecBroken)
	}
}

// A RUN OF REFUSALS IS EVIDENCE ON ITS OWN, because an agent being refused is
// not working and may never reach another verification.
func TestARunOfTestEditRefusalsReachesTheVerdict(t *testing.T) {
	s := &State{}
	s.LastTest = "./store_test.go:9:12: cannot use x (untyped string) as int value\n"
	s.RedIsExpected = false

	for i := 0; i < MaxTestEditRefusals; i++ {
		s.NoteTestEditRefusal(ModeDevelop, "store_test.go")
	}
	if s.SpecBroken == "" {
		t.Fatal("a run of refused test edits reached no verdict; the agent has no " +
			"other way to say what it has found")
	}
}

// AND NOT WHEN THE RED IS THE EXPECTED RED. A developer poking at a test file
// while the only failure is a symbol it has yet to write is out of bounds, not
// onto something.
func TestRefusalsDoNotConvictOnExpectedRed(t *testing.T) {
	s := &State{}
	s.LastTest = "./store_test.go:9:12: undefined: NewStore\n"
	s.RedIsExpected = true

	for i := 0; i < MaxTestEditRefusals*2; i++ {
		s.NoteTestEditRefusal(ModeDevelop, "store_test.go")
	}
	if s.SpecBroken != "" {
		t.Fatalf("SpecBroken = %q on the expected red of test-first", s.SpecBroken)
	}
}

func TestSpecRepairsSoFar(t *testing.T) {
	tk := ticket.Ticket{Comments: []ticket.Comment{
		{Body: "unrelated"},
		{Body: record.SpecRepairMarker + " once"},
		{Body: record.SpecRepairMarker + " twice"},
	}}
	if got := SpecRepairsSoFar(tk); got != 2 {
		t.Fatalf("SpecRepairsSoFar = %d, want 2", got)
	}
	if got := SpecRepairsSoFar(ticket.Ticket{}); got != 0 {
		t.Fatalf("SpecRepairsSoFar = %d on a fresh ticket, want 0", got)
	}
}

// A VERIFICATION THAT PASSES SETTLES IT. Whatever the earlier reds suggested,
// tests that now go green were plainly satisfiable — so a verdict left standing
// would hand the ticket back after it had actually succeeded.
func TestAGreenVerificationClearsTheVerdict(t *testing.T) {
	s := &State{Writes: 1}

	s.RecordVerification("./store_test.go:12:2: declared and not used: tasks\n",
		false, ModeDevelop)
	if s.SpecBroken == "" {
		t.Fatal("a conclusive test-file fault reached no verdict")
	}

	s.RecordVerification("ok  demo  0.2s\n", true, ModeDevelop)
	if s.SpecBroken != "" {
		t.Fatalf("SpecBroken = %q after the tests passed; the ticket would be "+
			"handed back having already succeeded", s.SpecBroken)
	}
	if s.SpecBrokenTries != 0 {
		t.Errorf("the streak survived a passing verification: %d", s.SpecBrokenTries)
	}
}

// EVERY VERIFICATION IS JUDGED, and by the one place they all pass through — a
// judgement made at the call site is one a second call site can forget. The
// failed push is such a second call site.
func TestARedVerificationIsJudgedWhereverItComesFrom(t *testing.T) {
	s := &State{Writes: 1}
	s.RecordVerification("./store_test.go:20:1: syntax error: unexpected }\n",
		false, ModeDevelop)
	if s.SpecBroken == "" {
		t.Fatal("RecordVerification did not judge the output it was given")
	}
}

// A NON-TEST COMPILE ERROR TAKES PRECEDENCE OVER A TEST-ONLY PANIC.
//
// The gate concatenates its sections, so a stale panic can sit in the same
// output as a fresh compile error. While the developer's own file does not
// compile the tests cannot be judged at all — falling through to the panic route
// would blame the author for a build the developer broke.
func TestTheDevelopersOwnBreakageOutranksATestOnlyPanic(t *testing.T) {
	out := "./main.go:12:2: declared and not used: x\n" +
		"panic: boom\n\tstore_test.go:41\n"

	s := &State{Writes: 5}
	s.JudgeSpec(out, ModeDevelop)
	if s.SpecBroken != "" {
		t.Fatalf("SpecBroken = %q while main.go does not compile; the panic cannot "+
			"be judged until the build is the developer's own again", s.SpecBroken)
	}
}

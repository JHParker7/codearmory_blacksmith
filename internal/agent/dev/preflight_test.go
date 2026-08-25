package dev

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/agent/referee"
	"github.com/code-armory-app/blacksmith/internal/edit"
	"github.com/code-armory-app/blacksmith/internal/forge"
	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/record"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/transcript"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

// judge is a referee whose verdict a test dictates.
type judge struct {
	verdict *referee.Verdict
	asked   int
	tests   map[string]string

	// The later half: what Judge was asked, and what it answered.
	judgement *referee.Verdict
	judged    int
	read      map[string]string
	out       string
}

func (j *judge) Preflight(_ context.Context, _ *transcript.Recorder, _ ticket.Ticket,
	tests map[string]string) *referee.Verdict {
	j.asked++
	j.tests = tests
	return j.verdict
}

// Judge is the later half of the second opinion. Recorded the same way, so a
// test can assert on which of the two was asked and with what.
func (j *judge) Judge(_ context.Context, _ *transcript.Recorder, _ ticket.Ticket,
	read map[string]string, out string) *referee.Verdict {
	j.judged++
	j.read = read
	j.out = out
	return j.judgement
}

func devWithJudge(g *gw, b *box, brd *board, j Referee) *Agent {
	a := devAgent(g, b, brd, Options{})
	a.ref = j
	return a
}

// A SPECIFICATION THAT COMPILES AND STILL CANNOT BE SATISFIED has no textual
// signature — the mechanical routes read the compiler, and this reads the tests.
// Caught BEFORE the first turn, because the ordinary route needs several failed
// verifications and by then the attempts are spent.
func TestAnImpossibleSpecificationIsCaughtBeforeTheFirstTurn(t *testing.T) {
	j := &judge{verdict: &referee.Verdict{
		Owner:      referee.OwnerSpec,
		Reason:     "the test asserts on doc comments its own parser discards",
		Confidence: "high",
	}}
	b := &box{
		tree:  []string{"main.go", "store_test.go"},
		files: map[string]string{"store_test.go": "package main\n"},
	}
	g := &gw{}
	brd := &board{}

	status, _, err := devWithJudge(g, b, brd, j).Handle(context.Background(), devTicket())
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if status != workflow.OutcomeReturned {
		t.Fatalf("status = %q, want it returned before any turn was spent", status)
	}
	if len(g.reqs) != 0 {
		t.Errorf("the developer took %d turns on a specification already judged "+
			"impossible", len(g.reqs))
	}
	if !brd.saidAny(record.SpecRepairMarker) {
		t.Errorf("no hand-back was recorded: %v", brd.comments)
	}
	// THE REASON IS NOT DECORATION: the author reads it, and a verdict without
	// one sends the author back to guess again.
	if said := strings.Join(brd.comments, "\n"); !strings.Contains(said, "doc comments") {
		t.Errorf("the referee's reason did not reach the author:\n%s", said)
	}
}

// ONLY THE TESTS ARE JUDGED. The referee is asked whether the specification can
// be satisfied, so handing it the implementation would invite it to review that
// instead.
func TestThePreflightIsGivenTheTestsAndNotTheImplementation(t *testing.T) {
	j := &judge{}
	b := &box{
		tree: []string{"main.go", "store.go", "store_test.go", "handlers_test.go"},
		// The implementation has content too, so a filter that let it through
		// would actually be seen — with an empty fake the read returns nothing
		// either way and the assertion cannot bite.
		files: map[string]string{
			"main.go":          storeGo,
			"store.go":         storeGo,
			"store_test.go":    "package main\n",
			"handlers_test.go": "package main\n",
		},
	}
	devWithJudge(&gw{replies: []model.ChatResult{writeCall(
		edit.Edit{Path: "main.go", OldStr: "x", Replace: "y"})}},
		b, &board{}, j).Handle(context.Background(), devTicket())

	if j.asked != 1 {
		t.Fatalf("the referee was asked %d times, want once", j.asked)
	}
	for p := range j.tests {
		if !edit.IsTestFile(p) {
			t.Errorf("the referee was given %q, which is not a test", p)
		}
	}
	if len(j.tests) != 2 {
		t.Errorf("the referee saw %d test files, want both", len(j.tests))
	}
}

// A LOW-CONFIDENCE OPINION IS WORTH LESS THAN THE DEVELOPER'S NEXT ATTEMPT.
// The action it triggers is bounded — two repairs and then a person — so a maybe
// must not spend one.
func TestAHesitantVerdictIsNotActedOn(t *testing.T) {
	j := &judge{verdict: &referee.Verdict{
		Owner: referee.OwnerSpec, Reason: "possibly wrong", Confidence: "medium",
	}}
	b := &box{
		tree:     []string{"main.go", "store_test.go"},
		files:    map[string]string{"main.go": storeGo, "store_test.go": "package main\n"},
		verdicts: []forge.Result{green()},
	}
	g := &gw{replies: []model.ChatResult{
		writeCall(edit.Edit{Path: "main.go", OldStr: "return nil", Replace: "return []Task{}"}),
	}}
	brd := &board{}

	status, _, err := devWithJudge(g, b, brd, j).Handle(context.Background(), devTicket())
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if status == workflow.OutcomeReturned {
		t.Fatal("a medium-confidence verdict spent one of two repairs")
	}
}

// AND A VERDICT THAT BLAMES THE DEVELOPER IS NOT A HAND-BACK. That is simply the
// job.
func TestAVerdictAgainstTheDeveloperIsNotAHandBack(t *testing.T) {
	j := &judge{verdict: &referee.Verdict{
		Owner: referee.OwnerDev, Reason: "the store is never initialised", Confidence: "high",
	}}
	b := &box{
		tree:     []string{"main.go", "store_test.go"},
		files:    map[string]string{"main.go": storeGo, "store_test.go": "package main\n"},
		verdicts: []forge.Result{green()},
	}
	g := &gw{replies: []model.ChatResult{
		writeCall(edit.Edit{Path: "main.go", OldStr: "return nil", Replace: "return []Task{}"}),
	}}
	brd := &board{}

	status, _, err := devWithJudge(g, b, brd, j).Handle(context.Background(), devTicket())
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if status == workflow.OutcomeReturned {
		t.Fatal("the ticket was handed back over a verdict blaming the developer")
	}
}

// A HOST WITH NO REFEREE STILL DEVELOPS. Nil is a valid configuration, not a
// missing dependency.
func TestNoRefereeMeansNoPreflight(t *testing.T) {
	b := &box{
		tree:     []string{"main.go", "store_test.go"},
		files:    map[string]string{"main.go": storeGo},
		verdicts: []forge.Result{green()},
	}
	g := &gw{replies: []model.ChatResult{
		writeCall(edit.Edit{Path: "main.go", OldStr: "return nil", Replace: "return []Task{}"}),
	}}
	if _, _, err := devAgent(g, b, &board{}, Options{}).Handle(context.Background(), devTicket()); err != nil {
		t.Fatalf("Handle without a referee: %v", err)
	}
}

// EVERY OTHER STAGE MAY EDIT THE TESTS IT IS GIVEN, so asking whether they can
// be satisfied is meaningless — and acting on it would hand a stage its own work.
func TestOnlyTheDeveloperIsPreflighted(t *testing.T) {
	for _, mode := range []Mode{ModeTest, ModeCoverage, ModeSpecMerge} {
		j := &judge{verdict: &referee.Verdict{
			Owner: referee.OwnerSpec, Reason: "impossible", Confidence: "high",
		}}
		b := &box{
			tree:     []string{"main.go", "store_test.go"},
			files:    map[string]string{"store_test.go": "package main\n"},
			verdicts: []forge.Result{green()},
		}
		g := &gw{replies: []model.ChatResult{
			writeCall(edit.Edit{Path: "store_test.go", OldStr: "package main", Replace: "package main\n"}),
		}}
		a := devAgent(g, b, &board{}, Options{Mode: mode})
		a.ref = j
		a.Handle(context.Background(), devTicket())

		if j.asked != 0 {
			t.Errorf("mode %d was preflighted; it may edit the tests itself", mode)
		}
	}
}

// A TREE WITH NO TESTS HAS NOTHING TO JUDGE, and asking costs a model call.
func TestATreeWithNoTestsIsNotPreflighted(t *testing.T) {
	j := &judge{}
	b := &box{
		tree:     []string{"main.go"},
		files:    map[string]string{"main.go": storeGo},
		verdicts: []forge.Result{green()},
	}
	g := &gw{replies: []model.ChatResult{
		writeCall(edit.Edit{Path: "main.go", OldStr: "return nil", Replace: "return []Task{}"}),
	}}
	devWithJudge(g, b, &board{}, j).Handle(context.Background(), devTicket())

	if j.asked != 0 {
		t.Error("the referee was asked about a tree with no tests")
	}
}

// A FAILED READ IS NOT A VERDICT. The developer will read the tests itself and
// the ordinary routes still apply; refusing to start over a second opinion that
// could not be gathered is worse than not having one.
func TestAFailedReadDoesNotStopTheAttempt(t *testing.T) {
	j := &judge{verdict: &referee.Verdict{
		Owner: referee.OwnerSpec, Reason: "impossible", Confidence: "high",
	}}
	b := &box{
		tree:     []string{"main.go", "store_test.go"},
		files:    map[string]string{"main.go": storeGo},
		verdicts: []forge.Result{green()},
		readFail: true,
	}
	g := &gw{replies: []model.ChatResult{
		writeCall(edit.Edit{Path: "main.go", OldStr: "return nil", Replace: "return []Task{}"}),
	}}
	brd := &board{}

	_, _, err := devWithJudge(g, b, brd, j).Handle(context.Background(), devTicket())
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("a failed preflight read ended the stage: %v", err)
	}
	if j.asked != 0 {
		t.Error("the referee was asked about tests that could not be read")
	}
	if brd.saidAny(record.SpecRepairMarker) {
		t.Error("the ticket was handed back on a verdict never actually obtained")
	}
}

package agents

import (
	"context"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/tools"
)

// A checkless stage is judged the same at every exit. The stall path took a
// written deliverable as completion and the budget path did not, so an
// architect that wrote its plan in one shot and spent the rest of its eight
// turns re-reading it was reported FAILED with the deliverable on disk — and
// which verdict a run got depended on nothing but whether the idle bound fired
// before the budget did.
func TestACheckessStageThatSpentItsBudgetAfterDeliveringHasPassed(t *testing.T) {
	o := unchecked()
	o.Guard = tools.AllowAll
	o.Tools = []string{tools.ReadFiles, tools.ListFiles, tools.WriteFile}
	o.MaxIterations = 5 // below MaxIdleTurns, so the budget exit fires first

	gw := &fakeGateway{replies: []model.ChatResult{
		calls(tools.WriteFile, `{"path":"PLAN.md","replace":"the plan\n","summary":"x","type":"docs"}`),
		calls(tools.ReadFiles, `{"paths":["PLAN.md"]}`),
		calls(tools.ReadFiles, `{"paths":["PLAN.md"]}`),
		calls(tools.ReadFiles, `{"paths":["PLAN.md"]}`),
		calls(tools.ReadFiles, `{"paths":["PLAN.md"]}`),
	}}
	a := Creator{Gateway: gw}.New(map[string]string{}, o)

	out, err := a.Run(context.Background(), "plan it")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !out.Passed {
		t.Fatal("a checkless stage that delivered and then spent its budget was failed")
	}
	if got := a.Files()["PLAN.md"]; got != "the plan\n" {
		t.Fatalf("the deliverable is not on disk: %q", got)
	}
}

// ...and one that spent its budget having written NOTHING has still failed.
func TestACheckessStageThatSpentItsBudgetEmptyHasFailed(t *testing.T) {
	o := unchecked()
	o.Guard = tools.AllowAll
	o.Tools = []string{tools.ReadFiles, tools.ListFiles, tools.WriteFile}
	o.MaxIterations = 5

	var replies []model.ChatResult
	for i := 0; i < 5; i++ {
		replies = append(replies, calls(tools.ReadFiles, `{"paths":["PLAN.md"]}`))
	}
	gw := &fakeGateway{replies: replies}
	a := Creator{Gateway: gw}.New(map[string]string{}, o)

	out, err := a.Run(context.Background(), "plan it")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Passed {
		t.Fatal("an empty-handed budget exit was reported as passed")
	}
}

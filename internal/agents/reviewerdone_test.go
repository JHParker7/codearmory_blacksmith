package agents

import (
	"context"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/model"

	"github.com/code-armory-app/blacksmith/internal/tools"
)

// THE FAILURE THAT REROLLED A SUCCESS THREE TIMES: the reviewer filed seven
// findings, hit its turn cap, and was reported failed because completion
// gated on tree writes — which it never makes, its deliverable being tickets.
// A checkless stage that produced tickets has delivered, at every exit.
func TestAReviewerThatFiledTicketsAndHitItsBudgetHasPassed(t *testing.T) {
	o := maker().PlanSec(nil).opts
	o.MaxIterations = 4

	gw := &fakeGateway{}
	for i := 0; i < o.MaxIterations; i++ {
		gw.replies = append(gw.replies, calls(tools.FileTicket,
			`{"title":"finding","body":"file.go:1 bad thing","severity":"high"}`))
	}
	filed := 0
	a := Creator{
		Gateway: gw,
		FileTicket: func(string, string, string) (string, error) {
			filed++
			return "tk", nil
		},
	}.New(map[string]string{"a.go": "package a\n"}, o)

	out, err := a.Run(context.Background(), "review")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !out.Passed {
		t.Fatal("a reviewer that filed findings to its budget was reported failed")
	}
	if filed == 0 {
		t.Fatal("nothing was filed")
	}
}

// ...but a reviewer that filed NOTHING and read in circles has not delivered,
// so the produced-gate cannot simply always pass a checkless stage.
func TestAReviewerThatFiledNothingAndStalledFails(t *testing.T) {
	o := maker().PlanSec(nil).opts
	o.MaxIterations = 3
	gw := &fakeGateway{}
	for i := 0; i < o.MaxIterations; i++ {
		gw.replies = append(gw.replies, calls(tools.ReadFiles, `{"paths":["a.go"]}`))
	}
	a := Creator{Gateway: gw, FileTicket: func(string, string, string) (string, error) { return "tk", nil }}.
		New(map[string]string{"a.go": "package a\n"}, o)

	out, err := a.Run(context.Background(), "review")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Passed {
		t.Fatal("a reviewer that read in circles and filed nothing was passed")
	}
}

// A clean bill is a real outcome: the reviewer that finds nothing files
// nothing and ANSWERS its verdict, and that prose finish passes.
func TestAReviewerWithACleanBillPassesOnItsVerdict(t *testing.T) {
	o := maker().PlanSec(nil).opts
	// A prose verdict, no tools, no tickets: the clean-bill path.
	gw := &fakeGateway{replies: []model.ChatResult{
		{Content: "No real findings. Verdict: ship."},
	}}
	a := Creator{Gateway: gw, FileTicket: func(string, string, string) (string, error) { return "tk", nil }}.
		New(map[string]string{"a.go": "package a\n"}, o)

	out, err := a.Run(context.Background(), "review")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !out.Passed {
		t.Fatal("a clean-bill reviewer that gave its verdict was reported failed")
	}
	if out.Answer == "" {
		t.Fatal("the verdict was lost")
	}
}

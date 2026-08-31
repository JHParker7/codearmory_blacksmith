package agents

import (
	"context"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/tools"
)

// APPROVAL IS TERMINAL: once the reviewer merges, the stage ends — it does not
// keep calling merge_fix on an already-merged fix. Measured on the first live
// end-to-end, where one approval cost four merges.
func TestApprovalEndsTheReviewImmediately(t *testing.T) {
	calls := 0
	var replies []model.ChatResult
	for i := 0; i < 10; i++ {
		replies = append(replies, model.ChatResult{
			Calls: []model.ToolCall{{Name: tools.MergeFix, Arguments: `{"reason":"good"}`}},
		})
	}
	gw := &fakeGateway{replies: replies}
	a := Creator{Gateway: gw, MergeFix: func(string) (string, error) {
		calls++
		return "fix -> dev", nil
	}}.New(map[string]string{"a.go": "package a\n"}, maker().MergeReviewer(nil).opts)

	out, err := a.Run(context.Background(), "review")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !out.Passed {
		t.Fatal("the approved review did not pass")
	}
	if calls != 1 {
		t.Fatalf("merge was called %d times, want exactly 1", calls)
	}
	if out.Iterations != 1 {
		t.Fatalf("the review ran %d turns after approving, want 1", out.Iterations)
	}
}

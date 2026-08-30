package agents

import (
	"context"
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/tools"
)

// The review agent reads and judges — it may merge, but it writes no code and
// runs no commands. Merging is its ONLY action beyond reading.
func TestTheMergeReviewerJudgesAndMergesNothingElse(t *testing.T) {
	a := maker().MergeReviewer(nil)

	if !offers(a, tools.MergeFix) {
		t.Fatal("the reviewer cannot merge")
	}
	for _, name := range []string{tools.WriteFile, tools.RunCommand, tools.UndoEdit, tools.FileTicket} {
		if offers(a, name) {
			t.Errorf("the reviewer is offered %s", name)
		}
	}
	for _, p := range []string{"store.go", "dev", "anything.go"} {
		if err := a.opts.Guard(p); err == nil {
			t.Errorf("the reviewer may write %s", p)
		}
	}
	if a.Check() != "" {
		t.Fatalf("the reviewer gates on a command: %q", a.Check())
	}
}

// Approval flows through merge_fix; rejection is prose. The tool must reach the
// MergeFix callback, and a rejecting reviewer must never call it.
func TestApprovalMergesAndRejectionDoesNot(t *testing.T) {
	// Approve: the reviewer calls merge_fix.
	merged := false
	gw := &fakeGateway{replies: []model.ChatResult{
		{Calls: []model.ToolCall{{Name: tools.MergeFix, Arguments: `{"reason":"correct and safe"}`}}},
	}}
	a := Creator{Gateway: gw, MergeFix: func(reason string) (string, error) {
		merged = true
		return "fix/abc → dev", nil
	}}.New(map[string]string{"a.go": "package a\n"}, maker().MergeReviewer(nil).opts)

	out, err := a.Run(context.Background(), "review it")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !merged || !out.Passed {
		t.Fatalf("approval did not merge: merged=%v passed=%v", merged, out.Passed)
	}

	// Reject: prose verdict, no merge.
	merged2 := false
	gw2 := &fakeGateway{replies: []model.ChatResult{
		{Content: "This weakens the test that proves the bug. Not merging."},
	}}
	a2 := Creator{Gateway: gw2, MergeFix: func(string) (string, error) {
		merged2 = true
		return "", nil
	}}.New(map[string]string{"a.go": "package a\n"}, maker().MergeReviewer(nil).opts)

	out2, err := a2.Run(context.Background(), "review it")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if merged2 {
		t.Fatal("a rejecting reviewer merged anyway")
	}
	if !strings.Contains(out2.Answer, "Not merging") {
		t.Fatalf("the rejection reason was lost: %q", out2.Answer)
	}
}

// A reviewer with no merge sink is told to give its verdict in prose, never to
// merge by other means.
func TestMergeFixWithoutASinkSaysSo(t *testing.T) {
	s := &tools.Set{
		Workspace: tools.NewWorkspace(nil, tools.DenyAll),
		Names:     []string{tools.MergeFix},
	}
	out, err := s.Invoke(context.Background(), tools.MergeFix, `{"reason":"looks good"}`)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !strings.Contains(out, "no merge is wired") {
		t.Fatalf("the missing-sink message is wrong: %q", out)
	}
}

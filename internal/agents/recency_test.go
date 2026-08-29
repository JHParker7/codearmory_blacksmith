package agents

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/tools"
)

// filler builds a file big enough that a few of them exhaust the prompt budget.
func filler(name string, lines int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "package main\n\n// %s\n", name)
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&b, "// %s padding line %d, long enough to consume the budget quickly\n", name, i)
	}
	return b.String()
}

// THE FAILURE THAT KILLED TWO-STAGE RUN 1.
//
// The developer hit a compile error in a 335-line test file, re-read that file
// fifteen times, and stalled at the idle bound. Under first-touch ordering the
// file it kept asking for was the one the budget kept discarding, so re-reading
// it could never bring it back — the one recovery move available to the agent
// was the one move that could not work.
//
// Re-reading must PROMOTE a file back into the prompt.
func TestReReadingAFileRescuesItFromTheBudget(t *testing.T) {
	files := map[string]string{
		"first.go":  filler("first", 700),
		"second.go": filler("second", 700),
		"third.go":  filler("third", 700),
	}

	o := checked()
	o.MaxIterations = 5
	gw := &fakeGateway{replies: []model.ChatResult{
		// Touch all three, oldest to newest.
		calls(tools.ReadFiles, `{"paths":["first.go"]}`),
		calls(tools.ReadFiles, `{"paths":["second.go"]}`),
		calls(tools.ReadFiles, `{"paths":["third.go"]}`),
		// Now go back to the oldest, which is the one the budget would drop.
		calls(tools.ReadFiles, `{"paths":["first.go"]}`),
		calls(tools.ListFiles, `{}`),
	}}
	a := Creator{Gateway: gw, Sandbox: fakeSandbox{exit: 1}}.New(files, o)

	if _, err := a.Run(context.Background(), "look"); err != nil {
		t.Fatalf("run: %v", err)
	}

	// The turn after the agent re-read first.go: it must be in front of it.
	last := gw.seen[len(gw.seen)-1].Messages[1].Content
	block := filesBlock(t, last)
	if !strings.Contains(block, "// first padding line 699") {
		t.Fatalf("re-reading first.go did not bring it back into the prompt.\nBudget=%d, block=%d chars",
			MaxKnownChars, len(block))
	}
}

// The corollary: something must still be dropped when the budget is exceeded,
// and it must be the least recently used file rather than an arbitrary one.
func TestTheLeastRecentlyUsedFileIsTheOneDropped(t *testing.T) {
	files := map[string]string{
		"stale.go": filler("stale", 700),
		"warm.go":  filler("warm", 700),
		"hot.go":   filler("hot", 700),
	}

	o := checked()
	o.MaxIterations = 4
	gw := &fakeGateway{replies: []model.ChatResult{
		calls(tools.ReadFiles, `{"paths":["stale.go"]}`),
		calls(tools.ReadFiles, `{"paths":["warm.go"]}`),
		calls(tools.ReadFiles, `{"paths":["hot.go"]}`),
		calls(tools.ListFiles, `{}`),
	}}
	a := Creator{Gateway: gw, Sandbox: fakeSandbox{exit: 1}}.New(files, o)

	if _, err := a.Run(context.Background(), "look"); err != nil {
		t.Fatalf("run: %v", err)
	}

	last := gw.seen[len(gw.seen)-1].Messages[1].Content
	block := filesBlock(t, last)
	if !strings.Contains(block, "// hot padding line 699") {
		t.Fatal("the most recently used file was not kept")
	}
	// And whatever was dropped is named rather than silently missing.
	if strings.Contains(block, "not shown") && !strings.Contains(block, "stale.go") {
		t.Errorf("a file was dropped but the stalest one was not the casualty:\n%s",
			block[strings.Index(block, "not shown"):])
	}
}

// A write is a use too: the file just edited is the one the next action depends
// on, so it must not be droppable in favour of something read long ago.
func TestWritingAFilePromotesIt(t *testing.T) {
	o := checked()
	o.Guard = tools.AllowAll
	o.MaxIterations = 4
	gw := &fakeGateway{replies: []model.ChatResult{
		calls(tools.ReadFiles, `{"paths":["a.go"]}`),
		calls(tools.ReadFiles, `{"paths":["b.go"]}`),
		calls(tools.WriteFile,
			`{"path":"a.go","old_str":"// marker","replace":"// EDITED MARKER","summary":"x","type":"fix"}`),
		calls(tools.ListFiles, `{}`),
	}}
	a := Creator{Gateway: gw, Sandbox: fakeSandbox{exit: 1}}.New(map[string]string{
		"a.go": "package main\n\n// marker\n",
		"b.go": "package main\n",
	}, o)

	if _, err := a.Run(context.Background(), "edit"); err != nil {
		t.Fatalf("run: %v", err)
	}
	known := a.space.Known()
	if len(known) == 0 || known[len(known)-1] != "a.go" {
		t.Fatalf("the file just written is not the most recent: %v", known)
	}
}

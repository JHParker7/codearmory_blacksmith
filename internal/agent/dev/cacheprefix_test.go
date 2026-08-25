package dev

import (
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/edit"
)

// THE CACHED PREFIX MUST SURVIVE AN EDIT, and it did not.
//
// Read is deliberately updated with staged content so the agent sees its own
// work, and Read was rendered inside the half a backend caches — so every
// SUCCESSFUL edit rewrote the prefix and the whole prompt was reprocessed. The
// stage that edits on nearly every turn therefore never got a cache hit at all.
//
// Measured on a live run: the developer paid ~30s per turn against a
// 25,000-token prompt whatever it asked for — one turn emitted 29 tokens in
// 30.8s — while spec-merge at 19,000 tokens dropped from 30.1s to 11.6s the
// moment its prefix stopped changing. 12.1 tok/s against 47.8.
func TestAnEditDoesNotDisturbTheCacheablePrefix(t *testing.T) {
	s := state()
	before := RenderWorld(job(), Reasons{}, s)

	// The agent edits a file it has read, the way Apply records it.
	s.Read["store.go"] = "package main\n\nfunc List() []Task { return nil }\n"
	s.Staged = map[string]string{"store.go": s.Read["store.go"]}

	after := RenderWorld(job(), Reasons{}, s)
	if before != after {
		t.Errorf("the cacheable half changed when an edit landed, so the whole "+
			"prompt is reprocessed:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}

	// And the edit is still shown, late, or the agent cannot see its own work.
	changes := RenderChanges(s)
	if !strings.Contains(changes, "func List() []Task") {
		t.Errorf("the agent is not shown what it changed:\n%s", changes)
	}
}

// READING A NEW FILE MAY ONLY APPEND. A read that inserts text into the middle
// of the prefix invalidates everything after it — the same fault as rewriting,
// arriving on reads instead of writes. This is why the files render in arrival
// order rather than sorted: "a.go" read second would sort in front of "store.go"
// read first.
func TestReadingAnotherFileOnlyAppends(t *testing.T) {
	s := state()
	before := RenderWorld(job(), Reasons{}, s)

	// A path that sorts BEFORE the one already read.
	s.RecordRead([]string{"a.go"}, map[string]string{"a.go": "package main\n"})
	after := RenderWorld(job(), Reasons{}, s)

	if !strings.HasPrefix(after, before) {
		t.Errorf("a later read did not append; the prefix moved:\n--- before ---\n%s"+
			"\n--- after ---\n%s", before, after)
	}
}

// THE WHOLE REQUEST ORDERS BY VOLATILITY. The changed files sit after the
// history and before the progress, so an edit costs the tail and nothing above.
func TestTheChangedFilesSitInTheVolatileTail(t *testing.T) {
	s := loopState()
	s.Iteration, s.Budget = 3, 10
	s.Read["store.go"] = storeGo + "\n// edited\n"
	s.Staged = map[string]string{"store.go": s.Read["store.go"]}

	req := BuildRequest(job(), Reasons{}, s, ModeDevelop, "SYSTEM", true)

	var worldAt, changesAt, progressAt = -1, -1, -1
	for i, m := range req.Messages {
		switch {
		case strings.Contains(m.Content, "REPOSITORY FILES"):
			worldAt = i
		case strings.Contains(m.Content, "CHANGED THEM"):
			changesAt = i
		case strings.Contains(m.Content, "ITERATION"):
			progressAt = i
		}
	}
	if worldAt < 0 || changesAt < 0 || progressAt < 0 {
		t.Fatalf("world=%d changes=%d progress=%d in %d messages",
			worldAt, changesAt, progressAt, len(req.Messages))
	}
	if !(worldAt < changesAt && changesAt <= progressAt) {
		t.Errorf("the order is world=%d changes=%d progress=%d; an edit must not "+
			"sit above the cacheable half", worldAt, changesAt, progressAt)
	}
}

// A FILE THE AGENT CREATED WAS NEVER READ, so it has no copy in the prefix and
// belongs wholly to the tail. Rendering it above would put a file that changes
// on every write into the half that must not change.
func TestACreatedFileIsShownOnlyInTheTail(t *testing.T) {
	s := state()
	if err := Apply(s, []edit.Edit{{
		Path: "new.go", Replace: "package main\n\nfunc New() {}\n",
	}}, ModeDevelop); err != nil {
		t.Fatalf("creating a file was refused: %v", err)
	}

	if strings.Contains(RenderWorld(job(), Reasons{}, s), "func New()") {
		t.Error("a created file was rendered in the cacheable half")
	}
	if !strings.Contains(RenderChanges(s), "func New()") {
		t.Error("a created file is not shown to the agent at all")
	}
}

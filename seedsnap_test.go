package main

import (
	"context"
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/agents"
	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/tools"
)

// THE SEED IS THE HISTORY'S FIRST REAL COMMIT, not a stowaway in the first
// agent write. On the first real project run the snapshot committed an empty
// directory — the seed lived in memory until a stage end — and the next
// write's add -A swept 2,044 project lines in under "feat: Add TaskCount
// method". A history whose first feature commit contains the whole inherited
// project is not a history of the run.
func TestTheSeedIsItsOwnCommitAndWritesStayTheirOwnSize(t *testing.T) {
	dir := t.TempDir()

	gw := &scriptGateway{replies: []model.ChatResult{
		{Calls: []model.ToolCall{{Name: tools.WriteFile,
			Arguments: `{"path":"NEW.md","replace":"new work\n","summary":"the run's own write","type":"feat"}`}}},
		{Calls: []model.ToolCall{{Name: tools.RunCommand, Arguments: `{}`}}},
	}}
	maker := agents.Creator{
		Gateway: gw,
		Sandbox: greenBox{},
		OnWrite: func(path, content string, deleted bool, message string) {
			curGit.write(path, content, deleted, message)
		},
	}

	seed := map[string]string{
		"seeded/a.go": "package seeded\n",
		"seeded/b.go": "package seeded\n",
	}
	if err := runWithReroll(context.Background(), maker, []string{stageBaseline}, seed, dir, "extend it"); err != nil {
		t.Fatalf("run: %v", err)
	}

	subjects := gitOut(t, dir, "log", "--reverse", "--format=%s")
	if !strings.Contains(subjects, "chore: the tree as the request found it") {
		t.Fatalf("the seed has no commit of its own:\n%s", subjects)
	}

	seedStat := gitOut(t, dir, "show", "--stat", "--format=", "HEAD~2")
	_ = seedStat
	writeStat := gitOut(t, dir, "log", "--format=%s|", "--stat", "--grep", "the run's own write")
	if !strings.Contains(writeStat, "NEW.md") {
		t.Fatalf("the write's commit does not carry its file:\n%s", writeStat)
	}
	if strings.Contains(writeStat, "seeded/") {
		t.Fatalf("the seed was swept into the write's commit:\n%s", writeStat)
	}
}

// scriptGateway replays a fixed set of replies.
type scriptGateway struct{ replies []model.ChatResult }

func (g *scriptGateway) Chat(context.Context, model.Class, model.ChatRequest) (model.ChatResult, error) {
	if len(g.replies) == 0 {
		return model.ChatResult{Content: "done"}, nil
	}
	r := g.replies[0]
	g.replies = g.replies[1:]
	return r, nil
}

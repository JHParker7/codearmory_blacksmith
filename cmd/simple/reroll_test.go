package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/agents"
	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/tools"
)

// seedGateway plays a bad seed and then a good one: on its first life it
// writes a junk marker and stalls (idle reads until the bound fires); once
// rescued by the reroll it finishes on its first check.
type seedGateway struct{ calls, badCalls int }

// seedBox is red while the seed is bad and green once it recovers, so the
// junk write's auto-check cannot accidentally pass the doomed attempt.
type seedBox struct{ gw *seedGateway }

func (b seedBox) Run(context.Context, map[string]string, string) (tools.Output, error) {
	if b.gw.calls <= b.gw.badCalls {
		return tools.Output{ExitCode: 1, Stdout: "red\n"}, nil
	}
	return tools.Output{ExitCode: 0, Stdout: "ok\n"}, nil
}

func (g *seedGateway) Chat(_ context.Context, _ model.Class, _ model.ChatRequest) (model.ChatResult, error) {
	g.calls++
	switch {
	case g.calls == 1:
		return model.ChatResult{Calls: []model.ToolCall{{
			Name:      tools.WriteFile,
			Arguments: `{"path":"junk.md","replace":"the bad seed wrote this\n","summary":"x","type":"docs"}`,
		}}}, nil
	case g.calls <= g.badCalls:
		return model.ChatResult{Calls: []model.ToolCall{{
			Name: tools.ReadFiles, Arguments: `{"paths":["junk.md"]}`,
		}}}, nil
	}
	return model.ChatResult{Calls: []model.ToolCall{{Name: tools.RunCommand, Arguments: "{}"}}}, nil
}

// THE OPERATOR'S RELIABILITY RULE: a failed run is rerolled from the ORIGINAL
// tree, at most three attempts. The revert matters as much as the retry —
// the reroll's whole value is a fresh draw, so the bad seed's tree must not
// leak into the next attempt, in memory or on disk.
func TestAFailedRunIsRerolledFromTheOriginalTree(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	gw := &seedGateway{badCalls: 1 + agents.MaxIdleTurns}
	maker := agents.Creator{Gateway: gw, Sandbox: seedBox{gw: gw}}

	err := runWithReroll(context.Background(), maker, []string{stageBaseline},
		map[string]string{"README.md": "original\n"}, dir, "build it")
	if err != nil {
		t.Fatalf("the reroll did not rescue the run: %v", err)
	}

	// The bad seed's junk must be gone from disk, the original still there.
	if _, err := os.Stat(filepath.Join(dir, "junk.md")); !os.IsNotExist(err) {
		t.Fatal("the failed attempt's file survived the revert on disk")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "README.md")); string(got) != "original\n" {
		t.Fatalf("the original file did not survive: %q", got)
	}
}

// Three bad seeds in a row is a failed run, reported with the attempt count —
// not a fourth quiet draw.
func TestTheRerollIsBoundedAtThreeAttempts(t *testing.T) {
	dir := t.TempDir()
	gw := &seedGateway{badCalls: 1 << 30} // never recovers
	maker := agents.Creator{Gateway: gw, Sandbox: seedBox{gw: gw}}

	err := runWithReroll(context.Background(), maker, []string{stageBaseline},
		map[string]string{}, dir, "build it")
	if err == nil {
		t.Fatal("a permanently bad seed did not fail the run")
	}
	if !strings.Contains(err.Error(), "after 3 attempts") {
		t.Fatalf("the error does not carry the attempt count: %v", err)
	}
}

// revertTree touches only what the failed attempt produced. It must never
// become a general wipe of a directory that can be a real checkout.
func TestRevertTreeTouchesOnlyTheAttemptsOwnFiles(t *testing.T) {
	dir := t.TempDir()
	// A bystander file the harness never knew about.
	if err := os.WriteFile(filepath.Join(dir, "bystander.txt"), []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	original := map[string]string{"README.md": "original\n"}
	dirty := map[string]string{"README.md": "mangled\n", "junk.go": "package junk\n"}
	if err := writeTree(dir, dirty); err != nil {
		t.Fatal(err)
	}

	if err := revertTree(dir, original, dirty); err != nil {
		t.Fatalf("revert: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "README.md")); string(got) != "original\n" {
		t.Fatalf("the changed file was not restored: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "junk.go")); !os.IsNotExist(err) {
		t.Fatal("the created file was not removed")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "bystander.txt")); string(got) != "keep me\n" {
		t.Fatal("a file the harness never produced was touched")
	}
}

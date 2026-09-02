package main

import (
	"strings"
	"testing"
)

// The agent trace is what a workflow run view shows for an agent step (the
// manifest names `stdout` as the output_field). These tests lock the contract
// the run view depends on: ONLY a write_file's call (a whole file body) is
// capped, everything else — reasoning, command/check output, results — is kept
// WHOLE (that content is what an operator reads), and the trace is still bounded
// so a 60-turn role cannot bloat the poll response the engine reads every few
// seconds.

func TestCapTraceLineCapsWriteFileCall(t *testing.T) {
	body := strings.Repeat("y", 5000)
	line := "spec: write_file({\"path\":\"task.go\",\"replace\":\"" + body + "\"}) -> Edited: task.go"

	got := capTraceLine(line)

	name, rest, ok := strings.Cut(got, ": ")
	if !ok || name != "spec" {
		t.Fatalf("lost the role prefix: %q", got)
	}
	call, res, ok := strings.Cut(rest, " -> ")
	if !ok {
		t.Fatalf("lost the call -> result split: %q", got)
	}
	if n := len([]rune(call)); n > 200 {
		t.Errorf("write_file call not capped to 200: got %d runes", n)
	}
	if res != "Edited: task.go" {
		t.Errorf("result should pass through whole: %q", res)
	}
}

func TestCapTraceLineKeepsNonWriteWhole(t *testing.T) {
	// An auto-check line: the check output is exactly what the operator reads, so
	// it must survive whole even when long.
	out := strings.Repeat("z", 3000)
	line := "req-dev: auto-check -> $ go test ./...\n" + out
	if got := capTraceLine(line); got != line {
		t.Errorf("non-write line was altered:\nwant %q\ngot  %q", line, got)
	}
}

func TestCapTraceLineKeepsReasoningWhole(t *testing.T) {
	line := "architect: thinking: " + strings.Repeat("z", 3000)
	if got := capTraceLine(line); got != line {
		t.Errorf("reasoning was capped; want whole:\ngot %q", got)
	}
}

func TestAgentTraceStringSeparatesEntries(t *testing.T) {
	tr := &agentTrace{}
	tr.add("dev: thinking: first")
	tr.add("dev: run_command({}) -> ok")
	if !strings.Contains(tr.String(), "first\n\ndev:") {
		t.Errorf("entries not separated by a blank line: %q", tr.String())
	}
}

func TestAgentTraceBoundedToMaxLines(t *testing.T) {
	tr := &agentTrace{}
	for i := 0; i < traceMaxLines*3; i++ {
		tr.add("dev: run_command({}) -> ok")
	}
	if got := len(tr.lines); got != traceMaxLines {
		t.Fatalf("trace not bounded: got %d lines, want %d", got, traceMaxLines)
	}
}

func TestAgentTraceWithCheckAppendsFooter(t *testing.T) {
	tr := &agentTrace{}
	tr.add("dev: write_file({}) -> ok")
	out := tr.withCheck("go build ./... && go test ./...: PASS")
	if !strings.Contains(out, "── check ──") {
		t.Errorf("check footer missing: %q", out)
	}
	if !strings.Contains(out, "dev: write_file") {
		t.Errorf("trace body missing from combined output: %q", out)
	}
}

func TestAgentTraceWithCheckEmptyTrace(t *testing.T) {
	tr := &agentTrace{}
	out := tr.withCheck("PASS")
	if strings.HasPrefix(out, "\n") {
		t.Errorf("empty trace should not lead with blank lines: %q", out)
	}
	if !strings.Contains(out, "PASS") {
		t.Errorf("check missing: %q", out)
	}
}

package main

import (
	"strings"
	"testing"
)

// The agent trace is what a workflow run view shows for an agent step (the
// manifest names `stdout` as the output_field). These tests lock the two
// properties that keep it useful: content is capped so one file write cannot
// fill the log, and the trace is bounded so a 60-turn role cannot bloat the
// poll response the engine reads every few seconds.

func TestCapTraceLineCapsEachSegmentTo50(t *testing.T) {
	longPath := strings.Repeat("x", 200)
	longRes := strings.Repeat("y", 200)
	line := "spec: write_file({\"path\":\"" + longPath + "\"}) -> " + longRes

	got := capTraceLine(line)

	name, body, ok := strings.Cut(got, ": ")
	if !ok || name != "spec" {
		t.Fatalf("lost the role prefix: %q", got)
	}
	call, res, ok := strings.Cut(body, " -> ")
	if !ok {
		t.Fatalf("lost the call -> result split: %q", got)
	}
	// truncate(s, 50) yields at most 50 runes (49 + the ellipsis).
	if n := len([]rune(call)); n > 50 {
		t.Errorf("call segment not capped to 50: got %d runes (%q)", n, call)
	}
	if n := len([]rune(res)); n > 50 {
		t.Errorf("result segment not capped to 50: got %d runes (%q)", n, res)
	}
}

func TestCapTraceLineNoArrow(t *testing.T) {
	line := "architect: thinking: " + strings.Repeat("z", 200)
	got := capTraceLine(line)
	if !strings.HasPrefix(got, "architect: ") {
		t.Fatalf("lost the role prefix: %q", got)
	}
	body := strings.TrimPrefix(got, "architect: ")
	if n := len([]rune(body)); n > 50 {
		t.Errorf("body not capped to 50: got %d runes (%q)", n, body)
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

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// readRecords parses every JSONL file in dir, in filename order.
func readRecords(t *testing.T, dir string) []Record {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []Record
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			var r Record
			if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
				t.Fatalf("file %s has a line that is not valid JSON: %v", p, err)
			}
			out = append(out, r)
		}
		f.Close()
	}
	return out
}

func newTestRecorder(t *testing.T) (*Recorder, string) {
	t.Helper()
	dir := t.TempDir()
	sink, err := NewJSONLSink(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sink.Close() })
	return NewRecorder(sink, "test-host"), dir
}

func TestRecorderWritesFullTranscript(t *testing.T) {
	rec, dir := newTestRecorder(t)

	ctx := rec.Start(context.Background(), "tr-1", "ticket-42", "dev-agent")
	rec.Turn(ctx,
		ChatRequest{Messages: []Message{{Role: "user", Content: "write a test"}}},
		ChatResult{Class: ClassLarge, Model: "qwen3-coder", Endpoint: "http://127.0.0.1:8080/v1",
			Content: "func TestX", PromptTokens: 100, CompletionTokens: 20,
			Queued: 5 * time.Millisecond, Latency: 250 * time.Millisecond},
		nil)
	rec.Action(ctx, "shell", "go test ./...", nil)
	rec.Finish(ctx, OutcomeSuccess, "merged")

	records := readRecords(t, dir)
	if len(records) != 4 {
		t.Fatalf("wrote %d records, want 4 (start, turn, action, outcome)", len(records))
	}

	kinds := []RecordKind{KindStart, KindTurn, KindAction, KindOutcome}
	for i, want := range kinds {
		if records[i].Kind != want {
			t.Errorf("record %d kind = %q, want %q", i, records[i].Kind, want)
		}
		if records[i].TranscriptID != "tr-1" {
			t.Errorf("record %d transcript_id = %q, want tr-1", i, records[i].TranscriptID)
		}
		if records[i].TaskID != "ticket-42" {
			t.Errorf("record %d task_id = %q: the join key back to the ticket must be on every record", i, records[i].TaskID)
		}
		// Host on every record: several agent hosts share one CodeArmory, so a
		// role alone does not identify the actor.
		if records[i].Host != "test-host" {
			t.Errorf("record %d host = %q, want test-host", i, records[i].Host)
		}
		if records[i].Seq != int64(i+1) {
			t.Errorf("record %d seq = %d, want %d", i, records[i].Seq, i+1)
		}
	}

	turn := records[1]
	if turn.PromptTokens != 100 || turn.CompletionTokens != 20 {
		t.Errorf("usage = %d/%d, want 100/20", turn.PromptTokens, turn.CompletionTokens)
	}
	if turn.QueuedMS != 5 || turn.LatencyMS != 250 {
		t.Errorf("timing = queued %dms / latency %dms, want 5/250", turn.QueuedMS, turn.LatencyMS)
	}
	if turn.Completion != "func TestX" || len(turn.Messages) != 1 {
		t.Errorf("turn did not carry both sides of the exchange: %+v", turn)
	}
	if records[3].Status != OutcomeSuccess {
		t.Errorf("outcome status = %q, want %q", records[3].Status, OutcomeSuccess)
	}
}

// A failed turn is training signal, not noise — an agent that produced a bad
// request is exactly what a future model should learn not to do.
func TestRecorderRecordsFailedTurns(t *testing.T) {
	rec, dir := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr-2", "ticket-9", "pm-agent")
	rec.Turn(ctx, ChatRequest{Messages: []Message{{Role: "user", Content: "x"}}}, ChatResult{Class: ClassTiny}, errors.New("connection refused"))

	records := readRecords(t, dir)
	turn := records[len(records)-1]
	if turn.Kind != KindTurn {
		t.Fatalf("last record kind = %q, want a turn", turn.Kind)
	}
	if !strings.Contains(turn.Error, "connection refused") {
		t.Errorf("error = %q, want the failure recorded", turn.Error)
	}
}

// A crash mid-task must leave the records already written intact — which is the
// whole reason records are appended per event rather than buffered per task.
func TestRecorderPartialTranscriptSurvives(t *testing.T) {
	rec, dir := newTestRecorder(t)
	ctx := rec.Start(context.Background(), "tr-3", "ticket-1", "dev-agent")
	rec.Turn(ctx, ChatRequest{Messages: []Message{{Role: "user", Content: "x"}}}, ChatResult{Class: ClassLarge}, nil)
	// No Finish — simulate the process dying here.

	records := readRecords(t, dir)
	if len(records) != 2 {
		t.Fatalf("wrote %d records for an interrupted task, want the 2 already produced", len(records))
	}
	for _, r := range records {
		if r.Kind == KindOutcome {
			t.Error("an interrupted task has an outcome record; a missing outcome is how an unfinished task is identified")
		}
	}
}

// Without a transcript on the context there is nothing to attribute a turn to,
// so it must be skipped rather than written with an empty id.
func TestRecorderIgnoresUnattributedEvents(t *testing.T) {
	rec, dir := newTestRecorder(t)
	ctx := context.Background()
	rec.Turn(ctx, ChatRequest{}, ChatResult{}, nil)
	rec.Action(ctx, "shell", "ls", nil)
	rec.Finish(ctx, OutcomeSuccess, "")

	if got := readRecords(t, dir); len(got) != 0 {
		t.Errorf("wrote %d unattributed records, want 0", len(got))
	}
}

// A sink failure must not kill a running agent, but it must not be silent
// either — the corpus now has a hole and the count is how that is noticed.
func TestRecorderCountsDropsWithoutFailingTheTask(t *testing.T) {
	rec := NewRecorder(failingSink{}, "test-host")
	ctx := rec.Start(context.Background(), "tr-4", "ticket-1", "dev-agent")
	rec.Turn(ctx, ChatRequest{Messages: []Message{{Role: "user", Content: "x"}}}, ChatResult{}, nil)
	rec.Finish(ctx, OutcomeSuccess, "")

	if got := rec.Dropped(); got != 3 {
		t.Errorf("Dropped() = %d, want 3", got)
	}
}

type failingSink struct{}

func (failingSink) Write(Record) error { return errors.New("disk full") }
func (failingSink) Close() error       { return nil }

// A nil recorder is the "capture disabled" path and must be safe to call.
func TestNilRecorderIsSafe(t *testing.T) {
	var rec *Recorder
	ctx := WithTranscript(context.Background(), "tr-5", "t", "r")
	rec.Turn(ctx, ChatRequest{}, ChatResult{}, nil)
	rec.Action(ctx, "shell", "ls", nil)
	rec.Finish(ctx, OutcomeSuccess, "")
}

func TestJSONLSinkRotatesDaily(t *testing.T) {
	dir := t.TempDir()
	sink, err := NewJSONLSink(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()

	day1 := time.Date(2026, 8, 11, 23, 59, 0, 0, time.UTC)
	day2 := day1.Add(2 * time.Minute)
	for _, at := range []time.Time{day1, day2} {
		if err := sink.Write(Record{TranscriptID: "tr", Kind: KindStart, At: at}); err != nil {
			t.Fatal(err)
		}
	}

	for _, want := range []string{"transcripts-2026-08-11.jsonl", "transcripts-2026-08-12.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("expected %s to exist: %v", want, err)
		}
	}
}

// Records are appended, never truncated — a restart must not erase the corpus.
func TestJSONLSinkAppendsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	for range 2 {
		sink, err := NewJSONLSink(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := sink.Write(Record{TranscriptID: "tr", Kind: KindStart, At: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		sink.Close()
	}
	if got := readRecords(t, dir); len(got) != 2 {
		t.Errorf("found %d records after a reopen, want 2: the sink truncated instead of appending", len(got))
	}
}

// Concurrent agents share one recorder; interleaved writes must not produce a
// torn line, which would make the whole file unparseable.
func TestRecorderConcurrentWritesStayWellFormed(t *testing.T) {
	rec, dir := newTestRecorder(t)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := rec.Start(context.Background(), "tr-"+string(rune('a'+i)), "ticket", "dev-agent")
			for range 5 {
				rec.Turn(ctx,
					ChatRequest{Messages: []Message{{Role: "user", Content: strings.Repeat("padding ", 200)}}},
					ChatResult{Class: ClassLarge, Content: strings.Repeat("out ", 200)}, nil)
			}
			rec.Finish(ctx, OutcomeSuccess, "")
		}()
	}
	wg.Wait()

	// readRecords fails the test if any line is not valid JSON.
	if got := readRecords(t, dir); len(got) != 8*7 {
		t.Errorf("wrote %d records, want %d", len(got), 8*7)
	}
	if rec.Dropped() != 0 {
		t.Errorf("Dropped() = %d, want 0", rec.Dropped())
	}
}

// Capture must be automatic. A caller that forgets to record cannot recover the
// transcript afterwards, so the gateway does it rather than the call site.
func TestGatewayRecordsEveryCompletion(t *testing.T) {
	integrationTest(t)
	rec, dir := newTestRecorder(t)
	gw, _ := testGateway(t, completionHandler("generated code"), 2)
	gw.SetRecorder(rec)

	ctx := rec.Start(context.Background(), "tr-gw", "ticket-7", "dev-agent")
	if _, err := gw.Chat(ctx, ClassLarge, ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Chat() = %v", err)
	}

	records := readRecords(t, dir)
	if len(records) != 2 {
		t.Fatalf("wrote %d records, want 2 (start + the turn the gateway recorded)", len(records))
	}
	turn := records[1]
	if turn.Kind != KindTurn || turn.Completion != "generated code" {
		t.Errorf("gateway did not record the completion: %+v", turn)
	}
	if turn.PromptTokens != 12 || turn.CompletionTokens != 34 {
		t.Errorf("usage = %d/%d, want 12/34 recorded from the response", turn.PromptTokens, turn.CompletionTokens)
	}
	if turn.LatencyMS < 0 || turn.Model != "qwen3-coder" {
		t.Errorf("turn missing routing/timing: %+v", turn)
	}
}

// Failures must be recorded with their routing intact, so a class that is
// consistently failing is visible in the corpus rather than absent from it.
func TestGatewayRecordsFailedCompletions(t *testing.T) {
	integrationTest(t)
	rec, dir := newTestRecorder(t)
	gw, _ := testGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("model crashed"))
	}, 1)
	gw.SetRecorder(rec)

	ctx := rec.Start(context.Background(), "tr-fail", "ticket-8", "dev-agent")
	if _, err := gw.Chat(ctx, ClassLarge, ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}}); err == nil {
		t.Fatal("Chat() = nil error against a 500, want an error")
	}

	records := readRecords(t, dir)
	turn := records[len(records)-1]
	if turn.Kind != KindTurn {
		t.Fatalf("last record kind = %q, want a turn", turn.Kind)
	}
	if turn.Error == "" {
		t.Error("failed turn recorded with no error")
	}
	if turn.Class != ClassLarge {
		t.Errorf("failed turn class = %q, want large: a failure must stay attributable to its class", turn.Class)
	}
}

// An unconfigured class fails before any HTTP call; it must still be recorded,
// since a misrouted agent role otherwise fails invisibly.
func TestGatewayRecordsUnconfiguredClass(t *testing.T) {
	integrationTest(t)
	rec, dir := newTestRecorder(t)
	gw, _ := testGateway(t, completionHandler("x"), 1)
	gw.SetRecorder(rec)

	ctx := rec.Start(context.Background(), "tr-unconf", "ticket-1", "pm-agent")
	if _, err := gw.Chat(ctx, ClassTiny, ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}}); err == nil {
		t.Fatal("Chat(tiny) = nil error, want an error")
	}

	records := readRecords(t, dir)
	turn := records[len(records)-1]
	if turn.Class != ClassTiny || turn.Error == "" {
		t.Errorf("unconfigured-class failure not recorded against its class: %+v", turn)
	}
}

// TOOL CALLS RETURN EMPTY CONTENT. Recording res.Content alone would put a blank
// completion on every action the department takes, and the transcript IS the
// training corpus this system exists to produce — capture that is silently empty
// is worse than none, because nothing looks wrong until the corpus is read.
func TestTranscriptCapturesToolCallsNotJustContent(t *testing.T) {
	got := completionFor(ChatResult{Calls: []ToolCall{
		{Name: "write_files", Arguments: `{"edits":[{"path":"a.go","search":"x","replace":"y"}]}`},
	}})
	for _, want := range []string{"write_files", `"path":"a.go"`, "search"} {
		if !strings.Contains(got, want) {
			t.Errorf("the recorded completion is missing %q:\n%s", want, got)
		}
	}
	if got == "" {
		t.Fatal("a tool call recorded an empty completion")
	}

	// A backend that answers with content still records it unchanged.
	if got := completionFor(ChatResult{Content: "plain reply"}); got != "plain reply" {
		t.Errorf("content reply = %q", got)
	}
	// Arguments-free calls must still record which tool ran.
	if got := completionFor(ChatResult{Calls: []ToolCall{{Name: "run_tests"}}}); !strings.Contains(got, "run_tests") {
		t.Errorf("an argument-free call lost its name: %q", got)
	}
}

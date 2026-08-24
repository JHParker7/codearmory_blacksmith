package transcript

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/code-armory-app/blacksmith/internal/model"
)

// memSink collects records, and can be made to fail so the drop path is
// exercised rather than assumed.
type memSink struct {
	mu      sync.Mutex
	records []Record
	fail    error
	closed  bool
}

func (s *memSink) Write(r Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	s.records = append(s.records, r)
	return nil
}

func (s *memSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *memSink) all() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, len(s.records))
	copy(out, s.records)
	return out
}

func (s *memSink) ofKind(k Kind) []Record {
	var out []Record
	for _, r := range s.all() {
		if r.Kind == k {
			out = append(out, r)
		}
	}
	return out
}

type countingMetrics struct {
	mu       sync.Mutex
	refusals []string
	outcomes []string
	actions  int
	turns    int
}

func (m *countingMetrics) TurnLatency(string, model.Class, int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.turns++
}

func (m *countingMetrics) Action(_, _ string, _ bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.actions++
}

func (m *countingMetrics) Refusal(_, code string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refusals = append(m.refusals, code)
}

func (m *countingMetrics) Outcome(_, status, _ string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.outcomes = append(m.outcomes, status)
}

func TestATranscriptRunsFromStartToOutcome(t *testing.T) {
	sink := &memSink{}
	rec := New(sink, "gpu-1")

	ctx := rec.Start(context.Background(), "tr-1", "t-1", "dev-agent")
	rec.Turn(ctx, model.ChatRequest{Messages: []model.Message{{Role: "user", Content: "hi"}}},
		model.ChatResult{Content: "ok", Class: model.ClassLarge, CompletionTokens: 5}, nil)
	rec.Action(ctx, "sandbox", "go test ./...", nil)
	rec.Refusal(ctx, RefusalTestFile, "store_test.go", "the developer may not write tests")
	rec.Finish(ctx, "success", "done")

	got := sink.all()
	if len(got) != 5 {
		t.Fatalf("wrote %d records, want start, turn, action, refusal, outcome", len(got))
	}
	for i, want := range []Kind{KindStart, KindTurn, KindAction, KindRefusal, KindOutcome} {
		if got[i].Kind != want {
			t.Errorf("record %d is %q, want %q", i, got[i].Kind, want)
		}
	}
	// EVERY RECORD CARRIES THE HOST. Several hosts may attach to one platform, and
	// without it "dev-agent" on three machines is one identity for three actors.
	for i, r := range got {
		if r.Host != "gpu-1" {
			t.Errorf("record %d has no host; a bad patch could not be traced to a box", i)
		}
		if r.TranscriptID != "tr-1" || r.TaskID != "t-1" {
			t.Errorf("record %d = %+v, want the transcript and task it belongs to", i, r)
		}
	}
	// Sequence numbers order records within a transcript, and must not repeat.
	for i := 1; i < len(got); i++ {
		if got[i].Seq <= got[i-1].Seq {
			t.Errorf("record %d has seq %d after %d", i, got[i].Seq, got[i-1].Seq)
		}
	}
}

// RECORDING OUTSIDE A TRANSCRIPT IS A NO-OP, not a record with no identity: a
// line that cannot be joined back to a task is noise in the corpus.
func TestNothingIsWrittenOutsideATranscript(t *testing.T) {
	sink := &memSink{}
	rec := New(sink, "gpu-1")
	ctx := context.Background()

	rec.Turn(ctx, model.ChatRequest{}, model.ChatResult{}, nil)
	rec.Action(ctx, "sandbox", "true", nil)
	rec.Refusal(ctx, RefusalNoopEdit, "main.go", "")
	rec.Finish(ctx, "success", "")

	if got := len(sink.all()); got != 0 {
		t.Errorf("wrote %d records with no transcript on the context", got)
	}
	if ID(ctx) != "" {
		t.Error("a context with no transcript reported one")
	}
}

// A FAILED TURN IS SIGNAL, NOT NOISE. It is recorded with its error rather than
// dropped, and it still carries the routing that makes it attributable.
func TestAFailedTurnIsRecordedWithItsError(t *testing.T) {
	sink := &memSink{}
	rec := New(sink, "gpu-1")
	ctx := rec.Start(context.Background(), "tr-1", "t-1", "dev-agent")

	rec.Turn(ctx, model.ChatRequest{Messages: []model.Message{{Role: "user"}}},
		model.ChatResult{Class: model.ClassLarge}, errors.New("the endpoint refused the connection"))

	turns := sink.ofKind(KindTurn)
	if len(turns) != 1 {
		t.Fatalf("wrote %d turns", len(turns))
	}
	if !strings.Contains(turns[0].Error, "refused the connection") {
		t.Errorf("the error was not recorded: %+v", turns[0])
	}
	if turns[0].Class != model.ClassLarge {
		t.Error("the failed turn does not say which class it was")
	}
}

// A SINK FAILURE NEVER FAILS THE TASK, and the loss is counted rather than
// silent: killing a running agent because a disk is full is worse than losing a
// line, but losing lines invisibly is worse than either.
func TestASinkFailureIsCountedNotFatal(t *testing.T) {
	sink := &memSink{fail: errors.New("disk full")}
	rec := New(sink, "gpu-1")

	ctx := rec.Start(context.Background(), "tr-1", "t-1", "dev-agent")
	rec.Action(ctx, "sandbox", "go build ./...", nil)

	if got := rec.Dropped(); got != 2 {
		t.Errorf("dropped = %d, want both records counted", got)
	}
}

// A NIL RECORDER IS SAFE TO CALL. Callers reach one through a context and would
// otherwise each need a branch — and the one that forgets it panics in
// production rather than in a test.
func TestANilRecorderIsHarmless(t *testing.T) {
	var rec *Recorder
	ctx := rec.Start(context.Background(), "tr-1", "t-1", "dev-agent")
	rec.Turn(ctx, model.ChatRequest{}, model.ChatResult{}, nil)
	rec.Action(ctx, "sandbox", "true", nil)
	rec.Refusal(ctx, RefusalOther, "", "")
	rec.Finish(ctx, "success", "")
	if rec.Dropped() != 0 {
		t.Error("a nil recorder counted drops")
	}
}

// THE REFUSAL CODE IS A CLOSED SET. It becomes a metric label, and free text on
// a label is a cardinality bug — so an unrecognised code folds to "other" here,
// once, rather than at each call site.
func TestAnUnknownRefusalCodeBecomesOther(t *testing.T) {
	sink := &memSink{}
	metrics := &countingMetrics{}
	rec := New(sink, "gpu-1").WithMetrics(metrics)
	ctx := rec.Start(context.Background(), "tr-1", "t-1", "dev-agent")

	rec.Refusal(ctx, "the model wrote something odd about /home/user/x", "main.go", "detail")
	rec.Refusal(ctx, RefusalStaleRead, "main.go", "read again with nothing new")

	got := sink.ofKind(KindRefusal)
	if len(got) != 2 {
		t.Fatalf("wrote %d refusals", len(got))
	}
	if got[0].Tool != RefusalOther {
		t.Errorf("an unknown code was recorded as %q", got[0].Tool)
	}
	if got[1].Tool != RefusalStaleRead {
		t.Errorf("a known code was folded to %q", got[1].Tool)
	}
	// The free text still reaches the TRANSCRIPT — it is only the label that is
	// closed.
	if !strings.Contains(got[0].Detail, "detail") || !strings.Contains(got[0].Detail, "main.go") {
		t.Errorf("the refusal lost what the agent was trying to do: %q", got[0].Detail)
	}
	for _, code := range metrics.refusals {
		if !refusalCodes[code] {
			t.Errorf("the metric received the unbounded label %q", code)
		}
	}
}

// TOOL CALLS RETURN EMPTY CONTENT, so a transcript that read Content alone would
// record a blank completion for every action this department takes — and nothing
// would look wrong until the corpus was read months later.
func TestAToolCallIsRecordedAsTheCompletion(t *testing.T) {
	res := model.ChatResult{Calls: []model.ToolCall{
		{Name: "edit", Arguments: `{"path":"main.go","replace":"x"}`},
	}}
	got := Completion(res)
	if got == "" {
		t.Fatal("a tool-call turn recorded an empty completion")
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("the recorded completion is not readable JSON: %q", got)
	}
	if decoded["tool"] != "edit" {
		t.Errorf("recorded %q", got)
	}

	// A call with no arguments must still be valid JSON, or the record cannot be
	// read back at all.
	bare := Completion(model.ChatResult{Calls: []model.ToolCall{{Name: "finish"}}})
	if err := json.Unmarshal([]byte(bare), &decoded); err != nil {
		t.Errorf("a call with no arguments recorded as %q: %v", bare, err)
	}

	// Some backends send both; keep the prose rather than choosing.
	both := Completion(model.ChatResult{
		Content: "I will edit main.go",
		Calls:   []model.ToolCall{{Name: "edit", Arguments: "{}"}},
	})
	if !strings.Contains(both, "I will edit main.go") || !strings.Contains(both, "edit") {
		t.Errorf("a reply carrying both lost one of them: %q", both)
	}

	// Plain content is unchanged.
	if got := Completion(model.ChatResult{Content: "just prose"}); got != "just prose" {
		t.Errorf("Completion(prose) = %q", got)
	}
}

// THE REASONING IS RECORDED. Without it a stuck run can only be diagnosed from
// what the agent did, and that has twice produced changes that made things
// measurably worse.
func TestTheReasoningReachesTheRecord(t *testing.T) {
	sink := &memSink{}
	rec := New(sink, "gpu-1")
	ctx := rec.Start(context.Background(), "tr-1", "t-1", "dev-agent")

	rec.Turn(ctx, model.ChatRequest{Messages: []model.Message{{Role: "user"}}},
		model.ChatResult{Content: "ok", Reasoning: "the test expects a nil map"}, nil)

	turns := sink.ofKind(KindTurn)
	if len(turns) != 1 || turns[0].Reasoning != "the test expects a nil map" {
		t.Errorf("the reasoning was not recorded: %+v", turns)
	}
}

// Timing is split the way the gateway splits it: a saturated box and a slow
// model must not look the same in the corpus either.
func TestQueueAndModelTimeAreRecordedSeparately(t *testing.T) {
	sink := &memSink{}
	rec := New(sink, "gpu-1")
	ctx := rec.Start(context.Background(), "tr-1", "t-1", "dev-agent")

	rec.Turn(ctx, model.ChatRequest{Messages: []model.Message{{Role: "user"}}},
		model.ChatResult{Queued: 250 * time.Millisecond, Latency: 3 * time.Second}, nil)

	turns := sink.ofKind(KindTurn)
	if turns[0].QueuedMS != 250 || turns[0].LatencyMS != 3000 {
		t.Errorf("queued=%d latency=%d", turns[0].QueuedMS, turns[0].LatencyMS)
	}
}

// A TRANSCRIPT WITH NO OUTCOME IS A TASK THAT NEVER FINISHED, and that has to
// stay visible — nothing may backfill one.
func TestAnUnfinishedTranscriptHasNoOutcome(t *testing.T) {
	sink := &memSink{}
	rec := New(sink, "gpu-1")
	ctx := rec.Start(context.Background(), "tr-1", "t-1", "dev-agent")
	rec.Action(ctx, "sandbox", "go test ./...", nil)

	if got := sink.ofKind(KindOutcome); len(got) != 0 {
		t.Errorf("an outcome appeared for a task that never finished: %+v", got)
	}
}

func TestMetricsSeeEveryKind(t *testing.T) {
	metrics := &countingMetrics{}
	rec := New(&memSink{}, "gpu-1").WithMetrics(metrics)
	ctx := rec.Start(context.Background(), "tr-1", "t-1", "dev-agent")

	rec.Turn(ctx, model.ChatRequest{Messages: []model.Message{{Role: "user"}}}, model.ChatResult{}, nil)
	rec.Action(ctx, "sandbox", "true", nil)
	rec.Refusal(ctx, RefusalNoopEdit, "main.go", "")
	rec.Finish(ctx, "success", "")

	if metrics.turns != 1 || metrics.actions != 1 {
		t.Errorf("turns=%d actions=%d", metrics.turns, metrics.actions)
	}
	if len(metrics.refusals) != 1 || len(metrics.outcomes) != 1 {
		t.Errorf("refusals=%v outcomes=%v", metrics.refusals, metrics.outcomes)
	}
}

// The file sink is what actually ships, so it is exercised against a real
// directory: the fake has no filesystem, and that is where several bugs in this
// repository's history have lived.
func TestTheFileSinkAppendsOneLinePerRecord(t *testing.T) {
	dir := t.TempDir()
	sink, err := NewJSONLSink(filepath.Join(dir, "transcripts"))
	if err != nil {
		t.Fatalf("NewJSONLSink: %v", err)
	}
	rec := New(sink, "gpu-1")

	ctx := rec.Start(context.Background(), "tr-1", "t-1", "dev-agent")
	rec.Action(ctx, "sandbox", "go test ./...", nil)
	rec.Finish(ctx, "success", "done")
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	files, _ := filepath.Glob(filepath.Join(dir, "transcripts", "transcripts-*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("wrote %d files, want one for today", len(files))
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("file holds %d lines, want three records", len(lines))
	}
	for i, line := range lines {
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Errorf("line %d is not a readable record: %v", i, err)
		}
		if r.TranscriptID != "tr-1" {
			t.Errorf("line %d belongs to %q", i, r.TranscriptID)
		}
	}
}

// Records from different days go to different files, so "what did the department
// do on this date" is answerable with a filename.
func TestTheFileSinkRotatesByDay(t *testing.T) {
	dir := t.TempDir()
	sink, err := NewJSONLSink(dir)
	if err != nil {
		t.Fatalf("NewJSONLSink: %v", err)
	}
	day := time.Date(2026, 3, 1, 23, 59, 0, 0, time.UTC)

	if err := sink.Write(Record{TranscriptID: "tr-1", Kind: KindStart, At: day}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := sink.Write(Record{TranscriptID: "tr-2", Kind: KindStart, At: day.Add(2 * time.Minute)}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	sink.Close()

	files, _ := filepath.Glob(filepath.Join(dir, "transcripts-*.jsonl"))
	if len(files) != 2 {
		t.Fatalf("wrote %d files, want one per day: %v", len(files), files)
	}
}

// The whole point of writing straight through is that a killed process keeps
// what it already recorded — so a record must be on disk before the sink is
// closed.
func TestARecordIsOnDiskBeforeTheSinkCloses(t *testing.T) {
	dir := t.TempDir()
	sink, _ := NewJSONLSink(dir)
	t.Cleanup(func() { sink.Close() })

	rec := New(sink, "gpu-1")
	ctx := rec.Start(context.Background(), "tr-1", "t-1", "dev-agent")
	rec.Action(ctx, "sandbox", "the last thing it did", nil)

	files, _ := filepath.Glob(filepath.Join(dir, "transcripts-*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("no file was written before close")
	}
	data, _ := os.ReadFile(files[0])
	if !strings.Contains(string(data), "the last thing it did") {
		t.Error("the last action was still buffered; a killed process would lose exactly the record worth keeping")
	}
}

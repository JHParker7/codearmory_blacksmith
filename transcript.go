package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Transcripts are the department's training corpus, and they are worthless
// retroactively — a task that ran before capture existed cannot be recovered.
// That is why this lands before any agent runs, not after.
//
// The unit written to disk is a RECORD, not a whole transcript: one line per
// event, appended the moment it happens, all sharing a transcript id. A
// transcript is then a group-by over records. The alternative — buffering a
// whole task and writing it at the end — loses everything for any task that
// crashes, times out or is killed by a shutdown, which is precisely the
// population whose transcripts are most interesting.

// RecordKind distinguishes the event types on one transcript.
type RecordKind string

const (
	// KindStart opens a transcript: what the task is and who is doing it.
	KindStart RecordKind = "start"
	// KindTurn is one model call — input messages, completion, usage, timing.
	KindTurn RecordKind = "turn"
	// KindAction is something the agent did to the world: an MCP tool call, a
	// shell command in the sandbox, a ticket comment.
	KindAction RecordKind = "action"
	// KindOutcome closes a transcript. Its absence in the log is itself a
	// signal: it marks a task that never finished.
	KindOutcome RecordKind = "outcome"
	// KindRefusal is an action the harness REJECTED — a no-op edit, a write to a
	// test file, an unparseable reply.
	//
	// Its absence was a real hole. Only sandbox commands and ticket comments were
	// ever recorded, so an attempt that died having made twenty rejected actions
	// left seven records, none of them the rejections. Four consecutive runs
	// failed the same way and each one destroyed the evidence needed to explain
	// it: the transcript showed only that the agent stopped, never what it kept
	// trying. What the agent attempted and why it was refused is the single most
	// useful thing to know about a failed attempt, and it was the one thing not
	// written down.
	KindRefusal RecordKind = "refusal"
)

// Refusal kinds. A CLOSED set, because these become a metric label — see
// telemetry_metrics.go on why free text may never be one.
const (
	RefusalNoopEdit        = "noop_edit"        // the edit would not change the file
	RefusalTestFile        = "test_file"        // the developer may not write tests
	RefusalUnparseable     = "unparseable"      // the reply was not a valid action
	RefusalUnknownTool     = "unknown_tool"     // no such action
	RefusalMissingFile     = "missing_file"     // read of a path the repo does not have
	RefusalRepeatAction    = "repeat_action"    // the identical action, again
	RefusalStaleRead       = "stale_read"       // a re-read that returned nothing new
	RefusalSpecGate        = "spec_gate"        // the specification was sent back
	RefusalSearchMiss      = "search_miss"      // the search text did not match the file
	RefusalPrematureFinish = "premature_finish" // finish before tests passed or before any change
	RefusalOther           = "other"
)

// Outcome statuses. "abandoned" is deliberately distinct from "failed": a task
// the host was shut down under is not a task the agent got wrong, and training
// on it as though it were would teach the wrong lesson.
//
// "conflicted" and "blocked" are outcomes rather than columns a handler names
// itself, so the routing table stays the ONE place that knows column names: a
// stage reports what happened and the dispatcher decides where that sends the
// ticket. An agent naming its own destination would be a second routing table,
// and the two would disagree the day either changed.
const (
	OutcomeSuccess   = "success"
	OutcomeFailed    = "failed"
	OutcomeAbandoned = "abandoned"
	// OutcomeConflicted is work that is finished but does not merge. It is the
	// resolver's queue, not a failure of the stage reporting it.
	OutcomeConflicted = "conflicted"
	// OutcomeBlocked is work no agent can carry further. It goes straight to the
	// column a person watches rather than spending the remaining attempts on a
	// retry that cannot succeed.
	OutcomeBlocked = "blocked"
	// OutcomeReturned is work handed BACK to an earlier stage: the reviewer
	// rejecting a change so the developer fixes it, or the resolver finding the
	// conflict gone so the integrator merges it normally. It is not a failure of
	// the stage reporting it — the stage did its job and the answer was "not
	// yet" — so it does not count against that stage's attempts. Where it lands
	// is the Returns column of the routing table, never a column the agent names.
	OutcomeReturned = "returned"
	// OutcomeHandled means the stage placed the ticket itself and routing must
	// not move it again. Rare and deliberate: the product manager sends a request
	// that needs no breakdown straight to the developers, which is neither the
	// success destination for its stage nor a failure. Without this the
	// dispatcher's move would silently undo the handler's.
	OutcomeHandled = "handled"
)

// Record is one line of the transcript log.
type Record struct {
	TranscriptID string     `json:"transcript_id"`
	Seq          int64      `json:"seq"`
	Kind         RecordKind `json:"kind"`
	At           time.Time  `json:"at"`

	// Host is carried on every record because several agent hosts may attach to
	// one CodeArmory. Without it, dev-agent on three machines is one identity
	// for three actors and a bad patch cannot be traced to the box that made it.
	Host string `json:"host"`
	Role string `json:"role,omitempty"`
	// TaskID is the ticket this work belongs to — the join key back to the
	// platform's own record of what happened.
	TaskID string `json:"task_id,omitempty"`

	// Turn fields.
	Class      Class     `json:"class,omitempty"`
	Model      string    `json:"model,omitempty"`
	Endpoint   string    `json:"endpoint,omitempty"`
	Messages   []Message `json:"messages,omitempty"`
	Completion string    `json:"completion,omitempty"`
	// Reasoning is why the model did what it did, as it stated at the time.
	//
	// Its absence was a hole of the same shape as the missing refusal records
	// above, and worse: an attempt that spent 41 refusals re-applying an edit
	// that changed nothing leaves a transcript showing the loop and never the
	// cause. Behaviour alone is not enough to fix a stuck agent — inferring from
	// it produced two changes that measurably made things worse before the
	// reasoning was read and named the real problem in one turn.
	Reasoning        string `json:"reasoning,omitempty"`
	PromptTokens     int    `json:"prompt_tokens,omitempty"`
	CompletionTokens int    `json:"completion_tokens,omitempty"`
	QueuedMS         int64  `json:"queued_ms,omitempty"`
	LatencyMS        int64  `json:"latency_ms,omitempty"`

	// Action fields.
	Tool   string `json:"tool,omitempty"`
	Detail string `json:"detail,omitempty"`

	// Outcome fields.
	Status string `json:"status,omitempty"`

	// Error is set on any kind. A failed turn is training signal, not noise, so
	// it is recorded rather than dropped.
	Error string `json:"error,omitempty"`
}

// Sink is where records go. Kept an interface because the local file is the
// starting point, not the destination: transcripts eventually want to reach the
// artifacts service so a training corpus outlives one workstation's disk.
type Sink interface {
	Write(Record) error
	Close() error
}

// JSONLSink appends records to a newline-delimited JSON file, rotating daily.
//
// Every write is flushed to the OS immediately: the process being killed is a
// normal event on a workstation, and a buffered final record is exactly the one
// worth keeping.
type JSONLSink struct {
	dir string

	mu   sync.Mutex
	file *os.File
	day  string
}

// NewJSONLSink prepares a sink writing into dir, creating it if needed.
func NewJSONLSink(dir string) (*JSONLSink, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("transcript dir %s: %w", dir, err)
	}
	return &JSONLSink{dir: dir}, nil
}

func (s *JSONLSink) Write(r Record) error {
	line, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encode transcript record: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.rotateLocked(r.At); err != nil {
		return err
	}
	if _, err := s.file.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("append transcript record: %w", err)
	}
	return nil
}

// rotateLocked opens the file for the record's day, closing yesterday's. Daily
// files keep any single file readable and make "what did the department do on
// this date" answerable with a filename.
func (s *JSONLSink) rotateLocked(at time.Time) error {
	day := at.UTC().Format("2006-01-02")
	if s.file != nil && s.day == day {
		return nil
	}
	if s.file != nil {
		_ = s.file.Close()
		s.file = nil
	}
	path := filepath.Join(s.dir, "transcripts-"+day+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open transcript file %s: %w", path, err)
	}
	s.file, s.day = f, day
	return nil
}

func (s *JSONLSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

// Recorder writes records for the tasks running on this host.
//
// A sink failure never fails the task. Losing a transcript line is bad; killing
// a running agent because a disk is full is worse, and the drop counter makes
// the loss visible rather than silent.
type Recorder struct {
	sink    Sink
	host    string
	seq     atomic.Int64
	dropped atomic.Int64
}

func NewRecorder(sink Sink, host string) *Recorder {
	return &Recorder{sink: sink, host: host}
}

// Dropped reports how many records failed to write. Non-zero means the training
// corpus has holes in it.
func (r *Recorder) Dropped() int64 { return r.dropped.Load() }

func (r *Recorder) write(rec Record) {
	if r == nil || r.sink == nil {
		return
	}
	rec.Seq = r.seq.Add(1)
	rec.Host = r.host
	if rec.At.IsZero() {
		rec.At = time.Now().UTC()
	}
	if err := r.sink.Write(rec); err != nil {
		r.dropped.Add(1)
		slog.Error("transcript record dropped", "error", err, "kind", string(rec.Kind), "transcript_id", rec.TranscriptID)
	}
}

// Start opens a transcript and returns a context carrying its id, so the
// gateway can attribute turns without every call site threading it manually.
func (r *Recorder) Start(ctx context.Context, transcriptID, taskID, role string) context.Context {
	r.write(Record{
		TranscriptID: transcriptID,
		Kind:         KindStart,
		TaskID:       taskID,
		Role:         role,
	})
	return WithRecorder(WithTranscript(ctx, transcriptID, taskID, role), r)
}

// Turn records one model call, successful or not.
func (r *Recorder) Turn(ctx context.Context, req ChatRequest, res ChatResult, callErr error) {
	tc := transcriptFrom(ctx)
	if tc.id == "" {
		return
	}
	rec := Record{
		TranscriptID: tc.id,
		Kind:         KindTurn,
		TaskID:       tc.taskID,
		Role:         tc.role,
		Messages:     req.Messages,
		Class:        res.Class,
		Model:        res.Model,
		Endpoint:     res.Endpoint,
		Completion:   completionFor(res),
		Reasoning:    res.Reasoning,

		PromptTokens:     res.PromptTokens,
		CompletionTokens: res.CompletionTokens,
		QueuedMS:         res.Queued.Milliseconds(),
		LatencyMS:        res.Latency.Milliseconds(),
	}
	if callErr != nil {
		rec.Error = callErr.Error()
	}
	r.write(rec)
	recordTurnLatency(ctx, tc.role, string(res.Class), res.Latency.Milliseconds())
}

// Action records something the agent did to the world.
func (r *Recorder) Action(ctx context.Context, tool, detail string, actionErr error) {
	tc := transcriptFrom(ctx)
	if tc.id == "" {
		return
	}
	rec := Record{
		TranscriptID: tc.id,
		Kind:         KindAction,
		TaskID:       tc.taskID,
		Role:         tc.role,
		Tool:         tool,
		Detail:       detail,
	}
	if actionErr != nil {
		rec.Error = actionErr.Error()
	}
	r.write(rec)
	recordAction(ctx, tc.role, tool, actionErr != nil)
}

// Refusal records an action the harness rejected, and what the agent was trying
// to do when it was rejected.
//
// kind is a closed-set code (RefusalNoopEdit and friends); target is the path or
// tool it applied to; detail is free text for the transcript only and is never
// used as a metric label.
func (r *Recorder) Refusal(ctx context.Context, kind, target, detail string) {
	tc := transcriptFrom(ctx)
	if tc.id == "" {
		return
	}
	r.write(Record{
		TranscriptID: tc.id,
		Kind:         KindRefusal,
		TaskID:       tc.taskID,
		Role:         tc.role,
		Tool:         kind,
		Detail:       target + " — " + detail,
	})
	recordRefusal(ctx, tc.role, kind)
}

// Finish closes a transcript. A transcript with no outcome record is a task
// that never finished — keep that distinguishable rather than backfilling it.
func (r *Recorder) Finish(ctx context.Context, status, detail string) {
	tc := transcriptFrom(ctx)
	if tc.id == "" {
		return
	}
	r.write(Record{
		TranscriptID: tc.id,
		Kind:         KindOutcome,
		TaskID:       tc.taskID,
		Role:         tc.role,
		Status:       status,
		Detail:       detail,
	})
	recordOutcome(ctx, tc.role, status, detail)
}

// context plumbing

type transcriptKey struct{}

type transcriptCtx struct {
	id     string
	taskID string
	role   string
}

// WithTranscript attaches transcript identity to a context.
func WithTranscript(ctx context.Context, id, taskID, role string) context.Context {
	return context.WithValue(ctx, transcriptKey{}, transcriptCtx{id: id, taskID: taskID, role: role})
}

func transcriptFrom(ctx context.Context) transcriptCtx {
	tc, _ := ctx.Value(transcriptKey{}).(transcriptCtx)
	return tc
}

// WithRecorder attaches a recorder to a context so a component reached through
// several layers can record without every signature carrying one.
func WithRecorder(ctx context.Context, r *Recorder) context.Context {
	return context.WithValue(ctx, recorderKey{}, r)
}

// recorderFrom returns the context's recorder, or nil. A nil *Recorder is safe
// to call, so callers need no branch.
func recorderFrom(ctx context.Context) *Recorder {
	r, _ := ctx.Value(recorderKey{}).(*Recorder)
	return r
}

type recorderKey struct{}

// TranscriptID reports the transcript a context belongs to, or "" if none.
func TranscriptID(ctx context.Context) string { return transcriptFrom(ctx).id }

// completionFor is what the model actually produced, whichever channel it used.
//
// TOOL CALLS RETURN EMPTY CONTENT, so reading res.Content alone would record a
// blank completion for every action the department takes — and the transcript is
// the training corpus this whole system exists to produce. Capture that is
// silently empty is worse than no capture, because nothing looks wrong until the
// corpus is read months later.
//
// A call is rendered as the JSON the model would have had to write by hand, so
// records from before and after the move to tool calling read the same way.
func completionFor(res ChatResult) string {
	if len(res.Calls) == 0 {
		return res.Content
	}
	parts := make([]string, 0, len(res.Calls))
	for _, c := range res.Calls {
		args := strings.TrimSpace(c.Arguments)
		if args == "" {
			args = "{}"
		}
		parts = append(parts, fmt.Sprintf(`{"tool":%q,"arguments":%s}`, c.Name, args))
	}
	joined := strings.Join(parts, "\n")
	if res.Content != "" {
		// Some backends send both. Keep the prose too rather than choosing.
		return joined + "\n" + res.Content
	}
	return joined
}

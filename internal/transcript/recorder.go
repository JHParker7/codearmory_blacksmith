package transcript

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/code-armory-app/blacksmith/internal/model"
)

// Metrics is the counters a recorder feeds as it writes.
//
// AN INTERFACE, so the transcript does not depend on a telemetry pipeline being
// configured, and so a test can read what was counted. Nil is fine: recording
// without metrics still records.
type Metrics interface {
	TurnLatency(role string, class model.Class, ms int64)
	Action(role, tool string, failed bool)
	Refusal(role, code string)
	Outcome(role, status, detail string)
}

// Recorder writes records for the tasks running on this host.
//
// A SINK FAILURE NEVER FAILS THE TASK. Losing a transcript line is bad; killing
// a running agent because a disk is full is worse. The drop counter makes the
// loss visible rather than silent.
type Recorder struct {
	sink    Sink
	host    string
	metrics Metrics

	seq     atomic.Int64
	dropped atomic.Int64
}

// New builds a recorder writing to sink, stamping every record with host.
func New(sink Sink, host string) *Recorder {
	return &Recorder{sink: sink, host: host}
}

// WithMetrics attaches counters.
func (r *Recorder) WithMetrics(m Metrics) *Recorder {
	if r == nil {
		return nil
	}
	r.metrics = m
	return r
}

// Dropped reports how many records failed to write. NON-ZERO MEANS THE CORPUS
// HAS HOLES IN IT, which is worth surfacing rather than discovering later.
func (r *Recorder) Dropped() int64 {
	if r == nil {
		return 0
	}
	return r.dropped.Load()
}

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
		slog.Error("transcript record dropped",
			"error", err, "kind", string(rec.Kind), "transcript_id", rec.TranscriptID)
	}
}

// Start opens a transcript and returns a context carrying its identity, so the
// gateway can attribute turns without every call site threading it by hand.
func (r *Recorder) Start(ctx context.Context, transcriptID, taskID, role string) context.Context {
	if r == nil {
		return ctx
	}
	r.write(Record{
		TranscriptID: transcriptID,
		Kind:         KindStart,
		TaskID:       taskID,
		Role:         role,
	})
	// THE RECORDER TRAVELS WITH THE IDENTITY. Attached here rather than by each
	// stage, because a stage that forgets records nothing and says nothing about
	// having forgotten — see recorderKey.
	return WithRecorder(With(ctx, transcriptID, taskID, role), r)
}

// Turn records one model call, successful or not. It satisfies the gateway's
// recorder interface.
func (r *Recorder) Turn(ctx context.Context, req model.ChatRequest, res model.ChatResult, callErr error) {
	if r == nil {
		return
	}
	id := From(ctx)
	if id.TranscriptID == "" {
		return
	}
	rec := Record{
		TranscriptID: id.TranscriptID,
		Kind:         KindTurn,
		TaskID:       id.TaskID,
		Role:         id.Role,
		Messages:     req.Messages,
		Class:        res.Class,
		Model:        res.Model,
		Endpoint:     res.Endpoint,
		Completion:   Completion(res),
		Reasoning:    res.Reasoning,
		FinishReason: res.FinishReason,

		PromptTokens:     res.PromptTokens,
		CompletionTokens: res.CompletionTokens,
		QueuedMS:         res.Queued.Milliseconds(),
		LatencyMS:        res.Latency.Milliseconds(),
	}
	if callErr != nil {
		rec.Error = callErr.Error()
	}
	r.write(rec)

	if r.metrics != nil {
		r.metrics.TurnLatency(id.Role, res.Class, res.Latency.Milliseconds())
	}
}

// Action records something the agent did to the world.
func (r *Recorder) Action(ctx context.Context, tool, detail string, actionErr error) {
	if r == nil {
		return
	}
	id := From(ctx)
	if id.TranscriptID == "" {
		return
	}
	rec := Record{
		TranscriptID: id.TranscriptID,
		Kind:         KindAction,
		TaskID:       id.TaskID,
		Role:         id.Role,
		Tool:         tool,
		Detail:       detail,
	}
	if actionErr != nil {
		rec.Error = actionErr.Error()
	}
	r.write(rec)

	if r.metrics != nil {
		r.metrics.Action(id.Role, tool, actionErr != nil)
	}
}

// Refusal records an action the harness rejected, and what the agent was trying
// to do when it was rejected.
//
// code is folded onto the closed set; target is the path or tool it applied to;
// detail is free text FOR THE TRANSCRIPT ONLY and never reaches a metric label.
func (r *Recorder) Refusal(ctx context.Context, code, target, detail string) {
	if r == nil {
		return
	}
	id := From(ctx)
	if id.TranscriptID == "" {
		return
	}
	code = RefusalCode(code)
	r.write(Record{
		TranscriptID: id.TranscriptID,
		Kind:         KindRefusal,
		TaskID:       id.TaskID,
		Role:         id.Role,
		Tool:         code,
		Detail:       refusalDetail(target, detail),
	})

	if r.metrics != nil {
		r.metrics.Refusal(id.Role, code)
	}
}

// Finish closes a transcript.
//
// A TRANSCRIPT WITH NO OUTCOME IS A TASK THAT NEVER FINISHED. That has to stay
// distinguishable, so nothing here backfills one.
func (r *Recorder) Finish(ctx context.Context, status, detail string) {
	if r == nil {
		return
	}
	id := From(ctx)
	if id.TranscriptID == "" {
		return
	}
	r.write(Record{
		TranscriptID: id.TranscriptID,
		Kind:         KindOutcome,
		TaskID:       id.TaskID,
		Role:         id.Role,
		Status:       status,
		Detail:       detail,
	})

	if r.metrics != nil {
		r.metrics.Outcome(id.Role, status, detail)
	}
}

// refusalDetail joins what the agent was trying to do with why it was refused.
//
// IT JOINS ONLY WHAT IS THERE. Formatting the pair unconditionally and trimming
// the result leaves a lone dash when both are empty -- a record that says
// nothing, in the one place whose whole purpose is to say what the agent
// attempted. An empty detail is better than a decorative one: it reads as
// missing, which it is.
func refusalDetail(target, detail string) string {
	parts := make([]string, 0, 2)
	for _, s := range []string{target, detail} {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " — ")
}

// Completion is what the model actually produced, whichever channel it used.
//
// TOOL CALLS RETURN EMPTY CONTENT, so reading Content alone would record a blank
// completion for every action this department takes — and the transcript is the
// corpus the whole system exists to produce. CAPTURE THAT IS SILENTLY EMPTY IS
// WORSE THAN NO CAPTURE, because nothing looks wrong until the corpus is read
// months later.
//
// A call is rendered as the JSON the model would have had to write by hand, so
// records from before and after the move to tool calling read the same way.
func Completion(res model.ChatResult) string {
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

package telemetry

import (
	"context"
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/transcript"
)

// The recorder feeds these counters, so the shapes must agree at compile time
// rather than at the first outcome of a live run.
var _ transcript.Metrics = (*Metrics)(nil)

// EVERY DETAIL THE AGENTS EMIT MAPS TO A CODE. A detail that stops matching does
// not error — it quietly becomes "other" — so this pins each phrase the runtime
// actually writes against the code it is supposed to produce.
func TestEveryFailureDetailTheAgentsEmitHasACode(t *testing.T) {
	cases := map[string]string{
		"20 actions in a row changed nothing":         "dead_actions",
		"read 96 times in a row":                      "read_loop",
		"100 reads in a row without acting":           "read_loop",
		"could not push the branch":                   "push_failed",
		"nothing to test":                             "nothing_to_test",
		"nothing to commit":                           "nothing_to_commit",
		"the specification is broken":                 "spec_broken",
		"spec is broken and was handed back":          "spec_broken",
		"could not parse the reply":                   "unparseable",
		"unparseable model output":                    "unparseable",
		"pushed agent/t-1 after 4 iterations":         "pushed",
		"merged agent/t-1 into dev":                   "merged",
		"opened 4 tasks and 6 specification sections": "planned",
		"planned as one unit of work":                 "planned",
		"designed: ARCHITECTURE.md":                   "designed",
		"clean (no findings)":                         "reviewed",
		"concerns (2 findings)":                       "reviewed",
		"the sections already compile together":       "already_compiles",
	}
	for detail, want := range cases {
		if got := Reason(detail); got != want {
			t.Errorf("Reason(%q) = %q, want %q", detail, got, want)
		}
	}
}

// ANYTHING UNRECOGNISED BECOMES "other". A growing "other" is the signal to add
// a code and is cheap; an unbounded label is not.
func TestAnUnrecognisedDetailIsBounded(t *testing.T) {
	prose := []string{
		"",
		"the model said something nobody has seen before",
		"failed at /home/jhp1403/projects/x/y/z.go:1841 with 0x7b7c60",
		strings.Repeat("long ", 500),
	}
	for _, p := range prose {
		if got := Reason(p); got != ReasonOther {
			t.Errorf("Reason(%q) = %q, want %q", clip(p), got, ReasonOther)
		}
	}
}

// THE LABEL SET IS CLOSED. Whatever a model writes, what reaches the metric must
// be one of a fixed list — this is the property the whole taxonomy exists for.
func TestNothingOutsideTheClosedSetEverReachesALabel(t *testing.T) {
	allowed := map[string]bool{}
	for _, c := range Codes() {
		allowed[c] = true
	}

	// Details drawn from every shape the runtime produces, plus prose it never
	// would.
	for _, detail := range []string{
		"pushed agent/t-9", "merged agent/t-9 into dev", "20 actions in a row changed nothing",
		"could not push the branch: authentication required", "", "🔥 unexpected",
		"a detail mentioning nothing_to_commit inside other words",
	} {
		if got := Reason(detail); !allowed[got] {
			t.Errorf("Reason(%q) = %q, which is outside the closed set", detail, got)
		}
	}
}

// FIRST MATCH WINS, and the order is part of the taxonomy: the failure codes
// come before the success ones, so a detail mentioning both is a failure.
func TestAFailureOutranksASuccessInTheSameDetail(t *testing.T) {
	got := Reason("pushed agent/t-1, but could not push the branch on the retry")
	if got != "push_failed" {
		t.Errorf("Reason() = %q, want the failure to win", got)
	}
}

// THE CONFIDENCE LABEL IS CLOSED TOO. The schema constrains the model to two
// words; this is what happens when a backend ignores the schema.
func TestConfidenceIsBounded(t *testing.T) {
	cases := map[string]string{
		"high":                  ConfidenceHigh,
		"low":                   ConfidenceLow,
		"":                      ConfidenceOther,
		"HIGH":                  ConfidenceOther, // the schema says lower case
		"very confident indeed": ConfidenceOther,
	}
	for in, want := range cases {
		if got := NormaliseConfidence(in); got != want {
			t.Errorf("NormaliseConfidence(%q) = %q, want %q", in, got, want)
		}
	}
}

// A WORKSTATION WITH NO COLLECTOR IS THE NORMAL CASE and must never be an error.
// The wiring hands back nil and the call sites carry no branch, so every method
// has to tolerate a nil receiver.
func TestANilMetricsIsSafeToCallEverywhere(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("a nil Metrics panicked: %v", r)
		}
	}()

	var m *Metrics
	m.Outcome("dev-agent", "success", "pushed agent/t-1")
	m.Refusal("dev-agent", transcript.RefusalNoopEdit)
	m.Action("dev-agent", "sandbox", true)
	m.TurnLatency("dev-agent", model.ClassLarge, 1200)
	m.SpecGate("vacuous", "failed")
	m.RefereeVerdict("spec", "high")
	if err := m.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown on a nil Metrics: %v", err)
	}
}

// The recorder is what actually calls these, so this drives them the way it
// does: a nil metrics reaching a real recorder must record the transcript and
// count nothing, rather than taking the attempt down.
func TestARecorderWithNoCollectorStillRecords(t *testing.T) {
	sink := &collectingSink{}
	var m *Metrics
	rec := transcript.New(sink, "gpu-1").WithMetrics(m)

	ctx := rec.Start(context.Background(), "tr-1", "t-1", "dev-agent")
	rec.Action(ctx, "sandbox", "go test ./...", nil)
	rec.Finish(ctx, "success", "pushed agent/t-1")

	if len(sink.records) != 3 {
		t.Errorf("wrote %d records with no collector configured", len(sink.records))
	}
}

type collectingSink struct{ records []transcript.Record }

func (s *collectingSink) Write(r transcript.Record) error {
	s.records = append(s.records, r)
	return nil
}

func (s *collectingSink) Close() error { return nil }

func clip(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}

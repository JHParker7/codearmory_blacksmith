package telemetry

import (
	"context"
	"testing"
	"time"

	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/transcript"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// recorded builds real instruments against a reader that keeps what it is given,
// so the assertions below are about the LABELS ACTUALLY EMITTED rather than
// about a call not panicking. What these counters are for is the labels.
func recorded(t *testing.T) (*Metrics, func() metricdata.ResourceMetrics) {
	t.Helper()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := Instruments(mp.Meter("test"))
	if err != nil {
		t.Fatalf("Instruments: %v", err)
	}

	return m, func() metricdata.ResourceMetrics {
		var out metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &out); err != nil {
			t.Fatalf("Collect: %v", err)
		}
		return out
	}
}

// labels returns every attribute set emitted for one metric, as maps.
func labels(rm metricdata.ResourceMetrics, name string) []map[string]string {
	var out []map[string]string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					out = append(out, attrs(p.Attributes.ToSlice()))
				}
			case metricdata.Histogram[int64]:
				for _, p := range d.DataPoints {
					out = append(out, attrs(p.Attributes.ToSlice()))
				}
			}
		}
	}
	return out
}

func attrs(kvs []attribute.KeyValue) map[string]string {
	out := map[string]string{}
	for _, kv := range kvs {
		out[string(kv.Key)] = kv.Value.Emit()
	}
	return out
}

// A MODEL'S PROSE MUST NOT REACH A LABEL. This drives the real counter with the
// kind of detail an agent actually writes and reads back what was emitted.
func TestTheOutcomeLabelCarriesACodeAndNeverTheProse(t *testing.T) {
	m, collect := recorded(t)

	m.Outcome("dev-agent", "failed", "20 actions in a row changed nothing, last was write_files on main.go")
	m.Outcome("dev-agent", "success", "pushed agent/t-1 after 4 iterations")
	m.Outcome("sec-agent", "success", "something nobody has a code for yet")

	got := labels(collect(), "blacksmith.agent.outcome")
	if len(got) != 3 {
		t.Fatalf("emitted %d series, want one per distinct label set: %v", len(got), got)
	}

	allowed := map[string]bool{}
	for _, c := range Codes() {
		allowed[c] = true
	}
	for _, l := range got {
		if !allowed[l["reason"]] {
			t.Errorf("a label carried %q, which is outside the closed set", l["reason"])
		}
		if len(l["reason"]) > 40 {
			t.Errorf("a label carried %d bytes of prose", len(l["reason"]))
		}
		if l["role"] == "" || l["status"] == "" {
			t.Errorf("a series is missing its role or status: %v", l)
		}
	}
}

// ONE SERIES PER DISTINCT LABEL SET is the property that keeps this affordable:
// a thousand attempts that fail the same way must be one series, not a thousand.
func TestRepeatedOutcomesShareOneSeries(t *testing.T) {
	m, collect := recorded(t)

	for i := 0; i < 50; i++ {
		// Details differ in their free text every time, as a model's do.
		m.Outcome("dev-agent", "failed",
			"could not push the branch: attempt "+string(rune('a'+i%26)))
	}

	got := labels(collect(), "blacksmith.agent.outcome")
	if len(got) != 1 {
		t.Fatalf("fifty attempts produced %d series; the taxonomy is not bounding the label", len(got))
	}
	if got[0]["reason"] != "push_failed" {
		t.Errorf("reason = %q", got[0]["reason"])
	}
}

func TestEveryCounterEmitsTheLabelsItPromises(t *testing.T) {
	m, collect := recorded(t)

	m.Refusal("dev-agent", transcript.RefusalNoopEdit)
	m.Action("dev-agent", "sandbox", true)
	m.TurnLatency("dev-agent", model.ClassLarge, 1200)
	m.SpecGate("vacuous", "failed")
	m.RefereeVerdict("spec", "high")

	rm := collect()

	cases := map[string][]string{
		"blacksmith.agent.refusal":      {"role", "kind"},
		"blacksmith.agent.action":       {"role", "tool", "failed"},
		"blacksmith.agent.turn.latency": {"role", "class"},
		"blacksmith.spec.gate":          {"gate", "result"},
		"blacksmith.referee.verdict":    {"owner", "confidence"},
	}
	for name, want := range cases {
		got := labels(rm, name)
		if len(got) != 1 {
			t.Errorf("%s emitted %d series, want 1", name, len(got))
			continue
		}
		for _, key := range want {
			if _, ok := got[0][key]; !ok {
				t.Errorf("%s is missing the %q label: %v", name, key, got[0])
			}
		}
	}
}

// A BACKEND THAT IGNORES THE SCHEMA must not be able to widen a label.
func TestAStrayConfidenceIsBoundedBeforeItReachesTheLabel(t *testing.T) {
	m, collect := recorded(t)

	m.RefereeVerdict("spec", "extremely confident, having considered the whole file")
	m.RefereeVerdict("dev", "high")

	got := labels(collect(), "blacksmith.referee.verdict")
	for _, l := range got {
		switch l["confidence"] {
		case ConfidenceHigh, ConfidenceLow, ConfidenceOther:
		default:
			t.Errorf("confidence label = %q, which is outside the closed set", l["confidence"])
		}
	}
}

// The recorder is what calls these in production, so this drives the whole path:
// an attempt's transcript and its counters come from the same call.
func TestARecorderFeedsTheCountersItIsGiven(t *testing.T) {
	m, collect := recorded(t)
	rec := transcript.New(&collectingSink{}, "gpu-1").WithMetrics(m)

	ctx := rec.Start(context.Background(), "tr-1", "t-1", "dev-agent")
	rec.Refusal(ctx, transcript.RefusalTestFile, "store_test.go", "the developer may not write tests")
	rec.Action(ctx, "sandbox", "go test ./...", nil)
	rec.Finish(ctx, "failed", "20 actions in a row changed nothing")

	rm := collect()

	refusals := labels(rm, "blacksmith.agent.refusal")
	if len(refusals) != 1 || refusals[0]["kind"] != transcript.RefusalTestFile {
		t.Errorf("refusal labels = %v", refusals)
	}
	outcomes := labels(rm, "blacksmith.agent.outcome")
	if len(outcomes) != 1 || outcomes[0]["reason"] != "dead_actions" {
		t.Errorf("outcome labels = %v, want the detail mapped to its code", outcomes)
	}
	if outcomes[0]["role"] != "dev-agent" {
		t.Errorf("the outcome is not attributed to a role: %v", outcomes[0])
	}
}

// THE WIRING ITSELF MUST NOT FAIL ON A HOST WITH NO COLLECTOR. Building the
// exporter does not connect to anything — that happens on export — so a
// workstation with nothing listening still gets working counters, and the caller
// is never handed an error it would have to decide what to do with at startup.
func TestTheWiringSucceedsWithNothingListening(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")

	m, err := New(context.Background(), "blacksmith-test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if m == nil {
		t.Fatal("New returned no metrics and no error")
	}

	// It counts, whether or not anything is there to receive it.
	m.Outcome("dev-agent", "success", "pushed agent/t-1")
	m.Action("dev-agent", "sandbox", false)

	// Shutdown must return rather than hanging on an endpoint that is not there.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Shutdown(ctx) }()

	select {
	case <-done:
		// Either outcome is fine: what matters is that it returned.
	case <-time.After(5 * time.Second):
		t.Error("Shutdown hung against an endpoint with nothing listening")
	}
}

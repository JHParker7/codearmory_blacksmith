package main

import (
	"context"
	"fmt"
	"regexp"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

// Metrics for the department.
//
// WHY THIS EXISTS. Every question asked of a run so far has been answered by
// grepping transcripts after the fact: how many attempts died on dead actions,
// which stage lost the time, whether a fix moved anything. That worked while the
// answer was one number, and stopped working the moment two runs had to be
// compared — the MoE/dense A/B was decided on numbers reconstructed by hand from
// JSONL, with no way to see a rate change while a run was still going.
//
// CARDINALITY IS THE WHOLE DESIGN PROBLEM. Outcome details are free text written
// by a model ("20 actions in a row changed nothing"), and a label taking model
// prose as its value is unbounded — it would make one time series per phrasing
// and eventually take Prometheus down. So details are mapped to a CLOSED set of
// reason codes by outcomeReason, and anything unrecognised becomes "other".
// A growing "other" is the signal to add a code, and is cheap; an unbounded
// label is not.

type agentMetrics struct {
	outcomes  metric.Int64Counter
	refusals  metric.Int64Counter
	actions   metric.Int64Counter
	specGates metric.Int64Counter
	referee   metric.Int64Counter
	turnMS    metric.Int64Histogram
	shutdown  func(context.Context) error
}

var (
	metricsMu sync.RWMutex
	metrics   *agentMetrics
)

// currentMetrics is nil on a workstation with no collector, which is the normal
// case and must never be an error. Every recording helper tolerates nil.
func currentMetrics() *agentMetrics {
	metricsMu.RLock()
	defer metricsMu.RUnlock()
	return metrics
}

func setAgentMetrics(m *agentMetrics) {
	metricsMu.Lock()
	defer metricsMu.Unlock()
	metrics = m
}

// setupAgentMetrics builds the meter provider. Returns (nil, nil) when telemetry
// is not configured.
func setupAgentMetrics(ctx context.Context) (*agentMetrics, error) {
	exp, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("metric exporter: %w", err)
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", "blacksmith")),
		resource.WithTelemetrySDK(),
		resource.WithHost(),
	)
	if err != nil {
		return nil, fmt.Errorf("metric resource: %w", err)
	}
	// A 10s period against runs measured in tens of minutes: frequent enough to
	// watch a stage go wrong live, which is the point of building this at all.
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(10*time.Second))),
	)
	m := mp.Meter("blacksmith")

	am := &agentMetrics{shutdown: mp.Shutdown}
	if am.outcomes, err = m.Int64Counter("blacksmith.agent.outcome",
		metric.WithDescription("agent attempts that ended, by role and reason")); err != nil {
		return nil, err
	}
	if am.refusals, err = m.Int64Counter("blacksmith.agent.refusal",
		metric.WithDescription("actions the harness rejected, by role and kind")); err != nil {
		return nil, err
	}
	if am.actions, err = m.Int64Counter("blacksmith.agent.action",
		metric.WithDescription("actions the agent took against the world")); err != nil {
		return nil, err
	}
	if am.specGates, err = m.Int64Counter("blacksmith.spec.gate",
		metric.WithDescription("specification gate decisions, by gate and result")); err != nil {
		return nil, err
	}
	if am.referee, err = m.Int64Counter("blacksmith.referee.verdict",
		metric.WithDescription("referee verdicts on whose fault a failure is, by owner and confidence")); err != nil {
		return nil, err
	}
	if am.turnMS, err = m.Int64Histogram("blacksmith.agent.turn.latency",
		metric.WithDescription("model turn latency"), metric.WithUnit("ms")); err != nil {
		return nil, err
	}
	return am, nil
}

// outcomeReason maps a free-text outcome detail onto a bounded label.
//
// The patterns are the failure taxonomy this runtime actually has, taken from
// the strings the agents emit. Keep them in sync with those strings — a code
// that stops matching does not error, it quietly becomes "other", so the
// "other" series is the thing to watch after changing any failure message.
var outcomeReasons = []struct {
	code string
	re   *regexp.Regexp
}{
	{"dead_actions", regexp.MustCompile(`(?i)actions in a row changed nothing`)},
	{"read_loop", regexp.MustCompile(`(?i)read \d+ times in a row|reads? in a row without acting`)},
	{"push_failed", regexp.MustCompile(`(?i)could not push the branch`)},
	{"nothing_to_test", regexp.MustCompile(`(?i)nothing to test`)},
	{"nothing_to_commit", regexp.MustCompile(`(?i)nothing to commit`)},
	{"spec_broken", regexp.MustCompile(`(?i)specification is broken|spec is broken`)},
	{"unparseable", regexp.MustCompile(`(?i)could not parse|unparseable`)},
	{"pushed", regexp.MustCompile(`(?i)^pushed `)},
	{"merged", regexp.MustCompile(`(?i)^merged `)},
	{"planned", regexp.MustCompile(`(?i)^opened \d+ tasks|^planned as`)},
	{"designed", regexp.MustCompile(`(?i)^designed:`)},
	{"reviewed", regexp.MustCompile(`(?i)^clean \(|^concerns \(`)},
	{"already_compiles", regexp.MustCompile(`(?i)sections already compile together`)},
}

func outcomeReason(detail string) string {
	for _, r := range outcomeReasons {
		if r.re.MatchString(detail) {
			return r.code
		}
	}
	return "other"
}

func recordOutcome(ctx context.Context, role, status, detail string) {
	m := currentMetrics()
	if m == nil {
		return
	}
	m.outcomes.Add(ctx, 1, metric.WithAttributes(
		attribute.String("role", role),
		attribute.String("status", status),
		attribute.String("reason", outcomeReason(detail)),
	))
}

// recordRefusal counts an action the harness rejected. kind must come from a
// closed set — see refusalKind constants.
func recordRefusal(ctx context.Context, role, kind string) {
	m := currentMetrics()
	if m == nil {
		return
	}
	m.refusals.Add(ctx, 1, metric.WithAttributes(
		attribute.String("role", role),
		attribute.String("kind", kind),
	))
}

func recordAction(ctx context.Context, role, tool string, failed bool) {
	m := currentMetrics()
	if m == nil {
		return
	}
	m.actions.Add(ctx, 1, metric.WithAttributes(
		attribute.String("role", role),
		attribute.String("tool", tool),
		attribute.Bool("failed", failed),
	))
}

// recordSpecGate counts a specification gate decision. This is how a gate's
// worth is measured: a gate that never fires is dead weight, and one that fires
// constantly is rejecting work the author cannot fix.
func recordSpecGate(ctx context.Context, gate, result string) {
	m := currentMetrics()
	if m == nil {
		return
	}
	m.specGates.Add(ctx, 1, metric.WithAttributes(
		attribute.String("gate", gate),
		attribute.String("result", result),
	))
}

func recordTurnLatency(ctx context.Context, role, class string, ms int64) {
	m := currentMetrics()
	if m == nil {
		return
	}
	m.turnMS.Record(ctx, ms, metric.WithAttributes(
		attribute.String("role", role),
		attribute.String("class", class),
	))
}

// recordRefereeVerdict counts who the referee blamed.
//
// Closed set on both labels — owner is an enum in the schema and confidence is
// high/low — so this cannot become the unbounded-cardinality mistake that
// free-text reasons would be. Worth measuring because the referee's value is
// entirely in whether it is RIGHT: a rising "spec" count next to a flat merge
// rate means it is sending healthy specifications back.
func recordRefereeVerdict(ctx context.Context, owner, confidence string) {
	m := currentMetrics()
	if m == nil {
		return
	}
	if confidence != "high" && confidence != "low" {
		confidence = "other" // the schema constrains it; this keeps a stray value bounded
	}
	m.referee.Add(ctx, 1, metric.WithAttributes(
		attribute.String("owner", owner),
		attribute.String("confidence", confidence),
	))
}

package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/code-armory-app/blacksmith/internal/model"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

// ExportInterval is how often counters are shipped.
//
// Ten seconds against runs measured in tens of minutes: frequent enough to watch
// a stage go wrong LIVE, which is the point of building this at all.
const ExportInterval = 10 * time.Second

// Metrics counts what the department does.
//
// A VALUE RATHER THAN A PACKAGE-LEVEL GLOBAL, which is the one structural change
// from the shape it replaces. That version kept a mutable global behind a mutex
// and every recording helper read it, which made "were these counted?" a
// question no test could ask without reaching into another package's state.
//
// EVERY METHOD TOLERATES A NIL RECEIVER. A workstation with no collector is the
// normal case and must never be an error, so the wiring hands back nil and the
// call sites carry no branch.
type Metrics struct {
	outcomes  metric.Int64Counter
	refusals  metric.Int64Counter
	actions   metric.Int64Counter
	specGates metric.Int64Counter
	referee   metric.Int64Counter
	turnMS    metric.Int64Histogram

	shutdown func(context.Context) error
}

// New builds the meter provider.
//
// A FAILURE HERE IS NOT FATAL TO THE DEPARTMENT and the caller is expected to
// carry on with nil: a host that cannot reach a collector should still do the
// work, and refusing to start would make telemetry a dependency of the thing it
// only observes.
func New(ctx context.Context, service string) (*Metrics, error) {
	exp, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("metric exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", service)),
		resource.WithTelemetrySDK(),
		resource.WithHost(),
	)
	if err != nil {
		return nil, fmt.Errorf("metric resource: %w", err)
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(ExportInterval))),
	)

	out, err := Instruments(mp.Meter(service))
	if err != nil {
		return nil, err
	}
	out.shutdown = mp.Shutdown
	return out, nil
}

// Instruments builds the counters against any meter.
//
// SEPARATED FROM THE EXPORTER so the recording paths can be driven against a
// reader that keeps what it is given. Without this the only thing a test could
// assert about a counter was that calling it did not panic — and what these
// counters are FOR is the label values, which is exactly what that cannot see.
func Instruments(m metric.Meter) (*Metrics, error) {
	out := &Metrics{}

	var firstErr error
	pick := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	var err error
	out.outcomes, err = m.Int64Counter("blacksmith.agent.outcome",
		metric.WithDescription("agent attempts that ended, by role and reason"))
	pick(err)
	out.refusals, err = m.Int64Counter("blacksmith.agent.refusal",
		metric.WithDescription("actions the harness rejected, by role and kind"))
	pick(err)
	out.actions, err = m.Int64Counter("blacksmith.agent.action",
		metric.WithDescription("actions the agent took against the world"))
	pick(err)
	out.specGates, err = m.Int64Counter("blacksmith.spec.gate",
		metric.WithDescription("specification gate decisions, by gate and result"))
	pick(err)
	out.referee, err = m.Int64Counter("blacksmith.referee.verdict",
		metric.WithDescription("referee verdicts on whose fault a failure is, by owner and confidence"))
	pick(err)
	out.turnMS, err = m.Int64Histogram("blacksmith.agent.turn.latency",
		metric.WithDescription("model turn latency"), metric.WithUnit("ms"))
	pick(err)

	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// QuietenExportErrors reports an export failure ONCE and counts the rest.
//
// A COLLECTOR THAT IS NOT THERE MUST NOT DROWN THE RUN. The SDK reports every
// failed upload, and the exporter defaults to localhost — so a host whose
// configured endpoint has nothing behind it prints a failure every export
// interval for the length of the run. Observed immediately: an endpoint left at
// http://localhost:4318 with no collector anywhere put a connection-refused line
// into the service log every ten seconds.
//
// The FIRST one stays loud, because a misconfigured endpoint is worth fixing.
// The rest say nothing new, and the total is reported at shutdown so the silence
// cannot be mistaken for success.
func QuietenExportErrors() {
	var once sync.Once
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		exportErrors.Add(1)
		once.Do(func() {
			slog.Warn("metrics are not reaching a collector; counters are being "+
				"dropped. Reported once — the total follows at shutdown",
				"error", err)
		})
	}))
}

// exportErrors counts what QuietenExportErrors swallowed. Package level because
// the handler is process-wide, which is the SDK's own shape.
var exportErrors atomic.Int64

// Shutdown flushes what has been counted.
func (m *Metrics) Shutdown(ctx context.Context) error {
	if m == nil || m.shutdown == nil {
		return nil
	}
	err := m.shutdown(ctx)
	// SAID AT THE END, so a run that exported nothing does not look like a run
	// that had nothing to export.
	if n := exportErrors.Load(); n > 0 {
		slog.Warn("metrics never reached a collector", "failed_exports", n)
	}
	return err
}

// Outcome counts an attempt that ended.
//
// The detail is model prose and is mapped to a closed code HERE rather than by
// the caller, so there is one place that can put an unbounded value on a label
// and it is this one.
func (m *Metrics) Outcome(role, status, detail string) {
	if m == nil {
		return
	}
	m.outcomes.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("role", role),
		attribute.String("status", status),
		attribute.String("reason", Reason(detail)),
	))
}

// Refusal counts an action the harness rejected. The code comes from the
// transcript's closed set.
func (m *Metrics) Refusal(role, code string) {
	if m == nil {
		return
	}
	m.refusals.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("role", role),
		attribute.String("kind", code),
	))
}

// Action counts something the agent did to the world.
func (m *Metrics) Action(role, tool string, failed bool) {
	if m == nil {
		return
	}
	m.actions.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("role", role),
		attribute.String("tool", tool),
		attribute.Bool("failed", failed),
	))
}

// TurnLatency records how long a model call took.
func (m *Metrics) TurnLatency(role string, class model.Class, ms int64) {
	if m == nil {
		return
	}
	m.turnMS.Record(context.Background(), ms, metric.WithAttributes(
		attribute.String("role", role),
		attribute.String("class", string(class)),
	))
}

// SpecGate counts a specification gate decision.
//
// THIS IS HOW A GATE'S WORTH IS MEASURED: one that never fires is dead weight,
// and one that fires constantly is rejecting work the author cannot fix.
func (m *Metrics) SpecGate(gate, result string) {
	if m == nil {
		return
	}
	m.specGates.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("gate", gate),
		attribute.String("result", result),
	))
}

// Confidence values the referee may report. Closed, like everything else that
// reaches a label.
const (
	ConfidenceHigh  = "high"
	ConfidenceLow   = "low"
	ConfidenceOther = "other"
)

// RefereeVerdict counts who the referee blamed.
//
// Closed on both labels — the owner is an enum in the schema and the confidence
// is one of two words — so this cannot become the unbounded-cardinality mistake
// free-text reasons would be. Worth measuring because the referee's value is
// entirely in whether it is RIGHT: a rising "spec" count next to a flat merge
// rate means it is sending healthy specifications back.
func (m *Metrics) RefereeVerdict(owner, confidence string) {
	if m == nil {
		return
	}
	m.referee.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("owner", owner),
		attribute.String("confidence", NormaliseConfidence(confidence)),
	))
}

// NormaliseConfidence keeps a stray value bounded. The schema constrains the
// model to two words; this is what happens when a backend ignores the schema.
func NormaliseConfidence(c string) string {
	switch c {
	case ConfidenceHigh, ConfidenceLow:
		return c
	}
	return ConfidenceOther
}

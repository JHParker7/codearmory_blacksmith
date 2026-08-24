package main

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// One service per agent role.
//
// service.name is a RESOURCE attribute, fixed for a whole TracerProvider, so a
// process reporting under one name is one service no matter how many kinds of
// work it does. That is wrong for this process: the department is five stages
// with different jobs, failure modes and latencies, and collapsing them into
// "blacksmith" means a service graph shows one box and a latency panel averages a
// 600 ms review against a 4-minute developer run. Giving each role its own
// provider makes them what they already are — separate services that happen to
// share a binary.
//
// They share ONE EXPORTER. The providers differ only in their resource, and a
// connection per role would be five connections to the same collector for no
// reason.

// agentTracers holds a Tracer per role, plus the one shutdown that flushes them.
type agentTracers struct {
	byRole   map[string]trace.Tracer
	shutdown func(context.Context) error
}

// Tracer returns the tracer for a role, falling back to the process-wide one so
// a role added without touching this file still traces — under the wrong service
// name, which is visible and fixable, rather than not at all.
func (a *agentTracers) Tracer(role string) trace.Tracer {
	if a == nil {
		return otel.Tracer("blacksmith")
	}
	if t, ok := a.byRole[role]; ok {
		return t
	}
	return otel.Tracer("blacksmith")
}

// setupAgentTracers builds a provider per role. It returns a nil *agentTracers
// when telemetry is not configured, which the Tracer method handles: a
// workstation with no collector is the normal case and must not be a failure.
func setupAgentTracers(ctx context.Context, roles []string) (*agentTracers, error) {
	if len(roles) == 0 {
		return nil, nil
	}
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("trace exporter: %w", err)
	}

	out := &agentTracers{byRole: make(map[string]trace.Tracer, len(roles))}
	providers := make([]*sdktrace.TracerProvider, 0, len(roles))
	for _, role := range roles {
		res, err := resource.New(ctx,
			resource.WithAttributes(attribute.String("service.name", "blacksmith-"+role)),
			resource.WithTelemetrySDK(),
			resource.WithHost(),
		)
		if err != nil {
			return nil, fmt.Errorf("resource for %s: %w", role, err)
		}
		tp := sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(exp),
			sdktrace.WithResource(res),
		)
		providers = append(providers, tp)
		out.byRole[role] = tp.Tracer("blacksmith")
	}

	out.shutdown = func(ctx context.Context) error {
		var firstErr error
		for _, tp := range providers {
			// Every provider is flushed even if one fails: a shutdown that stops at
			// the first error drops the traces of every role after it, and the
			// interesting ones are usually the later stages.
			if err := tp.Shutdown(ctx); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	}
	return out, nil
}

package main

import (
	"context"
	"os"
	"testing"
)

// blacksmith runs on a workstation, where no OTLP collector is the normal case.
// Telemetry is a diagnostic, not a dependency: if it were fatal the agent
// runtime would refuse to start on exactly the machine it was built for.
//
// Found by running the service under systemd — no unit or integration test
// exercises main(), and this failed on the first real start.
func TestSetupLoggingDegradesWithoutACollector(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	os.Unsetenv("OTEL_EXPORTER_OTLP_ENDPOINT")

	handler, shutdown, err := setupLogging(context.Background())
	if err == nil {
		t.Skip("telemetry configured itself; nothing to degrade from")
	}
	if handler == nil {
		t.Fatal("setupLogging returned a nil handler on failure; logging would panic")
	}
	if shutdown == nil {
		t.Fatal("setupLogging returned a nil shutdown on failure; the deferred call would panic")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("fallback shutdown = %v, want nil", err)
	}
}

package main

import (
	"os"
	"testing"
)

// Test tiers.
//
// Three levels, each answering a different question, and each runnable on its
// own so a failure says something specific:
//
//	unit         go test -short ./...        Is the logic right?
//	integration  go test ./...               Do the parts talk to each other?
//	e2e          go test -tags e2e ./...     Does it work against the real thing?
//
// UNIT tests touch nothing outside the process — pure functions and the
// filesystem. They are the ones that must stay fast enough to run on every save.
//
// INTEGRATION tests stand up an in-process HTTP server (a fake CodeArmory, a
// fake model backend) and drive the REAL client against it. They deliberately go
// over the wire rather than mocking an interface, because most of the bugs worth
// catching here — header handling, status-code classification, JSON shapes, the
// claim race — live in exactly the layer an interface mock would replace.
//
// E2E tests run against a live CodeArmory and a live llama-server. They are
// behind a build tag AND require explicit configuration, because they create
// real tickets on a real board. See e2e_test.go.

// integrationTest marks a test as tier 2. It skips under -short so the unit tier
// stays honest: a "unit" run that quietly spins up HTTP servers is not measuring
// what it claims to.
func integrationTest(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test skipped under -short")
	}
}

// envOr reads an environment variable with a fallback, for e2e configuration.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

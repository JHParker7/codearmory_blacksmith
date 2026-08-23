package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// THINKING IS THE LARGEST SINGLE COST IN A RUN, so whether it happens has to be
// a setting rather than whatever the backend defaults to. Measured on r71: 91%
// of wall clock was model generation and 0% was queueing, and on one judgement
// question qwen3.8 spent 358 tokens and 7 seconds to deliver a 193-character
// answer, against 45 tokens and 1 second with reasoning off.

// captureRequest serves one canned reply and hands back what was asked for.
func captureRequest(t *testing.T, cc ClassConfig, into any) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(into); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	cc.Endpoint = srv.URL
	gw := NewGateway(Config{Classes: map[Class]ClassConfig{ClassLarge: cc}})
	if _, err := gw.Chat(context.Background(), ClassLarge, ChatRequest{
		Messages: []Message{{Role: "user", Content: "hello"}},
	}); err != nil {
		t.Fatalf("chat: %v", err)
	}
}

func TestClassCarriesReasoningEffortToTheWire(t *testing.T) {
	var got wireRequest
	captureRequest(t, ClassConfig{
		Model: "m", Slots: 1, QueueDepth: 1, ReasoningEffort: "none",
	}, &got)

	if got.ReasoningEffort != "none" {
		t.Errorf("reasoning_effort reached the backend as %q, want %q", got.ReasoningEffort, "none")
	}
}

// A BACKEND THAT HAS NEVER HEARD OF THE FIELD MUST NOT SEE IT. Every deployment
// that has not opted in should send the body it sent before, unchanged.
func TestReasoningEffortIsAbsentWhenUnset(t *testing.T) {
	var raw map[string]any
	captureRequest(t, ClassConfig{Model: "m", Slots: 1, QueueDepth: 1}, &raw)

	if _, present := raw["reasoning_effort"]; present {
		t.Error("reasoning_effort was sent despite the class not setting one")
	}
}

// The operator turns thinking off per class, alongside every other per-class
// setting, so one board can be run with it on and off and the two compared.
//
// AGENTS_ENV_FILE IS NOT OPTIONAL HERE. LoadConfig calls loadOperatorEnv, which
// os.Setenv's every line of the host's real config into the process — a
// mutation t.Setenv cannot undo, because the test never set those keys. Without
// this redirect the suite passes on a clean machine and fails on the operator's:
// writing this test the first time put the live AGENTS_REPO_* values into every
// test that ran after it, and two dev-agent tests failed with a pipeline that
// was never triggered.
func TestReasoningEffortIsReadFromTheEnvironment(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatalf("write empty operator env: %v", err)
	}
	t.Setenv("AGENTS_ENV_FILE", empty)
	for _, name := range endpointVarNames() {
		t.Setenv(name, "")
	}
	t.Setenv("AGENTS_LARGE_ENDPOINT", "http://127.0.0.1:11434/v1")
	t.Setenv("AGENTS_LARGE_MODEL", "qwen3.8:latest")
	t.Setenv("AGENTS_LARGE_REASONING_EFFORT", "none")

	cfg, _ := LoadConfig()
	if got := cfg.Classes[ClassLarge].ReasoningEffort; got != "none" {
		t.Errorf("AGENTS_LARGE_REASONING_EFFORT read as %q, want %q", got, "none")
	}
}

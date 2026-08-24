package main

import (
	"context"
	"os"
	"testing"
	"time"
)

// A live check against whatever is actually serving, and a throughput number for
// the Go path.
//
// SKIPPED BY DEFAULT so `make test` stays hermetic. Run it with:
//
//	AGENTS_LIVE_ENDPOINT=http://192.168.58.1:11434/v1 \
//	AGENTS_LIVE_MODEL=qwen3.8:latest go test -run Live -v ./...
//
// It exists because the reasoning capture is worth exactly nothing if the server
// this department actually talks to spells the field differently from the ones in
// the unit tests. That is not a hypothetical: llama-server, ollama and the OpenAI
// schema disagree about both the name and the position, and the failure is
// silent — a transcript with no reasoning looks the same as a model that did not
// think.

func liveGateway(t *testing.T) (*Gateway, string) {
	t.Helper()
	endpoint := os.Getenv("AGENTS_LIVE_ENDPOINT")
	if endpoint == "" {
		t.Skip("set AGENTS_LIVE_ENDPOINT to run the live checks")
	}
	model := os.Getenv("AGENTS_LIVE_MODEL")
	if model == "" {
		model = "qwen3.8:latest"
	}
	return NewGateway(Config{Classes: map[Class]ClassConfig{
		ClassLarge: {Endpoint: endpoint, Model: model, Slots: 1, QueueDepth: 1},
	}}), model
}

func TestLiveReasoningIsCaptured(t *testing.T) {
	gw, model := liveGateway(t)

	res, err := gw.Chat(context.Background(), ClassLarge, ChatRequest{
		Messages: []Message{
			{Role: "user", Content: "Think about it, then answer: what is 17 * 3?"},
		},
		MaxTokens: 300,
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}

	t.Logf("model      %s", model)
	t.Logf("content    %d chars", len(res.Content))
	t.Logf("reasoning  %d chars", len(res.Reasoning))
	if res.Reasoning != "" {
		t.Logf("           %.200q", res.Reasoning)
	}

	if res.Reasoning == "" {
		// Not every model thinks out loud. Reported rather than failed, because
		// the point is to find out which the server does — a silent empty field
		// is the thing this test exists to make visible.
		t.Logf("NOTE: this model returned no reasoning; the field is captured but empty")
	}
}

func TestLiveThroughput(t *testing.T) {
	gw, model := liveGateway(t)

	// One warm-up: a cold model pays its load once, and folding that into the
	// rate understates steady-state throughput by a lot.
	_, _ = gw.Chat(context.Background(), ClassLarge, ChatRequest{
		Messages:  []Message{{Role: "user", Content: "hi"}},
		MaxTokens: 16,
	})

	start := time.Now()
	res, err := gw.Chat(context.Background(), ClassLarge, ChatRequest{
		Messages: []Message{
			{Role: "user", Content: "Write a Python function that merges two sorted lists. Explain as you go."},
		},
		MaxTokens: 300,
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	elapsed := time.Since(start)

	rate := float64(res.CompletionTokens) / elapsed.Seconds()
	t.Logf("model       %s", model)
	t.Logf("generated   %d tokens in %.2fs", res.CompletionTokens, elapsed.Seconds())
	t.Logf("throughput  %.1f tok/s (end to end, including HTTP)", rate)
	t.Logf("prompt      %d tokens", res.PromptTokens)

	if res.CompletionTokens == 0 {
		t.Fatal("no tokens generated")
	}
}

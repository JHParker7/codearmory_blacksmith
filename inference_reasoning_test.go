package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A thinking model's reasoning must reach the transcript.
//
// Without it a stuck run can be diagnosed only from what the agent DID, and that
// is not enough. Measured on the r-series board: the developer spent 41 of its 51
// refusals re-applying an edit that changed nothing, and no transcript says why —
// the loop is there and its cause is not.
//
// The two spellings and the two positions are all in the wild. llama-server and
// ollama disagree with each other and with the OpenAI schema, so a client that
// reads one of them records nothing against half the stack it runs on, silently.

func reasoningServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

func chatOnce(t *testing.T, body string) ChatResult {
	t.Helper()
	srv := reasoningServer(t, body)
	defer srv.Close()

	gw := NewGateway(Config{Classes: map[Class]ClassConfig{
		ClassLarge: {Endpoint: srv.URL, Model: "test-model", Slots: 1, QueueDepth: 1},
	}})
	res, err := gw.Chat(context.Background(), ClassLarge, ChatRequest{
		Messages: []Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	return res
}

func TestReasoningBesideTheMessage(t *testing.T) {
	res := chatOnce(t, `{"choices":[{"message":{"role":"assistant","content":"done"},
		"reasoning":"the file already has the change","finish_reason":"stop"}]}`)
	if res.Reasoning != "the file already has the change" {
		t.Fatalf("reasoning not captured, got %q", res.Reasoning)
	}
	if res.Content != "done" {
		t.Fatalf("reasoning leaked into content: %q", res.Content)
	}
}

func TestReasoningInsideTheMessage(t *testing.T) {
	res := chatOnce(t, `{"choices":[{"message":{"role":"assistant","content":"done",
		"reasoning":"inside the message"},"finish_reason":"stop"}]}`)
	if res.Reasoning != "inside the message" {
		t.Fatalf("reasoning not captured from the message, got %q", res.Reasoning)
	}
}

func TestReasoningContentSpelling(t *testing.T) {
	res := chatOnce(t, `{"choices":[{"message":{"role":"assistant","content":"done"},
		"reasoning_content":"the other spelling","finish_reason":"stop"}]}`)
	if res.Reasoning != "the other spelling" {
		t.Fatalf("reasoning_content not captured, got %q", res.Reasoning)
	}
}

func TestNoReasoningIsNotAnError(t *testing.T) {
	// Most models do not think out loud, and that must stay ordinary.
	res := chatOnce(t, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
	if res.Reasoning != "" {
		t.Fatalf("invented reasoning: %q", res.Reasoning)
	}
}

func TestReasoningReachesTheTranscript(t *testing.T) {
	// The point of capturing it. A field on ChatResult nothing writes down is the
	// same gap one turn later.
	sink := &captureSink{}
	rec := NewRecorder(sink, "box-a")
	ctx := rec.Start(context.Background(), "t-1", "T-1", roleDev)

	rec.Turn(ctx, ChatRequest{}, ChatResult{
		Content:   "editing main.go",
		Reasoning: "the change is already applied, but I will try again",
	}, nil)

	var found string
	for _, r := range sink.records {
		if r.Kind == KindTurn {
			found = r.Reasoning
		}
	}
	if !strings.Contains(found, "already applied") {
		t.Fatalf("reasoning did not reach the transcript, got %q", found)
	}
}

// captureSink keeps records in memory so a test can read them back.
type captureSink struct{ records []Record }

func (s *captureSink) Write(r Record) error { s.records = append(s.records, r); return nil }
func (s *captureSink) Close() error         { return nil }

func TestReasoningIsNotSentBackToTheModel(t *testing.T) {
	// Output only. Echoing a model's own scratchpad back as part of the
	// conversation is how a thinking model talks itself into a loop.
	encoded, err := json.Marshal(Message{Role: "assistant", Content: "hi"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), "reasoning") {
		t.Fatalf("an empty reasoning field was sent to the model: %s", encoded)
	}
}

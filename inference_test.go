package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testGateway wires a gateway to a stub server speaking the OpenAI-compatible
// shape, which is the only contract the department depends on.
func testGateway(t *testing.T, handler http.HandlerFunc, slots int) (*Gateway, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	cfg := Config{
		Host: "test-host",
		Classes: map[Class]ClassConfig{
			ClassLarge: {Endpoint: srv.URL + "/v1", Model: "qwen3-coder", Slots: slots, QueueDepth: 8},
		},
	}
	return NewGateway(cfg), srv
}

func completionHandler(content string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "qwen3-coder",
			"choices": []map[string]any{
				{"message": map[string]string{"role": "assistant", "content": content}, "finish_reason": "stop"},
			},
			"usage": map[string]int{"prompt_tokens": 12, "completion_tokens": 34},
		})
	}
}

func TestChatRoundTrip(t *testing.T) {
	integrationTest(t)
	var gotPath, gotAuth string
	var gotBody wireRequest
	gw, _ := testGateway(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		completionHandler("hello from the model")(w, r)
	}, 4)

	res, err := gw.Chat(context.Background(), ClassLarge, ChatRequest{
		Messages:  []Message{{Role: "user", Content: "hi"}},
		MaxTokens: 128,
	})
	if err != nil {
		t.Fatalf("Chat() = %v, want success", err)
	}

	if gotPath != "/v1/chat/completions" {
		t.Errorf("request path = %q, want /v1/chat/completions", gotPath)
	}
	if gotAuth != "" {
		t.Errorf("Authorization sent with no API key configured: %q", gotAuth)
	}
	if gotBody.Stream {
		t.Error("stream = true; the gateway reads whole completions")
	}
	if gotBody.Model != "qwen3-coder" {
		t.Errorf("model = %q, want qwen3-coder", gotBody.Model)
	}
	if res.Content != "hello from the model" {
		t.Errorf("Content = %q", res.Content)
	}
	if res.PromptTokens != 12 || res.CompletionTokens != 34 {
		t.Errorf("usage = %d/%d, want 12/34: transcripts need it recorded", res.PromptTokens, res.CompletionTokens)
	}
	if res.Class != ClassLarge || res.Endpoint == "" {
		t.Errorf("result did not carry its routing: class=%q endpoint=%q", res.Class, res.Endpoint)
	}
}

func TestChatSendsAPIKeyWhenConfigured(t *testing.T) {
	integrationTest(t)
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		completionHandler("ok")(w, r)
	}))
	defer srv.Close()

	gw := NewGateway(Config{Host: "h", Classes: map[Class]ClassConfig{
		ClassLarge: {Endpoint: srv.URL + "/v1", Model: "m", APIKey: "sk-test", Slots: 1, QueueDepth: 2},
	}})
	if _, err := gw.Chat(context.Background(), ClassLarge, ChatRequest{Messages: []Message{{Role: "user", Content: "x"}}}); err != nil {
		t.Fatalf("Chat() = %v", err)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want Bearer sk-test", gotAuth)
	}
}

// Asking for a class this host does not serve must fail, never quietly fall back
// to a weaker model — that would be an invisible quality regression.
func TestChatUnconfiguredClassFails(t *testing.T) {
	integrationTest(t)
	gw, _ := testGateway(t, completionHandler("x"), 1)
	_, err := gw.Chat(context.Background(), ClassTiny, ChatRequest{Messages: []Message{{Role: "user", Content: "x"}}})
	if err == nil {
		t.Fatal("Chat(tiny) = nil error on a host serving only large, want an error")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("error = %v, want it to name the missing class", err)
	}
}

func TestChatSurfacesServerErrors(t *testing.T) {
	integrationTest(t)
	gw, _ := testGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"no slots available"}`))
	}, 1)

	_, err := gw.Chat(context.Background(), ClassLarge, ChatRequest{Messages: []Message{{Role: "user", Content: "x"}}})
	if err == nil {
		t.Fatal("Chat() = nil error on a 503, want an error")
	}
	if !strings.Contains(err.Error(), "no slots available") {
		t.Errorf("error = %v, want the server's body included for diagnosis", err)
	}
}

// A 200 with no choices is the failure mode a backend swap tends to produce;
// it must not surface as an empty completion the agent then acts on.
func TestChatRejectsEmptyChoices(t *testing.T) {
	integrationTest(t)
	gw, _ := testGateway(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"model":"m","choices":[],"usage":{}}`))
	}, 1)

	if _, err := gw.Chat(context.Background(), ClassLarge, ChatRequest{Messages: []Message{{Role: "user", Content: "x"}}}); err == nil {
		t.Fatal("Chat() = nil error for a response with no choices, want an error")
	}
}

func TestChatRejectsEmptyMessages(t *testing.T) {
	integrationTest(t)
	gw, _ := testGateway(t, completionHandler("x"), 1)
	if _, err := gw.Chat(context.Background(), ClassLarge, ChatRequest{}); err == nil {
		t.Fatal("Chat() with no messages = nil error, want an error")
	}
}

// The gateway must not put more requests on the wire than the class has slots.
func TestChatRespectsSlotLimit(t *testing.T) {
	integrationTest(t)
	var mu struct {
		concurrent, peak int
	}
	lock := make(chan struct{}, 1)
	lock <- struct{}{}

	gw, _ := testGateway(t, func(w http.ResponseWriter, r *http.Request) {
		<-lock
		mu.concurrent++
		if mu.concurrent > mu.peak {
			mu.peak = mu.concurrent
		}
		lock <- struct{}{}

		time.Sleep(20 * time.Millisecond)

		<-lock
		mu.concurrent--
		lock <- struct{}{}
		completionHandler("ok")(w, r)
	}, 2)

	done := make(chan struct{})
	for range 6 {
		go func() {
			defer func() { done <- struct{}{} }()
			_, _ = gw.Chat(context.Background(), ClassLarge, ChatRequest{Messages: []Message{{Role: "user", Content: "x"}}})
		}()
	}
	for range 6 {
		<-done
	}

	<-lock
	peak := mu.peak
	lock <- struct{}{}
	if peak > 2 {
		t.Errorf("peak concurrent requests = %d against 2 slots: oversubscribing measurably lowers throughput", peak)
	}
	if peak < 2 {
		t.Errorf("peak concurrent requests = %d, want 2: undershooting wastes aggregate throughput", peak)
	}
}

// Queue wait and model latency must be reported separately, or a saturated box
// is indistinguishable from a slow model.
func TestChatSeparatesQueueTimeFromLatency(t *testing.T) {
	integrationTest(t)
	gw, _ := testGateway(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Millisecond)
		completionHandler("ok")(w, r)
	}, 1)

	first := make(chan ChatResult, 1)
	go func() {
		res, err := gw.Chat(context.Background(), ClassLarge, ChatRequest{Messages: []Message{{Role: "user", Content: "x"}}})
		if err == nil {
			first <- res
		}
	}()
	waitFor(t, "the first request to occupy the slot", func() bool { return gw.Queue(ClassLarge).InFlight() == 1 })

	second, err := gw.Chat(context.Background(), ClassLarge, ChatRequest{Messages: []Message{{Role: "user", Content: "y"}}})
	if err != nil {
		t.Fatalf("Chat() = %v", err)
	}
	if second.Queued <= 0 {
		t.Error("Queued = 0 for a request that demonstrably waited for a slot")
	}
	if second.Latency <= 0 {
		t.Error("Latency = 0, want the model's own time")
	}
	<-first
}

func TestErrorSnippetIsBounded(t *testing.T) {
	got := errorSnippet(strings.NewReader(strings.Repeat("x", 4096)))
	if len(got) > 600 {
		t.Errorf("errorSnippet returned %d bytes; error bodies go into log lines and must stay bounded", len(got))
	}
	if got := errorSnippet(strings.NewReader("")); got != "<empty body>" {
		t.Errorf("errorSnippet(empty) = %q", got)
	}
}

// A schema must reach the wire as the OpenAI-compatible json_schema form, with
// strict set — without strict, a server is free to treat it as a hint and the
// guarantee evaporates silently.
func TestChatRequestSchemaReachesTheWire(t *testing.T) {
	var got wireRequest
	gw, _ := testGateway(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)      //nolint:errcheck
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"choices": []map[string]any{{"message": map[string]string{"content": "{}"}}},
		})
	}, 1)
	_, err := gw.Chat(context.Background(), ClassLarge, ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Schema:   &ReplySchema{Name: "dev_action", Schema: map[string]any{"type": "object"}},
	})
	if err != nil {
		t.Fatalf("Chat() = %v", err)
	}
	if got.ResponseFormat == nil {
		t.Fatal("no response_format on the wire; the reply would be unconstrained")
	}
	if got.ResponseFormat.Type != "json_schema" {
		t.Errorf("type = %q, want json_schema", got.ResponseFormat.Type)
	}
	if !got.ResponseFormat.JSONSchema.Strict {
		t.Error("strict is false; the server may treat the schema as advisory")
	}
	if got.ResponseFormat.JSONSchema.Name != "dev_action" {
		t.Errorf("name = %q", got.ResponseFormat.JSONSchema.Name)
	}
}

// No schema must leave sampling free — every other agent still sends plain
// requests, and a stray empty response_format would constrain them to nothing.
func TestChatRequestWithoutSchemaSendsNoResponseFormat(t *testing.T) {
	var got wireRequest
	gw, _ := testGateway(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)      //nolint:errcheck
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"choices": []map[string]any{{"message": map[string]string{"content": "ok"}}},
		})
	}, 1)
	if _, err := gw.Chat(context.Background(), ClassLarge, ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Chat() = %v", err)
	}
	if got.ResponseFormat != nil {
		t.Error("an unconstrained request carried a response_format")
	}
}

// The developer agent's schema must admit exactly the actions the loop accepts;
// a drift either way means the model is steered to something the loop rejects.
func TestDevActionSchemaMatchesTheAllowlist(t *testing.T) {
	// ONE BRANCH PER ACTION, so the allowlist is the set of branches rather than a
	// shared enum. The flat shape this replaced required only "action", which let
	// {"action":"write_files"} through with nothing to apply once the dev turn
	// stopped offering tools.
	sc := devActionSchema()
	branches, ok := sc.Schema["oneOf"].([]any)
	if !ok {
		t.Fatal("the action schema is not a oneOf")
	}
	if len(branches) != len(devActions) {
		t.Fatalf("schema has %d branches, loop accepts %d actions", len(branches), len(devActions))
	}
	got := map[string]bool{}
	for _, b := range branches {
		m := b.(map[string]any)
		enum := m["properties"].(map[string]any)["action"].(map[string]any)["enum"].([]string)
		if len(enum) != 1 {
			t.Errorf("a branch admits %d actions; each must pin exactly one", len(enum))
			continue
		}
		got[enum[0]] = true
		if m["additionalProperties"] != false {
			t.Errorf("%s admits extra properties; an invented field would pass silently", enum[0])
		}
	}
	for _, a := range devActions {
		if !got[a] {
			t.Errorf("the schema cannot express %q, which the loop accepts", a)
		}
	}
}

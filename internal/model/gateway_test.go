package model

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/code-armory-app/blacksmith/internal/queue"
)

// fakeServer is an OpenAI-compatible endpoint, so what the gateway actually
// sends can be read off the wire rather than inferred.
type fakeServer struct {
	mu       sync.Mutex
	requests []map[string]any
	reply    wireResponse
	status   int
	body     string
	delay    time.Duration
}

func newFakeServer(t *testing.T) (*fakeServer, *Gateway, ClassConfig) {
	t.Helper()
	f := &fakeServer{}
	f.reply = wireResponse{
		Model:   "served-model",
		Choices: []wireChoice{{Message: Message{Role: "assistant", Content: "ok"}, FinishReason: "stop"}},
	}
	f.reply.Usage.PromptTokens, f.reply.Usage.CompletionTokens = 100, 20

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.requests = append(f.requests, body)
		status, raw, reply, delay := f.status, f.body, f.reply, f.delay
		f.mu.Unlock()

		if delay > 0 {
			time.Sleep(delay)
		}
		if status != 0 {
			http.Error(w, raw, status)
			return
		}
		json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(srv.Close)

	cc := ClassConfig{
		Endpoint: srv.URL, Model: "configured-model",
		Slots: 2, QueueDepth: 10, ToolsSupported: true,
	}
	return f, NewGateway("test-host", map[Class]ClassConfig{ClassLarge: cc}), cc
}

func (f *fakeServer) sent() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return nil
	}
	return f.requests[len(f.requests)-1]
}

func TestChatReturnsTheCompletionAndItsAccounting(t *testing.T) {
	_, g, _ := newFakeServer(t)

	res, err := g.Chat(context.Background(), ClassLarge, ChatRequest{
		Messages: []Message{{Role: "user", Content: "hello"}}, MaxTokens: 100,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if res.Content != "ok" || res.FinishReason != "stop" {
		t.Errorf("result = %+v", res)
	}
	if res.PromptTokens != 100 || res.CompletionTokens != 20 {
		t.Errorf("usage = %d/%d; a transcript is worthless retroactively", res.PromptTokens, res.CompletionTokens)
	}
	if res.Class != ClassLarge || res.Endpoint == "" {
		t.Errorf("the result does not say where it was served: %+v", res)
	}
	// The SERVED model, not the configured one: they differ when a host is running
	// something other than what the configuration believes.
	if res.Model != "served-model" {
		t.Errorf("model = %q, want what the server said it served", res.Model)
	}
}

func TestTheConfiguredModelIsTheFallbackName(t *testing.T) {
	f, g, cc := newFakeServer(t)
	f.reply.Model = ""

	res, err := g.Chat(context.Background(), ClassLarge, ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if res.Model != cc.Model {
		t.Errorf("model = %q, want the configured %q when the server names none", res.Model, cc.Model)
	}
}

// A CLASS THIS HOST DOES NOT SERVE IS AN ERROR, not a quiet downgrade. Silently
// serving a weaker model is an invisible quality regression the caller cannot
// detect.
func TestAnUnservedClassIsAnErrorNotAFallback(t *testing.T) {
	_, g, _ := newFakeServer(t)

	_, err := g.Chat(context.Background(), ClassTiny, ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("a class this host does not serve was answered anyway")
	}
	if !strings.Contains(err.Error(), string(ClassTiny)) || !strings.Contains(err.Error(), "test-host") {
		t.Errorf("err = %v, want it to name the class and the host", err)
	}
	if g.Serves(ClassTiny) {
		t.Error("Serves reported a class that is not configured")
	}
}

// TOOLS WIN OVER A SCHEMA. A response_format alongside tools confuses backends
// that support both: the grammar forces a JSON document, which is not the shape
// a tool call comes back in.
func TestToolsAndASchemaAreNeverSentTogether(t *testing.T) {
	f, g, _ := newFakeServer(t)

	_, err := g.Chat(context.Background(), ClassLarge, ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Schema:   &ReplySchema{Name: "reply", Schema: map[string]any{"type": "object"}},
		Tools:    []Tool{{Name: "edit", Parameters: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	sent := f.sent()
	if _, ok := sent["response_format"]; ok {
		t.Error("a response_format was sent alongside tools")
	}
	if _, ok := sent["tools"]; !ok {
		t.Error("the tools were dropped")
	}
	// "required" rather than "auto": every turn of an agent loop IS an action, and
	// a model answering with prose has simply skipped its turn.
	if got := sent["tool_choice"]; got != "required" {
		t.Errorf("tool_choice = %v, want required", got)
	}
}

// A BACKEND THAT CANNOT RETURN A TOOL CALL MUST BE GIVEN THE GRAMMAR INSTEAD.
// Offering tools to a template that does not understand them leaves the model
// completely unconstrained: it writes a tool call into the content and keeps
// going, because nothing tells it the turn is over.
func TestABackendWithoutToolsFallsBackToTheSchema(t *testing.T) {
	f, _, cc := newFakeServer(t)
	cc.ToolsSupported = false
	g := NewGateway("test-host", map[Class]ClassConfig{ClassLarge: cc})

	_, err := g.Chat(context.Background(), ClassLarge, ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Schema:   &ReplySchema{Name: "reply", Schema: map[string]any{"type": "object"}},
		Tools:    []Tool{{Name: "edit", Parameters: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	sent := f.sent()
	if _, ok := sent["tools"]; ok {
		t.Error("tools were offered to a backend that cannot return a tool call")
	}
	format, ok := sent["response_format"].(map[string]any)
	if !ok {
		t.Fatal("no grammar was sent to a backend that cannot use tools; the model is left unconstrained")
	}
	if format["type"] != "json_schema" {
		t.Errorf("response_format = %v", format)
	}
	schema, _ := format["json_schema"].(map[string]any)
	if schema["strict"] != true {
		t.Error("the schema was not sent as strict; a non-strict grammar constrains nothing")
	}
}

// A CLASS MAY RAISE THE SAMPLING FLOOR, only upward: a stage that deliberately
// chose a temperature keeps it.
func TestTheClassTemperatureIsAFloorNotASetting(t *testing.T) {
	cases := []struct {
		classTemp, asked, want float64
	}{
		{0.3, 0, 0.3},   // greedy caller, class raises it
		{0.3, 0.7, 0.7}, // the caller asked for more and keeps it
		{0, 0.2, 0.2},   // no floor configured changes nothing
		{0.3, 0.3, 0.3},
	}
	for _, c := range cases {
		f, _, cc := newFakeServer(t)
		cc.Temperature = c.classTemp
		g := NewGateway("h", map[Class]ClassConfig{ClassLarge: cc})

		_, err := g.Chat(context.Background(), ClassLarge, ChatRequest{
			Messages: []Message{{Role: "user", Content: "hi"}}, Temperature: c.asked,
		})
		if err != nil {
			t.Fatalf("Chat: %v", err)
		}
		if got := f.sent()["temperature"]; got != c.want {
			t.Errorf("class floor %v, asked %v: sent %v, want %v", c.classTemp, c.asked, got, c.want)
		}
	}
}

// A CLASS DECIDES WHETHER ITS MODEL THINKS, and a backend that has never heard
// of the field must not receive it.
func TestReasoningEffortComesFromTheClassAndIsOmittedWhenUnset(t *testing.T) {
	f, g, cc := newFakeServer(t)
	_, err := g.Chat(context.Background(), ClassLarge, ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if _, ok := f.sent()["reasoning_effort"]; ok {
		t.Error("reasoning_effort was sent to a class that configures none")
	}

	cc.ReasoningEffort = "high"
	g2 := NewGateway("h", map[Class]ClassConfig{ClassLarge: cc})
	if _, err := g2.Chat(context.Background(), ClassLarge, ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := f.sent()["reasoning_effort"]; got != "high" {
		t.Errorf("reasoning_effort = %v, want high", got)
	}
}

// A REQUEST OVERRIDES THE CLASS'S REASONING EFFORT. This is the seam the agents
// use to turn a thinking model's scratchpad off ("none") per turn, and it must
// win over whatever the class set — otherwise the ~15x per-turn token cut this
// bought does not happen. Empty leaves the class default untouched.
func TestRequestReasoningEffortOverridesTheClass(t *testing.T) {
	f, _, cc := newFakeServer(t)
	cc.ReasoningEffort = "high"
	g := NewGateway("h", map[Class]ClassConfig{ClassLarge: cc})

	// The override wins over the class's "high".
	if _, err := g.Chat(context.Background(), ClassLarge, ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}}, ReasoningEffort: "none",
	}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := f.sent()["reasoning_effort"]; got != "none" {
		t.Errorf("reasoning_effort = %v, want none (the override)", got)
	}

	// No override leaves the class default in place.
	if _, err := g.Chat(context.Background(), ClassLarge, ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := f.sent()["reasoning_effort"]; got != "high" {
		t.Errorf("reasoning_effort = %v, want high (the class default)", got)
	}
}

// THE SCRATCHPAD ARRIVES IN FOUR PLACES and all four are real. A client reading
// only one silently records nothing against half the stack it runs on — and
// without the reasoning a stuck run can only be diagnosed from what the agent
// did, which has twice produced fixes that made things worse.
func TestTheReasoningIsFoundWhereverTheServerPutIt(t *testing.T) {
	cases := map[string]wireChoice{
		"beside the message":        {Reasoning: "because"},
		"beside it, other spelling": {ReasoningContent: "because"},
		"inside the message":        {Message: Message{Reasoning: "because"}},
		"inside it, other spelling": {Message: Message{ReasoningContent: "because"}},
	}
	for name, choice := range cases {
		f, g, _ := newFakeServer(t)
		choice.FinishReason = "stop"
		f.reply.Choices = []wireChoice{choice}

		res, err := g.Chat(context.Background(), ClassLarge, ChatRequest{
			Messages: []Message{{Role: "user", Content: "hi"}},
		})
		if err != nil {
			t.Fatalf("%s: Chat: %v", name, err)
		}
		if res.Reasoning != "because" {
			t.Errorf("%s: reasoning = %q, want it found", name, res.Reasoning)
		}
	}
}

// A REPLY THE MODEL NEVER SENT is not the same as an empty one. A response with
// no choices must fail rather than being read as a model with nothing to say.
func TestAResponseWithNoChoicesIsAnError(t *testing.T) {
	f, g, _ := newFakeServer(t)
	f.reply.Choices = nil

	if _, err := g.Chat(context.Background(), ClassLarge, ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err == nil {
		t.Error("a response with no choices was accepted as an empty answer")
	}
}

func TestToolCallsComeBackToTheCaller(t *testing.T) {
	f, g, _ := newFakeServer(t)
	var call wireToolCall
	call.ID, call.Type = "c1", "function"
	call.Function.Name, call.Function.Arguments = "edit", `{"path":"main.go"}`
	f.reply.Choices = []wireChoice{{
		Message:      Message{Role: "assistant", ToolCalls: []wireToolCall{call}},
		FinishReason: "tool_calls",
	}}

	res, err := g.Chat(context.Background(), ClassLarge, ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools:    []Tool{{Name: "edit", Parameters: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(res.Calls) != 1 || res.Calls[0].Name != "edit" {
		t.Fatalf("calls = %+v", res.Calls)
	}
	if res.Calls[0].Arguments != `{"path":"main.go"}` {
		t.Errorf("arguments = %q", res.Calls[0].Arguments)
	}
}

// THE ERROR BODY IS BOUNDED. A backend failing mid-generation can return a very
// large one, and an error goes into a log line — the prompt must not follow it
// there.
func TestAFailingBackendIsReportedBriefly(t *testing.T) {
	f, g, _ := newFakeServer(t)
	f.status, f.body = http.StatusInternalServerError, strings.Repeat("x", 5000)

	_, err := g.Chat(context.Background(), ClassLarge, ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("a 500 was accepted")
	}
	if len(err.Error()) > 1000 {
		t.Errorf("the error is %d bytes; a log line must not carry the whole body", len(err.Error()))
	}
	// It must still say WHERE it failed, or the message names a symptom and not a
	// cause.
	if !strings.Contains(err.Error(), string(ClassLarge)) {
		t.Errorf("err = %v, want it to name the class", err)
	}
}

func TestAnEmptyRequestIsRefusedBeforeItReachesTheServer(t *testing.T) {
	f, g, _ := newFakeServer(t)
	if _, err := g.Chat(context.Background(), ClassLarge, ChatRequest{}); err == nil {
		t.Error("a request with no messages was sent")
	}
	if len(f.requests) != 0 {
		t.Error("a request with no messages reached the server")
	}
}

// QUEUE WAIT IS RECORDED APART FROM MODEL LATENCY. Without the split a saturated
// box looks like a slow model, and the two want opposite responses.
func TestQueueWaitIsReportedSeparatelyFromLatency(t *testing.T) {
	f, _, cc := newFakeServer(t)
	cc.Slots = 1
	g := NewGateway("h", map[Class]ClassConfig{ClassLarge: cc})
	f.delay = 40 * time.Millisecond

	var wg sync.WaitGroup
	results := make([]ChatResult, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := g.Chat(context.Background(), ClassLarge, ChatRequest{
				Messages: []Message{{Role: "user", Content: "hi"}},
			})
			if err != nil {
				t.Errorf("Chat: %v", err)
			}
			results[i] = res
		}(i)
	}
	wg.Wait()

	var queued int
	for _, r := range results {
		if r.Latency <= 0 {
			t.Error("a completion reported no latency")
		}
		if r.Queued > 20*time.Millisecond {
			queued++
		}
	}
	if queued == 0 {
		t.Error("neither request reported queue time on a one-slot class; a saturated box would look like a slow model")
	}
}

// EVERY COMPLETION IS RECORDED, INCLUDING THE ONES THAT FAIL — those are signal
// rather than noise, and a failed turn with no routing is unattributable.
type capturing struct {
	mu    sync.Mutex
	turns []ChatResult
	errs  []error
}

func (c *capturing) Turn(_ context.Context, _ ChatRequest, res ChatResult, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.turns = append(c.turns, res)
	c.errs = append(c.errs, err)
}

func TestAFailedTurnIsStillRecordedAndStillAttributable(t *testing.T) {
	f, g, _ := newFakeServer(t)
	rec := &capturing{}
	g.SetRecorder(rec)
	f.status, f.body = http.StatusServiceUnavailable, "busy"

	if _, err := g.Chat(context.Background(), ClassLarge, ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err == nil {
		t.Fatal("a 503 was accepted")
	}
	if len(rec.turns) != 1 {
		t.Fatalf("the recorder saw %d turns, want the failed one", len(rec.turns))
	}
	if rec.errs[0] == nil {
		t.Error("the failure was recorded as a success")
	}
	if rec.turns[0].Class != ClassLarge {
		t.Errorf("the failed turn records class %q; it is unattributable", rec.turns[0].Class)
	}
}

func TestASuccessfulTurnIsRecordedToo(t *testing.T) {
	_, g, _ := newFakeServer(t)
	rec := &capturing{}
	g.SetRecorder(rec)

	if _, err := g.Chat(context.Background(), ClassLarge, ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(rec.turns) != 1 || rec.errs[0] != nil {
		t.Errorf("recorded %d turns, first err %v", len(rec.turns), rec.errs[0])
	}
	if rec.turns[0].CompletionTokens == 0 {
		t.Error("the recorded turn carries no usage")
	}
}

// Admission is the queue's, and the gateway must pass the caller's priority
// through rather than flattening it.
func TestThePriorityReachesTheQueue(t *testing.T) {
	_, g, _ := newFakeServer(t)
	if g.Queue(ClassLarge) == nil {
		t.Fatal("the class has no admission queue")
	}
	if got := g.Queue(ClassLarge).Slots(); got != 2 {
		t.Errorf("slots = %d, want the configured 2", got)
	}
	_, err := g.Chat(context.Background(), ClassLarge, ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Priority: queue.Critical,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := g.Queue(ClassLarge).InFlight(); got != 0 {
		t.Errorf("in flight = %d after the call returned; the slot was not released", got)
	}
}

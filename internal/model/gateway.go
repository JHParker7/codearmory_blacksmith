package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/code-armory-app/blacksmith/internal/queue"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// Class is a serving class: a size of model, not a model.
//
// Stages ask for a class rather than for a model name so which weights are
// behind it stays a host's decision. A stage that named its model would have to
// be edited on every machine that serves a different one.
type Class string

const (
	ClassTiny  Class = "tiny"
	ClassSmall Class = "small"
	ClassLarge Class = "large"
)

// Classes in ascending size, which is also the order a chooser should consider
// them in.
var Classes = []Class{ClassTiny, ClassSmall, ClassLarge}

// ClassConfig is one class as this host serves it.
type ClassConfig struct {
	Endpoint string
	Model    string
	APIKey   string

	// Slots MUST MATCH the serving process's own parallelism. The queue keeps
	// exactly this many requests in flight, and a number larger than the server's
	// makes it queue internally where nothing here can see it.
	Slots      int
	QueueDepth int

	// Temperature is a FLOOR this class raises the sampling to, not a setting.
	// Some backends produce degenerate output at greedy sampling.
	Temperature float64

	// ReasoningEffort is how much a thinking model thinks. A CLASS DECIDES THIS,
	// not a caller: the stages are written against a model that may or may not
	// reason, and which backend is serving them is not their business.
	ReasoningEffort string

	// ToolsSupported reports whether this backend's template can return a tool
	// call. See the gateway: offering tools to one that cannot is worse than not
	// offering them.
	ToolsSupported bool
}

// Recorder captures completions. Optional, but wired by default at startup:
// capture has to be the default rather than something each call site opts into,
// because a transcript nobody remembered to record cannot be recovered.
type Recorder interface {
	Turn(ctx context.Context, req ChatRequest, res ChatResult, err error)
}

// Gateway routes a class to an endpoint and admits requests against that
// class's slots.
//
// THE ONLY COMPONENT THAT KNOWS A MODEL SERVER EXISTS. Everything else asks for
// a class.
type Gateway struct {
	host     string
	classes  map[Class]ClassConfig
	queues   map[Class]*queue.Queue
	client   *http.Client
	recorder Recorder
}

// NewGateway builds one admission queue per configured class.
//
// THE HTTP TIMEOUT IS GENEROUS ON PURPOSE. A large model producing a long
// completion legitimately runs for minutes, and a timeout that fires
// mid-generation wastes every token already spent. Bound the work with
// MaxTokens, not with the transport.
func NewGateway(host string, classes map[Class]ClassConfig) *Gateway {
	queues := make(map[Class]*queue.Queue, len(classes))
	for class, cc := range classes {
		queues[class] = queue.New(cc.Slots, cc.QueueDepth)
	}
	return &Gateway{
		host:    host,
		classes: classes,
		queues:  queues,
		client:  &http.Client{Timeout: 30 * time.Minute},
	}
}

// Queue exposes a class's admission queue, for metrics and for tests.
func (g *Gateway) Queue(class Class) *queue.Queue { return g.queues[class] }

// SetRecorder attaches transcript capture. Every completion is then recorded —
// prompt, completion, usage and timing — INCLUDING THE ONES THAT FAIL, which are
// signal rather than noise.
func (g *Gateway) SetRecorder(r Recorder) { g.recorder = r }

// Serves reports whether this host serves a class.
func (g *Gateway) Serves(class Class) bool {
	_, ok := g.classes[class]
	return ok
}

// Chat runs one completion against the given class and records it.
//
// A CLASS THIS HOST DOES NOT SERVE IS AN ERROR, not a fallback to a weaker one.
// Silently downgrading a model is an invisible quality regression, and the agent
// that asked has no way to detect it.
func (g *Gateway) Chat(ctx context.Context, class Class, req ChatRequest) (ChatResult, error) {
	// The model call, separated from the sandbox so a slow stage is attributed to
	// the right one. QUEUE WAIT is recorded apart from model latency for the same
	// reason the result reports them separately: a saturated box and a slow model
	// look identical from outside and want opposite responses.
	ctx, span := otel.Tracer("blacksmith").Start(ctx, "inference."+string(class))
	defer span.End()
	span.SetAttributes(attribute.String("model.class", string(class)))

	res, err := g.chat(ctx, class, req)

	span.SetAttributes(
		attribute.String("model.name", res.Model),
		attribute.Int64("model.queued_ms", res.Queued.Milliseconds()),
		attribute.Int64("model.latency_ms", res.Latency.Milliseconds()),
		attribute.Int("model.completion_tokens", res.CompletionTokens),
	)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "model call failed")
	}

	if g.recorder != nil {
		// The result carries its routing on most error paths; where it does not,
		// fill in what is known so a FAILED turn is still attributable.
		if res.Class == "" {
			res.Class = class
		}
		g.recorder.Turn(ctx, req, res, err)
	}
	return res, err
}

func (g *Gateway) chat(ctx context.Context, class Class, req ChatRequest) (ChatResult, error) {
	cc, ok := g.classes[class]
	if !ok {
		return ChatResult{}, fmt.Errorf("model class %q is not configured on host %s", class, g.host)
	}
	if len(req.Messages) == 0 {
		return ChatResult{}, fmt.Errorf("chat %s: no messages", class)
	}

	queueStart := time.Now()
	release, err := g.queues[class].Acquire(ctx, req.Priority)
	if err != nil {
		return ChatResult{}, fmt.Errorf("chat %s: %w", class, err)
	}
	defer release()
	queued := time.Since(queueStart)

	body, err := json.Marshal(g.buildRequest(cc, req))
	if err != nil {
		return ChatResult{}, fmt.Errorf("chat %s: encode request: %w", class, err)
	}

	start := time.Now()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		cc.Endpoint+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return ChatResult{}, fmt.Errorf("chat %s: %w", class, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if cc.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+cc.APIKey)
	}

	resp, err := g.client.Do(httpReq)
	if err != nil {
		return ChatResult{}, fmt.Errorf("chat %s at %s: %w", class, cc.Endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ChatResult{}, fmt.Errorf("chat %s at %s: %s: %s",
			class, cc.Endpoint, resp.Status, errorSnippet(resp.Body))
	}

	var wire wireResponse
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return ChatResult{}, fmt.Errorf("chat %s: decode response: %w", class, err)
	}
	if len(wire.Choices) == 0 {
		return ChatResult{}, fmt.Errorf("chat %s at %s: the response carried no choices", class, cc.Endpoint)
	}

	choice := wire.Choices[0]
	res := ChatResult{
		Content:          choice.Message.Content,
		FinishReason:     choice.FinishReason,
		Reasoning:        choice.reasoning(),
		Calls:            toolCalls(choice.Message.ToolCalls),
		Model:            wire.Model,
		Class:            class,
		Endpoint:         cc.Endpoint,
		PromptTokens:     wire.Usage.PromptTokens,
		CompletionTokens: wire.Usage.CompletionTokens,
		Latency:          time.Since(start),
		Queued:           queued,
	}
	if res.Model == "" {
		res.Model = cc.Model
	}
	return res, nil
}

// buildRequest turns an agent's request into the wire body for this class.
//
// Separated from the call so the three decisions in it — which constraint is
// applied, what the temperature ends up being, whether tools are offered at all
// — can be examined without a server.
func (g *Gateway) buildRequest(cc ClassConfig, req ChatRequest) wireRequest {
	// A BACKEND THAT CANNOT RETURN A TOOL CALL MUST BE GIVEN A GRAMMAR INSTEAD.
	// Offering tools to a template that does not understand them leaves the model
	// COMPLETELY UNCONSTRAINED — it writes a tool call into the content and then
	// keeps writing, because nothing tells it the turn is over.
	tools := req.Tools
	if !cc.ToolsSupported {
		tools = nil
	}

	// A CLASS MAY RAISE THE SAMPLING FLOOR. Only upward, and only when the caller
	// asked for greedy: a stage that deliberately chose a temperature keeps it,
	// and a backend with no floor configured changes nothing.
	temperature := req.Temperature
	if cc.Temperature > temperature {
		temperature = cc.Temperature
	}

	out := wireRequest{
		Model:           cc.Model,
		Messages:        req.Messages,
		Temperature:     temperature,
		MaxTokens:       req.MaxTokens,
		Stop:            req.Stop,
		Stream:          false,
		ReasoningEffort: cc.ReasoningEffort,
		Tools:           wireTools(tools),
	}

	if len(tools) > 0 {
		// "required" RATHER THAN "auto": every turn of an agent loop IS an action,
		// and a model that answers with prose instead has simply skipped its turn.
		out.ToolChoice = "required"
		// A response_format ALONGSIDE tools confuses backends that support both:
		// the grammar forces a JSON document, which is not the shape a tool call
		// comes back in. Offer one or the other, never both.
		return out
	}

	if req.Schema != nil {
		out.ResponseFormat = &wireResponseFormat{
			Type: "json_schema",
			JSONSchema: wireJSONSchema{
				Name: req.Schema.Name, Strict: true, Schema: req.Schema.Schema,
			},
		}
	}
	return out
}

func toolCalls(in []wireToolCall) []ToolCall {
	out := make([]ToolCall, 0, len(in))
	for _, c := range in {
		out = append(out, ToolCall{Name: c.Function.Name, Arguments: c.Function.Arguments})
	}
	return out
}

// errorSnippet reads a bounded prefix of an error body.
//
// Bounded because a serving backend that fails mid-generation can return a very
// large one, and because an error goes into a log line — THE PROMPT MUST NOT
// FOLLOW IT THERE.
func errorSnippet(r io.Reader) string {
	const max = 512
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return "<unreadable body>"
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return "<empty body>"
	}
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

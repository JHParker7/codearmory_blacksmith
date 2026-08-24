package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// Message is one turn in a chat completion. Roles follow the OpenAI convention
// ("system", "user", "assistant", "tool") because that is what every serving
// backend the gateway targets understands.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// Reasoning arrives here on some servers and beside the message on others;
	// see wireResponse. Never SENT — it is output only, and echoing a model's
	// scratchpad back to it as though it were part of the conversation is how a
	// thinking model talks itself into a loop.
	Reasoning        string `json:"reasoning,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
	// ToolCalls is populated on responses only. Never sent: the department does
	// not replay a tool-call turn, because each iteration rebuilds the prompt from
	// scratch rather than continuing a conversation.
	ToolCalls []wireToolCall `json:"tool_calls,omitempty"`
}

type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ChatRequest is what an agent asks for. It is deliberately narrower than the
// full OpenAI schema: every field here is one the department actually uses, and
// a field nobody sets is a field that can behave differently across backends
// without anyone noticing.
// thinkingHeadroom is the reply budget a THINKING MODEL spends before it says
// anything.
//
// Reasoning comes out of the same max_tokens as the answer, so a budget sized for
// the answer alone returns an empty one: measured against qwen3.8, a single
// judgement question produced 1,845 characters of reasoning and NO content at
// max_tokens=400, finishing on "length". The same question at 1,500 answered in
// 185 characters after 1,922 of reasoning.
//
// An empty reply is the worst shape this can take, because every stage reads it
// as a model that had nothing to say rather than one that was cut off mid-thought
// — the referee simply never returns a verdict, silently, and the deterministic
// route takes over as though it had been asked and declined.
//
// Added to every stage's own allowance rather than replacing it: the answer
// budgets below were each sized against real replies and are still right about
// the answer.
const thinkingHeadroom = 1500

// maxLargeReplyTokens is the ceiling for the stages that produce a JSON REPLY
// rather than a file — the reviewer, the resolver, the referee and the
// specification pre-flight.
//
// NOT the product manager. Constraining its breakdown was tried and measured
// worse: given a grammar the model folded depends_on and acceptance into the
// `file` string and returned one degenerate subtask in 150 tokens, where the
// same prompt unconstrained produced four tasks and six sections. The ceiling
// stays a runaway guard there and the shape stays the prompt's job.
const maxLargeReplyTokens = 4000 + thinkingHeadroom

type ChatRequest struct {
	Messages    []Message
	Temperature float64
	MaxTokens   int
	Stop        []string
	// Priority drives admission, not the request body. Critical work takes the
	// whole box; see Queue.
	Priority Priority
	// Schema constrains the reply to a JSON shape, enforced by the inference
	// server's sampler rather than checked afterwards.
	//
	// This is prevention where every other layer is repair. A grammar-constrained
	// sampler can only emit tokens that keep the JSON valid, so a missing escape or
	// a truncated object is not a mistake to be corrected — it cannot be generated.
	// Measured cost: the model is steered away from tokens it would otherwise pick,
	// which is a real if small quality risk, and sampling is slightly slower.
	//
	// Worth it because the failure it removes is not small. A ticket died on four
	// consecutive unparseable replies and took six dependent tickets with it; a
	// reply that cannot be malformed cannot do that. Nil leaves sampling free.
	Schema *ReplySchema
	// Tools offers the model a set of functions to call instead of asking it to
	// hand-write an envelope.
	//
	// PREFERRED OVER Schema wherever the backend supports it. A json_schema reply
	// still makes the model author the whole structure as prose-shaped JSON, and
	// the failures that produces are the ones this department kept hitting: a file
	// body put in the wrong place, a declared path with no content, a delimiter
	// from the prompt's example copied into a field. A tool call has typed named
	// arguments the server itself validates, so those are not mistakes that can be
	// made rather than mistakes to be caught.
	//
	// Both may be set: Tools wins when the backend answers with a tool call, and
	// Schema remains the fallback for one that answers with content anyway.
	Tools []Tool
}

// Tool is one function the model may call.
type Tool struct {
	Name        string
	Description string
	// Parameters is a JSON Schema for the arguments object.
	Parameters map[string]any
}

// ToolCall is a function the model chose to call.
type ToolCall struct {
	Name string
	// Arguments is the raw JSON object the model supplied.
	Arguments string
}

// ReplySchema is a JSON Schema the reply must satisfy. Name is required by the
// OpenAI-compatible shape and appears in nothing the user sees.
type ReplySchema struct {
	Name   string
	Schema map[string]any
}

// ChatResult carries the completion plus everything transcript capture needs.
// Usage is recorded even on the paths that ignore it, because transcripts are
// worthless retroactively.
type ChatResult struct {
	Content string
	// FinishReason is why the model stopped: "stop" for a complete answer,
	// "length" when it hit the token limit MID-SENTENCE.
	//
	// Carried because truncation is indistinguishable from malformed output once
	// the text is all you have — a tool call cut off halfway through its arguments
	// is simply invalid JSON — and the two need opposite responses. "Your reply
	// was rejected" makes a model try the same thing again; "you were cut off,
	// write less" makes it change what it does.
	FinishReason string
	// Reasoning is a thinking model's account of WHY it is about to do what it
	// does, kept apart from Content and never fed back as though it were an
	// answer.
	//
	// Recorded because without it a stuck run can only be diagnosed from what the
	// agent DID, and that is not enough. Read from r-series transcripts: the
	// developer spent 41 of its 51 refusals on edits that changed nothing, and
	// nothing anywhere says why — the loop is visible and its cause is not. The
	// same gap in the Python harness produced two "fixes" derived from behaviour
	// alone, both of which made the run measurably worse; printing the reasoning
	// gave the real cause on the first read.
	Reasoning string
	// Calls are the tool calls the model made, empty when it answered with
	// content. An agent that offered tools should read this first.
	Calls            []ToolCall
	Model            string
	Class            Class
	Endpoint         string
	PromptTokens     int
	CompletionTokens int
	Latency          time.Duration
	// Queued is how long the request waited for a slot, separate from how long
	// the model took. Without the split, a saturated box looks like a slow model.
	Queued time.Duration
}

// wire types — the OpenAI-compatible request and response bodies.

type wireRequest struct {
	Model       string    `json:"model,omitempty"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Stop        []string  `json:"stop,omitempty"`
	Stream      bool      `json:"stream"`
	// ResponseFormat carries the schema. llama.cpp and ollama both accept the
	// OpenAI-compatible json_schema form, so one field serves every backend the
	// department talks to.
	ResponseFormat *wireResponseFormat `json:"response_format,omitempty"`
	// Tools and ToolChoice are the OpenAI-compatible function-calling fields.
	// llama-server serves them when started with --jinja, which is how the
	// department's units are configured.
	Tools      []wireTool `json:"tools,omitempty"`
	ToolChoice string     `json:"tool_choice,omitempty"`
	// ReasoningEffort is OpenAI's field for how much a thinking model thinks.
	// Omitted when empty so a backend that has never heard of it is unaffected.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

type wireTool struct {
	Type     string           `json:"type"`
	Function wireToolFunction `json:"function"`
}

type wireToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

type wireResponseFormat struct {
	Type       string         `json:"type"`
	JSONSchema wireJSONSchema `json:"json_schema"`
}

type wireJSONSchema struct {
	Name   string         `json:"name"`
	Strict bool           `json:"strict"`
	Schema map[string]any `json:"schema"`
}

type wireResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
		// Reasoning is a thinking model's scratchpad, which the OpenAI-compatible
		// servers put BESIDE the message rather than inside it. Both spellings are
		// in the wild — llama-server and ollama disagree — and a client that reads
		// one of them silently records nothing against half the stack it runs on.
		Reasoning        string `json:"reasoning"`
		ReasoningContent string `json:"reasoning_content"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// Gateway routes a class to an endpoint and admits requests against that
// class's slots. It is the only component in the department that knows a model
// server exists.
type Gateway struct {
	cfg    Config
	queues map[Class]*Queue
	client *http.Client
	// recorder is optional but wired by default in main: capture has to be the
	// default rather than something each call site opts into, because a
	// transcript nobody remembered to record cannot be recovered afterwards.
	recorder *Recorder
}

// NewGateway builds the routing table's runtime counterpart: one admission
// queue per configured class.
//
// The HTTP timeout is generous on purpose. A large model producing a long
// completion legitimately runs for minutes, and a timeout that fires mid-
// generation wastes every token already spent. Bound the work with MaxTokens,
// not with the transport.
func NewGateway(cfg Config) *Gateway {
	queues := make(map[Class]*Queue, len(cfg.Classes))
	for class, cc := range cfg.Classes {
		queues[class] = NewQueue(cc.Slots, cc.QueueDepth)
	}
	return &Gateway{
		cfg:    cfg,
		queues: queues,
		client: &http.Client{Timeout: 30 * time.Minute},
	}
}

// Queue exposes a class's admission queue for metrics and tests.
func (g *Gateway) Queue(class Class) *Queue { return g.queues[class] }

// SetRecorder attaches transcript capture. Every completion the gateway runs is
// then recorded — prompt, completion, usage and timing — including the ones that
// fail, which are training signal rather than noise.
func (g *Gateway) SetRecorder(r *Recorder) { g.recorder = r }

// Chat runs one completion against the given class and records it.
//
// A class this host does not serve is an error rather than a fallback to a
// weaker class: silently downgrading a model is an invisible quality
// regression, and the agent that asked has no way to detect it.
func (g *Gateway) Chat(ctx context.Context, class Class, req ChatRequest) (ChatResult, error) {
	// The model call, separated from the sandbox so a slow stage can be attributed
	// to the right one. QUEUE WAIT is recorded apart from model latency for the
	// same reason the gateway reports them separately: a saturated box and a slow
	// model look identical from the outside and want opposite responses.
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
		// res carries the routing even on most error paths; where it does not,
		// fill in what we know so a failed turn is still attributable.
		if res.Class == "" {
			res.Class = class
		}
		g.recorder.Turn(ctx, req, res, err)
	}
	return res, err
}

func (g *Gateway) chat(ctx context.Context, class Class, req ChatRequest) (ChatResult, error) {
	cc, ok := g.cfg.Classes[class]
	if !ok {
		return ChatResult{}, fmt.Errorf("model class %q is not configured on host %s", class, g.cfg.Host)
	}
	if len(req.Messages) == 0 {
		return ChatResult{}, fmt.Errorf("chat: no messages")
	}

	queueStart := time.Now()
	release, err := g.queues[class].Acquire(ctx, req.Priority)
	if err != nil {
		return ChatResult{}, fmt.Errorf("chat %s: %w", class, err)
	}
	defer release()
	queued := time.Since(queueStart)

	// A BACKEND THAT CANNOT RETURN A TOOL CALL MUST BE GIVEN A GRAMMAR INSTEAD.
	// Offering tools to a template that does not understand them leaves the model
	// completely unconstrained — it writes a tool call into the content and then
	// keeps writing, because nothing tells it the turn is over.
	tools := req.Tools
	if !cc.ToolsSupported {
		tools = nil
	}

	// A CLASS MAY RAISE THE SAMPLING FLOOR. Only upward, and only when the caller
	// asked for greedy: a stage that has deliberately chosen a temperature keeps
	// it, and a backend with no floor configured changes nothing.
	temperature := req.Temperature
	if cc.Temperature > temperature {
		temperature = cc.Temperature
	}

	body, err := json.Marshal(wireRequest{
		Model:       cc.Model,
		Messages:    req.Messages,
		Temperature: temperature,
		MaxTokens:   req.MaxTokens,
		Stop:        req.Stop,
		Stream:      false,
		// A CLASS DECIDES WHETHER ITS MODEL THINKS. No caller asks for this: the
		// stages are written against a model that may or may not reason, and which
		// backend is serving them is not their business.
		ReasoningEffort: cc.ReasoningEffort,
		ResponseFormat: func() *wireResponseFormat {
			// A response_format alongside tools confuses backends that support
			// both: the grammar forces a JSON document, which is not the shape a
			// tool call is returned in. Offer one or the other.
			if req.Schema == nil || len(tools) > 0 {
				return nil
			}
			return &wireResponseFormat{
				Type:       "json_schema",
				JSONSchema: wireJSONSchema{Name: req.Schema.Name, Strict: true, Schema: req.Schema.Schema},
			}
		}(),
		Tools: wireTools(tools),
		ToolChoice: func() string {
			if len(tools) == 0 {
				return ""
			}
			// "required" rather than "auto": every turn of an agent loop IS an
			// action, and a model that answers with prose instead has simply
			// skipped its turn.
			return "required"
		}(),
	})
	if err != nil {
		return ChatResult{}, fmt.Errorf("chat %s: encode request: %w", class, err)
	}

	start := time.Now()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, cc.Endpoint+"/chat/completions", bytes.NewReader(body))
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
		return ChatResult{}, fmt.Errorf("chat %s at %s: %s: %s", class, cc.Endpoint, resp.Status, errorSnippet(resp.Body))
	}

	var wire wireResponse
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return ChatResult{}, fmt.Errorf("chat %s: decode response: %w", class, err)
	}
	if len(wire.Choices) == 0 {
		return ChatResult{}, fmt.Errorf("chat %s at %s: response carried no choices", class, cc.Endpoint)
	}

	model := wire.Model
	if model == "" {
		model = cc.Model
	}
	reasoning := wire.Choices[0].Reasoning
	if reasoning == "" {
		reasoning = wire.Choices[0].ReasoningContent
	}
	if reasoning == "" {
		reasoning = wire.Choices[0].Message.Reasoning
	}
	if reasoning == "" {
		reasoning = wire.Choices[0].Message.ReasoningContent
	}
	calls := make([]ToolCall, 0, len(wire.Choices[0].Message.ToolCalls))
	for _, c := range wire.Choices[0].Message.ToolCalls {
		calls = append(calls, ToolCall{Name: c.Function.Name, Arguments: c.Function.Arguments})
	}
	return ChatResult{
		Calls:            calls,
		FinishReason:     wire.Choices[0].FinishReason,
		Content:          wire.Choices[0].Message.Content,
		Reasoning:        reasoning,
		Model:            model,
		Class:            class,
		Endpoint:         cc.Endpoint,
		PromptTokens:     wire.Usage.PromptTokens,
		CompletionTokens: wire.Usage.CompletionTokens,
		Latency:          time.Since(start),
		Queued:           queued,
	}, nil
}

// errorSnippet reads a bounded prefix of an error body. Bounded because a
// serving backend that fails mid-generation can return a very large body, and
// because an error is going into a log line — the prompt must not follow it
// there.
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

// wireTools converts the agent-facing tool list to the OpenAI-compatible shape.
func wireTools(tools []Tool) []wireTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]wireTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, wireTool{
			Type: "function",
			Function: wireToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}
	return out
}

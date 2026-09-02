package model

import (
	"time"

	"github.com/code-armory-app/blacksmith/internal/queue"
)

// Message is one turn in a chat completion.
//
// Roles follow the OpenAI convention — system, user, assistant, tool — because
// that is what every serving backend this department targets understands.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`

	// Reasoning arrives here on some servers and BESIDE the message on others.
	//
	// NEVER SENT. It is output only, and echoing a model's scratchpad back to it
	// as though it were part of the conversation is how a thinking model talks
	// itself into a loop.
	Reasoning        string `json:"reasoning,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`

	// ToolCalls is populated on RESPONSES only. Never sent: this department does
	// not replay a tool-call turn, because each iteration rebuilds the prompt from
	// scratch rather than continuing a conversation.
	ToolCalls []wireToolCall `json:"tool_calls,omitempty"`
}

// ThinkingHeadroom is the reply budget a THINKING MODEL spends before it says
// anything.
//
// Reasoning comes out of the same token limit as the answer, so a budget sized
// for the answer alone returns an empty one. Measured: a single judgement
// question produced 1,845 characters of reasoning and NO content at 400 tokens,
// finishing on "length"; the same question at 1,500 answered in 185 characters
// after 1,922 of reasoning.
//
// AN EMPTY REPLY IS THE WORST SHAPE THIS CAN TAKE, because every stage reads it
// as a model that had nothing to say rather than one cut off mid-thought — the
// referee simply never returns a verdict, silently, and the deterministic route
// takes over as though it had been asked and declined.
//
// Added to each stage's own allowance rather than replacing it: those budgets
// were sized against real replies and are still right about the answer.
const ThinkingHeadroom = 1500

// MaxReplyTokens is the ceiling for stages producing a JSON REPLY rather than a
// file — the reviewer, the resolver, the referee, the specification pre-flight.
//
// NOT THE PRODUCT MANAGER. Constraining its breakdown was tried and measured
// worse: given a grammar the model folded dependencies and acceptance criteria
// into one string and returned a single degenerate subtask in 150 tokens, where
// the same prompt unconstrained produced four tasks and six sections. There the
// ceiling stays a runaway guard and the shape stays the prompt's job.
const MaxReplyTokens = 4000 + ThinkingHeadroom

// ChatRequest is what an agent asks for.
//
// DELIBERATELY NARROWER than the full OpenAI schema: every field here is one
// this department actually uses, and a field nobody sets is a field that can
// behave differently across backends without anyone noticing.
type ChatRequest struct {
	Messages    []Message
	Temperature float64
	MaxTokens   int
	Stop        []string

	// ReasoningEffort overrides the serving class's reasoning setting for THIS
	// request. Empty keeps the class default; "none" turns a thinking model's
	// scratchpad off. Measured on qwen3.8 via ollama 0.32.14: a trivial prompt
	// generated 62 completion tokens with thinking on and 4 with
	// reasoning_effort:"none" — a ~15x cut in per-turn decode, and far more on a
	// real agent turn. The tool-driven agents never read their own scratchpad (it
	// goes only to the log), so turning it off is pure latency saved. This is the
	// one field ollama's OpenAI-compatible endpoint honors for it: top-level
	// `think:false` and chat_template_kwargs.enable_thinking are both ignored
	// there; only reasoning_effort passes through.
	ReasoningEffort string

	// Priority drives ADMISSION, not the request body. Critical work takes the
	// whole box — see internal/queue.
	Priority queue.Priority

	// Schema constrains the reply to a JSON shape, enforced by the inference
	// server's SAMPLER rather than checked afterwards.
	//
	// This is prevention where every other layer is repair. A grammar-constrained
	// sampler can only emit tokens that keep the JSON valid, so a missing escape
	// or a truncated object is not a mistake to be corrected — it cannot be
	// generated. The measured cost is real if small: the model is steered away
	// from tokens it would otherwise pick, and sampling is slightly slower.
	//
	// Worth it because the failure it removes is not small. A ticket died on four
	// consecutive unparseable replies and took six dependent tickets with it; a
	// reply that cannot be malformed cannot do that. Nil leaves sampling free.
	Schema *ReplySchema

	// Tools offers the model a set of functions to call instead of asking it to
	// hand-write an envelope.
	//
	// PREFERRED OVER Schema wherever the backend supports it. A schema-constrained
	// reply still makes the model author the whole structure as prose-shaped JSON,
	// and the failures that produces are the ones this department kept hitting: a
	// file body put in the wrong place, a declared path with no content, a
	// delimiter from the prompt's example copied into a field. A tool call has
	// typed named arguments the server itself validates, so those become mistakes
	// that cannot be made rather than mistakes to be caught.
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
// OpenAI-compatible shape and appears in nothing a person sees.
type ReplySchema struct {
	Name   string
	Schema map[string]any
}

// ChatResult carries the completion plus everything transcript capture needs.
//
// Usage is recorded even on the paths that ignore it, because A TRANSCRIPT IS
// WORTHLESS RETROACTIVELY: what was not captured at the time cannot be
// reconstructed from the board afterwards.
type ChatResult struct {
	Content string

	// FinishReason is why the model stopped: "stop" for a complete answer,
	// "length" when it hit the token limit MID-SENTENCE.
	//
	// Carried because truncation is indistinguishable from malformed output once
	// the text is all you have — a tool call cut off halfway through its arguments
	// is simply invalid JSON — and the two need OPPOSITE responses. "Your reply
	// was rejected" makes a model try the same thing again; "you were cut off,
	// write less" makes it change what it does.
	FinishReason string

	// Reasoning is a thinking model's account of WHY it is about to do what it
	// does, kept apart from Content and never fed back as though it were an
	// answer.
	//
	// Recorded because without it a stuck run can only be diagnosed from what the
	// agent DID, and that is not enough. Read from transcripts: a developer spent
	// 41 of its 51 refusals on edits that changed nothing, and nothing anywhere
	// said why — the loop was visible and its cause was not. The same gap produced
	// two "fixes" derived from behaviour alone, both of which made the run
	// measurably worse; printing the reasoning gave the real cause on the first
	// read.
	Reasoning string

	// Calls are the tool calls the model made, empty when it answered with
	// content. An agent that offered tools should read this FIRST.
	Calls []ToolCall

	Model    string
	Class    Class
	Endpoint string

	PromptTokens     int
	CompletionTokens int

	Latency time.Duration

	// Queued is how long the request waited for a slot, SEPARATE from how long the
	// model took. Without the split a saturated box looks like a slow model, and
	// the two want opposite responses.
	Queued time.Duration
}

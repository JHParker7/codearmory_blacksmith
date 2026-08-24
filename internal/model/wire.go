package model

// The wire types: the OpenAI-compatible request and response bodies.
//
// Kept apart from the agent-facing types on purpose. What a stage asks for and
// what a particular server wants are two different vocabularies, and the places
// they disagree — two spellings of the same field, a tool call that arrives
// beside the message rather than in it — are exactly where a client silently
// records nothing.

type wireRequest struct {
	Model       string    `json:"model,omitempty"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Stop        []string  `json:"stop,omitempty"`
	Stream      bool      `json:"stream"`

	// ResponseFormat carries the schema. llama.cpp and ollama both accept the
	// OpenAI-compatible json_schema form, so one field serves every backend this
	// department talks to.
	ResponseFormat *wireResponseFormat `json:"response_format,omitempty"`

	// Tools and ToolChoice are the OpenAI-compatible function-calling fields.
	// llama-server serves them when started with --jinja, which is how this
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

type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireResponse struct {
	Model   string       `json:"model"`
	Choices []wireChoice `json:"choices"`
	Usage   struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

type wireChoice struct {
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`

	// A thinking model's scratchpad, which the OpenAI-compatible servers put
	// BESIDE the message rather than inside it. Both spellings are in the wild —
	// llama-server and ollama disagree — and a client reading only one of them
	// silently records nothing against half the stack it runs on.
	Reasoning        string `json:"reasoning"`
	ReasoningContent string `json:"reasoning_content"`
}

// reasoning finds the scratchpad wherever this server put it.
//
// FOUR PLACES, and all four are real: beside the message under two names, and
// inside it under the same two. The order does not matter because a server
// populates one; what matters is that none is missed, since a missing one reads
// as a model that did not think.
func (c wireChoice) reasoning() string {
	for _, s := range []string{
		c.Reasoning, c.ReasoningContent,
		c.Message.Reasoning, c.Message.ReasoningContent,
	} {
		if s != "" {
			return s
		}
	}
	return ""
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

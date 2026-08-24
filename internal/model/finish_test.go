package model

import "testing"

// TWO SIGNALS, EITHER SUFFICIENT, because neither is reliable alone: some
// backends report finish_reason faithfully and some omit it entirely.
//
// It matters because a cut-off object and a refusal both fail to parse and need
// OPPOSITE fixes — "your reply was rejected" makes a model try the same thing
// again, where "you were cut off" makes it write less.
func TestTruncationIsDetectedFromEitherSignal(t *testing.T) {
	cases := []struct {
		name    string
		res     ChatResult
		ceiling int
		want    bool
	}{
		{"the backend said length", ChatResult{FinishReason: FinishLength}, 4000, true},
		{"the backend said length and no ceiling was set",
			ChatResult{FinishReason: FinishLength}, 0, true},
		{"the backend said nothing but the count reached the ceiling",
			ChatResult{CompletionTokens: 4000}, 4000, true},
		{"the count passed the ceiling", ChatResult{CompletionTokens: 4200}, 4000, true},

		{"a complete answer", ChatResult{FinishReason: FinishStop, CompletionTokens: 900}, 4000, false},
		{"a short refusal", ChatResult{CompletionTokens: 8}, 4000, false},
		// A ceiling of 0 means the caller set none, so only the reported reason
		// counts — otherwise every reply would read as truncated.
		{"no ceiling and no reason", ChatResult{CompletionTokens: 99999}, 0, false},
		// An invented finish reason is NOT read as "complete", and is not read as
		// truncated either: it says nothing, so the count decides.
		{"a word outside the set", ChatResult{FinishReason: "content_filter", CompletionTokens: 8}, 4000, false},
		{"a word outside the set at the ceiling",
			ChatResult{FinishReason: "content_filter", CompletionTokens: 4000}, 4000, true},
	}

	for _, c := range cases {
		if got := c.res.Truncated(c.ceiling); got != c.want {
			t.Errorf("%s: Truncated(%d) = %v, want %v", c.name, c.ceiling, got, c.want)
		}
	}
}

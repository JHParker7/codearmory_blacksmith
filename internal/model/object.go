package model

import (
	"errors"
	"strings"
)

// DecodeObject pulls the JSON object out of a model's reply and decodes it,
// tolerating the fences and preamble models add despite being asked not to.
//
// A REPLY THAT ALREADY IS A JSON OBJECT IS NEVER UNFENCED. Unwrapping is right
// when a model put its answer inside a code block and CATASTROPHIC when the
// answer itself contains one — and the answers here routinely do, because they
// carry file contents. A leading brace is the reliable signal: nothing needs
// unwrapping, so nothing is unwrapped.
//
// ONE COPY, DELIBERATELY. This lived in each agent that needed it, and the
// duplicates did not stay identical — two of them carried a fence bug that the
// stage they were copied from had already fixed. See Unfence.
func DecodeObject(raw string, into any) error {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(s, "{") {
		if fenced := Unfence(s); fenced != "" {
			s = fenced
		}
	}
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start == -1 || end == -1 || end < start {
		return errors.New("no JSON object in the model output")
	}
	return DecodeJSON(s[start:end+1], into)
}

// Unfence returns the contents of a reply the model wrapped in a code fence, or
// "" when it wrapped nothing.
//
// THE CLOSING FENCE IS THE LAST ONE, NOT THE NEXT ONE. A payload that itself
// contains fences is the normal case here rather than an exotic one: the
// architect asks for markdown, and a README with a ```bash block in it is
// ordinary. Taking the NEXT ``` as the close cuts the payload at its first inner
// fence and returns a fragment with no closing brace, which then reads as "no
// JSON object in the model output" against a reply that was perfectly well
// formed.
//
// Measured on the dense 14B, which wraps its answers in ```json where the MoE
// emitted them bare — so the same harness bug was invisible on one model and
// fatal on the other, and would have been scored as the model being worse.
//
// Trailing prose after the last fence is harmless: callers take the outermost
// { … } out of whatever this returns.
func Unfence(s string) string {
	_, rest, ok := strings.Cut(s, "```")
	if !ok {
		return ""
	}
	// A fence may name a language on the same line.
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[nl+1:]
	}
	j := strings.LastIndex(rest, "```")
	if j == -1 {
		// An opening fence with no close: a truncated reply. What is there is
		// still the best available reading of it.
		return strings.TrimSpace(rest)
	}
	return strings.TrimSpace(rest[:j])
}

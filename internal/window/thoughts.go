package window

import (
	"regexp"
	"strings"
	"time"

	"github.com/code-armory-app/blacksmith/internal/transcript"
)

// Thought is one thing the model SAID on a ticket, with the tool it went on to
// call.
type Thought struct {
	At     time.Time
	Role   string
	Tool   string
	Prose  string
	Failed bool
}

// MaxThoughtsShown bounds the reasoning panel.
//
// Twenty turns is enough to see a loop develop and break — the same diagnosis
// three times running is the signature that matters — while the whole history
// stays in the transcript file for anyone who wants it. The panel scrolls, so
// this is not a fitting constraint; it is a reading one.
const MaxThoughtsShown = 20

// ReadThoughts returns what the model said on one ticket, NEWEST LAST.
//
// THE PROSE IS THE USEFUL SIGNAL AND IT IS ONLY IN THE FILE. Watching a stuck
// ticket through counters says "41 refusals" and leaves the cause to guesswork;
// the reasoning beside it says "the routing uses HasPrefix on a path missing its
// leading slash", which is the actual fault. Every diagnosis worth having came
// from reading these, and reading them meant leaving the tool and grepping a
// JSONL file by hand.
func ReadThoughts(dir, ticketID string) []Thought {
	if ticketID == "" {
		return nil
	}
	var out []Thought
	scanRecords(dir, func(r transcript.Record) {
		if r.TaskID != ticketID {
			return
		}
		if th, ok := thoughtOf(r); ok {
			out = append(out, th)
			if len(out) > MaxThoughtsShown {
				out = out[len(out)-MaxThoughtsShown:]
			}
		}
	})
	return out
}

// thoughtOf is the one rule for turning a record into something the reasoning
// panel shows, shared so the digest and the direct read cannot drift.
func thoughtOf(r transcript.Record) (Thought, bool) {
	switch r.Kind {
	case transcript.KindTurn:
		return Thought{
			At: r.At, Role: r.Role, Tool: ToolOf(r.Completion), Prose: ProseOf(r),
		}, true
	case transcript.KindRefusal:
		// A REFUSAL BELONGS IN THE STREAM, not just in the counters. The reason an
		// agent is looping is almost always in the refusal it keeps earning, and
		// reading the turns without them shows an agent that appears to decide the
		// same thing repeatedly for no reason.
		return Thought{
			At: r.At, Role: r.Role, Tool: r.Tool,
			Prose: strings.TrimSpace(r.Detail), Failed: true,
		}, true
	}
	return Thought{}, false
}

// ProseOf is what the model MEANT, as distinct from what it emitted.
//
// The recorded Reasoning field is preferred whenever the endpoint supplied one,
// because it is the model's own account rather than a guess made by stripping
// syntax. The completion is the fallback for the endpoints that return no
// separate reasoning — most of them.
func ProseOf(r transcript.Record) string {
	if p := strings.TrimSpace(r.Reasoning); p != "" {
		return clipRunes(strings.Join(strings.Fields(p), " "), 400)
	}
	if r.Completion == "" {
		return ""
	}
	out := jsonBlob.ReplaceAllString(r.Completion, " ")
	out = strings.TrimSpace(strings.Join(strings.Fields(out), " "))
	if out == "" {
		// SAYING SO BEATS AN EMPTY ROW. A blank line reads as a failed read of the
		// transcript; this says the turn genuinely carried no prose, which is
		// itself a finding — a model that has stopped explaining itself is usually
		// one that has started looping.
		return "(no reasoning — tool call only)"
	}
	return clipRunes(out, 400)
}

// jsonBlob matches a JSON object spanning the reply, including the fenced and
// tagged wrappers models add unprompted. Non-greedy on the fences so two blocks
// in one reply are removed separately rather than swallowing the prose between
// them.
var jsonBlob = regexp.MustCompile(`(?s)` + "```" + `(?:json)?.*?` + "```" + `|<[a-z_]+>.*?</[a-z_]+>|\{.*\}`)

// ToolOf names the tool a completion called, for the line above its reasoning.
//
// A string search rather than a parse: the completion may be truncated, fenced
// or accompanied by prose, and all this needs is the name.
func ToolOf(completion string) string {
	const key = `"tool":"`
	i := strings.Index(completion, key)
	if i < 0 {
		return ""
	}
	rest := completion[i+len(key):]
	if j := strings.Index(rest, `"`); j >= 0 {
		return rest[:j] // empty when the name is, which is the same answer as absent
	}
	return ""
}

// clipRunes bounds a string BY RUNE, because clipping by byte splits a multi-byte
// character and puts a replacement glyph on screen. Unlike clip it does not trim,
// so it can be used on content whose leading space is meaningful.
func clipRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

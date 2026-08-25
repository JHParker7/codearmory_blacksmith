// Package referee answers one question: when a verification fails, whose fault
// is it?
//
// SIX HAND-WRITTEN CLASSIFIERS ANSWERED IT BEFORE, and each needed three
// attempts to get right. Every one was a regular expression over compiler and
// test output, deciding routing from text. They cover the MECHANICAL failures
// well and they cannot cover the semantic ones at all.
//
// The case that forced this: a test built its own local value and then called a
// handler reading a package-level one. The tests compile, nothing panics, the
// assertions simply fail against state the code cannot reach — so no mechanical
// route fires, and the developer spent 25 verification runs on a specification
// that could not be satisfied from its side. There is no pattern to match there;
// there is only a judgement about two files read together, which is what a model
// is for and a regular expression is not.
//
// DELIBERATELY NOT THE ONLY ROUTE. The certain cases stay deterministic: tests
// that will not parse are the author's, and an undefined symbol alone is the
// expected red of test-first. Neither needs an opinion. The referee is asked
// about the AMBIGUOUS MIDDLE, which is where the hand-written rules kept being
// wrong.
package referee

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/code-armory-app/blacksmith/internal/edit"
	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/transcript"
)

// Who a verdict can blame.
const (
	OwnerSpec        = "spec"
	OwnerDev         = "dev"
	OwnerExpectedRed = "expected_red"
)

// Verdict is what the referee decided.
type Verdict struct {
	// Owner is who must act.
	Owner string `json:"owner"`

	// Reason is one line, and it is NOT decoration: the hand-back comment carries
	// it, and the author reads it. A verdict without a usable reason sends the
	// author back to guess again.
	Reason string `json:"reason"`

	// Confidence lets a weak opinion be ignored rather than acted on.
	Confidence string `json:"confidence"`
}

// Blames reports whether the verdict is worth acting on.
//
// HIGH CONFIDENCE ONLY, because the action it triggers is bounded: two repairs
// and then a person. A maybe is worth less than the developer's next attempt —
// and the asymmetry matters, since a wrong "spec" burns one of two repairs while
// a wrong "dev" costs only turns the developer would have spent anyway.
func (v *Verdict) Blames(owner string) bool {
	return v != nil && v.Owner == owner && strings.EqualFold(v.Confidence, "high")
}

// Prompt is deliberately short on process and long on the ONE distinction that
// matters, because the failure it exists to catch reads as an ordinary assertion
// failure until you look at both files together.
const Prompt = `You are settling a dispute between two agents working on one ticket.

A specification author wrote the tests. A developer wrote the implementation and
MAY NOT EDIT THE TESTS. The tests are failing. Decide who has to change something.

Answer "spec" when NO possible edit to the implementation could make these tests
pass. The clearest examples:
  - a test builds its own local value and then calls code that reads a package
    level one, so the code cannot see what the test set up
  - a test asserts two things that contradict each other
  - a test calls a function with the wrong signature for what it then asserts
  - the test file's own setup is broken: a malformed request, a nil map, a bad literal

Answer "expected_red" when the tests fail only because the implementation does
not exist yet — undefined functions or types. That is the normal state of
test-first work and nobody is at fault.

Answer "dev" for everything else: the implementation is wrong or incomplete and
the developer can fix it.

Be concrete about WHERE. "The test uses a local store the handler cannot read"
is useful; "the test is wrong" is not.`

// Schema constrains the verdict.
//
// The descriptions are on the FIELDS rather than only in the prompt, because the
// schema travels with the request while the system prompt is a separate message
// — and a backend that drops one still has the other.
func Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"owner": map[string]any{
				"type": "string",
				"enum": []string{OwnerSpec, OwnerDev, OwnerExpectedRed},
				"description": "Who must change something for this to pass. \"spec\" if no edit to the " +
					"implementation could make these tests pass. \"dev\" if the implementation is wrong. " +
					"\"expected_red\" if the tests fail only because the implementation is not written yet.",
			},
			"reason": map[string]any{
				"type":        "string",
				"maxLength":   400,
				"description": "One sentence naming the specific thing that is wrong, and where.",
			},
			"confidence": map[string]any{
				"type": "string",
				"enum": []string{"high", "low"},
				"description": "\"high\" only if you are sure. Say \"low\" if the failure could plausibly " +
					"be fixed from either side.",
			},
		},
		"required":             []string{"owner", "reason", "confidence"},
		"additionalProperties": false,
	}
}

// The pre-flight verdicts.
//
// THE WORDS ARE THE WHOLE ANSWER, so they say what they mean on their own and
// cannot be read as anything else.
const (
	PreflightPossible   = "can_be_implemented"
	PreflightImpossible = "no_implementation_could_pass"
)

// PreflightPrompt asks the ONE question that can be answered before any code
// exists: could any implementation satisfy these tests?
//
// The ordinary referee is shown a failure and asked whose fault it is. Before the
// developer has written anything there IS no failure to show, and an undefined
// symbol is the expected red of test-first rather than evidence of anything. So
// this asks about the tests alone, and its default is to say nothing is wrong: a
// false "impossible" costs a repair, and tests being merely incomplete is normal.
const PreflightPrompt = `You are reviewing a specification before an implementation is written.

A specification author wrote these tests. A developer will now write code to
satisfy them and MAY NOT EDIT THE TESTS. Your only question is whether that is
possible at all.

Answer "no_implementation_could_pass" ONLY when no implementation could ever make
a test pass, no matter how it is written. The clearest examples:
  - the test's own setup cannot produce what it then asserts: a parser given no
    comment mode and then asked for comments, a nil map written to, a request
    built without the field the handler reads
  - two assertions that contradict each other
  - a call whose signature does not match what is then asserted about the result
  - a test that asserts on state the code it calls has no way to reach

Answer "can_be_implemented" for everything else. Tests that reference functions
and types that do not exist yet are CORRECT and expected — that is what test-first
looks like before the work starts, and it is not a fault.

You are NOT being asked whose work this is. The tests always belong to the
specification; that is not the question and never makes an answer of
"no_implementation_could_pass" correct.

If you answer "no_implementation_could_pass", name the file and what makes it
impossible, concretely enough that its author can fix it. "The test is wrong" is
not usable.`

// PreflightSchema asks whether the tests can be satisfied, and NEVER asks whose
// fault anything is.
//
// IT USED TO ASK FOR AN "owner" OF "spec" OR "ok", AND THAT WORDING WAS THE BUG.
// Faced with a field called owner and a choice containing "spec", the model names
// the stage the tests belong to — which is always the specification — and the
// caller read that as blame. Measured, and it took a whole board: the reviewer
// answered spec with high confidence while its own reason read "the tests are
// internally consistent ... all assertions are satisfiable by a straightforward
// implementation". It was AGREEING with the specification and its answer was
// recorded as condemning it. Four identical verdicts sent one ticket back until
// its repairs ran out, and five tasks blocked behind it.
//
// The prompt already said all of this and saying it did not work. A LABEL THE
// MODEL CANNOT MISREAD does.
func PreflightSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"verdict": map[string]any{
				"type": "string",
				"enum": []string{PreflightPossible, PreflightImpossible},
				"description": "\"" + PreflightImpossible + "\" only when NO implementation could ever " +
					"make one of these tests pass, however it is written. Tests that name functions " +
					"and types which do not exist yet CAN be implemented — that is what test-first " +
					"looks like before the work starts. Everything else is \"" + PreflightPossible + "\".",
			},
			"reason": map[string]any{
				"type":        "string",
				"maxLength":   400,
				"description": "One sentence naming the test that cannot pass and what makes it impossible.",
			},
			"confidence": map[string]any{
				"type":        "string",
				"enum":        []string{"high", "medium", "low"},
				"description": "How sure you are. Only \"high\" is acted on.",
			},
		},
		"required":             []string{"verdict", "reason", "confidence"},
		"additionalProperties": false,
	}
}

// Gateway is the model call this needs.
type Gateway interface {
	Chat(ctx context.Context, class model.Class, req model.ChatRequest) (model.ChatResult, error)
}

// Referee asks a model whose fault a failure is.
type Referee struct {
	gw       Gateway
	class    model.Class
	counters Counters
}

func New(gw Gateway, class model.Class) *Referee {
	return &Referee{gw: gw, class: class}
}

// Counters records what the referee decided. Optional; nil is a valid
// configuration and every use is guarded.
//
// AN INTERFACE RATHER THAN THE TELEMETRY TYPE, so this package does not depend
// on the metrics stack and a test can count verdicts without one.
type Counters interface {
	RefereeVerdict(owner, confidence string)
}

// WithCounters attaches the counters.
//
// THE REFEREE'S VALUE IS ENTIRELY IN WHETHER IT IS RIGHT, and neither of its two
// failure modes is visible from a log line. One that never blames the spec is
// dead weight; one that blames it constantly is sending healthy specifications
// back, which costs an author's repair budget and reaches a person as "cannot be
// satisfied" about tests that could be. Counting the verdicts is what makes
// either legible.
func (r *Referee) WithCounters(c Counters) *Referee {
	if r == nil {
		return nil
	}
	r.counters = c
	return r
}

// record files a verdict, including a nil one — a referee that declined to rule
// is a different thing from one that was never asked, and only the counter can
// tell them apart afterwards.
func (r *Referee) record(v *Verdict) *Verdict {
	if r == nil || r.counters == nil {
		return v
	}
	if v == nil {
		r.counters.RefereeVerdict("none", "none")
		return v
	}
	r.counters.RefereeVerdict(v.Owner, v.Confidence)
	return v
}

// Preflight reads the tests and says whether they can be satisfied at all.
//
// READ-ONLY BY CONSTRUCTION: one model call over text already fetched, with no
// tools and no sandbox, so it cannot change the branch it is judging.
//
// It runs BEFORE the developer edits anything, which is the whole point. The
// ordinary referee needs three failed verifications first, and by then the
// attempts are spent — measured on a live board, a developer given a test that
// asserted on doc comments the test's own parser had discarded rewrote the same
// correct file until it ran out of turns. It was right every time; the test could
// not pass. Catching that first costs one call.
func (r *Referee) Preflight(ctx context.Context, rec *transcript.Recorder, t ticket.Ticket, tests map[string]string) *Verdict {
	if len(tests) == 0 {
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "TICKET: %s\n\n%s\n\n", t.Title, clip(t.Description, 1200))
	for _, path := range sortedKeys(tests) {
		fmt.Fprintf(&b, "=== %s\n```go\n%s\n```\n\n", path, clip(tests[path], 6000))
	}

	res, err := r.gw.Chat(ctx, r.class, model.ChatRequest{
		Messages: []model.Message{
			{Role: "system", Content: PreflightPrompt},
			{Role: "user", Content: b.String()},
		},
		// Zero, because the same specification should get the same answer on a
		// retry — or the repair budget is spent on the sampler's mood.
		Temperature: 0,
		MaxTokens:   model.MaxReplyTokens,
		Schema:      &model.ReplySchema{Name: "preflight", Schema: PreflightSchema()},
	})
	if err != nil {
		// AN UNAVAILABLE REVIEWER IS NOT A VERDICT. The developer proceeds exactly
		// as it did before this existed.
		return nil
	}
	rec.Turn(ctx, model.ChatRequest{}, res, nil)

	return r.record(ParsePreflight(res.Content))
}

// ParsePreflight reads a pre-flight reply, translating it to a verdict ONLY once
// the answer is unambiguous — so the rest of the pipeline keeps one vocabulary
// for "the specification is at fault".
func ParsePreflight(content string) *Verdict {
	var pre struct {
		Verdict    string `json:"verdict"`
		Reason     string `json:"reason"`
		Confidence string `json:"confidence"`
	}
	if err := model.DecodeJSON(content, &pre); err != nil {
		return nil
	}
	if pre.Verdict != PreflightImpossible {
		return nil
	}
	return &Verdict{Owner: OwnerSpec, Reason: pre.Reason, Confidence: pre.Confidence}
}

// Judge rules on a failure, given the failing output and the files read from the
// branch.
//
// NO EVIDENCE, NO VERDICT. A referee that cannot see both sides is guessing, and
// its guess arrives wearing a confidence field the caller treats as authority.
// Measured: asked to rule with no source at all, it answered "owner: dev,
// confidence: high, the implementation is missing DeleteTaskHandler" — a function
// that was defined, on a branch that compiled. It reached the right owner by
// luck, through a fabricated reason, and a high-confidence verdict is the one
// thing this design acts on without asking again.
//
// Silence sends the ticket down the ordinary retry path, which is the right
// default when nobody actually knows.
func (r *Referee) Judge(ctx context.Context, rec *transcript.Recorder, t ticket.Ticket, read map[string]string, out string) *Verdict {
	if !HasBothSides(read) {
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "TICKET: %s\n\n%s\n\n", t.Title, clip(t.Description, 1200))
	fmt.Fprintf(&b, "THE FAILURE:\n```\n%s\n```\n\n", clipEnds(out, 500, 1500))

	// BOTH SIDES, because the verdict is about the RELATIONSHIP between them and
	// neither file answers it alone. TESTS FIRST: they are the thing the developer
	// cannot change, so they are where an unsatisfiable demand lives.
	for _, p := range sortedKeys(read) {
		if edit.IsTestFile(p) {
			fmt.Fprintf(&b, "TEST FILE %s:\n```go\n%s\n```\n\n", p, clip(read[p], 3000))
		}
	}
	for _, p := range sortedKeys(read) {
		if !edit.IsTestFile(p) {
			fmt.Fprintf(&b, "IMPLEMENTATION %s:\n```go\n%s\n```\n\n", p, clip(read[p], 3000))
		}
	}

	res, err := r.gw.Chat(ctx, r.class, model.ChatRequest{
		Messages: []model.Message{
			{Role: "system", Content: Prompt},
			{Role: "user", Content: b.String()},
		},
		// Zero, because this is a judgement to be REPRODUCED rather than explored.
		Temperature: 0,
		MaxTokens:   400 + model.ThinkingHeadroom,
		Schema:      &model.ReplySchema{Name: "verdict", Schema: Schema()},
	})
	if err != nil {
		// An unavailable referee is not a verdict: everything falls through to the
		// deterministic routes, which is where it was before this existed.
		return nil
	}
	rec.Turn(ctx, model.ChatRequest{}, res, nil)

	return r.record(ParseVerdict(res.Content))
}

// ParseVerdict reads a verdict out of a reply.
//
// TWO ATTEMPTS, because a schema-constrained reply is a bare object and an
// unconstrained one arrives wrapped in prose. A verdict lost to a sentence of
// preamble is a ticket sent down the retry path for no reason.
func ParseVerdict(content string) *Verdict {
	var v Verdict
	if err := model.DecodeJSON(content, &v); err == nil && v.Owner != "" {
		return &v
	}

	i := strings.Index(content, "{")
	j := strings.LastIndex(content, "}")
	if i < 0 || j <= i {
		return nil
	}
	v = Verdict{}
	if err := model.DecodeJSON(content[i:j+1], &v); err != nil || v.Owner == "" {
		return nil
	}
	return &v
}

// HasBothSides reports whether there is a test AND an implementation to compare.
//
// BOTH, NOT EITHER: the question is whether any implementation could satisfy
// these tests, and one side alone cannot answer it.
func HasBothSides(read map[string]string) bool {
	var test, impl bool
	for p, c := range read {
		if strings.TrimSpace(c) == "" {
			continue
		}
		switch {
		case edit.IsTestFile(p):
			test = true
		case strings.HasSuffix(p, ".go"):
			impl = true
		}
	}
	return test && impl
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func clip(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// clipEnds keeps the head and the tail of a long output.
//
// THE CAUSE IS AT ONE END AND THE VERDICT AT THE OTHER: a test run names what
// failed first and summarises last, so cutting either end alone loses half the
// evidence.
func clipEnds(s string, head, tail int) string {
	r := []rune(s)
	if len(r) <= head+tail {
		return s
	}
	return string(r[:head]) + "\n…\n" + string(r[len(r)-tail:])
}

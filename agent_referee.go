package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

// The referee answers one question: when a verification fails, whose fault is it?
//
// SIX HAND-WRITTEN CLASSIFIERS ANSWER IT TODAY and each needed three attempts to
// get right — compileErrorFiles, nonUndefinedCompileErrors, intrinsicTestErrors,
// redIsExpected, panicOnlyInTests, explainConfusingFailure. Every one is a regex
// over compiler and test output, deciding routing from text. They cover the
// mechanical failures well and they cannot cover the semantic ones at all.
//
// The case that forced this: a test wrote `store := NewStore()` as a LOCAL and
// then called a handler reading the package-level `store`. The tests compile,
// nothing panics, the assertions simply fail against state the code cannot
// reach — so no mechanical route fires, and the developer spent 25 verification
// runs on a specification that could not be satisfied from its side. There is no
// pattern to match there; there is only a judgement about two files read
// together, which is what a model is for and a regex is not.
//
// DELIBERATELY NOT THE ONLY ROUTE. The certain cases stay deterministic: tests
// that will not parse are the author's, "undefined:" alone is the expected red of
// test-first, and neither needs an opinion. The referee is asked about the
// ambiguous middle, which is where the hand-written rules kept being wrong.
type refereeVerdict struct {
	// Owner is who must act: "spec", "dev", or "expected_red".
	Owner string `json:"owner"`
	// Reason is one line, and it is not decoration — endOnBrokenSpec puts it in
	// the hand-back comment, which the author now reads (see specRepairReason).
	// A verdict without a usable reason sends the author back to guess again.
	Reason string `json:"reason"`
	// Confidence lets a weak opinion be ignored rather than acted on. A wrong
	// "spec" burns one of two repairs; a wrong "dev" costs only the turns the
	// developer would have spent anyway.
	Confidence string `json:"confidence"`
}

const (
	ownerSpec        = "spec"
	ownerDev         = "dev"
	ownerExpectedRed = "expected_red"
)

// blames reports whether the verdict is worth acting on.
//
// High confidence only, because the action it triggers is bounded: two repairs
// and then a person. A maybe is worth less than the developer's next attempt.
func (v *refereeVerdict) blames(owner string) bool {
	return v != nil && v.Owner == owner && strings.EqualFold(v.Confidence, "high")
}

func refereeSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"owner": map[string]any{
				"type": "string",
				"enum": []string{ownerSpec, ownerDev, ownerExpectedRed},
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

// refereePrompt is deliberately short on process and long on the one distinction
// that matters, because the failure it exists to catch reads as an ordinary
// assertion failure until you look at both files together.
const refereePrompt = `You are settling a dispute between two agents working on one ticket.

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

// askReferee puts the failure, the tests and the implementation in front of a
// model and asks whose problem it is.
// hasBothSides reports whether there is a test AND an implementation to compare.
//
// Both, not either: the question is whether any implementation could satisfy
// these tests, and one side alone cannot answer it.
// preflightPrompt asks the ONE question that can be answered before any code
// exists: could any implementation satisfy these tests?
//
// The ordinary referee is shown a failure and asked whose fault it is. Before the
// developer has written anything there is no failure to show, and "undefined:
// Store" is the expected red of test-first rather than evidence of anything. So
// this asks about the tests alone, and its default is to say nothing is wrong —
// a false "spec" costs a repair, and the tests being merely incomplete is normal.
const preflightPrompt = `You are reviewing a specification before an implementation is written.

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

// Preflight verdicts. The words are the whole answer, so they say what they mean
// on their own and cannot be read as anything else.
const (
	preflightPossible   = "can_be_implemented"
	preflightImpossible = "no_implementation_could_pass"
)

// preflightSchema asks whether the tests can be satisfied, and NEVER asks whose
// fault anything is.
//
// IT USED TO ASK FOR AN "owner" OF "spec" OR "ok", AND THAT WORDING WAS THE BUG.
// Faced with a field called owner and a choice containing "spec", the model names
// the stage the tests belong to — which is always the specification — and the
// caller read that as blame. Measured on r79 and it took the whole board: the
// reviewer answered {"owner": "spec", "confidence": "high"} while its own reason
// read "the tests are internally consistent ... All assertions are satisfiable by
// a straightforward implementation". It was agreeing with the specification and
// its answer was recorded as condemning it. Four identical verdicts sent one
// ticket back until its repairs ran out, and five tasks blocked behind it.
//
// The prompt already said all of this — answer "ok" for everything else, tests
// naming things that do not exist yet are CORRECT and expected — and saying it
// did not work. A label the model cannot misread does.
//
// The descriptions are on the fields for the same reason the referee's are: the
// schema travels with the request and the system prompt is a separate message.
func preflightSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"verdict": map[string]any{
				"type": "string",
				"enum": []string{preflightPossible, preflightImpossible},
				"description": "\"" + preflightImpossible + "\" only when NO implementation could ever " +
					"make one of these tests pass, however it is written. Tests that name functions " +
					"and types which do not exist yet CAN be implemented — that is what test-first " +
					"looks like before the work starts. Everything else is \"" + preflightPossible + "\".",
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

// askSpecPreflight reads the tests and says whether they can be satisfied.
//
// READ-ONLY BY CONSTRUCTION: it is one model call over text already fetched, with
// no tools and no sandbox, so it cannot change the branch it is judging.
//
// It runs BEFORE the developer edits anything, which is the whole point. The
// ordinary referee needs three failed verifications first, and by then the
// attempts are spent — measured on a live board, a developer given a test that
// asserted on doc comments the test's own parser had discarded rewrote the same
// correct file until it ran out of turns. It was right every time; the test could
// not pass. Catching that first costs one call.
func (a *DevAgent) askSpecPreflight(ctx context.Context, rec *Recorder, t Ticket, tests map[string]string) *refereeVerdict {
	if len(tests) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "TICKET: %s\n\n%s\n\n", t.Title, clip(t.Description, 1200))
	for _, path := range sortedKeys(tests) {
		fmt.Fprintf(&b, "=== %s\n```go\n%s\n```\n\n", path, clip(tests[path], 6000))
	}

	res, err := a.gw.Chat(ctx, a.class, ChatRequest{
		Messages: []Message{
			{Role: "system", Content: preflightPrompt},
			{Role: "user", Content: b.String()},
		},
		// Zero for the same reason the referee uses it: the same specification
		// should get the same answer on a retry.
		Temperature: 0,
		MaxTokens:   maxLargeReplyTokens,
		Schema:      &ReplySchema{Name: "preflight", Schema: preflightSchema()},
	})
	if err != nil {
		// An unavailable reviewer is not a verdict. The developer proceeds exactly
		// as it did before this existed.
		return nil
	}
	if rec != nil {
		rec.Turn(ctx, ChatRequest{}, res, nil)
	}

	var pre struct {
		Verdict    string `json:"verdict"`
		Reason     string `json:"reason"`
		Confidence string `json:"confidence"`
	}
	if err := decodeModelJSON(res.Content, &pre); err != nil {
		return nil
	}
	if pre.Verdict != preflightImpossible {
		return nil
	}
	// Translated to the referee's shape only once the answer is unambiguous, so
	// the rest of the pipeline keeps one vocabulary for "the spec is at fault".
	return &refereeVerdict{Owner: ownerSpec, Reason: pre.Reason, Confidence: pre.Confidence}
}

func hasBothSides(read map[string]string) bool {
	var test, impl bool
	for p, c := range read {
		if strings.TrimSpace(c) == "" {
			continue
		}
		if isTestFile(p) {
			test = true
		} else if strings.HasSuffix(p, ".go") {
			impl = true
		}
	}
	return test && impl
}

// readRepoGoFiles pulls every Go file on the branch out of the sandbox.
//
// A GLOB, NOT A PATH LIST, because in delegate mode nothing on this side knows
// what the external agent created — the whole point of delegating is that the
// file set is its business. The listing and the read are one command so the
// answer cannot go stale between them.
func (a *DevAgent) readRepoGoFiles(ctx context.Context, rec *Recorder, s *devState) (map[string]string, error) {
	res, err := a.run(ctx, rec, s,
		`for f in *.go; do [ -f "$f" ] || continue; echo "===FILE $f"; base64 -w0 < "$f"; echo; done`)
	if err != nil {
		return nil, err
	}
	if !res.OK() {
		return nil, fmt.Errorf("exit %d: %s", res.ExitCode, clip(res.Stderr, 300))
	}
	return decodeFileBlocks(res.Stdout), nil
}

func (a *DevAgent) askReferee(ctx context.Context, rec *Recorder, t Ticket, s *devState, out string) *refereeVerdict {
	var b strings.Builder
	fmt.Fprintf(&b, "TICKET: %s\n\n%s\n\n", t.Title, clip(t.Description, 1200))
	fmt.Fprintf(&b, "THE FAILURE:\n```\n%s\n```\n\n", clipEnds(out, 500, 1500))

	// THE EVIDENCE HAS TO BE FETCHED IN DELEGATE MODE. s.read is filled by the
	// native loop's own read tool, and a delegated developer does its reading
	// inside the sandbox where nothing here sees it — so this map is EMPTY for
	// every delegated ticket, and the referee was being asked to rule on a ticket
	// title and a failure log with no source at all.
	//
	// It answered anyway, confidently. Measured on r70: "owner: dev, confidence:
	// high, reason: the implementation is missing the DeleteTaskHandler function"
	// — a function that was defined, on a branch that compiled. It reached the
	// right owner by luck, through a fabricated reason, and a high-confidence
	// verdict is the one thing this design acts on without asking again.
	if !hasBothSides(s.read) {
		if fetched, err := a.readRepoGoFiles(ctx, rec, s); err == nil {
			for path, content := range fetched {
				if _, have := s.read[path]; !have {
					s.read[path] = content
				}
			}
		}
	}
	// NO EVIDENCE, NO VERDICT. A referee that cannot see both sides is guessing,
	// and its guess arrives wearing a confidence field that the caller treats as
	// authority. Silence sends the ticket down the ordinary retry path, which is
	// the right default when nobody actually knows.
	if !hasBothSides(s.read) {
		slog.WarnContext(ctx, "referee not asked: no source to judge from",
			"ticket_id", t.TicketID, "files", len(s.read))
		return nil
	}

	// Both sides, because the verdict is about the RELATIONSHIP between them and
	// neither file answers it alone. Tests first: they are the thing the developer
	// cannot change, so they are where an unsatisfiable demand lives.
	for _, p := range sortedKeys(s.read) {
		if !isTestFile(p) {
			continue
		}
		fmt.Fprintf(&b, "TEST FILE %s:\n```go\n%s\n```\n\n", p, clip(s.read[p], 3000))
	}
	for _, p := range sortedKeys(s.read) {
		if isTestFile(p) {
			continue
		}
		fmt.Fprintf(&b, "IMPLEMENTATION %s:\n```go\n%s\n```\n\n", p, clip(s.read[p], 3000))
	}

	res, err := a.gw.Chat(ctx, a.class, ChatRequest{
		Messages: []Message{
			{Role: "system", Content: refereePrompt},
			{Role: "user", Content: b.String()},
		},
		// Zero, because this is a judgement to be reproduced rather than explored:
		// the same failure should get the same verdict on a retry, or the hand-back
		// budget is spent on the sampler's mood.
		Temperature: 0,
		MaxTokens:   400 + thinkingHeadroom,
		Schema:      &ReplySchema{Name: "verdict", Schema: refereeSchema()},
	})
	if err != nil {
		// An unavailable referee is not a verdict. Everything falls through to the
		// deterministic routes, which is exactly where it was before this existed.
		return nil
	}

	var v refereeVerdict
	if json.Unmarshal([]byte(strings.TrimSpace(res.Content)), &v) != nil {
		if i := strings.Index(res.Content, "{"); i >= 0 {
			if j := strings.LastIndex(res.Content, "}"); j > i {
				if json.Unmarshal([]byte(res.Content[i:j+1]), &v) != nil {
					return nil
				}
			}
		}
	}
	if v.Owner == "" {
		return nil
	}
	return &v
}

package referee

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/ticket"
)

// gateway answers with whatever a test puts in it, and keeps the request so the
// prompt can be inspected.
type gateway struct {
	content string
	err     error
	req     model.ChatRequest
	calls   int
}

func (g *gateway) Chat(_ context.Context, _ model.Class, req model.ChatRequest) (model.ChatResult, error) {
	g.calls++
	g.req = req
	if g.err != nil {
		return model.ChatResult{}, g.err
	}
	return model.ChatResult{Content: g.content}, nil
}

func reply(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func bothSides() map[string]string {
	return map[string]string{
		"store_test.go": "package store\n\nfunc TestAdd(t *testing.T) {}\n",
		"store.go":      "package store\n\nfunc Add() {}\n",
	}
}

func aTicket() ticket.Ticket {
	return ticket.Ticket{ID: "t-1", Title: "Add a store", Description: "It should store things."}
}

// HIGH CONFIDENCE ONLY. The action a verdict triggers is bounded — two repairs
// and then a person — so a maybe is worth less than the developer's next attempt.
func TestOnlyAConfidentVerdictIsActedOn(t *testing.T) {
	cases := []struct {
		v     *Verdict
		owner string
		want  bool
	}{
		{&Verdict{Owner: OwnerSpec, Confidence: "high"}, OwnerSpec, true},
		{&Verdict{Owner: OwnerSpec, Confidence: "HIGH"}, OwnerSpec, true},
		{&Verdict{Owner: OwnerSpec, Confidence: "low"}, OwnerSpec, false},
		{&Verdict{Owner: OwnerSpec, Confidence: ""}, OwnerSpec, false},
		{&Verdict{Owner: OwnerDev, Confidence: "high"}, OwnerSpec, false},
		{nil, OwnerSpec, false},
	}
	for _, c := range cases {
		if got := c.v.Blames(c.owner); got != c.want {
			t.Errorf("(%+v).Blames(%q) = %v, want %v", c.v, c.owner, got, c.want)
		}
	}
}

// NO EVIDENCE, NO VERDICT. A referee that cannot see both sides is guessing, and
// its guess arrives wearing a confidence field the caller treats as authority —
// measured at "the implementation is missing DeleteTaskHandler" against a branch
// where it was defined and compiling.
func TestTheRefereeIsNotEvenAskedWithoutBothSides(t *testing.T) {
	cases := map[string]map[string]string{
		"nothing at all":         {},
		"only tests":             {"store_test.go": "package store"},
		"only an implementation": {"store.go": "package store"},
		"an empty test file":     {"store_test.go": "  ", "store.go": "package store"},
		"non-Go files":           {"README.md": "hello", "store_test.go": "package store"},
	}
	for name, read := range cases {
		g := &gateway{content: reply(Verdict{Owner: OwnerDev, Confidence: "high", Reason: "made up"})}
		got := New(g, model.ClassLarge).Judge(context.Background(), nil, aTicket(), read, "FAIL")

		if got != nil {
			t.Errorf("%s: a verdict was reached with no evidence: %+v", name, got)
		}
		if g.calls != 0 {
			t.Errorf("%s: the model was asked to rule with nothing to read", name)
		}
	}
}

func TestBothSidesIsTestsAndImplementation(t *testing.T) {
	if !HasBothSides(bothSides()) {
		t.Error("a test and an implementation did not read as both sides")
	}
	if HasBothSides(map[string]string{"a_test.go": "x", "b_test.go": "y"}) {
		t.Error("two test files read as both sides")
	}
}

// THE TESTS COME FIRST IN THE EVIDENCE: they are the thing the developer cannot
// change, so they are where an unsatisfiable demand lives.
func TestTheEvidencePutsTheTestsBeforeTheImplementation(t *testing.T) {
	g := &gateway{content: reply(Verdict{Owner: OwnerDev, Confidence: "low", Reason: "x"})}
	New(g, model.ClassLarge).Judge(context.Background(), nil, aTicket(), bothSides(), "FAIL: TestAdd")

	if g.calls != 1 {
		t.Fatalf("the model was asked %d times", g.calls)
	}
	user := g.req.Messages[len(g.req.Messages)-1].Content
	tests := strings.Index(user, "TEST FILE store_test.go")
	impl := strings.Index(user, "IMPLEMENTATION store.go")
	if tests < 0 || impl < 0 {
		t.Fatalf("the evidence is missing a side:\n%s", user)
	}
	if tests > impl {
		t.Error("the implementation is shown before the tests")
	}
	// The failure itself has to be there, or the referee is ruling on the files
	// alone.
	if !strings.Contains(user, "FAIL: TestAdd") {
		t.Error("the failure was not shown")
	}
}

// A JUDGEMENT TO BE REPRODUCED, not explored: the same failure must get the same
// verdict on a retry, or the hand-back budget is spent on the sampler's mood.
func TestTheRefereeIsAskedDeterministically(t *testing.T) {
	g := &gateway{content: reply(Verdict{Owner: OwnerDev, Confidence: "low", Reason: "x"})}
	New(g, model.ClassLarge).Judge(context.Background(), nil, aTicket(), bothSides(), "FAIL")

	if g.req.Temperature != 0 {
		t.Errorf("temperature = %v, want greedy", g.req.Temperature)
	}
	if g.req.Schema == nil {
		t.Fatal("the verdict was not constrained by a schema")
	}
	// The enum is what stops a free-text owner reaching the routing.
	props := g.req.Schema.Schema["properties"].(map[string]any)
	owner := props["owner"].(map[string]any)
	got, _ := owner["enum"].([]string)
	if len(got) != 3 {
		t.Errorf("the owner enum is %v, want the three answers", got)
	}
}

// AN UNAVAILABLE REFEREE IS NOT A VERDICT. Everything falls through to the
// deterministic routes, which is where it was before this existed.
func TestAnUnavailableRefereeSaysNothing(t *testing.T) {
	g := &gateway{err: errors.New("the endpoint refused the connection")}
	got := New(g, model.ClassLarge).Judge(context.Background(), nil, aTicket(), bothSides(), "FAIL")
	if got != nil {
		t.Errorf("an unreachable model produced a verdict: %+v", got)
	}
}

func TestAVerdictIsReadBackOffTheReply(t *testing.T) {
	g := &gateway{content: reply(Verdict{
		Owner: OwnerSpec, Confidence: "high",
		Reason: "the test uses a local store the handler cannot read",
	})}
	got := New(g, model.ClassLarge).Judge(context.Background(), nil, aTicket(), bothSides(), "FAIL")

	if got == nil {
		t.Fatal("no verdict was read")
	}
	if !got.Blames(OwnerSpec) {
		t.Errorf("verdict = %+v, want it to blame the specification confidently", got)
	}
	// THE REASON IS NOT DECORATION: the hand-back comment carries it and the
	// author reads it.
	if !strings.Contains(got.Reason, "local store") {
		t.Errorf("reason = %q", got.Reason)
	}
}

// TWO ATTEMPTS AT PARSING, because a schema-constrained reply is a bare object
// and an unconstrained one arrives wrapped in prose. A verdict lost to a sentence
// of preamble is a ticket sent down the retry path for no reason.
func TestAVerdictIsFoundEvenWrappedInProse(t *testing.T) {
	body := reply(Verdict{Owner: OwnerDev, Confidence: "high", Reason: "the handler ignores the id"})
	cases := map[string]string{
		"bare":             body,
		"with a preamble":  "Here is my verdict:\n" + body,
		"with a trailer":   body + "\n\nI hope that helps.",
		"fenced":           "```json\n" + body + "\n```",
		"both sides of it": "Thinking...\n" + body + "\nDone.",
	}
	for name, content := range cases {
		got := ParseVerdict(content)
		if got == nil {
			t.Errorf("%s: no verdict was read from %q", name, content)
			continue
		}
		if got.Owner != OwnerDev {
			t.Errorf("%s: owner = %q", name, got.Owner)
		}
	}
}

func TestAReplyWithNoVerdictIsNotOne(t *testing.T) {
	for _, content := range []string{
		"",
		"I am not sure.",
		"{}",
		`{"reason":"something","confidence":"high"}`, // no owner
		"{ not json at all",
	} {
		if got := ParseVerdict(content); got != nil {
			t.Errorf("ParseVerdict(%q) = %+v, want nothing", content, got)
		}
	}
}

// THE PRE-FLIGHT NEVER ASKS WHOSE WORK IT IS. Faced with a field called owner
// and a choice containing "spec", the model names the stage the tests belong to
// — which is always the specification — and the caller read that as blame. Four
// identical verdicts sent one ticket back until its repairs ran out.
func TestThePreflightSchemaCannotBeReadAsBlame(t *testing.T) {
	props, ok := PreflightSchema()["properties"].(map[string]any)
	if !ok {
		t.Fatal("the pre-flight schema has no properties")
	}
	if _, blames := props["owner"]; blames {
		t.Error("the pre-flight asks for an owner; the model would name the stage the tests belong to")
	}

	verdict, ok := props["verdict"].(map[string]any)
	if !ok {
		t.Fatal("the pre-flight schema asks for no verdict")
	}
	enum, _ := verdict["enum"].([]string)
	for _, v := range enum {
		if v == OwnerSpec || v == "ok" {
			t.Errorf("the pre-flight offers %q, which reads as blame rather than as an answer", v)
		}
		// The words have to say what they mean on their own.
		if !strings.Contains(v, "_") || len(v) < 10 {
			t.Errorf("the pre-flight answer %q is too terse to be unambiguous", v)
		}
	}
}

// ITS DEFAULT IS TO SAY NOTHING IS WRONG: a false "impossible" costs a repair,
// and tests being merely incomplete is normal.
func TestThePreflightIsSilentUnlessNothingCouldPass(t *testing.T) {
	cases := map[string]string{
		"the tests are fine": reply(map[string]string{
			"verdict": PreflightPossible, "reason": "all satisfiable", "confidence": "high"}),
		"an unreadable reply": "I could not decide.",
		"an empty reply":      "",
		// A BACKEND THAT IGNORES THE ENUM must not be read as blame. This is the
		// shape of the failure that took a whole board: a label the caller did not
		// expect, treated as a verdict against the specification.
		"a label outside the enum": reply(map[string]string{
			"verdict": "maybe", "reason": "hard to say", "confidence": "high"}),
		"an empty verdict": reply(map[string]string{
			"verdict": "", "reason": "", "confidence": "high"}),
		"the pipeline's own word for blame": reply(map[string]string{
			"verdict": OwnerSpec, "reason": "the tests belong to the specification", "confidence": "high"}),
	}
	for name, content := range cases {
		if got := ParsePreflight(content); got != nil {
			t.Errorf("%s: the pre-flight blamed the specification: %+v", name, got)
		}
	}
}

func TestAnImpossibleSpecificationIsTranslatedToTheOneVocabulary(t *testing.T) {
	content := reply(map[string]string{
		"verdict":    PreflightImpossible,
		"reason":     "store_test.go asserts on comments the parser was told to discard",
		"confidence": "high",
	})
	got := ParsePreflight(content)
	if got == nil {
		t.Fatal("an impossible specification produced no verdict")
	}
	// Translated only once the answer is unambiguous, so the rest of the pipeline
	// keeps ONE vocabulary for "the specification is at fault".
	if got.Owner != OwnerSpec {
		t.Errorf("owner = %q, want the pipeline's own word", got.Owner)
	}
	if !got.Blames(OwnerSpec) {
		t.Error("a high-confidence impossibility is not acted on")
	}
	if !strings.Contains(got.Reason, "store_test.go") {
		t.Errorf("reason = %q, want it to name the file", got.Reason)
	}
}

func TestThePreflightIsNotAskedWithoutTests(t *testing.T) {
	g := &gateway{content: reply(map[string]string{"verdict": PreflightImpossible})}
	if got := New(g, model.ClassLarge).Preflight(context.Background(), nil, aTicket(), nil); got != nil {
		t.Errorf("a verdict was reached with no tests: %+v", got)
	}
	if g.calls != 0 {
		t.Error("the model was asked about a specification that does not exist")
	}
}

func TestThePreflightShowsEveryTestFile(t *testing.T) {
	g := &gateway{content: reply(map[string]string{"verdict": PreflightPossible})}
	tests := map[string]string{
		"store_test.go":   "package store // one",
		"handler_test.go": "package store // two",
	}
	New(g, model.ClassLarge).Preflight(context.Background(), nil, aTicket(), tests)

	user := g.req.Messages[len(g.req.Messages)-1].Content
	for path := range tests {
		if !strings.Contains(user, path) {
			t.Errorf("the pre-flight was not shown %s", path)
		}
	}
	if !strings.Contains(user, aTicket().Title) {
		t.Error("the pre-flight was not told what the ticket asks for")
	}
}

// THE EVIDENCE IS BOUNDED AT BOTH ENDS. A test run names what failed first and
// summarises last, so cutting either end alone loses half of it.
func TestALongFailureKeepsItsHeadAndItsTail(t *testing.T) {
	out := "FIRST LINE: the compile error\n" + strings.Repeat("noise\n", 5000) + "LAST LINE: 12 tests failed"
	g := &gateway{content: reply(Verdict{Owner: OwnerDev, Confidence: "low", Reason: "x"})}
	New(g, model.ClassLarge).Judge(context.Background(), nil, aTicket(), bothSides(), out)

	user := g.req.Messages[len(g.req.Messages)-1].Content
	if !strings.Contains(user, "FIRST LINE") {
		t.Error("the head of the failure was cut")
	}
	if !strings.Contains(user, "LAST LINE") {
		t.Error("the tail of the failure was cut")
	}
	if len([]rune(user)) > 30000 {
		t.Errorf("the prompt is %d runes; the whole log was pasted in", len([]rune(user)))
	}
}

// The prompt has to name the failure it exists to catch, because it reads as an
// ordinary assertion failure until you look at both files together.
func TestThePromptNamesTheFailureItExistsFor(t *testing.T) {
	for _, want := range []string{"MAY NOT EDIT THE TESTS", "local", "expected_red"} {
		if !strings.Contains(Prompt, want) {
			t.Errorf("the prompt does not mention %q", want)
		}
	}
	// And it must ask for a concrete answer, since a vague one is unusable to the
	// author who receives it.
	if !strings.Contains(Prompt, "is not") {
		t.Error("the prompt does not say what an unusable answer looks like")
	}
}

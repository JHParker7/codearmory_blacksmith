package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// THE SPECIFICATION IS CHECKED BEFORE THE DEVELOPER WRITES ANYTHING.
//
// The ordinary referee needs three failed verifications first, and by then the
// attempts are spent. Measured on a live board: a section whose tests asserted on
// doc comments — using a parser the test itself had given no comment mode, so the
// comments were discarded before the assertion ran — took a developer that wrote
// correct doc comments, watched them fail, and rewrote the same file until it ran
// out of turns. Its own reasoning said "it DOES have doc comments". It was right
// every time and could not win, because it may not edit the test.

// THE ANSWER MUST NOT BE A NAME FOR WHOSE WORK THIS IS.
//
// The field used to be "owner" with a choice of "spec" or "ok", and that wording
// was the bug. Faced with a field called owner and an option called spec, the
// model names the stage the tests belong to — always the specification — and the
// caller read it as blame.
//
// Measured on r79 and it took the whole board: the reviewer answered
// {"owner": "spec", "confidence": "high"} while its own reason read "the tests
// are internally consistent ... All assertions are satisfiable by a
// straightforward implementation". It was agreeing with the specification and its
// answer was recorded as condemning it. Four identical verdicts sent one ticket
// back until its repairs ran out, and five tasks blocked behind it.
func TestPreflightCannotBeAnsweredWithWhoseWorkItIs(t *testing.T) {
	schema := preflightSchema()
	props := schema["properties"].(map[string]any)

	if _, present := props["owner"]; present {
		t.Error("the preflight asks for an owner again; the tests always belong to the spec and that is not the question")
	}
	verdict, present := props["verdict"].(map[string]any)
	if !present {
		t.Fatal("no verdict field")
	}
	enum, ok := verdict["enum"].([]string)
	if !ok || len(enum) != 2 {
		t.Fatalf("verdict enum = %v, want exactly two answers", verdict["enum"])
	}
	for _, v := range enum {
		if v == ownerSpec || v == ownerDev || v == "ok" {
			t.Errorf("%q is selectable; it names a stage rather than an answer", v)
		}
	}
	// The words have to carry the meaning on their own, because the schema travels
	// with the request and the prompt is a separate message.
	if !strings.Contains(strings.Join(enum, " "), "implement") {
		t.Errorf("enum = %v; neither answer says anything about implementability", enum)
	}
	if _, described := verdict["description"]; !described {
		t.Error("the verdict field has no description; the referee schema documents its fields and this one did not, which is how the wording drifted")
	}
}

func TestPreflightSchemaIsValidJSON(t *testing.T) {
	if _, err := json.Marshal(preflightSchema()); err != nil {
		t.Fatalf("schema does not marshal: %v", err)
	}
}

func TestPreflightPromptSaysUndefinedSymbolsAreFine(t *testing.T) {
	// The failure mode of a review that runs BEFORE the code: everything is
	// undefined, and a reviewer that reads that as a broken specification would
	// hand every single ticket straight back.
	// Whitespace-normalised: the prompt is hard-wrapped, so a phrase can fall
	// across a line break and an exact-substring check would fail on formatting
	// rather than on meaning.
	lower := strings.Join(strings.Fields(strings.ToLower(preflightPrompt)), " ")
	if !strings.Contains(lower, "do not exist yet") {
		t.Error("the prompt does not tell the reviewer that missing symbols are expected")
	}
	if !strings.Contains(lower, "test-first") {
		t.Error("the prompt does not name test-first as the normal state")
	}
}

func TestPreflightVerdictMustBeHighConfidence(t *testing.T) {
	// Acting on a maybe costs one of two repairs. The developer's own attempt is
	// worth more than that.
	low := &refereeVerdict{Owner: ownerSpec, Confidence: "low"}
	if low.blames(ownerSpec) {
		t.Error("a low-confidence verdict would hand the ticket back")
	}
	high := &refereeVerdict{Owner: ownerSpec, Confidence: "high"}
	if !high.blames(ownerSpec) {
		t.Error("a high-confidence verdict does not act")
	}
}

func TestHasTestFileAsksTheSurveyNotTheSandbox(t *testing.T) {
	// A branch with no tests has nothing to review, and finding that out must not
	// cost a sandbox round trip — which is also what keeps the read-cache test
	// honest, since any fetch shows up there as a read.
	if hasTestFile([]string{"main.go", "store.go"}) {
		t.Error("found a test file where there is none")
	}
	if !hasTestFile([]string{"main.go", "store_test.go"}) {
		t.Error("missed a test file that is there")
	}
	if hasTestFile(nil) {
		t.Error("an empty survey reported a test file")
	}
}

func TestPreflightWithNoTestsAsksNothing(t *testing.T) {
	// No tests, no question. A model call here would be spent to be told there is
	// nothing to look at.
	a := &DevAgent{}
	if v := a.askSpecPreflight(t.Context(), nil, Ticket{}, nil); v != nil {
		t.Errorf("asked for a verdict with no tests to review: %+v", v)
	}
}

// THE REPLY THAT TOOK R79, DRIVEN THROUGH THE REAL PATH.
//
// A model that judges the specification sound must not produce a hand-back. The
// old schema let it: the reviewer picked the label naming whose work the tests
// were, and the caller read that as a fault.
func TestAReviewerAgreeingWithTheSpecDoesNotHandItBack(t *testing.T) {
	reply := `{"verdict":"can_be_implemented","confidence":"high","reason":"The tests are internally consistent and reference types that do not yet exist, which is expected in a test-first workflow."}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + strconv.Quote(reply) + `},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	a := &DevAgent{class: ClassLarge, gw: NewGateway(Config{Classes: map[Class]ClassConfig{
		ClassLarge: {Endpoint: srv.URL, Model: "m", Slots: 1, QueueDepth: 1},
	}})}
	v := a.askSpecPreflight(t.Context(), nil, Ticket{Title: "x"}, map[string]string{"a_test.go": "package tracker"})
	if v != nil {
		t.Errorf("a reviewer that called the spec implementable handed it back anyway: %+v", v)
	}
}

// And the case it exists for still fires.
func TestAnImpossibleSpecStillHandsBack(t *testing.T) {
	reply := `{"verdict":"no_implementation_could_pass","confidence":"high","reason":"a_test.go parses with mode 0 then asserts on comments"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + strconv.Quote(reply) + `},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	a := &DevAgent{class: ClassLarge, gw: NewGateway(Config{Classes: map[Class]ClassConfig{
		ClassLarge: {Endpoint: srv.URL, Model: "m", Slots: 1, QueueDepth: 1},
	}})}
	v := a.askSpecPreflight(t.Context(), nil, Ticket{Title: "x"}, map[string]string{"a_test.go": "package tracker"})
	if !v.blames(ownerSpec) {
		t.Errorf("an unsatisfiable spec was not handed back: %+v", v)
	}
}

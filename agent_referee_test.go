package main

import (
	"strings"
	"testing"
)

// THE VERDICT MUST BE ACTED ON ONLY WHEN IT IS CONFIDENT, because a wrong "spec"
// spends one of two repairs and then a person's attention, while a wrong "dev"
// costs only turns the developer would have spent anyway. The asymmetry is the
// whole reason confidence is in the schema.
func TestRefereeBlamesOnlyOnHighConfidence(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    *refereeVerdict
		want bool
	}{
		{"sure it is the spec", &refereeVerdict{Owner: ownerSpec, Confidence: "high"}, true},
		{"unsure", &refereeVerdict{Owner: ownerSpec, Confidence: "low"}, false},
		{"sure it is the developer", &refereeVerdict{Owner: ownerDev, Confidence: "high"}, false},
		{"expected red", &refereeVerdict{Owner: ownerExpectedRed, Confidence: "high"}, false},
		{"case insensitive", &refereeVerdict{Owner: ownerSpec, Confidence: "HIGH"}, true},
		{"no verdict at all", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.v.blames(ownerSpec); got != tc.want {
				t.Errorf("blames(spec) = %v, want %v", got, tc.want)
			}
		})
	}
}

// The schema is the contract with the sampler: a reply that cannot be malformed
// cannot strand a ticket, which is why every field is required and both
// vocabularies are closed.
func TestRefereeSchemaIsClosed(t *testing.T) {
	sc := refereeSchema()
	req, _ := sc["required"].([]string)
	if len(req) != 3 {
		t.Errorf("required = %v, want owner, reason and confidence", req)
	}
	if sc["additionalProperties"] != false {
		t.Error("the schema admits extra properties, so the reply shape is not pinned")
	}
	props := sc["properties"].(map[string]any)
	owner := props["owner"].(map[string]any)["enum"].([]string)
	if len(owner) != 3 {
		t.Errorf("owner enum = %v, want exactly the three verdicts", owner)
	}
	// An open reason field is fine; an open OWNER would put free text into a
	// routing decision and an unbounded label into the metric.
	for _, want := range []string{ownerSpec, ownerDev, ownerExpectedRed} {
		if !containsStr(owner, want) {
			t.Errorf("owner enum is missing %q", want)
		}
	}
}

// The brief must teach the case that forced this to exist — a test asserting
// against state the implementation cannot reach — because that failure reads as
// an ordinary assertion failure until both files are read together.
func TestRefereePromptNamesTheUnreachableStateCase(t *testing.T) {
	for _, want := range []string{"local", "package", "undefined", "MAY NOT EDIT"} {
		if !strings.Contains(refereePrompt, want) {
			t.Errorf("the brief never mentions %q", want)
		}
	}
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// THE GATE MUST COUNT THE RIGHT THING. It was first written against
// specBrokenTries, which only advances inside the compile-error branch — the
// very case the referee exists to complement. A semantic failure, where the
// tests compile and merely assert against unreachable state, left that counter
// at zero, so the referee could never fire on the ticket it was built for.
func TestRefereeIsGatedOnFailedVerifications(t *testing.T) {
	a := &DevAgent{mode: modeDevelop}

	// Below the threshold: not asked, and no verdict invented.
	s := &devState{failedVerifications: minTriesBeforeReferee - 1}
	if v := a.refereeOn(t.Context(), nil, s, "boom"); v != nil {
		t.Errorf("asked too early and returned %v", v)
	}
	if s.refereeAsked {
		t.Error("marked as asked before the threshold")
	}

	// A compile-only counter must NOT be what opens the gate.
	s2 := &devState{specBrokenTries: 99}
	if v := a.refereeOn(t.Context(), nil, s2, "boom"); v != nil {
		t.Errorf("opened on specBrokenTries, which is the wrong signal: %v", v)
	}

	// Wrong mode is never asked: the specification author has no counterpart to
	// blame, and the coverage stage is not in this dispute at all.
	for _, mode := range []agentMode{modeTest, modeCoverage, modeSpecMerge} {
		other := &DevAgent{mode: mode}
		st := &devState{failedVerifications: 99}
		if v := other.refereeOn(t.Context(), nil, st, "boom"); v != nil {
			t.Errorf("mode %v consulted the referee", mode)
		}
	}
}

// NO EVIDENCE, NO VERDICT.
//
// s.read is filled by the native loop's read tool, and a delegated developer does
// all its reading inside the sandbox — so for every delegated ticket the referee
// was handed a title and a failure log and nothing else. It answered anyway:
// measured on r70, "owner: dev, confidence: high, the implementation is missing
// the DeleteTaskHandler function", on a branch where that function was defined
// and compiling. High confidence is the one thing the caller acts on unasked.
func TestRefereeDeclinesWithoutBothSides(t *testing.T) {
	if hasBothSides(map[string]string{"main_test.go": "package main"}) {
		t.Error("tests alone counted as evidence; nothing says what the implementation does")
	}
	if hasBothSides(map[string]string{"main.go": "package main"}) {
		t.Error("an implementation alone counted as evidence; nothing says what is demanded of it")
	}
	if hasBothSides(map[string]string{"main.go": "package main", "main_test.go": ""}) {
		t.Error("an empty file counted as evidence")
	}
	if !hasBothSides(map[string]string{"main.go": "package main", "main_test.go": "package main"}) {
		t.Error("both sides present but not recognised; the referee would never be asked")
	}
}

// The fetch protocol has to match what the sandbox emits, or the referee silently
// gets nothing and declines on every ticket instead of only the blind ones.
func TestDecodeFileBlocksReadsTheSandboxProtocol(t *testing.T) {
	got := decodeFileBlocks("===FILE main.go\ncGFja2FnZSBtYWlu\n===FILE a_test.go\ncGFja2FnZSBtYWlu\n")
	if len(got) != 2 || got["main.go"] != "package main" || got["a_test.go"] != "package main" {
		t.Errorf("decodeFileBlocks() = %#v, want both files decoded", got)
	}
}

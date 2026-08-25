package dev

import (
	"strings"
	"testing"
)

// THE HAND-BACK MUST NAME THE FAULT IT FOUND, not the one the marker happens to
// mention.
//
// Observed on a live run: a specification registered the same route twice, so it
// COMPILED PERFECTLY and panicked in its own fixture. The author was twice told
// "the specification does not compile", spent both repairs without touching the
// panic, and the ticket blocked with the real cause never named — while the
// panic sat in the output nobody had been pointed at.
func TestEachSpecFaultIsNamedInItsOwnTerms(t *testing.T) {
	panicOut := `--- FAIL: TestBoard (0.00s)
panic: pattern "/tickets" (registered at /workspace/handlers_test.go:21) conflicts with pattern /tickets [recovered, repanicked]
	/workspace/handlers_test.go:21 +0x1a4
	/workspace/handlers_test.go:63 +0x88`

	compileOut := "./store_test.go:12:9: declared and not used: got\n" +
		"./store_test.go:20:2: missing return"

	clashOut := "found packages main (store.go) and demo (store_test.go) in /workspace"

	for name, tc := range map[string]struct {
		out  string
		want string
	}{
		"a panic in the fixture": {panicOut, "PANIC"},
		"a local compile error":  {compileOut, "compile"},
		"two packages":           {clashOut, "package"},
	} {
		t.Run(name, func(t *testing.T) {
			s := &State{Writes: 9, SpecBrokenWrites: 0}
			s.JudgeSpec(tc.out, ModeDevelop)

			if s.SpecBroken == "" {
				t.Fatalf("no verdict was reached at all for %s", name)
			}
			got := s.FaultOrDefault()
			if !strings.Contains(got, tc.want) {
				t.Errorf("the fault reads %q, which does not name %q", got, tc.want)
			}
		})
	}
}

// A PANIC IS NOT A COMPILE ERROR, and saying so is the whole fix. This pins the
// specific sentence the author was misled by.
func TestAPanicIsNotReportedAsAFailureToCompile(t *testing.T) {
	s := &State{}
	s.JudgeSpec(`panic: pattern "/tickets" (registered at /workspace/handlers_test.go:21) conflicts
	/workspace/handlers_test.go:21 +0x1a4`, ModeDevelop)

	got := s.FaultOrDefault()
	if strings.Contains(strings.ToLower(got), "do not compile") {
		t.Errorf("a panic was described as a compile failure: %q", got)
	}
	if !strings.Contains(got, "PANIC") {
		t.Errorf("the fault does not say it panicked: %q", got)
	}
}

// NEVER A BLANK. A route that reaches a verdict without saying why must still
// produce a sentence, or the author reads a gap where the cause belongs.
func TestAVerdictWithoutAStatedFaultStillReadsAsASentence(t *testing.T) {
	s := &State{SpecBroken: "store_test.go"}
	if got := s.FaultOrDefault(); strings.TrimSpace(got) == "" {
		t.Error("the hand-back would carry an empty cause")
	}
}

// AND IT IS CLEARED WITH THE VERDICT. A stale reason outliving the verdict it
// explained would describe the wrong failure on the next hand-back.
func TestTheFaultIsClearedWithTheVerdict(t *testing.T) {
	s := &State{}
	s.JudgeSpec("./store_test.go:12:9: declared and not used: got", ModeDevelop)
	if s.SpecFault == "" {
		t.Fatal("no fault was recorded to clear")
	}

	// An ordinary red run: the developer is working on something else now.
	s.JudgeSpec("--- FAIL: TestList (0.00s)\n    store_test.go:63: got 2 want 1", ModeDevelop)
	if s.SpecFault != "" {
		t.Errorf("the fault survived the verdict: %q", s.SpecFault)
	}
}

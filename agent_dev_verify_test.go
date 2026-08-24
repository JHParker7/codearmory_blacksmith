package main

import (
	"strings"
	"testing"
)

// Production code may not import a testing package.
//
// Measured on r70, the most expensive single thing a delegated agent did: asked to
// add validation, it wrote `func createTask(w *httptest.ResponseRecorder, ...)`
// into main.go, with its own throwaway store, so the store the caller passed in
// never saw the task. The suite went red and the agent blamed "URL routing causing
// 301 redirects" — a fault it could never find by reading assertions, because
// every failing assertion pointed at a test that was correct.
func TestVerifyRejectsTestingImportsInProductionCode(t *testing.T) {
	sc := testImportGateScript()

	for _, want := range []string{`"testing"`, `"net/http/httptest"`} {
		if !strings.Contains(sc, want) {
			t.Errorf("the gate does not look for %s in production files", want)
		}
	}
	// It must EXCLUDE the test files, or it fails every repository immediately.
	if !strings.Contains(sc, `grep -v '_test\.go$'`) {
		t.Error("test files are not excluded; the gate would reject every Go repo")
	}
	// A named gate, so the failure is attributed rather than reported as "tests failed".
	if !strings.Contains(sc, gateMarker+"test-imports-in-production") {
		t.Error("the gate is unnamed; the failure would be misattributed to the tests")
	}
	// Guarded on go.mod: the rule is Go's, and another language must skip it.
	if !strings.Contains(sc, "[ -f go.mod ]") {
		t.Error("the Go-specific gate is not guarded; it would run against any repo")
	}
	// And it must say what to do, not only what is wrong.
	if !strings.Contains(sc, "values the caller passes in") {
		t.Error("the message names the symptom without the cause")
	}
}

// The gate and the agent's prompt must detect the SAME thing.
//
// They were briefly separate, which is a bug with a long fuse: a production file
// importing a testing package still compiles, so the suite is silent, and the
// agent would have been failed by a gate whose reason never appeared in anything
// it was shown.
func TestDelegateShowsTheSameTestImportReasonTheGateUses(t *testing.T) {
	sc := delegateScript(Ticket{Title: "x"}, "go test ./...", 900, true, "")
	if !strings.Contains(sc, testImportOffenders()) {
		t.Error("the prompt does not run the gate's own detection")
	}
	if !strings.Contains(sc, "THIS ALONE WILL FAIL THE BRANCH") {
		t.Error("the agent is not told this fails the branch regardless of the tests")
	}
	// Reported, not fatal, in the prompt: the delegate script must go on to run the
	// agent so it can FIX it. Exiting here would fail the attempt with no work done.
	i := strings.Index(sc, "THIS ALONE WILL FAIL THE BRANCH")
	if i >= 0 && strings.Contains(sc[i:i+400], "exit 1") {
		t.Error("the prompt-side check exits instead of reporting; the agent never gets to fix it")
	}
}

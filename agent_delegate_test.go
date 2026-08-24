package main

import (
	"strings"
	"testing"
)

// THE BOUNDARY MUST SURVIVE THE AGENT. chmod makes the tests read-only, but a
// third-party agent runs as root inside the sandbox and root ignores the mode
// bits — so anything it manages to change is reverted before the commit rather
// than argued about. The specification is not the developer's to edit however
// capable the developer is.
func TestDelegateScriptProtectsTheTests(t *testing.T) {
	sc := delegateScript(Ticket{Title: "add a handler", Description: "make the tests pass"}, "go test ./...", 900, false, "")
	for _, want := range []string{
		"chmod a-w *_test.go",              // the boundary
		"git checkout -- '*_test.go'",      // and the backstop, for root
		"READ-ONLY",                        // stated to the agent as well
	} {
		if !strings.Contains(sc, want) {
			t.Errorf("the script does not contain %q", want)
		}
	}
}

// ONE EXECUTION, because RunOnBranch resets the working tree on every call —
// git reset --hard and git clean -xfd. Work that is not committed and pushed
// inside the same command is wiped by the verification that follows it.
func TestDelegateScriptCommitsAndPushesInOnePass(t *testing.T) {
	sc := delegateScript(Ticket{Title: "x"}, "go test ./...", 900, false, "")
	iAgent := strings.Index(sc, "qwen --prompt")
	iCommit := strings.Index(sc, "commit -q -m")
	iPush := strings.Index(sc, "git push")
	if iAgent < 0 || iCommit < 0 || iPush < 0 {
		t.Fatalf("script is missing a stage: agent=%d commit=%d push=%d", iAgent, iCommit, iPush)
	}
	if !(iAgent < iCommit && iCommit < iPush) {
		t.Error("the agent must run, then commit, then push, in that order")
	}
	// A run that changed nothing must say so rather than pushing an empty commit.
	if !strings.Contains(sc, "changed nothing") {
		t.Error("a no-change run is not reported")
	}
}

// THE TITLE REACHES THE AGENT AND THE DESCRIPTION DELIBERATELY DOES NOT.
//
// A ticket's description is written for the SPECIFICATION AUTHOR — the real one
// on this board reads "This slice: ... Write a test for each of these and STOP".
// Passing that to a developer contradicts the rules in the same prompt telling it
// not to touch tests, and the delegated agent duly wrote tests plus a 243-line
// index.html nobody asked for. The tests are the specification; the developer
// needs the title for context and the tests for detail.
func TestDelegateScriptCarriesTheTitleNotTheSpecInstructions(t *testing.T) {
	sc := delegateScript(Ticket{
		Title:       "Create GET /api/tasks handlers",
		Description: "Write a test for each of these and STOP",
	}, "go test ./...", 900, false, "")

	if !strings.Contains(sc, "Create GET /api/tasks handlers") {
		t.Error("the prompt does not carry the ticket title")
	}
	if !strings.Contains(sc, "go test ./...") {
		t.Error("the prompt does not name the command that decides success")
	}
	if strings.Contains(sc, "Write a test for each of these") {
		t.Error("the specification author's instructions reached the developer")
	}
}

// AN EXTERNAL AGENT TALKS TO THE MODEL FROM INSIDE THE LEASE, which the native
// loop never does — blacksmith calls the model from the host and the sandbox only
// runs commands. Without the endpoint travelling with the lease the agent exits
// in seconds having done nothing: measured as four "delegated agent left the
// tests failing" outcomes fifteen seconds apart, on a run where it never started.
func TestDelegateEnvCarriesTheModelAndProxy(t *testing.T) {
	t.Setenv("AGENTS_DEV_ENGINE", "qwen-code")
	t.Setenv("AGENTS_LARGE_ENDPOINT", "http://192.168.58.1:8080/v1")
	t.Setenv("AGENTS_LARGE_MODEL", "qwen3-coder")
	t.Setenv("AGENTS_SANDBOX_PROXY", "http://egress-proxy:3128")

	env := withDelegateEnv(sandboxEnv())
	for k, want := range map[string]string{
		"OPENAI_BASE_URL": "http://192.168.58.1:8080/v1",
		"OPENAI_MODEL":    "qwen3-coder",
		"HTTPS_PROXY":     "http://egress-proxy:3128",
	} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}
	// The model is reached directly, not through the proxy — the proxy refuses
	// private addresses by design, which is the SSRF protection worth keeping.
	if !strings.Contains(env["NO_PROXY"], "192.168.58.1") {
		t.Errorf("the model host is not exempt from the proxy: %q", env["NO_PROXY"])
	}
	// The cache settings the native loop needs must survive.
	if env["GOCACHE"] == "" || env["HOME"] == "" {
		t.Error("adding the delegate environment dropped the repository's own")
	}
}

// With no engine configured nothing changes, so the native path cannot be
// affected by a half-configured delegate.
func TestNoDelegateMeansNoExtraEnvironment(t *testing.T) {
	t.Setenv("AGENTS_DEV_ENGINE", "")
	t.Setenv("AGENTS_LARGE_ENDPOINT", "http://192.168.58.1:8080/v1")
	env := withDelegateEnv(sandboxEnv())
	if _, ok := env["OPENAI_BASE_URL"]; ok {
		t.Error("the delegate environment leaked into a native run")
	}
}

// BUILD OUTPUT IS NOT A DELIVERABLE. The repository has no .gitignore, so an
// agent that compiles leaves a binary beside the source and a blanket add commits
// it — measured twice on one branch, "tracker" and "task-tracker" at 8 MB each.
func TestDelegateScriptDropsBuildOutput(t *testing.T) {
	sc := delegateScript(Ticket{Title: "x"}, "go test ./...", 900, false, "")
	if !strings.Contains(sc, "dropped build output") {
		t.Error("nothing removes compiled binaries before the commit")
	}
	// Source must survive the filter, or the change itself is discarded.
	for _, keep := range []string{"*.go", "*.mod", "*.sum"} {
		if !strings.Contains(sc, keep) {
			t.Errorf("%s is not protected from the filter", keep)
		}
	}
	// The drop has to happen BEFORE staging, or it commits them anyway.
	if strings.Index(sc, "dropped build output") > strings.Index(sc, "git add -A") {
		t.Error("build output is filtered after staging, which is too late")
	}
}

// EVERY COUNTER IN THIS SYSTEM IS BUILT ON TURNS, and a delegated developer makes
// none — it does its own inference inside the sandbox. So the busiest stage in the
// pipeline reads zero while it works, which was mistaken for a hang twice from
// outside: a GPU that looked idle, and a ticket sitting at "0 turns" for two
// minutes. The work is recorded as a turn so the activity line and the reasoning
// panel show it.
func TestDelegateTaskIsTheTitleNotTheDescription(t *testing.T) {
	got := delegateTaskOf(Ticket{
		Title:       "Create GET /api/tasks handlers",
		Description: "Write a test for each of these and STOP",
	})
	if !strings.Contains(got, "Create GET /api/tasks handlers") {
		t.Errorf("the task does not name the ticket: %q", got)
	}
	if strings.Contains(got, "STOP") {
		t.Errorf("the specification author's instructions leaked into it: %q", got)
	}
}

// TOLD ONLY THAT "THE TESTS ARE THE SPECIFICATION", the delegated agent read the
// README instead, implemented from that, and reported success. Its own words:
// "The tests in handlers_create_test.go are just placeholders and will be
// implemented once the handler is working properly." It had never opened them.
//
// An instruction whose consequences it cannot see is one it will rationalise
// around. The failing output is not arguable, so the suite runs first and its
// output goes into the prompt.
func TestDelegateScriptShowsTheAgentTheFailures(t *testing.T) {
	sc := delegateScript(Ticket{Title: "Create POST /api/tasks"}, "go test ./...", 900, false, "")

	if !strings.Contains(sc, "THESE TESTS ARE FAILING RIGHT NOW") {
		t.Error("the prompt does not carry the current failures")
	}
	// The suite must run BEFORE the prompt is assembled, or there is nothing to show.
	iRun := strings.Index(sc, "/tmp/failing.txt")
	iPrompt := strings.Index(sc, "qwen --prompt")
	if iRun < 0 || iPrompt < 0 || iRun > iPrompt {
		t.Errorf("tests are not run before the agent is prompted: run=%d prompt=%d", iRun, iPrompt)
	}
	// And the rules must close the two doors it walked through.
	for _, want := range []string{"not placeholders", "READ the failing test files", "the test wins"} {
		if !strings.Contains(sc, want) {
			t.Errorf("the rules do not say %q", want)
		}
	}
}

// THE PREAMBLE SETS -e AND THE TEST COMMAND IS EXPECTED TO FAIL — that is the
// entire reason for running it. Without a guard the script exits at that line,
// the agent never runs, and the stage fails in twenty seconds looking exactly
// like an agent that refused the work.
func TestDelegateScriptSurvivesTheExpectedTestFailure(t *testing.T) {
	sc := delegateScript(Ticket{Title: "x"}, "go test ./...", 900, false, "")
	i := strings.Index(sc, "/tmp/failing.txt")
	if i < 0 {
		t.Fatal("the suite is not run before the prompt")
	}
	line := sc[i:]
	if end := strings.Index(line, "\n"); end > 0 {
		line = line[:end]
	}
	if !strings.Contains(line, "|| true") {
		t.Errorf("an expected failure is unguarded under -e: %q", line)
	}
}

// THE AGENT DECIDES IT IS FINISHED; THE GATE DECIDES IF IT IS. Left alone it
// writes plausible code, reports "I have successfully implemented..." and pushes
// without re-running anything — measured: five minutes of work, a confident
// summary, and a suite still red. So it is asked again with the failures its own
// attempt left, which is the feedback the native loop gets for free by verifying
// after every write.
func TestDelegateRetriesWithTheFailuresItLeft(t *testing.T) {
	first := delegateScript(Ticket{Title: "x"}, "go test ./...", 900, false, "")
	again := delegateScript(Ticket{Title: "x"}, "go test ./...", 900, true, "")

	// EACH ATTEMPT IS ITS OWN EXECUTION. A shell loop inside one execution was
	// reaped out from under itself: forge advances last_used_at on SUBMISSION, not
	// while a container works, so a multi-minute run looks idle and is killed at
	// the 300s timeout mid-agent.
	if strings.Contains(first, "while [ $attempt") {
		t.Error("the retry loop is still inside a single execution")
	}
	// A retry must say what happened, or the agent has no reason to try anything
	// different from the attempt that just failed.
	if strings.Contains(first, "YOUR PREVIOUS ATTEMPT") {
		t.Error("the first attempt claims a previous one failed")
	}
	if !strings.Contains(again, "YOUR PREVIOUS ATTEMPT DID NOT WORK") {
		t.Error("a retry does not tell the agent its last attempt failed")
	}
	if !strings.Contains(again, "Do not repeat the same change") {
		t.Error("nothing discourages re-sending the same edit")
	}
	// Green means stop: the agent is not invited to polish working code.
	if !strings.Contains(first, "tests already pass") {
		t.Error("a passing suite does not short-circuit the attempt")
	}
	// Each attempt pushes, so the next attempt's checkout inherits its work.
	if strings.Index(first, "git push") < strings.Index(first, "qwen --prompt") {
		t.Error("the attempt pushes before the agent has run")
	}
}

// AN EXECUTION HAS A DEADLINE AND THE RETRY LOOP MUST FIT INSIDE IT. Three
// unbounded attempts ran past the 900-second limit, so the delegate execution was
// still holding the lease when verification tried to submit — "already claimed",
// reported as a failure by a stage that had actually done its work.
func TestDelegateAttemptsFitTheExecutionBudget(t *testing.T) {
	// BOUNDED BY THE IDLE TIMEOUT, NOT THE EXECUTION BUDGET. forge reaps a lease
	// 300s after its last submission whether or not a container is working, so an
	// attempt longer than that kills the sandbox it runs in — measured four times
	// as "already claimed" from stages that had done their work.
	// BOUNDED BY THE EXECUTION BUDGET, not by forge's idle timeout — that limit was
	// removed by fixing the reaper, and leaving the bound at 210s then became the
	// thing that killed the work: "Operation cancelled." mid-edit, three attempts
	// running, on a section the agent might have finished.
	per := perAttemptSeconds(900)
	if per >= 900 {
		t.Errorf("an attempt may run %ds, past its own execution timeout", per)
	}
	if 900-per < 100 {
		t.Errorf("only %ds left for the test run, commit and push", 900-per)
	}
	// And it must be long enough to be worth having: the successful sections took
	// 80 seconds, the hard ones considerably more.
	if per < 600 {
		t.Errorf("an attempt gets only %ds, which cut real work short before", per)
	}
	// A tiny or missing budget must still yield something an agent can use.
	if perAttemptSeconds(0) < 60 || perAttemptSeconds(60) < 60 {
		t.Error("a small budget produced an unusable per-attempt limit")
	}
	// And the bound has to actually reach the script.
	sc := delegateScript(Ticket{Title: "x"}, "go test ./...", 900, false, "")
	if !strings.Contains(sc, "timeout ") {
		t.Error("the agent invocation is unbounded")
	}
}

// QWEN-CODE ASSUMES THE STOCK 256K WINDOW and builds prompts to suit, so against
// a 131,072-token slot it composed a 67,546-token request the server refused with
// a 400 — an attempt lost to arithmetic rather than to the work. It has to be told
// the real size, and told to compact before it reaches it.
func TestDelegateTellsTheAgentItsRealContextWindow(t *testing.T) {
	sc := delegateScript(Ticket{Title: "x"}, "go test ./...", 900, false, "")

	if !strings.Contains(sc, "contextWindowSize") {
		t.Fatal("the agent is never told the window size")
	}
	// ONE KEY, NOT TWO. The modelProviders form requires an "id" field, and writing
	// it without one made qwen-code refuse to start — "Model config in authType
	// 'openai' missing required field: id", reported as "produced no change"
	// because a crashed agent leaves no diff.
	// The JSON KEY, not the word — the comment above it in the script explains why
	// the form was removed and would otherwise match.
	if strings.Contains(sc, `"modelProviders"`) {
		t.Error("the strict provider form is back; it needs an id and breaks startup without one")
	}
	if !strings.Contains(sc, "chatCompression") {
		t.Error("compaction is not configured, so a long attempt grows until it is refused")
	}
	// The settings must be written BEFORE the agent runs, or they cannot apply.
	if strings.Index(sc, "settings.json") > strings.Index(sc, "qwen --prompt") {
		t.Error("settings are written after the agent has already started")
	}
	// UNDER the serving slot, not equal to it: the server counts the completion it
	// is about to generate, so an agent filling the window exactly is refused.
	if delegateContextTokens >= 131072 {
		t.Errorf("the agent is told it has %d tokens, leaving no room for a reply", delegateContextTokens)
	}
}

// The failing-test NAMES must be complete even though the detail is bounded.
//
// Measured on r70: testify writes an Error Trace, an expected/actual pair and a
// unified diff per assertion, so one failing subtest filled 45 lines and the
// 60-line window the agent was given ended inside it. The second failing test was
// never shown at all — and the agent, working from what looked like the whole
// report, spent three attempts and the ticket's entire budget on the half it
// could see, then blamed "routing issues" in its summary.
func TestDelegateScriptNamesEveryFailingTest(t *testing.T) {
	sc := delegateScript(Ticket{Title: "Create PUT /api/tasks/{id}"}, "go test ./...", 900, true, "")

	if !strings.Contains(sc, "FAILING TESTS:") || !strings.Contains(sc, `grep -E "^\s*--- FAIL"`) {
		t.Error("the prompt does not list the failing tests by name; a truncated detail window is the agent's only view")
	}
	// The names come BEFORE the detail, so truncation can never reach them.
	iNames := strings.Index(sc, "FAILING TESTS:")
	iDetail := strings.Index(sc, "DETAIL:")
	if iNames < 0 || iDetail < 0 || iNames > iDetail {
		t.Errorf("names must precede the bounded detail: names=%d detail=%d", iNames, iDetail)
	}
	if strings.Contains(sc, "head -60 /tmp/failing.txt") {
		t.Error("the 60-line window is back; it cut a 79-line report and hid a whole failing test")
	}
}

// An editor's backup copy is not a deliverable either.
//
// Measured on r70: the agent wrote main.go.backup before editing and committed
// it. Being text, it passes the binary check that catches compiled output, and
// being unbuilt it trips no gate — it simply stays on the branch, where the next
// agent reads a stale copy of main.go as though it were live.
func TestDelegateScriptDropsEditorLeftovers(t *testing.T) {
	sc := delegateScript(Ticket{Title: "x"}, "go test ./...", 900, false, "")
	for _, pat := range []string{"*.backup", "*.bak", "*.orig"} {
		if !strings.Contains(sc, pat) {
			t.Errorf("%s is not dropped before staging; it commits alongside the real source", pat)
		}
	}
}

// The retry must show the agent its own last diff.
//
// Each attempt is a fresh process with no memory, so "do not repeat the same
// change" asks it to obey a fact it cannot check. Measured on r70: four attempts
// on one ticket, each re-deriving the same wrong theory from the same failure and
// writing the same code, each ending with a confident summary blaming "routing
// issues" — while the defect was six lines away in Add(), which never assigned an
// ID, so the test's URL carried an empty one.
func TestDelegateRetryShowsThePreviousDiff(t *testing.T) {
	retry := delegateScript(Ticket{Title: "Create PUT /api/tasks/{id}"}, "go test ./...", 900, true, "")
	if !strings.Contains(retry, "git diff HEAD~1") {
		t.Error("the retry does not show the agent what it already tried")
	}
	// Tests excluded: the developer cannot change them, so a diff of them is noise
	// at best and an invitation at worst.
	if !strings.Contains(retry, "':!*_test.go'") {
		t.Error("the diff includes test files; the developer may not change those")
	}
}

// A killed attempt must still report what it had done.
//
// Measured on r70: exit 15 — SIGTERM — 163 seconds in, with stdout and stderr
// both empty, no reap logged and the execution's own 900-second timeout nowhere
// near expiry. The signal killed the shell before the line that flushes the
// agent's log, so the only record of a three-minute attempt was its exit code.
func TestDelegateScriptReportsWhenItIsKilled(t *testing.T) {
	sc := delegateScript(Ticket{Title: "x"}, "go test ./...", 900, false, "")
	if !strings.Contains(sc, "trap ") || !strings.Contains(sc, "SIGTERM") {
		t.Error("a killed attempt leaves no evidence at all")
	}
	// The agent's own log has to be flushed by the trap; that is the whole point.
	i := strings.Index(sc, "SIGTERM after")
	if i < 0 || !strings.Contains(sc[i:i+200], "tail -20 /tmp/agent.log") {
		t.Error("the trap does not flush what the agent had managed to write")
	}
}

// The prompt must rule out the explanation the agent kept reaching for — but only
// on evidence from the tests themselves.
//
// Measured on r70: four attempts, every one blaming routing, path extraction or
// "how the handlers interact with the testing framework", ending in a claim that
// the ticket was beyond what it could determine. Those tests call the handler as
// a plain function against an httptest recorder; no router exists to be wrong.
func TestDelegateScriptRulesOutTheEnvironmentExcuse(t *testing.T) {
	sc := delegateScript(Ticket{Title: "x"}, "go test ./...", 900, false, "")
	if !strings.Contains(sc, "httptest.NewRecorder") {
		t.Error("nothing checks whether the tests call the code directly")
	}
	// Gated on the check, not asserted unconditionally: a suite that really does
	// stand up a server must not be told routing cannot matter.
	if !strings.Contains(sc, "httptest.NewServer|ListenAndServe") {
		t.Error("the note is not gated; a suite with a real server would be told a falsehood")
	}
	if !strings.Contains(sc, "cannot be caused by routing") {
		t.Error("the note does not close off the wrong explanation")
	}
}

// A regression must be named as a regression, not mixed in with the ticket's work.
//
// Measured on r70: an agent adding validation broke TestCreateTaskHandler, saw it
// in one undifferentiated list of failures, and spent its attempts theorising
// about 301 redirects in a test it had never been asked to touch. "Your work is
// not finished" and "your work broke something that worked" call for opposite
// next moves.
func TestDelegateSeparatesRegressionsFromTheTicketsRedState(t *testing.T) {
	sc := delegateScript(Ticket{Title: "x"}, "go test ./...", 900, true, "TestA TestB")

	if !strings.Contains(sc, "YOU BROKE THESE") {
		t.Error("a regression is not called out as one")
	}
	// comm needs both sides sorted; an unsorted baseline silently reports garbage.
	if !strings.Contains(sc, "comm -13 /tmp/baseline-names.txt /tmp/failing-names.txt") {
		t.Error("regressions are not computed as failing-now minus failing-before")
	}
	if !strings.Contains(sc, "sort -u > /tmp/baseline-names.txt") {
		t.Error("the baseline is not sorted; comm would misreport")
	}
	// The baseline has to leave the sandbox: each attempt is a fresh execution
	// against a reset tree, so nothing persists in there between attempts.
	if !strings.Contains(sc, "===FAILING-AT-START") {
		t.Error("the red state is never published, so the next attempt cannot know it")
	}
}

// The first attempt has no baseline and must not claim regressions from nothing.
func TestDelegateFirstAttemptHasNoBaseline(t *testing.T) {
	first := delegateScript(Ticket{Title: "x"}, "go test ./...", 900, false, "")
	// The guard is `[ -s ... ]`: an empty baseline file means the block is skipped
	// entirely rather than reporting every failing test as a regression.
	if !strings.Contains(first, "if [ -s /tmp/baseline-names.txt ]; then") {
		t.Error("nothing guards the empty baseline; every red test would read as a regression")
	}
}

func TestFailingAtStartReadsThePublishedRedState(t *testing.T) {
	got := failingAtStart("some noise\n===FAILING-AT-START TestA TestB/sub \nmore noise\n")
	if got != "TestA TestB/sub" {
		t.Errorf("failingAtStart() = %q, want the published list", got)
	}
	if got := failingAtStart("nothing here"); got != "" {
		t.Errorf("failingAtStart() = %q, want empty when absent", got)
	}
}

// Two attempts must fit inside the lease they share.
//
// Every attempt on a ticket runs in ONE held lease, whose lifetime forge caps at
// 3600s. Sizing the per-attempt budget without that ceiling in mind is how the
// third attempt of a previous arrangement would have been killed by the lease
// rather than by its own timeout — a failure that reads like the agent giving up.
func TestDelegateAttemptsFitTheLeaseLifetime(t *testing.T) {
	const leaseLifetime = 3600
	const configured = 1650 // AGENTS_REPO_TIMEOUT_SECONDS

	total := perAttemptSeconds(configured) * int64(maxDelegateAttempts)
	if total >= leaseLifetime {
		t.Errorf("%d attempts x %ds = %ds, which does not fit a %ds lease: the last attempt dies with the sandbox",
			maxDelegateAttempts, perAttemptSeconds(configured), total, leaseLifetime)
	}
	// And it must leave room for the verification and push that share the lease.
	if leaseLifetime-total < 300 {
		t.Errorf("only %ds spare in the lease; the verify and push after the last attempt need room", leaseLifetime-total)
	}
}

// A section whose tests already pass must succeed, not fail.
//
// Sections overlap, so the implementation merged for one can already satisfy the
// next one's tests. The delegate script reports "tests already pass" and rightly
// does not run the agent — so nothing changes, and reading only "the agent changed
// nothing" turned that into a failure. Measured on a clean run: four such failures
// eleven seconds apart, taking the board from one merged ticket to seven blocked
// in forty-four seconds.
func TestDelegateDoesNotFailWhenTheTestsAlreadyPass(t *testing.T) {
	green := "===FAILING-AT-START \n--- tests already pass ---\n--- the agent changed nothing ---"
	if delegateProducedNothing(green) {
		t.Error("an already-green branch is reported as producing no change; the ticket is failed and blocked")
	}
	// A genuine no-op — the agent ran and wrote nothing — must still fail.
	idle := "--- the agent changed nothing ---"
	if !delegateProducedNothing(idle) {
		t.Error("an agent that ran and changed nothing is no longer detected")
	}
}

// The referee's threshold must follow the attempt ceiling it is measured against.
//
// failedVerifications counts within one stage run, so a fixed 3 was unreachable
// once the delegate got 2 attempts — measured on a clean run: six consecutive dev
// failures on one ticket and not one verdict ever asked for. A threshold that
// silently switches a whole mechanism off when an unrelated constant moves is the
// kind of coupling that has to be pinned.
func TestRefereeThresholdFollowsTheDelegateCeiling(t *testing.T) {
	delegated := &DevAgent{delegate: delegateEngineQwen}
	if got := delegated.refereeThreshold(); got > maxDelegateAttempts {
		t.Errorf("refereeThreshold() = %d with a ceiling of %d: the referee can never be asked",
			got, maxDelegateAttempts)
	}
	native := &DevAgent{}
	if got := native.refereeThreshold(); got != minTriesBeforeReferee {
		t.Errorf("refereeThreshold() = %d for the native loop, want %d", got, minTriesBeforeReferee)
	}
}

// The empty-value hint must fire on the failure that actually beat the agent.
//
// "Expected first task to be id task-0, got " — an ABSENT value, not a wrong one,
// which is only ever written where the value is created. The agent read that line
// six times and inspected the reader every time.
func TestDelegateHintsAtTheConstructorWhenAValueIsAbsent(t *testing.T) {
	sc := delegateScript(Ticket{Title: "x"}, "go test ./...", 900, false, "")
	if !strings.Contains(sc, "READ THIS BEFORE ANYTHING ELSE:") {
		t.Error("nothing points the agent at the constructor when a value is absent")
	}
	if !strings.Contains(sc, "look at where the value is CREATED") {
		t.Error("the hint does not name where the fault must be")
	}
	// Narrow on purpose: expected-something-got-nothing. A merely wrong value must
	// not trigger it, or the hint becomes noise on every failing assertion.
	if !strings.Contains(sc, `got $`) {
		t.Error("the match is not anchored on an empty actual value")
	}
}

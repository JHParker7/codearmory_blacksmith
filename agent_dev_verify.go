// Running the checks and reporting what they said.
//
// Verification is the only thing that decides whether a stage succeeded, so it is
// deliberately separate from the loop that calls it: what "passing" means must not
// depend on which agent asked.

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// verify pushes the current state to the branch and runs the repository's tests
// against what was pushed — through the platform's pipeline when there is one,
// and in the sandbox directly when there is not.
//
// The agent does not decide what "tested" means in either mode: it pushes, some
// definition it did not write runs, and it reads the verdict. With a pipeline
// that definition is CI's own, which is why PipelineID wins whenever it is set.
// Without one it is the operator's TestCommand — weaker, because it can drift
// from CI, but it keeps the loop closed on a host that cannot reach a pipeline.
//
// Both modes verify the PUSHED BRANCH rather than the agent's local state, so
// neither can pass on work that did not survive the commit and its hooks.
func (a *DevAgent) verify(ctx context.Context, rec *Recorder, t Ticket, s *devState) (string, bool, error) {
	if len(s.staged) == 0 {
		// A RETRY INHERITS A BRANCH, and refusing to test it is the same trap as
		// refusing to test a tree with nothing to commit — one layer up.
		//
		// staged holds what THIS attempt has written, so it is empty at the start of
		// every attempt after the first. The branch, meanwhile, already carries the
		// previous attempts' commits. An agent that opens by asking where it stands
		// was told "you have not changed anything yet" and made to write something
		// before it could find out — and writing what the branch already held was
		// then refused as a no-op. Measured on r62: 62 refusals on one section, all
		// of them this message, across four attempts.
		//
		// So when this attempt has no verdict yet and the branch has files, the
		// tests run against it as it stands. That is a real verdict on real code.
		//
		// The original guard survives for the case it was written for: nothing
		// staged, nothing on the branch, and nothing tested is genuinely not a
		// verdict, and recording it as one poisoned lastTest and refused every
		// later run.
		if s.nothingToTest() {
			return "", false, errNothingToTest
		}
	}
	branch := a.branchFor(t)
	if err := a.push(ctx, rec, t, s, branch); err != nil {
		// NOTHING TO COMMIT IS NOT A REASON TO SKIP THE TESTS, and treating it as
		// one was a trap with no exit.
		//
		// It means the tree the agent wants is ALREADY on the branch — which on a
		// retry is the ordinary case, because an earlier attempt at this ticket
		// committed it. Returning here meant such an attempt could never obtain a
		// test result at all: it could not commit, so it could not test, so it
		// could not finish. It rewrote the same file, was told that changed
		// nothing, rewrote it again, and was killed at the refusal ceiling.
		// Measured on r60: 82 of 89 refusals, every one of the four attempts, and
		// the suite never ran once.
		//
		// A tree identical to the branch is still a tree worth testing. So the push
		// simply had nothing to add, and verification carries on — ONCE. After this
		// attempt has a result for the tree, a further no-op adds nothing and would
		// only buy another clone and another full suite, so it goes back to the
		// caller as the no-op it is, where the advice can finally be specific: the
		// tests ran, and they either passed or they did not.
		if !errors.Is(err, errNothingToCommit) {
			return "", false, fmt.Errorf("%w: %v", errPushFailed, err)
		}
		if s.lastTest != "" {
			return "", false, err
		}
	}
	// THE TEST AUTHOR IS HELD TO A DIFFERENT GATE, because the usual one cannot
	// pass for it. It writes tests against an implementation that does not exist
	// yet, so `go test` fails on undefined symbols by construction — that is what
	// red means in test-first, not a defect. Judging it by the suite would leave
	// it iterating forever on a failure it is not allowed to fix, since it may not
	// write the implementation.
	//
	// What CAN be checked is that the tests are well-formed Go, which is a syntax
	// question and needs no symbols to answer. Anything beyond that is the
	// developer's gate, one stage later, and it is a real gate precisely because
	// these tests are sitting there waiting for it.
	if a.mode == modeTest || a.mode == modeSpecMerge {
		return a.verifyTestsParse(ctx, rec, t, s, branch)
	}
	if a.mode == modeCoverage {
		return a.verifyCoverage(ctx, rec, s, branch)
	}
	if a.repo.PipelineID == "" {
		return a.verifyInSandbox(ctx, rec, s, branch)
	}

	run, err := a.api.TriggerRun(ctx, a.repo.PipelineID, map[string]string{
		a.branchInput(): branch,
		"ticket_id":     t.TicketID,
	})
	if err != nil {
		return "", false, fmt.Errorf("trigger pipeline: %w", err)
	}
	rec.Action(ctx, "pipeline", "triggered "+run.RunID+" on "+branch, nil)

	final, err := a.api.WaitForRun(ctx, run.RunID)
	if err != nil {
		return "", false, err
	}
	s.lastRun = final.RunID
	rec.Action(ctx, "pipeline", fmt.Sprintf("run %s → %s", final.RunID, final.Status), nil)

	switch {
	case final.Passed():
		return fmt.Sprintf("Pipeline run %s passed on branch %s.", final.RunID, branch), true, nil
	case final.Status == RunAwaitingApproval:
		// A human gate is not the agent's to open. Report and stop rather than
		// blocking a slot for as long as the person takes.
		return fmt.Sprintf("Pipeline run %s is waiting for approval; a person must decide. Stop here.", final.RunID), false, nil
	default:
		return fmt.Sprintf("Pipeline run %s %s at step %d. Fix the cause and run the tests again.",
			final.RunID, final.Status, final.CurrentStep), false, nil
	}
}

// verifyInSandbox runs TestCommand against the pushed branch, with no platform
// involved.
//
// It clones the branch fresh rather than reusing the push sandbox, for the same
// reason CI does not test a developer's working tree: what is verified must be
// what was actually committed and pushed, not what the agent believed it wrote.
// An uncommitted file, a hook that rewrote content, a .gitignore that swallowed
// a new file — each of those is a real failure, and each is invisible to a check
// that runs against the state the agent already has in hand.
//
// A non-zero exit is a NORMAL result, reported back to the model as the failure
// to fix. Only a sandbox that could not run at all is an error.
func (a *DevAgent) verifyInSandbox(ctx context.Context, rec *Recorder, s *devState, branch string) (string, bool, error) {
	res, err := s.sb.RunOnBranch(ctx, rec, branch, a.verifyScript())
	if err != nil {
		return "", false, fmt.Errorf("verify on %s: %w", branch, err)
	}
	advice, out := splitAdvice(stripToolChatter(res.Stdout + res.Stderr))
	gate := failedGate(out)
	rec.Action(ctx, "verify", fmt.Sprintf("%s on %s → exit %d", gate, branch, res.ExitCode), nil)

	if res.OK() {
		msg := fmt.Sprintf("Tests passed on branch %s.", branch)
		if advice != "" {
			// Offered, not demanded — and said plainly, because a model told about
			// findings without being told they are optional will spend iterations
			// it does not have chasing them, which is the trap this design avoids.
			msg += "\n\nThe linter also reported the following. These do NOT block you and the" +
				" branch is already acceptable: fix any that are quick and obviously right," +
				" ignore the rest, and finish either way.\n\n" + clip(advice, maxAdviceRunes)
		}
		return msg, true, nil
	}
	// EVERY RED COUNTS, whatever kind it is — this is what tells the referee the
	// developer has genuinely been trying, as opposed to meeting the expected red
	// of a ticket whose implementation is not written yet.
	s.failedVerifications++
	// NAME THE GATE. "the tests failed" when it was the linter sends the model
	// looking in the wrong place, and it has few enough iterations that one wasted
	// on a misattributed failure is expensive. The output itself is reported rather
	// than summarised: a compiler error nobody can read is a wasted iteration too.
	// Both ends: the compile errors at the top and the failing assertions at the
	// bottom. See clipEnds — head-only truncation was hiding the assertion message
	// behind the module downloads that precede it.
	// A SPEC THAT DOES NOT COMPILE IS NOT THE DEVELOPER'S TO FIX — but WHERE an
	// error is reported is not where the fault is, and this used to conclude
	// otherwise on the first try.
	//
	// Go reports a type mismatch at the USE SITE, and in test-first the use site
	// is always a test file. Measured: a ticket blocked three times on
	//
	//	./store_test.go:27:18: cannot use time.Now().Add(time.Hour)
	//	    (value of struct type time.Time) as *time.Time value in struct literal
	//
	// where the fault was the DEVELOPER's own store.go declaring DueDate as
	// *time.Time. One line of the implementation fixes it, and the ticket was
	// abandoned before the agent got a second turn. The same applies to
	// "undefined: NewStore", which is the expected red state of every test-first
	// ticket and is cleared by writing the code.
	//
	// So an unfixable spec is now proved by ATTEMPTS rather than asserted from a
	// filename. The streak advances only when the developer has changed something
	// since the last verification — otherwise repeat verifications, which are no
	// longer refused, would run it up without a single edit being tried.
	//
	// What genuinely cannot be fixed from the implementation side is a test file
	// that will not PARSE, and the spec author's own gate now guarantees it does.
	// That is why this can afford to be slow: the case it protects against is
	// rare, and the case it was misfiring on is every ticket.
	if files, allTests := compileErrorFiles(out); allTests && a.mode == modeDevelop {
		s.lastCompileBroke = strings.Join(files, ", ")
		if s.writes > s.specBrokenWrites {
			s.specBrokenTries++
			s.specBrokenWrites = s.writes
		}
		// Only "undefined:" left? That is the expected red state, not a fault.
		s.redIsExpected = len(nonUndefinedCompileErrors(out)) == 0
		// A fault local to the test file is conclusive on the FIRST verification:
		// there is no implementation that could change it, so there is nothing for a
		// streak to establish.
		//
		// THE STREAK ROUTES ARE GATED ON THE RED NOT BEING THE EXPECTED ONE, and
		// leaving that off the streak while adding it to the refusal route only
		// half-fixed the false positive. "undefined: NewStore" persists across every
		// verification until the implementation is finished, so a developer three
		// writes into a large ticket trips minSpecBrokenTries on a specification that
		// is perfectly sound. Measured on the implementation fixture: a healthy spec
		// handed back after the refusal route had already been gated.
		//
		// intrinsicTestErrors stays ungated because it cannot fire on expected red by
		// construction: it matches faults local to the file, and "undefined:" is not
		// one of them.
		if len(intrinsicTestErrors(out)) > 0 ||
			(!s.redIsExpected &&
				(s.specBrokenTries >= minSpecBrokenTries || s.testEditRefusals >= maxTestEditRefusals)) {
			s.specBroken = strings.Join(files, ", ")
		}
	} else if panics := panicOnlyInTests(out); len(panics) > 0 && a.mode == modeDevelop {
		// A PANIC RAISED ENTIRELY INSIDE THE TESTS is conclusive on the first
		// verification, for the same reason a fault local to the test file is: no
		// edit to the implementation can reach it. It is deliberately NOT gated on
		// specBrokenTries or on redIsExpected — both exist to stop a healthy spec
		// being blamed for the expected red of test-first, and expected red is a
		// compile failure on undefined symbols, never a panic in the test's own
		// fixture setup.
		s.specBroken = strings.Join(panics, ", ")
		s.lastCompileBroke = ""
		s.redIsExpected = false
	} else if clash := packageConflictFiles(out); len(clash) > 0 && a.mode == modeDevelop {
		// TWO PACKAGES IN ONE DIRECTORY, which is the specification's to fix and
		// nobody else's. The name is set by a test file this stage may not edit, so
		// every implementation file beside it must match a declaration the developer
		// cannot change — there is no edit that compiles.
		//
		// Ungated, like the panic route: it cannot fire on the expected red of
		// test-first, because expected red is undefined symbols and this is the
		// package clause. Waiting for a streak here only buys more turns of the
		// same impossible move.
		s.specBroken = strings.Join(clash, ", ")
		s.lastCompileBroke = strings.Join(clash, ", ")
		s.redIsExpected = false
	} else if v := a.refereeOn(ctx, rec, s, out); v.blames(ownerSpec) {
		// THE AMBIGUOUS MIDDLE, where the mechanical routes have nothing to match.
		// A test that compiles, does not panic, and simply asserts against state the
		// implementation cannot reach has no textual signature — it is a judgement
		// about the tests and the code read together. Measured: 25 verification runs
		// spent on a test that populated a LOCAL store and called a handler reading
		// the package-level one.
		s.specBroken = v.Reason
		s.lastCompileBroke = ""
		s.redIsExpected = false
	} else {
		// Any verification that is not test-file-only compile errors clears it:
		// the developer is making progress on a different failure.
		s.specBrokenTries, s.specBroken, s.lastCompileBroke = 0, "", ""
		// Cleared with the rest: it is only meaningful alongside a compile result,
		// and leaving it stale is what let a runtime panic reach the evidence gate.
		s.redIsExpected = false
		// The suspect is cleared with the evidence; keeping it would let a later,
		// unrelated refusal inherit a filename from a failure already resolved.
		s.lastTestEditTarget = ""
	}
	msg := fmt.Sprintf("The %s gate failed on branch %s (exit %d). Fix this before anything else:\n%s",
		gate, branch, res.ExitCode, clipEnds(out, maxFileForModel/3, 2*maxFileForModel/3))
	if gate == "critical-security" {
		// Say what kind of failure this is. A model that reads a scanner's output
		// without being told it is a security gate treats it as one more lint
		// complaint and works around the symptom — deleting the call the scanner
		// flagged rather than removing the credential it found.
		msg += "\n\nThis is a SECURITY finding above the severity threshold, not a style issue." +
			" It must be fixed in the code, not suppressed or worked around, and no change ships carrying it."
	}
	if advice != "" {
		msg += "\n\nSeparately, the linter reported the following. It does not block; the failure above is what matters.\n\n" + clip(advice, maxAdviceRunes)
	}
	return msg, false, nil
}

// stripToolChatter removes progress lines the toolchain prints that say nothing
// about the change under test.
//
// `go: downloading example.com/x v1.2.3` can be dozens of lines on a first run,
// and every one of them competes for the model's context with the failure it is
// supposed to be reading. It is also the reason head-truncation hurt so much:
// the noise sits at the TOP, so a clipped log could be almost entirely downloads.
//
// Only the succeeded-quietly lines go. A download that FAILS reads differently
// ("go: module ...: reading ...: 404") and is kept, because then the network is
// the bug and hiding it would send the developer hunting through its own code
// for something that is not there.
func stripToolChatter(out string) string {
	lines := strings.Split(out, "\n")
	kept := make([]string, 0, len(lines))
	for i, l := range lines {
		if strings.HasPrefix(l, "go: downloading ") || strings.HasPrefix(l, "go: extracting ") {
			continue
		}
		// A PANIC BURIES ITS OWN CAUSE. When a test panics — and a spec that
		// asserts a length and then indexes the result panics whenever the length
		// is wrong — Go prints the message, then twenty-odd frames of runtime and
		// testing internals. The frames from the standard library say nothing about
		// the change under test: testing.tRunner and runtime.panic are the same on
		// every failure there has ever been. They are dropped, and the frames in the
		// repository's own files are kept, because those name the line to look at.
		//
		// Measured: the assertion "should have 2 item(s), but has 0" was line two of
		// the output and everything after it was stack. That one line is the whole
		// diagnosis.
		if isStdlibFrame(l) || (i > 0 && isStdlibFrame(lines[i-1]) && strings.HasPrefix(l, "\t")) {
			continue
		}
		kept = append(kept, l)
	}
	return strings.Join(kept, "\n")
}

const gateMarker = "HARNESS_GATE_FAILED="

// testGateScript runs the test command and, when it fails, QUOTES THE SOURCE the
// failure points at.
//
// A failing assertion names a file and a line — "task_store_test.go:155" — and
// nothing else about it. The developer's next move is therefore always the same:
// spend a turn on read_files to see what line 155 actually asserts. That is a
// whole model round trip, on every failure, to fetch five lines the sandbox is
// already standing next to. Printing them here costs nothing and removes the
// round trip entirely.
//
// The test file is the SPECIFICATION and the developer may not edit it, so this
// is the one thing it most needs to see and the one thing it cannot change.
//
// Bounded on purpose: at most a handful of distinct locations, a few lines each.
// A run with forty failures should not paste the whole suite into the prompt —
// the point is to answer "what does the failing line say", not to mirror the
// repository.
func testGateScript(testCommand string) string {
	// BRACED, BECAUSE THE COMMAND MAY BE A CHAIN. A redirect binds to the last
	// element of an && chain, not to the whole of it — so with
	// "go build ./... && go test ./..." the build's output escaped to the console
	// and, when the build failed, the test never ran and never created the file
	// the branches below cat. The agent was handed "cat: /tmp/.gate-out: No such
	// file or directory" above its real error, which is a refusal pointing at
	// nothing.
	//
	// Grouping captures the whole chain, so the build error lands in the file
	// where the source-quoting grep can find the file and line it names.
	return fmt.Sprintf(`if { %s ; } > /tmp/.gate-out 2>&1; then
  cat /tmp/.gate-out
else
  cat /tmp/.gate-out
  echo '--- source of the failing assertions (you may READ these; you may not edit them) ---'
  grep -oE '[A-Za-z0-9_./-]+\.go:[0-9]+' /tmp/.gate-out | sort -u | head -%d | while IFS=: read -r f l; do
    [ -f "$f" ] || continue
    s=$((l-%d)); [ "$s" -lt 1 ] && s=1
    echo "=== $f around line $l ==="
    awk -v a="$s" -v b="$((l+%d))" 'NR>=a && NR<=b { printf "%%6d\t%%s\n", NR, $0 }' "$f"
  done
  echo '%s%s'
  exit 1
fi
`, testCommand, maxFailureSites, failureContextBefore, failureContextAfter, gateMarker, "test")
}

// Bounds for the quoted source above.
const (
	maxFailureSites      = 6
	failureContextBefore = 6
	failureContextAfter  = 4
)

// verifyScript clones the pushed branch and runs the gates over it, in order.
//
// CHEAPEST AND MOST PRECISE FIRST, stopping at the first failure. A linter knows
// the file and the line; a test runner reports the same fault as a wall of output
// with the cause buried in it. Running both and reporting everything would hand
// the model a linter complaint and a hundred lines of test noise for one mistake,
// and it has eight iterations to spend.
//
// One sandbox, not one per gate: the clone and the toolchain warm-up dominate,
// and paying them three times to run three commands would make the fast feedback
// this exists for slower than what it replaced.
func (a *DevAgent) verifyScript() string {
	// The branch is already checked out: RunOnBranch fetched it from the remote,
	// so these gates read what was PUSHED rather than the tree the agent has been
	// editing. That distinction is the whole point of verifying here.
	script := ""
	// The linter runs FIRST and cannot fail the run: `|| true`. It is here for the
	// fast, precise feedback — a file and a line, before the test runner buries the
	// same fault in output — not to decide anything.
	if a.repo.LintCommand != "" {
		script += fmt.Sprintf("echo '%s'\n%s 2>&1 || true\necho '%s'\n",
			adviceOpen, a.repo.LintCommand, adviceClose)
	}
	// BEFORE the tests, because a production file that imports a testing package
	// makes every later result meaningless — the thing under test is no longer the
	// thing that ships.
	script += testImportGateScript()
	if a.repo.TestCommand != "" {
		script += testGateScript(a.repo.TestCommand)
	}
	// AFTER the tests, deliberately. A change that does not build or pass cannot be
	// meaningfully analysed — most scanners need the code to compile — and a broken
	// change should hear about being broken first.
	if a.repo.CriticalCommand != "" {
		script += fmt.Sprintf("echo '--- critical analysis ---'\n%s || { echo '%s%s'; exit 1; }\n",
			a.repo.CriticalCommand, gateMarker, "critical-security")
	}
	script += a.dependencyRegressionScript()
	return script
}

// testImportGateScript rejects a production file that imports a testing package.
//
// MEASURED ON R70, and it is the most expensive single thing a delegated agent
// did all run. Asked to add validation, it wrote this into main.go:
//
//	func createTask(w *httptest.ResponseRecorder, r *http.Request) {
//		// Create a temporary store for test purposes
//		testStore := NewStore()
//		...
//		testStore.Add(&task)
//
// A production handler taking an httptest recorder, adding the task to a store it
// invented, so the store the caller passed in never saw it. The suite went red,
// the agent called the failure "an infrastructure issue with URL routing causing
// 301 redirects", and burned its attempts there.
//
// It is worth a GATE rather than advice because the rule is absolute in Go —
// `testing` and `httptest` have no legitimate place in a non-test file — so it can
// never produce a false accusation, and because it names the cause exactly. The
// agent could not find this fault by reading assertion output: every failing
// assertion pointed at the test, and the test was right.
//
// Guarded on go.mod so a repository in another language simply skips it.
// testImportOffenders lists the non-test Go files that import a testing package,
// leaving the list in $bad. Shared by the GATE, which fails the branch on it, and
// by the delegate's own prompt, which has to SHOW the agent the same thing.
//
// One definition, because the two were briefly separate and that is a bug with a
// long fuse: the gate rejected the branch while the agent's prompt — built from
// its own `go test` run, which compiles such a file happily — never mentioned it.
// The developer would have been failed for a reason it was never told.
func testImportOffenders() string {
	return `
prod=$(ls *.go 2>/dev/null | grep -v '_test\.go$' || true)
bad=""
if [ -f go.mod ] && [ -n "$prod" ]; then
  bad=$(grep -lE '"testing"|"net/http/httptest"' $prod 2>/dev/null || true)
fi
`
}

// testImportAdvice is what the offender is told, in both places.
func testImportAdvice() string {
	return `echo "These files are NOT tests and must not import a testing package:"
      echo "$bad"
      echo
      echo "A testing package in shipped code means the code was written to satisfy the"
      echo "test rather than to do the job: a handler taking an httptest recorder, or one"
      echo "that builds its own store instead of using the one it is given. Use the types"
      echo "and the values the caller passes in. Remove the import and make the real"
      echo "parameters carry the work."`
}

func testImportGateScript() string {
	return fmt.Sprintf(`
echo '--- test-only imports in production code ---'
%s
if [ -n "$bad" ]; then
      %s
      echo '%s%s'
      exit 1
fi
echo 'no testing packages in production files'
`, testImportOffenders(), testImportAdvice(), gateMarker, "test-imports-in-production")
}

// verifyTestsParse is the test author's gate: the tests must be well-formed Go.
//
// gofmt is the whole check, and it is the right one because it PARSES without
// resolving anything. A test calling a function nobody has written yet is
// perfectly valid Go — it simply does not compile yet — so gofmt accepts it and
// a compiler would not. That is exactly the line this stage needs: it catches the
// unbalanced brace and the mangled string literal, which are the author's own
// mistakes, and stays silent about the undefined symbols, which are the
// developer's job to supply.
//
// It runs against the PUSHED branch for the same reason every other verification
// here does: what is checked must be what actually landed, not what the agent
// believed it wrote.
func (a *DevAgent) verifyTestsParse(ctx context.Context, rec *Recorder, t Ticket, s *devState, branch string) (string, bool, error) {
	// AN EMPTY TEST BODY IS CAUGHT HERE RATHER THAN IN THE SANDBOX, because the
	// contents are already in hand and the answer is exact: a function whose body
	// holds no statements cannot assert anything, whatever it is named or
	// commented. Measured — a spec merged as `func TestTaskValidation(t *testing.T)`
	// containing four comment lines describing the assertions it did not make. The
	// gate went green, the developer had nothing to satisfy, and 121 lines of
	// unverified implementation merged on the strength of it.
	// A SPEC MAY NOT SUPPLY ITS OWN IMPLEMENTATION. Observed as a hard loop: the
	// author wrote task_store_test.go beginning `type Task struct {...}`, so the
	// tests compiled and passed against nothing, the red gate below refused them
	// for passing, and the author rewrote the identical file six times in two and
	// a half minutes. It could not have worked even if it had merged — the
	// developer may not edit test files, so a type declared there is a type it can
	// never provide, and the two definitions would collide the moment it tried.
	//
	// EXPORTED is the line, because it is where Go already draws it. The types and
	// functions under test are the package's API and are exported; the scaffolding
	// a test legitimately owns — a testCase struct, a setup helper, a fixture
	// builder — is conventionally not. So this refuses `type Task` and allows
	// `type testCase`, without needing to know what the ticket asked for.
	if own := implementationInTests(s.staged); len(own) > 0 {
		recordSpecGate(ctx, "implementation_in_tests", "rejected")
		rec.Refusal(ctx, RefusalSpecGate, "implementation_in_tests", strings.Join(own, ", "))
		return fmt.Sprintf(
			"These declarations belong to the IMPLEMENTATION, and you have put them in a test file: %s.\n\n"+
				"Your tests must call code that does not exist yet — that is what makes them a specification, "+
				"and their failure to compile is the developer's starting point. Declaring the types and "+
				"functions yourself makes the tests pass against nothing, and the developer cannot even fix it: "+
				"it is not allowed to edit test files, so it can never provide what you have already declared.\n\n"+
				"Delete these declarations and write the tests as if the implementation were already there.",
			strings.Join(own, ", ")), false, nil
	}

	if stubs := stubbedTests(s.staged); len(stubs) > 0 {
		recordSpecGate(ctx, "stubbed_tests", "rejected")
		rec.Refusal(ctx, RefusalSpecGate, "stubbed_tests", strings.Join(stubs, ", "))
		return fmt.Sprintf(
			"These test functions have EMPTY BODIES: %s.\n\n"+
				"A comment describing an assertion is not an assertion, and neither is t.Skip. Each of "+
				"these must actually call the code and check the result — t.Errorf/t.Fatalf on a wrong "+
				"value, require.Equal, and so on. Write the assertions you described.",
			strings.Join(stubs, ", ")), false, nil
	}

	if inv := invertedErrorAssertions(s.staged); len(inv) > 0 {
		recordSpecGate(ctx, "inverted_assertion", "rejected")
		rec.Refusal(ctx, RefusalSpecGate, "inverted_assertion", strings.Join(inv, "; "))
		return fmt.Sprintf(
			"These assertions CONTRADICT THEMSELVES — each one fails in the case its own message calls "+
				"correct:\n\n%s\n\nRead the first one back: the message says an error is expected, and the "+
				"condition runs the failure when an error is returned. An implementation that does exactly "+
				"what you asked fails your test, and one that does the opposite passes it.\n\n"+
				"This is the one mistake the developer cannot work around — it may not edit test files, so "+
				"it can only rewrite the implementation back and forth until it is killed. Fix the "+
				"conditions so each fires on the case its message describes.",
			strings.Join(inv, "\n")), false, nil
	}

	// THE ONE-TEST-PER-CRITERION BOUND IS GONE, and the measurement is why. It was
	// meant to remove the variance in specification size — the same store ticket
	// took 4, 17, 65, 107 and 334 developer turns across five runs — but it fought
	// the stub gate instead: 32 refusals for writing too many tests against 103
	// for writing hollow ones in the same seven minutes, with authors that used to
	// finish in 4-8 turns looping at 27, 42 and 74. The cheapest way to satisfy
	// "write fewer tests" is to empty their bodies, which is the one thing the
	// specification must never do.
	// A REPAIR IS NOT A FRESH SPECIFICATION, and holding it to the same red gate is
	// incoherent.
	//
	// The gate demands that tests FAIL, because a specification written before its
	// implementation must. But a ticket handed BACK by the developer arrives on a
	// branch where the implementation already exists — so the moment the author
	// fixes whatever was wrong, its tests pass, and it is told it has specified
	// nothing. Measured on the dev fixture: the author corrected the compile error
	// it was sent back for, was refused for the tests then passing, and had no move
	// left that could satisfy both demands at once.
	//
	// On a repair the question is only whether the tests are well-formed and
	// compile. Whether they pass is the developer's business, one stage on.
	repairing := specRepairsSoFar(t) > 0
	res, err := s.sb.RunOnBranch(ctx, rec, branch, a.testSpecScript(repairing))
	if err != nil {
		return "", false, fmt.Errorf("check tests parse on %s: %w", branch, err)
	}
	out := clip(strings.TrimSpace(res.Stdout+res.Stderr), maxTestOutput)
	rec.Action(ctx, "verify", fmt.Sprintf("test syntax on %s → exit %d", branch, res.ExitCode), nil)

	// RED MUST MEAN FAILING ASSERTIONS, NOT A BROKEN SPECIFICATION. The gate
	// requires the suite to fail, and with no implementation it fails to COMPILE
	// on undefined symbols — that is the expected red of test-first and is
	// correct. Any OTHER compile error is the specification's own defect, and it
	// reads as red just the same, so it sailed through.
	//
	// Measured: two sections of one task each declared TestConcurrentAccess, the
	// branch stopped compiling, and the DEVELOPER inherited it — 86 turns, almost
	// all reads, with no legal move available because it may not edit test files.
	// The author can fix that in one rename; the developer never can.
	if bad := nonUndefinedCompileErrors(out); len(bad) > 0 {
		return fmt.Sprintf(
			"Your tests do not COMPILE, and not because the implementation is missing:\n\n```\n%s\n```\n\n"+
				"An undefined function or type is expected at this stage — anything else is a fault in the "+
				"tests themselves. If a name is already declared, another section of this task declared it "+
				"first: rename yours after this section rather than reusing it.",
			strings.Join(bad, "\n")), false, nil
	}

	// A PANIC IN THE TESTS' OWN FRAMES FAILS A REPAIR, however well it compiles.
	//
	// Repairing relaxes the demand that the suite go red, because on a hand-back
	// the implementation already exists. It must not relax this: a panic raised
	// inside the tests is unreachable by any implementation, and it is the exact
	// thing a developer hands a specification back FOR. Letting it through tells
	// the author its work is finished and the developer to satisfy something it
	// cannot, forever.
	//
	// Measured on r81: reflect.Elem on a string-kind type panics in the assertion
	// itself. The suite compiled, the repair gate passed, and the task ping-ponged
	// between the two stages until its retries ran out.
	if repairing {
		if panics := panicOnlyInTests(out); len(panics) > 0 {
			return fmt.Sprintf(
				"Your tests PANIC before their assertions run, in %s.\n\nThis is not something the "+
					"implementation can answer: the panic is raised inside the test's own code, so no "+
					"code the developer writes reaches the assertion. Fix the test that panics — the "+
					"failure is below — and change nothing else.\n\n```\n%s\n```",
				strings.Join(panics, ", "), clipEnds(out, 400, 1200)), false, nil
		}
	}

	if res.OK() {
		// ALREADY BUILT IS ALSO DONE. The author is told the truth — its tests pass
		// because the behaviour exists, not because they assert nothing — and the
		// stage ends. Anything else asks it to invent a failure for working code,
		// which is the one thing a specification must never do.
		if strings.Contains(out, alreadyImplementedMarker) {
			return fmt.Sprintf("The tests on branch %s are valid Go and they PASS, because the behaviour "+
				"they describe is already implemented on this branch — another section of this task built "+
				"it, and the sections share a branch.\n\n"+
				"That is a finished specification, not an empty one: it pins behaviour that exists and will "+
				"fail if a later change breaks it. There is nothing to write. You are finished.", branch), true, nil
		}
		return fmt.Sprintf("The tests on branch %s are valid Go, they FAIL against the current code as they "+
			"should, and they are pushed.\n\n"+
			"They do NOT pass yet, and are not supposed to: the implementation they describe has not been "+
			"written. Making them pass is the developer's work, and it is the next stage. "+
			"You are finished — call finish.", branch), true, nil
	}
	// THE TWO FAILURES NEED OPPOSITE ADVICE, so they are told apart by a marker
	// rather than by guessing from the output. "Fix your syntax" is actively
	// misleading to an author whose problem is that its tests passed.
	if strings.Contains(out, vacuousSpecMarker) {
		return fmt.Sprintf(
			"Your tests PASS against the current code, and that means they do not specify anything.\n\n"+
				"The implementation for this ticket has not been written yet, so a specification of it must "+
				"FAIL right now — that failure is what the developer is given to fix. Tests that already pass "+
				"leave nothing to be done and would let the ticket merge unverified.\n\n"+
				"Assert the behaviour the ticket asks for, against functions and types that do not exist yet.\n\n"+
				"```\n%s\n```", strings.ReplaceAll(out, vacuousSpecMarker, "")), false, nil
	}
	return fmt.Sprintf("The tests do not parse as Go. Fix the syntax — this is your own mistake to correct, "+
		"and it is NOT about undefined functions, which are expected at this stage.\n\n```\n%s\n```", out), false, nil
}

// vacuousSpecMarker distinguishes "your tests passed" from "your tests do not
// parse". Both are a non-zero exit from the same script.
const vacuousSpecMarker = "<<<SPEC-PASSED-AGAINST-CURRENT-CODE>>>"

// alreadyImplementedMarker distinguishes "your tests passed because they assert
// nothing" from "your tests passed because the behaviour is ALREADY BUILT".
//
// Both are a green suite from an author that is supposed to write red, and the
// gate used to call both vacuous. That is wrong whenever the implementation is
// already on the branch, and sections make it routine: the sections of one task
// SHARE a branch, so whichever lands first leaves its code there for the rest.
//
// Measured on r67. "Implement basic task operations" was speccing a store that
// the sibling section "concurrent access control" had already built and merged —
// Add, Get, List, Update, Delete, all green. The author diagnosed it correctly
// and said so every turn ("the tests are already written but they're passing
// against the existing implementation"), then had no move: the only way to go
// red was to write a test it knew to be wrong. It rewrote the correct file
// instead, which is byte-identical, which is a no-op refusal — 18 times, and it
// would have run to the 200-turn budget.
//
// This is the same exemption `repairing` already makes, for the same reason,
// reached by a different route. See testSpecScript.
const alreadyImplementedMarker = "<<<SPEC-ALREADY-IMPLEMENTED>>>"

// testSpecScript checks the specification is well-formed AND that it fails.
//
// RED IS THE POINT OF A TEST-FIRST STAGE. Checking only that the tests parse
// accepts any syntactically valid file, including one that asserts nothing, and
// the cost of that lands two stages later: the developer's gate passes on the
// first commit because there is nothing to satisfy, the reviewer sees green, and
// unverified code merges looking exactly like verified code. A specification
// that passes before the implementation exists is not a specification.
func (a *DevAgent) testSpecScript(repairing bool) string {
	cmd := a.repo.TestCommand
	if cmd == "" {
		return testParseScript
	}
	// REPAIRING RELAXES THE RED REQUIREMENT, NOT THE COMPILE CHECK, and conflating
	// the two was the first version of this and it was worse than the problem.
	//
	// Skipping the test command entirely meant nothing compiled the tests — and
	// the parse script uses gofmt, which catches syntax errors and NOT type errors
	// like "no new variables on left side of :=". That is exactly the fault the
	// developer sends a specification back for. Measured immediately: the author
	// passed its gate, pushed, and the developer found the identical error and
	// handed it straight back.
	//
	// So the command still runs and its output is still printed for
	// nonUndefinedCompileErrors to read. Only the demand that it FAIL is lifted,
	// because on a repair the implementation already exists.
	if repairing {
		return testParseScript + fmt.Sprintf(`
echo '--- the tests must COMPILE; they are not required to fail on a repair ---'
rc=0
out=$(%s 2>&1) || rc=$?
echo "$out" | tail -40
echo "checks ran (exit $rc); on a repair only compilation is required"
`, cmd)
	}
	// THE FAILURE TEXT IS ALWAYS PRINTED, and it used to be printed only when the
	// tests PASSED. That looks like a tidy-output decision and is not: the caller
	// classifies the failure by reading this output — nonUndefinedCompileErrors
	// separates "undefined: NewStore", which is the expected red state, from a
	// fault in the tests themselves. Discarding the text on the red path left that
	// check with nothing to read, so it never fired, and a specification whose own
	// tests did not COMPILE was accepted as valid red.
	//
	// Measured on the dev fixture: `no new variables on left side of :=` passed
	// this gate twice. The author saw exit 0, believed its tests were fine and
	// pushed them back unchanged, while the developer — which could see the error
	// but may not edit tests — handed the ticket straight back. Two agents, each
	// correct on what it could see, ping-ponging one broken file.
	// GREEN HAS TWO CAUSES AND THEY NEED OPPOSITE ANSWERS, so the tree is asked
	// which one this is rather than the gate assuming. A suite that passes because
	// it asserts nothing must be sent back; a suite that passes because its subject
	// is ALREADY BUILT is finished work, and telling that author "the implementation
	// has not been written yet" is simply false.
	//
	// THE QUESTION IS WHETHER THE CODE EXISTS, NOT WHERE IT CAME FROM. The first
	// version of this diffed the branch against its base, which answers "did THIS
	// BRANCH add an implementation" — a different question, and it is wrong by
	// exactly one route. Sections of a task share a branch, so a sibling's work
	// reaches an author two ways: still on the shared branch, where a diff sees it,
	// or already MERGED to the base, where a diff cannot. Measured across both runs:
	// r67 hit the first (diff caught it), r68 hit the second — main.go held Task,
	// Store, NewStore, Add, Get, List, Update and Delete, and the branch diff showed
	// one test file, so the exemption missed and the author looped 60 times.
	//
	// Counting EXPORTED declarations in non-test files answers the real question
	// wherever the code lives. The pristine baseline has none — package main with a
	// single func main — so a stub reads as zero and any real implementation reads
	// as more.
	return testParseScript + fmt.Sprintf(`
echo '--- the specification must fail against the current code ---'
rc=0
out=$(%s 2>&1) || rc=$?
echo "$out" | tail -40
if [ "$rc" -eq 0 ]; then
  srcs=$(ls *.go 2>/dev/null | grep -v '_test[.]go$') || true
  impl=0
  if [ -n "$srcs" ]; then
    impl=$(grep -hE '^(type [A-Z]|func [A-Z]|func \([a-z]+ \*?[A-Z])' $srcs | wc -l)
  fi
  if [ "$impl" -gt 0 ]; then
    echo "the subject of these tests is already built: $impl exported declaration(s) in non-test files"
    echo '%s'
    exit 0
  fi
  echo '%s'
  exit 1
fi
echo "the specification fails as it should (exit $rc), which is what the developer is given to fix"
`, cmd, alreadyImplementedMarker, vacuousSpecMarker)
}

// testParseScript checks the TEST files for syntax errors without compiling.
//
// `gofmt -l -e` prints a file that is not correctly formatted and reports parse
// errors on stderr; the exit code is what distinguishes them. Formatting alone
// must not fail the stage — the commit hook already runs the formatter — so this
// looks only for a genuine parse error.
//
// IT NAMES THE TEST FILES RATHER THAN ".". Measured on r92: this script took
// 8.5 seconds per section while the developer's gate, which runs the same git
// preamble AND `go test ./...`, took 512 milliseconds. The difference was the
// walk — "." descends the whole tree, and the check is about the files the
// author is allowed to write. Narrowing it agrees with the guard directly above,
// which already looks for *_test.go in the root, and with the message the author
// is given, which says the TESTS do not parse.
const testParseScript = `
if ! ls *_test.go >/dev/null 2>&1; then
  echo "no test files were written"
  exit 1
fi
# The "|| true" is load-bearing: the preamble sets -e, and gofmt EXITS NON-ZERO
# on a parse error — which is the case this check exists to catch. Without it the
# script dies on the assignment, the error text is never echoed, and the stage
# reports a failure with an empty body. Observed exactly that way: an author was
# told "the tests do not parse" with nothing after it, had no idea what was
# wrong, and rewrote the file blindly until its budget ran out.
err=$(gofmt -e -l *_test.go 2>&1 >/dev/null) || true
if [ -n "$err" ]; then
  echo "$err"
  exit 1
fi
echo "--- tests written ---"
ls *_test.go
`

// verifyCoverage is the coverage author's gate: the suite must PASS and reach
// the target.
//
// Both halves matter and for different reasons. Passing is the same bar as
// everyone else's — a test that does not pass is not coverage, it is a broken
// branch. The target is what makes the stage finish: without a number it has no
// definition of done, and the measured behaviour of these agents is that they do
// not stop on their own.
//
// FALLING SHORT IS NOT A FAILURE, it is an unfinished job, so the shortfall is
// reported and the loop carries on. Only running out of turns ends the stage
// without the target, and that lands in front of a person with the figure it
// reached — which is a far more useful thing to read than "coverage stage
// failed".
func (a *DevAgent) verifyCoverage(ctx context.Context, rec *Recorder, s *devState, branch string) (string, bool, error) {
	res, err := s.sb.RunOnBranch(ctx, rec, branch, a.coverageScript())
	if err != nil {
		return "", false, fmt.Errorf("coverage on %s: %w", branch, err)
	}
	out := clip(strings.TrimSpace(res.Stdout+res.Stderr), maxTestOutput)
	pct, found := parseCoverage(out)
	rec.Action(ctx, "verify", fmt.Sprintf("coverage on %s → %.1f%% (exit %d)", branch, pct, res.ExitCode), nil)

	if !res.OK() {
		return fmt.Sprintf("The tests FAIL, so there is no coverage to speak of. Fix this before "+
			"adding more:\n\n```\n%s\n```", out), false, nil
	}
	if !found {
		return fmt.Sprintf("The tests passed but no coverage figure could be read from the output. "+
			"The command must report one — see the repository's coverage command.\n\n```\n%s\n```", out), false, nil
	}
	target := a.repo.CoverageTarget
	if pct+0.05 >= float64(target) {
		return fmt.Sprintf("Coverage is %.1f%%, at or above the %d%% target, and the suite passes. "+
			"This stage is DONE — call finish.", pct, target), true, nil
	}
	return fmt.Sprintf("Coverage is %.1f%%, below the %d%% target. The suite passes, so keep ADDING tests "+
		"for the branches that are not exercised — error paths, empty and nil inputs, boundary values. "+
		"Read the implementation to find them.\n\n```\n%s\n```", pct, target, out), false, nil
}

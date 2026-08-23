package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Delegating the developer stage to a third-party coding agent.
//
// WHY THIS EXISTS. Today's failures split in two. Edit-layer mechanics — line
// arithmetic, a quote copied into its own replacement, malformed tool arguments,
// addressing a declaration that is not there — cost a dozen fixes, four of which
// caused worse faults than the one they fixed. Pipeline semantics — an
// unsatisfiable specification, the hand-back, attempt ceilings, the referee — no
// coding CLI has, because none of them has an author who owns the tests and a
// developer forbidden to touch them.
//
// So this replaces ONLY the developer's inner loop. The board, the roles, the
// spec gate, the review and the hand-back stay exactly where they are.
//
// Measured before writing it: qwen-code inside a kata lease completed "Create GET
// /api/tasks handlers" in one pass — 97 insertions in main.go, `go test -race`
// clean, no test file touched — on the ticket blacksmith's own developer failed
// repeatedly through r69.
//
// THE TEST-FIRST BOUNDARY IS A FILESYSTEM FACT HERE. chmod a-w on *_test.go needs
// no cooperation from the agent and works for one that has never heard of this
// pipeline, where the native loop needs a refusal, a hand-back path, an attempt
// ceiling and a referee to police the same rule.
const delegateEngineQwen = "qwen-code"

// delegateTaskOf is what the delegated developer is asked to do.
//
// The ticket's DESCRIPTION is written for the specification author — the real one
// on this board reads "This slice: ... Write a test for each of these and STOP" —
// so passing it to a developer contradicts the rules in the same prompt. The
// tests are the specification; the title is the context.
func delegateTaskOf(t Ticket) string {
	return "Implement this on the current branch: " + t.Title
}

// delegateScript is the whole developer stage as one command.
//
// ONE EXECUTION, because RunOnBranch resets the working tree on every call —
// git reset --hard and git clean -xfd — so anything the agent writes must be
// committed and pushed before the next command, or verification wipes it.
// perAttemptSeconds divides the stage's execution budget between the retries.
//
// AN EXECUTION HAS A DEADLINE AND THE LOOP MUST FIT INSIDE IT. Three unbounded
// attempts ran past the 900-second limit, so the delegate execution was still
// holding the lease when verification tried to submit — "already claimed", from a
// stage that had actually done its work. A reserve is kept back for the test runs,
// the commit and the push, which happen after the last attempt.
func perAttemptSeconds(budget int64) int64 {
	// BOUNDED BY THE EXECUTION BUDGET, which is now the only limit that applies.
	//
	// This was derived from forge's 300s idle timeout — a lease was reaped that
	// long after its last SUBMISSION whether or not a container was working, so an
	// attempt could not outlive it. Fixing that in forge (a lease with a running
	// execution is not idle) removed the constraint, and leaving the bound at 210s
	// then became the thing that killed the work: "Operation cancelled." mid-edit,
	// three attempts running, on a section the agent might well have finished.
	//
	// Each attempt is its own execution now, so each gets the whole budget less a
	// reserve for the test run, the commit and the push that share it.
	const reserve = 150
	if budget <= 0 {
		budget = 900
	}
	per := budget - reserve
	if per < 60 {
		per = 60
	}
	return per
}

func delegateScript(t Ticket, testCommand string, budgetSecs int64, retry bool, baseline string) string {
	// THE DESCRIPTION IS WRITTEN FOR THE SPECIFICATION AUTHOR, not the developer.
	// Passing it through told a delegated agent "Write a test for each of these and
	// STOP" while the rules below told it not to touch tests — contradictory
	// instructions, and it wrote tests and a 243-line index.html nobody asked for.
	//
	// The tests ARE the specification here, which is the whole design, so the
	// developer needs the title for context and the tests for detail. Nothing else
	// from the ticket survives.
	task := delegateTaskOf(t)
	// The rules the harness would otherwise enforce by refusing, stated once and
	// backed by the filesystem below.
	// SHOW IT THE FAILURES, do not merely assert that tests are the specification.
	//
	// Told only that "the tests are the specification", the delegated agent read
	// the README instead, implemented from that, and reported success — its own
	// words: "The tests in handlers_create_test.go are just placeholders and will
	// be implemented once the handler is working properly." It had never opened
	// them. An instruction it cannot see the consequences of is one it will
	// rationalise around; the failing output is not arguable.
	task += "\n\nRULES:\n" +
		"- The *_test.go files ARE the specification. They are finished, not placeholders,\n" +
		"  and they are READ-ONLY: you may not edit, delete or regenerate them.\n" +
		"- READ the failing test files before writing anything.\n" +
		"- The README is background only. Where it disagrees with a test, the test wins.\n" +
		"- Change only non-test Go files.\n" +
		"- Run `" + testCommand + "` until it passes. You are not finished until it does.\n" +
		"- Do not create new modules or move files."

	// RETRY ONLY. On a first attempt there is no previous theory to rule out, and
	// telling a fresh agent that "your previous attempt" failed invents a history
	// it then reasons from.
	prevDiff := ""
	headline := "THESE TESTS ARE FAILING RIGHT NOW. Make them pass:"
	if retry {
		prevDiff = `    # WHAT IT ALREADY TRIED, as a diff rather than as an instruction.
    #
    # "Do not repeat the same change" is a sentence a fresh process has no way to
    # obey: each attempt starts with no memory, so it re-derives the same wrong
    # theory from the same failure and writes the same code again. Measured on
    # r70: four attempts on one ticket, each ending with a confident summary
    # blaming "routing issues", while the actual defect was six lines away in
    # Add() — which never assigns an ID, so the test's URL is /api/tasks/ with an
    # empty id and the handler rejects its own request.
    #
    # Its own diff is the only thing that makes "you already tried that" checkable
    # rather than merely asserted.
    if git rev-parse HEAD~1 >/dev/null 2>&1; then
      echo
      echo "YOUR PREVIOUS ATTEMPT MADE THIS CHANGE, AND THE TESTS ABOVE STILL FAIL."
      echo "Whatever theory produced it was wrong. Look somewhere else:"
      git diff HEAD~1 -- '*.go' ':!*_test.go' | head -120
    fi`
		headline = "YOUR PREVIOUS ATTEMPT DID NOT WORK. These tests are STILL failing. " +
			"Do not repeat the same change. Read the test file and the error again:"
	}
	return fmt.Sprintf(`
# TELL THE AGENT HOW BIG THE WINDOW ACTUALLY IS. qwen-code assumes the stock
# Qwen3-Coder window (256K) and builds prompts to suit, so against a 131,072-token
# slot it composed a 67,546-token request that the server refused outright with a
# 400 — an attempt lost to arithmetic rather than to the work.
#
# ONE KEY, NOT TWO. Writing the modelProviders form as well "to be safe" cost more
# than it could ever save: that shape requires an "id" field, so the model registry
# failed validation and qwen-code refused to start at all —
#   Error: Model config in authType 'openai' missing required field: id
# reported as "the delegated agent produced no change", twice, because a crashed
# agent leaves no diff. A setting that might be ignored is cheap; a setting that
# breaks startup is not.
#
# Compaction at 0.7 rather than the documented 0.85: the same issues report later
# compactions reclaiming almost nothing (81,651 -> 81,273 tokens in one), so it
# needs to start well before the ceiling rather than at it.
# A KILLED ATTEMPT MUST STILL SAY SOMETHING. Measured on r70: one attempt ended
# with exit 15 — SIGTERM — after 163 seconds, with stdout and stderr both empty,
# no reap in forge's log and a 900-second timeout nowhere near expiry. Nothing
# could be attributed, because the signal killed the shell before the line that
# flushes the agent's own log.
#
# The trap costs nothing and turns the next one into evidence: how long it had
# been running, and what the agent had managed to do by then.
delegate_started=$(date +%%s)
trap 'echo "--- TERMINATED by SIGTERM after $(( $(date +%%s) - delegate_started ))s ---"; tail -20 /tmp/agent.log 2>/dev/null; exit 15' TERM
trap 'echo "--- TERMINATED by SIGINT ---"; tail -20 /tmp/agent.log 2>/dev/null; exit 130' INT

mkdir -p "$HOME/.qwen"
cat > "$HOME/.qwen/settings.json" <<'QWENSETTINGS'
{
  "model": {
    "generationConfig": { "contextWindowSize": %d },
    "chatCompression": { "contextPercentageThreshold": 0.7 }
  }
}
QWENSETTINGS

chmod a-w *_test.go 2>/dev/null || true
printf '%%s' %s > /tmp/task-base.md

# THE AGENT DECIDES IT IS FINISHED; THE GATE DECIDES IF IT IS. Left to itself it
# writes plausible code, reports "I have successfully implemented..." and pushes
# without re-running anything — measured: five minutes of work, a confident
# summary, and a suite still red.
#
# So it is asked again, with the failures its own last attempt left. Each run is a
# fresh process with no memory of the previous one, and that is fine: the failing
# output IS the continuity, and it is the only part that matters. This is the
# feedback the native loop gets for free by verifying after every write.
# ONE ATTEMPT PER EXECUTION, and the loop lives in Go. It was a shell loop inside
# a single execution, which forge reaped out from under it: last_used_at advances
# on SUBMISSION, not while a container works, so a multi-minute execution looks
# idle and is killed at the 300s timeout mid-agent. The verification that followed
# then found a stopped lease and reported "already claimed" — a stage failing on
# work it had actually done.
#
# Each attempt now submits its own execution, which refreshes the lease, and each
# ends by pushing so the next attempt's checkout inherits it.
%s > /tmp/failing.txt 2>&1 || true
grep -E "^[[:space:]]*--- FAIL" /tmp/failing.txt | sed "s/^[[:space:]]*--- FAIL: //; s/[[:space:]].*//" | sort -u > /tmp/failing-names.txt
printf '%%s\n' %s | tr ' ' '\n' | grep -v '^$' | sort -u > /tmp/baseline-names.txt
# PUBLISHED ON STDOUT so the Go side can carry it into the next attempt. Each
# attempt is its own execution against a reset working tree, so nothing survives
# in the sandbox — the baseline has to travel out and come back.
echo "===FAILING-AT-START $(tr '\n' ' ' < /tmp/failing-names.txt)"
if ! grep -qE "^(FAIL|# |.*\[build failed\])" /tmp/failing.txt; then
  echo "--- tests already pass ---"
else
  { cat /tmp/task-base.md
    echo
    echo "%s"
    echo
    # EVERY FAILING TEST BY NAME, THEN THE DETAIL. Naming them is cheap and
    # COMPLETE; the detail is verbose and has to be bounded, so the two are
    # separated rather than traded off.
    #
    # Measured on r70: testify prints an Error Trace, an expected/actual pair and
    # a unified diff for EVERY assertion, so one failing subtest ran to 45 lines
    # and a 79-line report was cut at 60 — hiding the DELETE failure completely.
    # The agent spent three attempts, twelve minutes and a whole ticket budget on
    # the only failure it could see, and its own summary blamed "routing issues".
    # A truncated report does not read as truncated; it reads as the whole story.
    echo "FAILING TESTS:"
    grep -E "^\s*--- FAIL" /tmp/failing.txt | sed "s/^ *//"
    # A REGRESSION IS NOT THE TICKET'S RED STATE, and telling the two apart is the
    # difference between "your work is not finished" and "your work broke something
    # that worked". Measured on r70: an agent adding validation broke
    # TestCreateTaskHandler, saw it in an undifferentiated failure list, and spent
    # its attempts theorising about redirects in a test it had never been asked to
    # touch.
    #
    # The baseline is the set that was failing BEFORE this agent changed anything —
    # test-first, so that set is exactly the ticket's own work.
    if [ -s /tmp/baseline-names.txt ]; then
      comm -13 /tmp/baseline-names.txt /tmp/failing-names.txt > /tmp/regressions.txt 2>/dev/null || true
      if [ -s /tmp/regressions.txt ]; then
        echo
        echo "STOP. YOU BROKE THESE. They PASSED before you started:"
        cat /tmp/regressions.txt
        echo
        echo "These are NOT part of your ticket and nothing was wrong with them. Your own"
        echo "change caused them to fail. Fix that first: find what your edit altered for"
        echo "the code they exercise, and stop doing it. Do not edit those tests, and do"
        echo "not explain the failures away."
      fi
    fi
    echo
    # THE GATE'S REASON, IN THE AGENT'S OWN PROMPT. A production file importing a
    # testing package still COMPILES, so the suite says nothing about it and the
    # agent would be failed by a gate whose reason it never saw. Same detection as
    # the gate, printed rather than fatal: here it is feedback, there it is a
    # verdict.
    %s
    if [ -n "$bad" ]; then
      echo
      echo "THIS ALONE WILL FAIL THE BRANCH, whatever the tests say:"
      %s
      echo
    fi
    # AN EMPTY ACTUAL VALUE IS A CONSTRUCTOR PROBLEM, and saying so is the single
    # highest-value hint this report can carry.
    #
    # Measured across two runs: "Expected first task to be id task-0, got " —
    # the field is never assigned, and Add() is four lines away. The agent read
    # the same line six times and every time inspected the code it had just
    # written, because the assertion names the READER and never the writer. It
    # failed the ticket six ways without once opening the constructor.
    #
    # The rule is safe because it is narrow: expected something, got nothing. That
    # is not a wrong value, it is an absent one, and an absent one is only ever
    # written where the value is created.
    if grep -qE "got $|got \"\"$|actual +: *\"\"$|actual +: *$" /tmp/failing.txt; then
      echo
      echo "READ THIS BEFORE ANYTHING ELSE:"
      echo "A test expected a value and got an EMPTY one. That is not a wrong value, it is"
      echo "an absent one — nothing ever assigned it. Do not look at the code that reads it;"
      echo "look at where the value is CREATED (the constructor, the Add/New/insert path)"
      echo "and make it set the field. The assertion names the reader and never the writer,"
      echo "which is why this one is easy to stare past."
    fi
    echo "DETAIL:"
    head -200 /tmp/failing.txt
%s
    # RULE OUT THE THEORY IT KEEPS REACCHING FOR, but only when the code says so.
    #
    # Measured on r70: four attempts on one ticket, every one of them blaming
    # "routing issues", "path extraction edge cases" and finally "how the handlers
    # interact with the testing framework" — concluding the ticket was beyond what
    # it could determine. Every one of those tests calls the handler as a plain
    # function against an httptest recorder: there is no router, no server and no
    # framework anywhere in the failure. The actual defect was six lines away in
    # Add(), which never assigned an ID, so the request URL carried an empty one.
    #
    # This is a FACT ABOUT THE TESTS, checked against the tests themselves, not a
    # hint at the answer: it is printed only when they really do call the code
    # directly, and it closes off a whole class of wrong explanation without
    # suggesting a right one.
    if grep -l "httptest.NewRecorder" *_test.go >/dev/null 2>&1 &&
       ! grep -lE "httptest.NewServer|ListenAndServe" *_test.go >/dev/null 2>&1; then
      echo
      echo "NOTE ON THESE TESTS: they call your functions DIRECTLY and record the"
      echo "response — no router, no server, no framework is involved. So a failure"
      echo "here cannot be caused by routing, path matching or the test environment."
      echo "Whatever the test passes in, your function receives verbatim. If your"
      echo "explanation is the environment, it is wrong; read what the test builds"
      echo "and what your code does with it."
    fi
    echo
    echo "The test files on this branch:"
    ls *_test.go
  } > /tmp/task.md
  # Bounded so the execution finishes well inside forge's idle timeout.
  timeout %d qwen --prompt "$(cat /tmp/task.md)" --yolo >/tmp/agent.log 2>&1 || true
  tail -12 /tmp/agent.log
fi

chmod u+w *_test.go 2>/dev/null || true
git checkout -- '*_test.go' 2>/dev/null || true
# BUILD OUTPUT IS NOT A DELIVERABLE. The repository has no .gitignore, an agent
# that compiles leaves a binary beside the source, and adding everything then
# commits 8 MB of it - measured twice on one branch, "tracker" and "task-tracker".
# Anything binary and not source is dropped before staging.
for f in $(git status --porcelain | awk '{print $2}'); do
  case "$f" in
    # A COPY OF A SOURCE FILE IS NOT A SOURCE FILE. Measured on r70: the agent
    # saved main.go.backup before editing and it went in with the commit. It is
    # text, so the binary check below waves it through, and it compiles to nothing
    # so no gate ever objects — it just accretes on the branch and the next agent
    # reads it as though it were live code.
    *.backup|*.bak|*.orig|*.rej|*~) rm -f "$f"; echo "dropped editor leftover: $f"; continue ;;
    *.go|*.md|*.mod|*.sum|*.html|*.txt|*.json|*.yaml|*.yml) ;;
    *) if [ -f "$f" ] && ! head -c 1024 "$f" | LC_ALL=C grep -qI .; then
         rm -f "$f"; echo "dropped build output: $f"
       fi ;;
  esac
done
if [ -n "$(git status --porcelain)" ]; then
  git add -A
  git -c user.email=dev-agent@platform.invalid -c user.name=dev-agent \
      commit -q -m %s
  git push -q origin HEAD && echo "--- pushed ---"
else
  echo "--- the agent changed nothing ---"
fi
`, delegateContextTokens,
		shellSingleQuote(task), testCommand, shellSingleQuote(baseline), headline,
		testImportOffenders(), testImportAdvice(), prevDiff, perAttemptSeconds(budgetSecs),
		shellSingleQuote("feat: "+clip(t.Title, 60)))
}

// delegateProducedNothing reports an attempt that ran and wrote nothing — as
// opposed to one that had nothing to write.
func delegateProducedNothing(out string) bool {
	if strings.Contains(out, "tests already pass") {
		return false
	}
	return strings.Contains(out, "the agent changed nothing")
}

// failingAtStart reads the ticket's red state out of the first attempt's output.
//
// Taken from the FIRST attempt only. Every later attempt opens against a tree the
// agent has already edited, so its failures are a mix of the ticket's own work and
// whatever the agent has broken — which is precisely the mixture this is here to
// separate.
func failingAtStart(stdout string) string {
	for _, line := range strings.Split(stdout, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "===FAILING-AT-START "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// runDelegated runs the external agent, then hands the result to the SAME
// verification and reporting the native loop uses.
//
// Any test file the agent managed to modify is reverted before the commit rather
// than argued about: read-only should have stopped it, and if something got
// through, the specification is still not the developer's to change.
func (a *DevAgent) runDelegated(ctx context.Context, rec *Recorder, t Ticket, state *devState) (string, string, error) {
	branch := a.branchFor(t)

	// THE LOOP LIVES HERE, NOT IN THE SHELL. forge advances a lease's last_used_at
	// on SUBMISSION, not while a container works, so one long execution looks idle
	// and is reaped at the 300s timeout mid-agent — measured four times, each
	// reported as "already claimed" by the verification that followed. Separate
	// executions keep the lease alive, and each attempt pushes so the next one's
	// checkout inherits its work.
	var verdict string
	var pass bool
	// THE RED STATE OF THE TICKET, captured before the agent has changed anything.
	// Test-first means the first attempt opens with exactly the failures this ticket
	// exists to clear, so anything failing later that is NOT in this set is damage
	// the agent did on its way — a distinction it cannot make from a flat list, and
	// one it got badly wrong on r70.
	baseline := ""
	for attempt := range maxDelegateAttempts {
		res, err := state.sb.RunOnBranch(ctx, rec, branch,
			delegateScript(t, a.repo.TestCommand, a.repo.TimeoutSecs, attempt > 0, baseline))
		if err != nil {
			return OutcomeFailed, "delegate: " + err.Error(), nil
		}
		out := clip(strings.TrimSpace(res.Stdout+res.Stderr), maxTestOutput)
		if attempt == 0 {
			baseline = failingAtStart(res.Stdout)
		}
		rec.Action(ctx, "delegate",
			fmt.Sprintf("%s attempt %d on %s → exit %d", delegateEngineQwen, attempt+1, branch, res.ExitCode), nil)

		// Recorded as a turn even though this process made no model call: the agent
		// infers inside the sandbox, so every counter built on turns reads zero while
		// the developer works hardest. Mistaken for a hang twice.
		rec.Turn(ctx,
			ChatRequest{Messages: []Message{{Role: "user", Content: clip(delegateTaskOf(t), 2000)}}},
			ChatResult{Model: delegateEngineQwen, Content: clip(out, 6000)}, nil)

		// NOTHING TO DO IS NOT A FAILURE, and conflating the two took a board from
		// one merged ticket to seven blocked in forty-four seconds.
		//
		// Sections overlap: the implementation merged for one section can already
		// satisfy the next section's tests. The script says so — "tests already
		// pass" — and correctly does not run the agent, so of course nothing
		// changes. Reading only "the agent changed nothing" turned the healthiest
		// possible outcome into a failure, spent both attempts on it, and blocked
		// the ticket. Measured on the clean run: four such failures eleven seconds
		// apart, each one a section that was already green.
		//
		// The verification below is what decides now. If the branch really is green
		// it passes, which is the truth; if it is not, the ordinary red path runs.
		if delegateProducedNothing(out) && attempt == 0 {
			return OutcomeFailed, "the delegated agent produced no change", nil
		}

		verdict, pass, err = a.verifyInSandbox(ctx, rec, state, branch)
		if err != nil {
			return OutcomeFailed, "verify: " + err.Error(), nil
		}
		state.lastTest, state.testsPass = verdict, pass
		if pass {
			return a.finish(ctx, rec, t, state, clip(t.Title, 60),
				fmt.Sprintf("Written by %s inside the sandbox in %d attempt(s); verified by the usual gate.",
					delegateEngineQwen, attempt+1))
		}
	}

	// A FAILING DELEGATED ATTEMPT IS STILL AN ATTEMPT. It goes back through the
	// same routing as a native one — including the referee, which is what decides
	// whether an unsatisfiable specification is the author's problem.
	if v := a.refereeOn(ctx, rec, state, verdict); v.blames(ownerSpec) {
		state.specBroken = v.Reason
		if outcome, detail, done := a.endOnBrokenSpec(ctx, t, state, verdict); done {
			return outcome, detail, nil
		}
	}
	a.comment(ctx, t, "**"+delegateEngineQwen+" could not make the tests pass.**\n\n```\n"+
		clipEnds(verdict, 400, 900)+"\n```")
	return OutcomeFailed, "delegated agent left the tests failing", nil
}

// maxDelegateAttempts bounds the retry loop inside one stage.
//
// TWO, AND THE SECOND IS ALREADY GENEROUS. Counted over every delegated ticket in
// this run: attempt 1 produced 2 successes and 4 failures; attempts 2 and 3
// produced 0 successes from 6 tries. Not one later attempt has ever converted a
// failure. Three attempts were not buying recovery, they were dividing a fixed
// lease budget three ways and cutting the only attempt that works off at 750
// seconds — measured, one was killed mid-edit at 12m46s with "Operation
// cancelled." after doing real work.
//
// The budget is fixed by forge's one-hour lease lifetime, which all attempts
// share, so this is a reallocation rather than an increase: two attempts of ~1500s
// fit where three of 750s did, and the first one now has room to finish.
//
// If later attempts start converting, raise it back — but on evidence, not on the
// intuition that more tries must be better.
const maxDelegateAttempts = 2

// verifyClaimRetries and verifyClaimBackoff bound the wait for a lease to free
// itself between two executions.
//
// Short and few: this is a handover measured in seconds, not a queue. If it has
// not cleared after these, something is genuinely holding the lease and the stage
// should say so rather than sit there.
const (
	verifyClaimRetries = 4
	verifyClaimBackoff = 3 * time.Second
)

// delegateContextTokens is the window the delegated agent is told it has.
//
// Under the serving slot, not equal to it: the server counts the whole request
// including the completion it is about to generate, so an agent that fills the
// window exactly is refused. The margin is what a reply needs.
//
// Lowering this is also how several agents run at once — the slot count and the
// per-slot context divide the same fixed KV budget, so two agents at 65,536 cost
// the same VRAM as one at 131,072. Worth doing once a single agent is reliably
// inside its window, and not before.
//
// THAT CONDITION IS NOW MET, so this is 60,000 against a 65,536 slot, with
// compaction at 0.7 putting the working ceiling near 42,000.
//
// The number is chosen from the growth curve, not from the slot. Measured across
// one session: 46,783 tokens on a fresh start, 58,592 an hour later as sections
// merged, 73,748 the day before on a mature branch. That climb is what broke the
// previous two-slot attempt — 67,546 against 65,536, over by a thousand tokens,
// losing the whole attempt to a 400. Sizing to the slot would only move the
// collision later; sizing below it and compacting early removes it.
const delegateContextTokens = 60000

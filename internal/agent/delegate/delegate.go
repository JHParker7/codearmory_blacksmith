// Package delegate runs an EXTERNAL coding agent as the developer, instead of
// the native loop.
//
// EVERYTHING HERE IS ABOUT A PROCESS WITH NO MEMORY. The external agent is a
// fresh process per attempt, so nothing survives between them except what this
// package carries out of one execution and back into the next — and every rule
// below exists because something that looked like continuity was not.
package delegate

import (
	"fmt"
	"sort"
	"strings"

	"github.com/code-armory-app/blacksmith/internal/forge"
	"github.com/code-armory-app/blacksmith/internal/ticket"
)

// EngineQwen names the one external engine this supports.
const EngineQwen = "qwen-code"

// MaxAttempts is how many times the external agent is asked.
//
// TWO, because a third buys almost nothing: measured on r70, four attempts on
// one ticket each ended with a confident summary blaming "routing issues" while
// the actual defect was six lines away. What changed the outcome was showing the
// agent its own previous diff, not giving it more tries.
const MaxAttempts = 2

// ContextTokens is the window the delegated agent is TOLD it has.
//
// UNDER THE SERVING SLOT, NOT EQUAL TO IT: the server counts the whole request
// including the completion it is about to generate, so an agent that fills the
// window exactly is refused outright with a 400 — an attempt lost to arithmetic
// rather than to the work.
//
// The number is chosen from the GROWTH CURVE, not from the slot. Measured across
// one session: 46,783 tokens on a fresh start, 58,592 an hour later as sections
// merged, 73,748 the day before on a mature branch. That climb is what broke a
// previous attempt at two slots — 67,546 against 65,536, over by a thousand
// tokens. Sizing to the slot only moves the collision later; sizing below it and
// compacting early removes it.
const ContextTokens = 60000

// CompressionThreshold is when the agent starts compacting its context.
//
// 0.7 rather than the documented 0.85, because later compactions reclaim almost
// nothing — one measured run went 81,651 to 81,273 tokens — so it has to start
// well before the ceiling rather than at it.
const CompressionThreshold = 0.7

// Reserve is the seconds kept back from an execution's budget for the test run,
// the commit and the push that follow the agent.
//
// AN EXECUTION HAS A DEADLINE AND THE WORK MUST FIT INSIDE IT. Unbounded
// attempts ran past the limit, so the delegate execution was still holding the
// lease when verification tried to submit — reported as "already claimed", from
// a stage that had actually done its work.
const Reserve = 150

// DefaultBudget is the execution budget assumed when none is configured.
const DefaultBudget = 900

// MinAttempt is the floor: an attempt shorter than this cannot do anything, so
// a tiny budget produces one short attempt rather than several useless ones.
const MinAttempt = 60

// AttemptSeconds is how long one attempt may run.
//
// EACH ATTEMPT IS ITS OWN EXECUTION, so each gets the whole budget less the
// reserve. It was bounded far lower once, derived from forge's 300s idle
// timeout — a lease was reaped that long after its last SUBMISSION whether or
// not a container was working. Fixing that in forge removed the constraint, and
// leaving the old bound in place then became the thing that killed the work:
// "Operation cancelled." mid-edit, on a section the agent might well have
// finished.
func AttemptSeconds(budget int64) int64 {
	if budget <= 0 {
		budget = DefaultBudget
	}
	return max(budget-Reserve, MinAttempt)
}

// Task is what the delegated developer is asked to do.
//
// THE TICKET'S DESCRIPTION IS DELIBERATELY EXCLUDED. It is written for the
// SPECIFICATION AUTHOR — the real one on this board reads "This slice: … Write a
// test for each of these and STOP" — so passing it to a developer contradicts
// the rules in the same prompt. Measured: an agent given both wrote tests and a
// 243-line index.html nobody asked for.
//
// The tests ARE the specification, which is the whole design, so the developer
// needs the TITLE for context and the TESTS for detail. Nothing else survives.
func Task(t ticket.Ticket) string {
	return "Implement this on the current branch: " + strings.TrimSpace(t.Title)
}

// Rules are what the harness would otherwise enforce by refusing, stated once
// and backed by the filesystem.
//
// THE TEST FILES ARE MADE READ-ONLY AS WELL AS FORBIDDEN, because a rule the
// agent may decline to follow is not a rule. Told only that "the tests are the
// specification", one agent read the README instead, implemented from that, and
// reported success — its own words: "The tests in handlers_create_test.go are
// just placeholders and will be implemented once the handler is working
// properly." It had never opened them.
func Rules(testCommand string) string {
	return "RULES:\n" +
		"- The *_test.go files ARE the specification. They are finished, not placeholders,\n" +
		"  and they are READ-ONLY: you may not edit, delete or regenerate them.\n" +
		"- READ the failing test files before writing anything.\n" +
		"- The README is background only. Where it disagrees with a test, the test wins.\n" +
		"- Change only non-test Go files.\n" +
		"- Run `" + testCommand + "` until it passes. You are not finished until it does.\n" +
		"- Do not create new modules or move files."
}

// Brief is the whole instruction for one attempt.
//
// THE RETRY WORDING IS DIFFERENT, and only on a retry. On a first attempt there
// is no previous theory to rule out, and telling a fresh agent that "your
// previous attempt" failed invents a history it then reasons from.
func Brief(t ticket.Ticket, testCommand string, retry bool) string {
	headline := "THESE TESTS ARE FAILING RIGHT NOW. Make them pass:"
	if retry {
		headline = "YOUR PREVIOUS ATTEMPT DID NOT WORK. These tests are STILL failing. " +
			"Do not repeat the same change. Read the test file and the error again:"
	}
	return Task(t) + "\n\n" + Rules(testCommand) + "\n\n" + headline
}

// FailingMarker publishes the ticket's red state out of an execution, so it can
// travel back into the next one.
//
// EACH ATTEMPT IS ITS OWN EXECUTION AGAINST A RESET WORKING TREE, so nothing
// survives in the sandbox. The baseline has to come out on stdout and go back in
// as an argument.
const FailingMarker = "===FAILING-AT-START "

// FailingAtStart reads the ticket's red state out of an attempt's output.
//
// TAKEN FROM THE FIRST ATTEMPT ONLY. Every later attempt opens against a tree
// the agent has already edited, so its failures are a mix of the ticket's own
// work and whatever the agent has broken — which is precisely the mixture this
// exists to separate.
func FailingAtStart(stdout string) []string {
	for _, line := range strings.Split(stdout, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), FailingMarker); ok {
			return names(rest)
		}
	}
	return nil
}

// NewlyFailing are the tests THIS ATTEMPT broke, as opposed to the ones the
// ticket was given to fix.
//
// THE DISTINCTION DECIDES WHO IS AT FAULT. A suite that is still red because the
// ticket's own tests have not been satisfied is an unfinished job; one that is
// red because the agent broke something that used to pass is a regression, and
// reporting the second as the first sends the next attempt to fix the wrong
// thing.
func NewlyFailing(baseline, nowFailing []string) []string {
	was := make(map[string]bool, len(baseline))
	for _, n := range baseline {
		was[n] = true
	}
	var out []string
	for _, n := range nowFailing {
		if !was[n] {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// names splits a whitespace-separated list, dropping the blanks a shell leaves
// behind.
func names(s string) []string {
	out := strings.Fields(s)
	sort.Strings(out)
	return out
}

// The markers an attempt leaves so its outcome can be read without guessing.
const (
	ChangedNothingMarker = "the agent changed nothing"
	AlreadyPassingMarker = "tests already pass"
	TerminatedMarker     = "--- TERMINATED by"
)

// ProducedNothing reports whether an attempt left the tree untouched.
//
// A SUITE THAT ALREADY PASSED IS NOT NOTHING. The agent correctly did no work,
// and treating that as a failed attempt would retry a ticket that is finished —
// so the passing case is checked first and wins.
func ProducedNothing(out string) bool {
	if strings.Contains(out, AlreadyPassingMarker) {
		return false
	}
	return strings.Contains(out, ChangedNothingMarker)
}

// WasKilled reports whether the attempt was terminated rather than finishing.
//
// A KILLED ATTEMPT MUST STILL SAY SOMETHING. Measured on r70: one attempt ended
// with exit 15 — SIGTERM — after 163 seconds, with stdout and stderr both empty,
// no reap in forge's log and the timeout nowhere near expiry. Nothing could be
// attributed, because the signal killed the shell before the line that flushes
// the agent's own log. The trap that fixes it costs nothing and turns the next
// one into evidence.
func WasKilled(out string) bool { return strings.Contains(out, TerminatedMarker) }

// Settings is the configuration written for the external agent.
//
// ONE KEY, NOT TWO. Writing a second provider form "to be safe" cost more than
// it could ever save: that shape requires an "id" field, so the model registry
// failed validation and the agent refused to start at all —
//
//	Error: Model config in authType 'openai' missing required field: id
//
// reported as "the delegated agent produced no change", twice, because a crashed
// agent leaves no diff. A setting that might be ignored is cheap; a setting that
// breaks startup is not.
func Settings() string {
	return fmt.Sprintf(`{
  "model": {
    "generationConfig": { "contextWindowSize": %d },
    "chatCompression": { "contextPercentageThreshold": %g }
  }
}`, ContextTokens, CompressionThreshold)
}

// TrapScript makes a killed attempt say what it had managed to do.
const TrapScript = `delegate_started=$(date +%s)
trap 'echo "` + TerminatedMarker + ` SIGTERM after $(( $(date +%s) - delegate_started ))s ---"; ` +
	`tail -20 /tmp/agent.log 2>/dev/null; exit 15' TERM
trap 'echo "` + TerminatedMarker + ` SIGINT ---"; tail -20 /tmp/agent.log 2>/dev/null; exit 130' INT
`

// LockTestsScript makes the specification unwritable.
//
// PREFER MAKING A MISTAKE UNREPRESENTABLE OVER FORBIDDING IT. The rules already
// say the tests may not be edited; this is what makes it true for an agent that
// decides otherwise.
const LockTestsScript = "chmod a-w *_test.go 2>/dev/null || true\n"

// PreviousDiffScript shows a retrying agent what it already tried.
//
// AS A DIFF RATHER THAN AS AN INSTRUCTION. "Do not repeat the same change" is a
// sentence a fresh process has no way to obey: each attempt starts with no
// memory, so it re-derives the same wrong theory from the same failure and
// writes the same code again. Measured on r70: four attempts on one ticket, each
// ending with a confident summary blaming "routing issues", while the actual
// defect was six lines away in Add() — which never assigns an ID, so the test's
// URL is /api/tasks/ with an empty id and the handler rejects its own request.
//
// Its own diff is the only thing that makes "you already tried that" CHECKABLE
// rather than merely asserted.
const PreviousDiffScript = `if git rev-parse HEAD~1 >/dev/null 2>&1; then
  echo
  echo "YOUR PREVIOUS ATTEMPT MADE THIS CHANGE, AND THE TESTS ABOVE STILL FAIL."
  echo "Whatever theory produced it was wrong. Look somewhere else:"
  git diff HEAD~1 -- '*.go' ':!*_test.go' | head -120
fi
`

// SettingsScript writes the agent's configuration.
func SettingsScript() string {
	return "mkdir -p \"$HOME/.qwen\"\ncat > \"$HOME/.qwen/settings.json\" <<'QWENSETTINGS'\n" +
		Settings() + "\nQWENSETTINGS\n"
}

// PublishFailingScript records which tests are failing and publishes the list so
// the Go side can carry it into the next attempt.
func PublishFailingScript(testCommand string, baseline []string) string {
	return fmt.Sprintf(`%s > /tmp/failing.txt 2>&1 || true
grep -E "^[[:space:]]*--- FAIL" /tmp/failing.txt | sed "s/^[[:space:]]*--- FAIL: //; s/[[:space:]].*//" | sort -u > /tmp/failing-names.txt
printf '%%s\n' %s | tr ' ' '\n' | grep -v '^$' | sort -u > /tmp/baseline-names.txt
echo "%s$(tr '\n' ' ' < /tmp/failing-names.txt)"
`, testCommand, forge.Quote(strings.Join(baseline, " ")), FailingMarker)
}

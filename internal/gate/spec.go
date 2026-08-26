package gate

import (
	"fmt"
	"strings"
)

// The specification author's gate is the one that reads a PASSING suite as a
// failure, so it needs to say which kind of pass it saw.
const (
	// SpecVacuousMarker distinguishes "your tests passed" from "your tests do not
	// parse". Both are a non-zero exit from the same script.
	SpecVacuousMarker = "<<<SPEC-PASSED-AGAINST-CURRENT-CODE>>>"

	// SpecAlreadyBuiltMarker distinguishes "your tests passed because they assert
	// nothing" from "your tests passed because the behaviour is ALREADY BUILT".
	//
	// Both are a green suite from an author that is supposed to write red, and
	// the gate used to call both vacuous. That is wrong whenever the
	// implementation is already on the branch, and sections make it routine: the
	// sections of one task SHARE a branch, so whichever lands first leaves its
	// code there for the rest.
	//
	// Measured on r67. "Implement basic task operations" was speccing a store
	// that the sibling section "concurrent access control" had already built and
	// merged — Add, Get, List, Update, Delete, all green. The author diagnosed it
	// correctly and said so every turn ("the tests are already written but
	// they're passing against the existing implementation"), then had no move:
	// the only way to go red was to write a test it knew to be wrong. It rewrote
	// the correct file instead, which is byte-identical, which is a no-op refusal
	// — 18 times, and it would have run to the 200-turn budget.
	SpecAlreadyBuiltMarker = "<<<SPEC-ALREADY-IMPLEMENTED>>>"
)

// SpecVerdict is what the author's gate decided.
type SpecVerdict int

const (
	// SpecRed is the wanted outcome: the tests compile and fail, which is what
	// the developer is given to fix.
	SpecRed SpecVerdict = iota

	// SpecVacuous is a suite that passes before the implementation exists. It is
	// not a specification, and it must be sent back.
	SpecVacuous

	// SpecAlreadyBuilt is a suite that passes because its subject is already
	// there. That is finished work, not a fault.
	SpecAlreadyBuilt

	// SpecBroken is a suite that does not parse or compile. The author's own
	// mistake to correct.
	SpecBroken
)

// ReadSpec classifies one run of SpecScript.
//
// GREEN HAS TWO CAUSES AND THEY NEED OPPOSITE ANSWERS, so the script is asked
// which one it saw rather than the caller assuming. Telling an author whose
// subject is already built that "the implementation has not been written yet" is
// simply false, and it leaves the agent with no move that is both honest and
// accepted.
func ReadSpec(output string, ok bool) SpecVerdict {
	switch {
	case strings.Contains(output, SpecAlreadyBuiltMarker):
		return SpecAlreadyBuilt
	case strings.Contains(output, SpecVacuousMarker):
		return SpecVacuous
	case ok:
		return SpecRed
	default:
		return SpecBroken
	}
}

// SpecScript checks the specification is well-formed AND that it fails.
//
// RED IS THE POINT OF A TEST-FIRST STAGE. Checking only that the tests parse
// accepts any syntactically valid file, including one that asserts nothing, and
// the cost of that lands two stages later: the developer's gate passes on the
// first commit because there is nothing to satisfy, the reviewer sees green, and
// unverified code merges looking exactly like verified code. A specification
// that passes before the implementation exists is not a specification.
//
// REPAIRING RELAXES THE RED REQUIREMENT, NOT THE COMPILE CHECK, and conflating
// the two was the first version of this and it was worse than the problem.
// Skipping the test command entirely meant nothing compiled the tests — and the
// parse check uses gofmt, which catches syntax errors and NOT type errors like
// "no new variables on left side of :=". That is exactly the fault the developer
// sends a specification back for. Measured immediately: the author passed its
// gate, pushed, and the developer found the identical error and handed it
// straight back.
func SpecScript(testCommand string, repairing bool) string {
	if testCommand == "" {
		return SpecParseScript
	}

	if repairing {
		return SpecParseScript + fmt.Sprintf(`
echo '--- the tests must COMPILE; they are not required to fail on a repair ---'
cat > /tmp/.gate-cmd <<'BLACKSMITH_GATE_CMD'
{ %s ; }
BLACKSMITH_GATE_CMD
rc=0
out=$(timeout %d sh /tmp/.gate-cmd 2>&1) || rc=$?
echo "$out" | tail -40
if [ "$rc" -eq 124 ]; then
  echo '%s'
fi
echo "checks ran (exit $rc); on a repair only compilation is required"
`, testCommand, TestTimeoutSeconds, TimedOutNotice)
	}

	// THE FAILURE TEXT IS ALWAYS PRINTED, and it used to be printed only when the
	// tests PASSED. That looks like a tidy-output decision and is not: the caller
	// classifies the failure by reading this output, separating "undefined:
	// NewStore" — the expected red state — from a fault in the tests themselves.
	// Discarding the text on the red path left that check with nothing to read,
	// so it never fired, and a specification whose own tests did not COMPILE was
	// accepted as valid red.
	//
	// Measured on the dev fixture: `no new variables on left side of :=` passed
	// this gate twice. The author saw exit 0, believed its tests were fine and
	// pushed them back unchanged, while the developer — which could see the error
	// but may not edit tests — handed the ticket straight back. Two agents, each
	// correct on what it could see, ping-ponging one broken file.
	//
	// THE QUESTION IS WHETHER THE CODE EXISTS, NOT WHERE IT CAME FROM. The first
	// version diffed the branch against its base, which answers "did THIS BRANCH
	// add an implementation" — a different question, and wrong by exactly one
	// route. Sections of a task share a branch, so a sibling's work reaches an
	// author two ways: still on the shared branch, where a diff sees it, or
	// already MERGED to the base, where a diff cannot. Measured across both runs:
	// r67 hit the first and the diff caught it; r68 hit the second — main.go held
	// Task, Store, NewStore, Add, Get, List, Update and Delete, the branch diff
	// showed one test file, so the exemption missed and the author looped 60
	// times.
	//
	// Counting EXPORTED declarations in non-test files answers the real question
	// wherever the code lives. The pristine baseline has none — package main with
	// a single func main — so a stub reads as zero and any real implementation
	// reads as more.
	return SpecParseScript + fmt.Sprintf(`
echo '--- the specification must fail against the current code ---'
cat > /tmp/.gate-cmd <<'BLACKSMITH_GATE_CMD'
{ %s ; }
BLACKSMITH_GATE_CMD
rc=0
out=$(timeout %d sh /tmp/.gate-cmd 2>&1) || rc=$?
echo "$out" | tail -40
if [ "$rc" -eq 124 ]; then
  echo '%s'
  exit 1
fi
if [ "$rc" -eq 0 ]; then
  srcs=$(find . -name '*.go' ! -name '*_test.go' -not -path './.git/*' -not -path './vendor/*') || true
  impl=0
  if [ -n "$srcs" ]; then
    impl=$(echo "$srcs" | xargs grep -hE '^(type [A-Z]|func [A-Z]|func \([a-z]+ \*?[A-Z])' | wc -l)
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
`, testCommand, TestTimeoutSeconds, TimedOutNotice,
		SpecAlreadyBuiltMarker, SpecVacuousMarker)
}

// SpecParseScript checks the TEST files for syntax errors without compiling.
//
// `gofmt -l -e` prints a file that is not correctly formatted and reports parse
// errors on stderr; the exit code is what distinguishes them. Formatting alone
// must not fail the stage — the commit hook already runs the formatter — so this
// looks only for a genuine parse error.
//
// IT NAMES THE TEST FILES RATHER THAN ".". Measured on r92: this took 8.5
// seconds per section while the developer's gate, which runs the same git
// preamble AND `go test ./...`, took 512 milliseconds. The difference was the
// walk — "." descends the whole tree, and the check is about the files the
// author is allowed to write. Narrowing it agrees with the guard directly above,
// which already looks for *_test.go in the root, and with the message the author
// is given, which says the TESTS do not parse.
const SpecParseScript = `
tests=$(find . -name '*_test.go' -not -path './.git/*' -not -path './vendor/*' | sort)
if [ -z "$tests" ]; then
  echo "no test files were written"
  exit 1
fi
# The "|| true" is load-bearing: the preamble sets -e, and gofmt EXITS NON-ZERO
# on a parse error — which is the case this check exists to catch. Without it the
# script dies on the assignment, the error text is never echoed, and the stage
# reports a failure with an empty body. Observed exactly that way: an author was
# told "the tests do not parse" with nothing after it, had no idea what was
# wrong, and rewrote the file blindly until its budget ran out.
err=$(echo "$tests" | xargs gofmt -e -l 2>&1 >/dev/null) || true
if [ -n "$err" ]; then
  echo "$err"
  exit 1
fi
echo "--- tests written ---"
echo "$tests"
`

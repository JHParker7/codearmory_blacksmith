// Package gate builds the scripts that decide whether a branch is good, and
// reads back what they said.
//
// EVERY GATE RUNS AGAINST THE PUSHED BRANCH, never against the tree the agent
// has been editing. That distinction is the whole point of verifying: a run
// where the two differ is precisely the run worth catching, and one of them left
// an agent being told for twenty turns that a file it had written did not exist.
//
// CHEAPEST AND MOST PRECISE FIRST, stopping at the first failure. A linter knows
// the file and the line; a test runner reports the same fault as a wall of
// output with the cause buried in it. Running both and reporting everything
// would hand the model a linter complaint and a hundred lines of test noise for
// one mistake, and it has a fixed budget to spend.
package gate

import (
	"fmt"
	"strings"
)

// Marker is what a failing gate prints so the harness can name which one it was.
const Marker = "HARNESS_GATE_FAILED="

// The advisory fence. What falls between these is reported to the agent as
// something it MAY act on, and never fails a branch.
const (
	AdviceOpen  = "===ADVISORY-OPEN==="
	AdviceClose = "===ADVISORY-CLOSE==="
)

// Reason codes a gate can report. A CLOSED SET, because they become a metric
// label and an unbounded one is a cardinality bug.
const (
	ReasonTest       = "test"
	ReasonBuild      = "build"
	ReasonTestImport = "test-imports-in-production"
	ReasonCritical   = "critical-security"
	ReasonUnknown    = "checks"
)

// Bounds for the source quoted beside a failure.
//
// A run with forty failures must not paste the whole suite into the prompt: the
// point is to answer "what does the failing line say", not to mirror the
// repository.
const (
	MaxFailureSites      = 6
	FailureContextBefore = 6
	FailureContextAfter  = 4
)

// Failed reads the marker a script leaves.
//
// "checks" IS THE HONEST ANSWER when nothing was marked — a sandbox that died
// before any gate ran did not fail a gate, and naming one would send the reader
// to a command that never executed.
//
// The LAST marker wins: the script stops at the first failure, but a test's own
// output can contain the marker text, and what the harness wrote is always last.
func Failed(output string) string {
	i := strings.LastIndex(output, Marker)
	if i < 0 {
		return ReasonUnknown
	}
	rest := output[i+len(Marker):]
	if j := strings.IndexAny(rest, " \n\r"); j >= 0 {
		rest = rest[:j]
	}
	if rest == "" {
		return ReasonUnknown
	}
	return rest
}

// Options describes the commands this repository verifies with.
type Options struct {
	// LintCommand runs FIRST and cannot fail the run. It is here for the fast,
	// precise feedback — a file and a line, before the test runner buries the same
	// fault in output — not to decide anything.
	LintCommand string

	// TestCommand is the gate. "Does this work" is binary and answerable, which is
	// what makes it the one check allowed to be a gate at all.
	TestCommand string

	// CriticalCommand runs AFTER the tests, deliberately: a change that does not
	// build or pass cannot be meaningfully analysed — most scanners need the code
	// to compile — and a broken change should hear about being broken first.
	CriticalCommand string
}

// Script composes the gates in order.
//
// ONE SANDBOX, NOT ONE PER GATE: the clone and the toolchain warm-up dominate,
// and paying them three times to run three commands would make the fast feedback
// this exists for slower than what it replaced.
func Script(o Options) string {
	var b strings.Builder

	if o.LintCommand != "" {
		// BRACED, for the same reason the test gate is. A configured command may be a
		// chain, and both `2>&1` and `|| true` bind to its LAST element rather than to
		// the whole of it — so an earlier element that fails runs unguarded, and under
		// the sandbox preamble's `set -e` that aborts the entire verification before
		// the tests are ever reached. An advisory check must not be able to do that.
		fmt.Fprintf(&b, "echo '%s'\n{ %s ; } 2>&1 || true\necho '%s'\n",
			AdviceOpen, o.LintCommand, AdviceClose)
	}

	// BEFORE the tests, because a production file importing a testing package
	// makes every later result meaningless — the thing under test is no longer the
	// thing that ships.
	b.WriteString(TestImportScript())

	if o.TestCommand != "" {
		b.WriteString(TestScript(o.TestCommand))
	}

	if o.CriticalCommand != "" {
		// Braced for the same reason: unbraced, a chain whose last element succeeds
		// reports success however the earlier ones went, and one whose earlier
		// element fails never reaches the marker — so the gate that DOES block would
		// block without saying which check blocked it.
		fmt.Fprintf(&b, "echo '--- critical analysis ---'\nif { %s ; }; then :; else echo '%s%s'; exit 1; fi\n",
			o.CriticalCommand, Marker, ReasonCritical)
	}
	return b.String()
}

// TestScript runs the test command and, when it fails, QUOTES THE SOURCE the
// failure points at.
//
// A failing assertion names a file and a line and nothing else about it. The
// developer's next move is therefore always the same: spend a turn reading the
// file to see what that line actually asserts. That is a whole model round trip,
// on every failure, to fetch five lines the sandbox is already standing next to.
// Printing them here costs nothing and removes the round trip entirely.
//
// The test file is the SPECIFICATION and the developer may not edit it, so this
// is the one thing it most needs to see and the one thing it cannot change.
func TestScript(testCommand string) string {
	// BRACED, BECAUSE THE COMMAND MAY BE A CHAIN. A redirect binds to the last
	// element of an && chain rather than to the whole of it — so with
	// "go build ./... && go test ./..." the build's output escaped to the console
	// and, when the build failed, the test never ran and never created the file
	// the branches below read. The agent was handed "No such file or directory"
	// above its real error, which is a refusal pointing at nothing.
	//
	// Grouping captures the whole chain, so a build error lands in the file where
	// the source-quoting search can find the file and line it names.
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
`, testCommand, MaxFailureSites, FailureContextBefore, FailureContextAfter, Marker, ReasonTest)
}

// TestImportOffenders lists the non-test Go files importing a testing package,
// leaving the list in $bad.
//
// ONE DEFINITION, shared by the GATE that fails the branch on it and by the
// prompt that has to SHOW the agent the same thing. The two were briefly
// separate, and that is a bug with a long fuse: the gate rejected the branch
// while the agent's own test run — which compiles such a file happily — never
// mentioned it, so the developer would have been failed for a reason it was
// never told.
//
// Guarded on the module file so a repository in another language simply skips it.
func TestImportOffenders() string {
	return `
prod=$(ls *.go 2>/dev/null | grep -v '_test\.go$' || true)
bad=""
if [ -f go.mod ] && [ -n "$prod" ]; then
  bad=$(grep -lE '"testing"|"net/http/httptest"' $prod 2>/dev/null || true)
fi
`
}

// TestImportAdvice is what the offender is told, in both places.
func TestImportAdvice() string {
	return `echo "These files are NOT tests and must not import a testing package:"
      echo "$bad"
      echo
      echo "A testing package in shipped code means the code was written to satisfy the"
      echo "test rather than to do the job: a handler taking an httptest recorder, or one"
      echo "that builds its own store instead of using the one it is given. Use the types"
      echo "and the values the caller passes in. Remove the import and make the real"
      echo "parameters carry the work."`
}

// TestImportScript rejects a production file that imports a testing package.
//
// THE MOST EXPENSIVE SINGLE THING A DELEGATED AGENT DID IN A RUN. Asked to add
// validation, it wrote a production handler taking an httptest recorder, which
// added the task to a store it invented — so the store the caller passed in
// never saw it. The suite went red, the agent called the failure "an
// infrastructure issue with URL routing causing 301 redirects", and burned its
// attempts there.
//
// A GATE rather than advice because the rule is ABSOLUTE in Go: the testing
// packages have no legitimate place in a non-test file, so this can never
// produce a false accusation, and it names the cause exactly. The agent could
// not find this fault by reading assertion output — every failing assertion
// pointed at the test, and the test was right.
func TestImportScript() string {
	return fmt.Sprintf(`
echo '--- test-only imports in production code ---'
%s
if [ -n "$bad" ]; then
      %s
      echo '%s%s'
      exit 1
fi
echo 'no testing packages in production files'
`, TestImportOffenders(), TestImportAdvice(), Marker, ReasonTestImport)
}

// StripToolChatter removes output that says nothing about the change.
//
// A PANIC BURIES ITS OWN CAUSE. When a test panics — and a specification that
// asserts a length and then indexes the result panics whenever the length is
// wrong — the runtime prints the message and then twenty-odd frames of testing
// internals. Those frames are the same on every failure there has ever been.
// They are dropped; frames in the repository's own files are kept, because those
// name the line to look at.
//
// Measured: the assertion "should have 2 item(s), but has 0" was line two of the
// output and everything after it was stack. That one line is the whole diagnosis.
func StripToolChatter(out string) string {
	lines := strings.Split(out, "\n")
	kept := make([]string, 0, len(lines))

	for i, l := range lines {
		if strings.HasPrefix(l, "go: downloading ") || strings.HasPrefix(l, "go: extracting ") {
			continue
		}
		// A frame is two lines: the function, then an indented file:line. Dropping
		// the first without the second would leave an orphaned path.
		if isStdlibFrame(l) || (i > 0 && isStdlibFrame(lines[i-1]) && strings.HasPrefix(l, "\t")) {
			continue
		}
		kept = append(kept, l)
	}
	return strings.Join(kept, "\n")
}

// isStdlibFrame reports whether a stack frame belongs to the runtime or the
// testing package rather than to the repository.
func isStdlibFrame(l string) bool {
	t := strings.TrimSpace(l)
	if strings.HasPrefix(t, "/usr/local/go/src/") || strings.HasPrefix(t, "/usr/lib/go/src/") {
		return true
	}
	for _, p := range []string{
		"testing.tRunner", "testing.(*T).Run", "runtime.gopanic", "runtime.goexit",
		"created by testing.",
	} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

// Advice extracts what the advisory gates said, for a prompt that reports
// findings the agent MAY act on without any of them failing the branch.
func Advice(out string) string {
	start := strings.Index(out, AdviceOpen)
	if start < 0 {
		return ""
	}
	rest := out[start+len(AdviceOpen):]
	end := strings.Index(rest, AdviceClose)
	if end < 0 {
		return strings.TrimSpace(rest)
	}
	return strings.TrimSpace(rest[:end])
}

// Deciding whose fault a failing verification is.
//
// The developer may not edit tests, so a specification it cannot satisfy is a dead
// end unless something routes it back to the author. These are the mechanical
// routes — a compile error local to a test file, a panic raised entirely inside
// one, a toolchain message that points away from its own cause. The judgement
// call for everything else lives in agent_referee.go.

package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// gateMarker is printed by the verify script when a gate fails, so the failure
// can be attributed to the right tool rather than to whatever ran last.
// compileErrorFiles pulls the files named by Go compile errors out of a gate's
// output, and reports whether EVERY one of them is a test file.
//
// The distinction is the whole point. A compile error in main.go is the
// developer's to fix. A compile error in a *_test.go file is not — the developer
// is forbidden to edit tests, by design, so no action available to it can clear
// the failure. It is a BROKEN SPECIFICATION, and the only useful thing to do is
// say so and stop.
//
// Measured, and it is why this exists: a spec dereferenced Task.Title as a
// pointer while treating Task.Done as a value — internally inconsistent, and
// impossible to satisfy without contradicting the architecture document. The
// developer spent 100 turns at ~11 a minute discovering that, and would have
// spent 200.
//
// Only a COMPILE error counts. A failing assertion in a test file is the normal
// state of test-first development and means the code is not written yet.
// packageConflictFiles names the files in a "found packages X (a.go) and Y
// (b.go)" failure, or nothing when the output holds no such conflict.
//
// IT IS CONCLUSIVE ON THE FIRST VERIFICATION, like a panic raised inside the
// tests and for the same reason: no edit to the implementation can reach it. The
// package name comes from the TEST file, which this stage may not edit, so every
// implementation file beside it must match a declaration the developer cannot
// change. There is no legal move.
//
// It needs its own matcher because the toolchain reports it WITHOUT a file, line
// and column, so goCompileError does not see it and neither does anything built
// on compileErrorFiles. Measured on r75: the tree stopped compiling, the
// hand-back that exists for exactly this never fired, and the developer looped
// on an unfixable failure until its budget ran out — 24 turns and climbing when
// the run was stopped.
func packageConflictFiles(out string) []string {
	m := goPackageConflict.FindStringSubmatch(out)
	if m == nil {
		return nil
	}
	return []string{m[2], m[4]}
}

// goPackageConflict matches Go's "found packages a (a.go) and b (b.go) in DIR",
// which is reported per directory rather than per line — hence no position.
var goPackageConflict = regexp.MustCompile(`found packages (\w+) \(([\w.-]+\.go)\) and (\w+) \(([\w.-]+\.go)\)`)

func compileErrorFiles(out string) (files []string, allTests bool) {
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		l := strings.TrimSpace(line)
		l = strings.TrimPrefix(l, "./")
		m := goCompileError.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		if !seen[m[1]] {
			seen[m[1]] = true
			files = append(files, m[1])
		}
	}
	if len(files) == 0 {
		return nil, false
	}
	for _, f := range files {
		if !isTestFile(f) {
			return files, false
		}
	}
	return files, true
}

// goCompileError matches "path/file.go:12:34: message", which is how the Go
// toolchain reports a compile error. The column is required: a test FAILURE logs
// "file_test.go:96:" with no column, and those are ordinary red tests rather
// than a broken spec.
var goCompileError = regexp.MustCompile(`^([\w./-]+\.go):\d+:\d+: `)

// intrinsicTestFaults are compile errors whose cause is entirely inside the file
// that reports them. NO implementation change can affect one.
//
// The distinction is the whole reason the streak counters exist. Go reports a type
// error at the USE SITE, and under test-first that site is always a test file: the
// measured case was a specification using Task.DueDate as a value while the
// DEVELOPER had declared it *time.Time — one line of the implementation from
// fixed, and blaming the spec for it abandoned tickets that were nearly done.
// "undefined: NewStore" is the same story, and is the expected red state of every
// test-first ticket.
//
// These are not that. "declared and not used" and "no new variables on left side
// of :=" are local facts about that file's own text. There is no implementation
// that makes them go away, so they need no streak and no corroboration — they are
// conclusive on sight. Waiting for the developer to prove it by flailing is what
// cost r57 (`declared and not used: tasks`) and r62 (`no new variables`): both
// spent every attempt on a verdict already visible in the first test run.
var intrinsicTestFaults = []string{
	"declared and not used",
	"no new variables on left side of :=",
	"imported and not used",
	"syntax error",
	"missing return",
	"unexpected newline",
	"non-declaration statement outside function body",
}

// intrinsicTestErrors returns the test-file compile errors that no implementation
// change could fix.
func intrinsicTestErrors(out string) []string {
	var found []string
	for _, line := range strings.Split(out, "\n") {
		l := strings.TrimPrefix(strings.TrimSpace(line), "./")
		m := goCompileError.FindStringSubmatch(l)
		if m == nil || !isTestFile(m[1]) {
			continue
		}
		msg := strings.ToLower(l[len(m[0]):])
		for _, p := range intrinsicTestFaults {
			if strings.Contains(msg, p) {
				found = append(found, l)
				break
			}
		}
	}
	return found
}

// panicOnlyInTests names the test files in a panic whose every REPO frame is a
// test file — which makes the panic the specification's own defect.
//
// Runtime failures were deliberately kept out of the hand-back evidence, and the
// reasoning was sound: a panic is normally the implementation misbehaving under a
// test, which is exactly the developer's job. But it is not always. Measured on
// r68, a section stalled on
//
//	panic: invalid NewRequest arguments; malformed HTTP version "2 HTTP/1.0"
//	  tracker.TestStatusParameterFiltering.func3() handlers_status_test.go:189
//
// from httptest.NewRequest("GET", "/api/tasks?q=Task 2", nil) — an unescaped
// space in the query string, so "2 HTTP/1.0" parsed as the version. The tests
// COMPILE, so the compile route could not see it; the developer diagnosed it
// correctly, may not edit test files, and tried to compensate inside main.go
// instead, landing statements at file scope and breaking the build repeatedly.
//
// THE STACK SETTLES WHOSE FAULT IT IS. A panic the implementation could cause
// puts an implementation frame in the trace; this one names no repo file but the
// test. Toolchain frames are excluded by their GOROOT path, which always carries
// a /src/ segment, so what remains is this module's own code.
func panicOnlyInTests(out string) []string {
	if !strings.Contains(out, "panic:") {
		return nil
	}
	var tests []string
	seen := map[string]bool{}
	for _, m := range stackFrameFile.FindAllStringSubmatch(out, -1) {
		path := m[1]
		if strings.Contains(path, "/src/") {
			continue // Go's own source, not this repo's
		}
		base := path[strings.LastIndex(path, "/")+1:]
		if !isTestFile(base) {
			// A repo frame outside the tests: the implementation is in the trace,
			// so this panic is the developer's to fix after all.
			return nil
		}
		if !seen[base] {
			seen[base] = true
			tests = append(tests, base)
		}
	}
	return tests
}

// explainConfusingFailure translates toolchain errors whose WORDING points away
// from their cause, so the agent reading them is not sent to the wrong place.
//
// Most failures say what is wrong. A few actively mislead, and those cost far
// more than their frequency suggests: the agent's reasoning is sound, it acts on
// what the message says, and no number of retries converges because every attempt
// is aimed at the wrong argument. Measured on r68 across two files — a test built
// httptest.NewRequest("GET", "/api/tasks?q=Task 1", nil) and Go reported
// `malformed HTTP version "1 HTTP/1.0"`, because NewRequest splits the target on
// whitespace and reads the tail as a version. The author concluded, twice and in
// two different sections, that "the method parameter is being" passed wrongly.
// The method is fine; the space in the query string is the bug.
//
// A TABLE RATHER THAN A SPECIAL CASE, because the property that matters is not
// this message — it is that a message can point away from its cause, and the
// harness is where that gets corrected once for every agent that meets it.
func explainConfusingFailure(out string) string {
	for _, e := range confusingFailures {
		if strings.Contains(out, e.marker) {
			return e.explain
		}
	}
	return ""
}

var confusingFailures = []struct{ marker, explain string }{
	{
		marker: "malformed HTTP version",
		explain: "WHAT THIS ERROR ACTUALLY MEANS: the request TARGET contains a raw space, not that " +
			"the method or the version is wrong. httptest.NewRequest splits its second argument on " +
			"whitespace and reads what follows as the HTTP version, so \"/api/tasks?q=Task 1\" is " +
			"parsed as target \"/api/tasks?q=Task\" and version \"1\". Escape the query value — " +
			"\"?q=Task+1\" or url.QueryEscape(\"Task 1\") — and leave the method alone.",
	},
	{
		marker: "assignment to entry in nil map",
		explain: "WHAT THIS ERROR ACTUALLY MEANS: a map was declared but never made. The fix is " +
			"make(map[K]V) where the value is constructed — a struct literal that omits the map " +
			"leaves it nil, and writing to it panics however correct the surrounding code is.",
	},
}

// brokenSpecMarker records a ticket stopped because its tests do not compile.
// Distinct from any other stop reason because the work it asks for is different:
// nobody should re-run the developer, and the fix is in a file the developer may
// not touch.
const brokenSpecMarker = "**Stopped: the specification does not compile.**"

// specRepairMarker records a hand-back to the specification's author. It is
// counted off the ticket's own comments rather than held in memory, because the
// developer that writes it and the developer that reads it are different
// attempts in different processes — the same reason revivalsSoFar counts this
// way.
const specRepairMarker = "**Returned: the specification does not compile.**"

// specRepairsSoFar counts how many times this ticket has already gone back to
// its specification author.
func specRepairsSoFar(t Ticket) int {
	n := 0
	for _, c := range t.Comments {
		// A RETRY IS A CLEAN SLATE, exactly as it is for the attempt count. A
		// ticket put back after blocking that arrives with its hand-backs already
		// spent gets one turn and blocks again, which is not a retry — it is a
		// slower way to reach the same place.
		if strings.Contains(c.Body, retryMarker) {
			n = 0
			continue
		}
		if strings.Contains(c.Body, specRepairMarker) {
			n++
		}
	}
	return n
}

// testFileInNotice pulls the file name out of a test-file refusal, which names
// it first: `<path> is a test file, and tests are written by ...`.
func testFileInNotice(notice string) string {
	const marker = " is a test file"
	i := strings.Index(notice, marker)
	if i < 0 {
		return ""
	}
	head := strings.TrimSpace(notice[:i])
	if j := strings.LastIndexAny(head, " :"); j >= 0 {
		head = head[j+1:]
	}
	if !isTestFile(head) {
		return ""
	}
	return head
}

// specFaultKind describes HOW the specification is unsatisfiable, because the
// two cases read very differently to whoever picks the ticket up. A spec that
// will not compile is unambiguous; one that compiles and fails is a judgement,
// and saying "do not COMPILE" about it would send the author looking for a
// syntax error that is not there.
func specFaultKind(compileBroke string) string {
	if compileBroke != "" {
		return "do not COMPILE"
	}
	return "FAIL, and the developer could not make them pass without editing them"
}

// specFaultKindFor is specFaultKind with the panic case named, because "FAIL"
// sends an author looking at its assertions when the fault is in the fixture it
// built before any assertion ran.
func specFaultKindFor(compileBroke string, panicked bool) string {
	if panicked && compileBroke == "" {
		return "PANIC before their assertions run, from the tests' own setup"
	}
	return specFaultKind(compileBroke)
}

// endOnBrokenSpec ends the attempt when the specification has been judged
// unsatisfiable: back to its author while there is budget for that, and stopping
// for a person once there is not.
//
// BACK TO THE AUTHOR FIRST. This used to stop dead, reasoning that a
// specification which contradicts itself is a decision to make rather than work
// to redo. That holds for a contradiction. It does not hold for what actually
// arrives, which is a mechanical defect: r57 stranded an entire nineteen-ticket
// board on `declared and not used: tasks`, and the one agent allowed to fix it
// was never asked.
//
// Bounded, because an author that cannot fix its own tests twice will not manage
// it on a third pass, and then a person really is the right answer.
func (a *DevAgent) endOnBrokenSpec(ctx context.Context, t Ticket, s *devState, out string) (string, string, bool) {
	if s.specBroken == "" {
		return "", "", false
	}
	if repairs := specRepairsSoFar(t); repairs < maxSpecRepairs {
		a.comment(ctx, t, fmt.Sprintf(
			"%s\n\nThe tests in `%s` %s, and this stage may not edit test files — so no change "+
				"to the implementation could make them pass. The fault looks to be in the "+
				"specification, not in the code.\n\nGoing back to the agent that CAN correct it. "+
				"Repair attempt %d of %d.\n\n```\n%s\n```%s",
			specRepairMarker, s.specBroken, specFaultKindFor(s.lastCompileBroke, len(panicOnlyInTests(out)) > 0),
			repairs+1, maxSpecRepairs,
			clipEnds(out, 600, 1200)+explainSuffix(out), trailSuffix(s.trail)))
		return OutcomeReturned, "specification is unsatisfiable; returned to its author", true
	}
	a.comment(ctx, t, fmt.Sprintf(
		"%s\n\nThe tests in `%s` %s, and this stage is not allowed to edit test files — so no "+
			"change to the implementation can make them pass.\n\nIt has been sent back to its "+
			"author %d times and still does not, so a person needs to correct the tests, or the "+
			"ticket needs re-scoping.\n\n```\n%s\n```%s",
		brokenSpecMarker, s.specBroken, specFaultKindFor(s.lastCompileBroke, len(panicOnlyInTests(out)) > 0), maxSpecRepairs,
		clipEnds(out, 600, 1200)+explainSuffix(out), trailSuffix(s.trail)))
	return OutcomeBlocked, "specification is unsatisfiable", true
}

// nonUndefinedCompileErrors returns the compile errors that are NOT "undefined:".
//
// An undefined symbol is the expected red of a specification written before its
// implementation. Everything else — a redeclaration, a type mismatch, a wrong
// argument count — is a defect in the tests, and it must not be mistaken for the
// failure the gate is looking for.
// undefinedSymbol returns the symbol named by an "undefined: X" compile error, or
// "" when the line is not one.
// stdlibPackageNames are the identifiers a Go file refers to a standard library
// package by. A closed set, in the same spirit as the reason codes: anything not
// listed is treated as an ordinary symbol, which is the safe direction — it
// costs a turn, where the opposite costs the whole attempt.
//
// THEY EXIST BECAUSE A MISSING IMPORT LOOKS EXACTLY LIKE A MISSING SYMBOL. Go
// reports both as "undefined: X", and for an unimported package X is the bare
// package name with no dot to give it away. The dotted case was already handled
// — "undefined: sync.atomic" cannot be defined by any code in this package — but
// the far commoner one, a file that uses url.QueryEscape and forgets to import
// net/url, was indistinguishable from the expected red of test-first.
//
// Measured on r82, and it cost the task its whole budget: handlers_read_test.go
// called url.QueryEscape without importing net/url, the compiler said
// "undefined: url", the filter read that as a symbol the implementation had yet
// to write, and the spec was never handed back. The developer — which may not
// edit tests — tried to satisfy it by declaring a QueryEscape helper, which
// cannot work, and then rewrote the same file until its turns ran out.
var stdlibPackageNames = map[string]bool{
	"bufio": true, "bytes": true, "context": true, "crypto": true, "csv": true,
	"errors": true, "exec": true, "filepath": true, "fmt": true, "hex": true,
	"html": true, "http": true, "httptest": true, "io": true, "ioutil": true,
	"json": true, "log": true, "math": true, "net": true, "os": true,
	"rand": true, "reflect": true, "regexp": true, "runtime": true, "slices": true,
	"sort": true, "strconv": true, "strings": true, "sync": true, "template": true,
	"testing": true, "time": true, "unicode": true, "url": true, "utf8": true,
	"atomic": true, "base64": true, "sha256": true, "signal": true, "tls": true,
}

// missingImport reports whether an "undefined: X" names a package the file
// forgot to import rather than a symbol the implementation owes it.
//
// No code the developer writes can answer it: declaring something called url
// does not make url.QueryEscape resolve, because the compiler is looking for a
// package qualifier and not an identifier.
func missingImport(line string) bool {
	u := undefinedSymbol(line)
	return u != "" && stdlibPackageNames[u]
}

func undefinedSymbol(line string) string {
	i := strings.Index(line, "undefined: ")
	if i < 0 {
		return ""
	}
	rest := strings.TrimSpace(line[i+len("undefined: "):])
	if j := strings.IndexAny(rest, " \t("); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

func nonUndefinedCompileErrors(out string) []string {
	var bad []string
	for _, line := range strings.Split(out, "\n") {
		l := strings.TrimSpace(line)
		l = strings.TrimPrefix(l, "./")
		if goCompileError.FindStringSubmatch(l) == nil {
			continue
		}
		// "undefined: X" IS THE EXPECTED RED STATE — but only for an unqualified X.
		//
		// The implementation is missing, so every symbol it should provide is
		// undefined, and writing the code clears it. That is the whole premise of
		// test-first and why this exemption exists.
		//
		// "undefined: sync.atomic" is a different animal. A dotted name is a package
		// selector, and NO code written in this package can define it — the test
		// meant to import sync/atomic and referenced sync.atomic instead. Treating
		// it as expected red hid a genuine specification fault: measured on the
		// implementation fixture, the developer wrote a correct 138-line
		// implementation, the only remaining error was this one, and the hand-back
		// was suppressed because the line began with "undefined:".
		//
		// AND NEITHER IS A BARE PACKAGE NAME. An unimported package is reported as
		// "undefined: url", with no dot to tell it apart from a symbol the
		// implementation has yet to write — see missingImport. Measured on r82: a
		// test called url.QueryEscape without importing net/url, this filter read it
		// as expected red, and the developer spent its whole budget trying to
		// declare a package.
		if u := undefinedSymbol(l); u != "" && !strings.Contains(u, ".") && !missingImport(l) {
			continue
		}
		// "too many errors" IS NOT AN ERROR. It is the marker the Go compiler
		// appends when it stops after ten, and it names no defect — the file and
		// line on it belong to whatever error happened to be tenth.
		//
		// LEFT IN, IT INVERTS THIS FUNCTION'S VERDICT. A specification written
		// before its implementation refers to undefined symbols, every one of them
		// is filtered above as the expected red, and on a slice with more than ten
		// the ONLY line still standing is the cutoff. The gate then tells the author
		// its tests do not compile "and not because the implementation is missing",
		// which is the exact opposite of what happened.
		//
		// Measured on r72: a store slice declaring New, Create and StatusOpen went
		// past ten undefined symbols, and the author was handed
		// "store_create_test.go:50:7: too many errors" as a fault in its own tests.
		// It rewrote the identical correct file 64 times over 20 minutes, each one
		// refused as a no-op edit, and the attempt never finished. The same
		// misreading cost turns on three earlier runs.
		//
		// What the cutoff DOES mean is that this list is incomplete, and a real
		// defect may be hiding past the tenth error. That is the developer's stage
		// to find; claiming a defect that is not in evidence is worse, because the
		// author cannot act on it.
		if strings.Contains(l, "too many errors") {
			continue
		}
		bad = append(bad, l)
		if len(bad) >= 8 {
			break
		}
	}
	return bad
}

// explainSuffix renders explainConfusingFailure for appending under a quoted
// failure, or nothing when the message speaks for itself.
func explainSuffix(out string) string {
	if e := explainConfusingFailure(out); e != "" {
		return "\n\n" + e
	}
	return ""
}
